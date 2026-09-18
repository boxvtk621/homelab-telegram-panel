package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

func TestPostgresHistorySearchStableSnapshotACLAndExpiry(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	database, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	database.now = func() time.Time { return now }
	owner := fmt.Sprintf("hl296-stability-%d", time.Now().UnixNano())
	verified, snapshot := inventoryFixture(owner, 2, now)
	if _, err := database.Import(ctx, verified, snapshot); err != nil {
		t.Fatal(err)
	}
	firstBinding := listAllBindings(t, ctx, database, owner, snapshot.Nodes[0].NodeID, 10)[0]
	secondBinding := listAllBindings(t, ctx, database, owner, snapshot.Nodes[1].NodeID, 10)[0]
	base := now.Add(-time.Minute)
	firstIDs := seedSearchDialog(t, ctx, database, owner, snapshot.Nodes[0].NodeID, firstBinding, base, 0, 3)
	secondIDs := seedSearchDialog(t, ctx, database, owner, snapshot.Nodes[1].NodeID, secondBinding, base, time.Second, 3)

	spec, err := ParseHistorySearchQuery(model.HistorySearchQuery{Q: "stable corpus"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := database.CreateHistorySearch(ctx, owner, spec, HistorySearchQueryHash(spec), 2)
	if err != nil || first.Page.TotalCount != 6 || len(first.Page.Items) != 2 || !first.HasMore {
		t.Fatalf("first page=%+v err=%v", first, err)
	}
	seen := map[string]bool{}
	for _, item := range first.Page.Items {
		seen[item.EntryID] = true
	}

	appendedID := appendSearchEntry(t, ctx, database, owner, snapshot.Nodes[1].NodeID, secondBinding,
		model.HistoryStableUUID("r14-stability-stream", owner+":"+secondBinding.LogicalDialogID), 4, base.Add(10*time.Second))
	stable, err := database.ReadHistorySearch(ctx, owner, spec.Query, first.Position, 2)
	if err != nil || stable.Page.TotalCount != 6 || len(stable.Page.Items) != 2 {
		t.Fatalf("stable page=%+v err=%v", stable, err)
	}
	for _, item := range stable.Page.Items {
		if item.EntryID == appendedID || seen[item.EntryID] {
			t.Fatalf("snapshot admitted append or duplicate: %+v", item)
		}
	}

	if _, err := database.pool.Exec(ctx, `UPDATE agent_service.logical_dialogs SET deleted_at=$3
		WHERE owner_id=$1 AND logical_dialog_id=$2`, owner, firstBinding.LogicalDialogID, now); err != nil {
		t.Fatal(err)
	}
	filtered, err := database.ReadHistorySearch(ctx, owner, spec.Query, first.Position, 2)
	if err != nil || filtered.Page.TotalCount != 3 {
		t.Fatalf("filtered page=%+v err=%v", filtered, err)
	}
	for _, item := range filtered.Page.Items {
		if item.LogicalDialogID == firstBinding.LogicalDialogID || item.EntryID == appendedID {
			t.Fatalf("deleted or appended row escaped snapshot guard: %+v", item)
		}
	}
	if _, err := database.HistoryEntryByID(ctx, owner, firstBinding.LogicalDialogID, firstIDs[2]); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted exact entry err=%v", err)
	}
	lookup, err := database.HistoryEntryByID(ctx, owner, secondBinding.LogicalDialogID, secondIDs[2])
	if err != nil || lookup.Entry.EntryID != secondIDs[2] {
		t.Fatalf("offline exact lookup=%+v err=%v", lookup, err)
	}
	secondStreamID := model.HistoryStableUUID("r14-stability-stream", owner+":"+secondBinding.LogicalDialogID)
	messageRankID := appendSearchEntryText(t, ctx, database, owner, snapshot.Nodes[1].NodeID, secondBinding,
		secondStreamID, 5, base.Add(20*time.Second), "weighted exact phrase")
	toolRankID := appendSearchEntryText(t, ctx, database, owner, snapshot.Nodes[1].NodeID, secondBinding,
		secondStreamID, 6, base.Add(21*time.Second), "neutral tool target")
	titleRankID := appendSearchEntryText(t, ctx, database, owner, snapshot.Nodes[1].NodeID, secondBinding,
		secondStreamID, 7, base.Add(22*time.Second), "neutral title target")
	metadata := model.HistoryDialogMetadata{SchemaID: model.HistoryMetadataSchemaID, NodeID: snapshot.Nodes[1].NodeID,
		NodeDialogID: secondBinding.NodeDialogID, BindingGeneration: secondBinding.BindingVersion,
		Title: "weighted exact phrase", UpdatedAt: now.Format(time.RFC3339Nano)}
	if _, err := database.UpsertHistoryDialogMetadata(ctx, owner, secondBinding.LogicalDialogID, metadata); err != nil {
		t.Fatal(err)
	}
	toolText := []byte("weighted exact phrase")
	toolTextID := model.HistoryStableUUID("r14-ranking-tool", owner)
	toolAttemptID := model.HistoryStableUUID("r14-ranking-attempt", owner)
	chain := strings.Repeat("b", 64)
	rankTx, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rankTx.Rollback(context.Background()) }()
	if _, err := rankTx.Exec(ctx, `INSERT INTO agent_service.history_replica_records(
		owner_id,stream_id,stream_seq,record_id,record_type,entity_id,revision,record_hash,prev_hash,chain_hash,record_json)
		VALUES($1,$2,8,$3,'text_manifest',$4,1,$5,$5,$5,'{}')`, owner, secondStreamID,
		model.HistoryStableUUID("r14-ranking-record", owner), toolTextID, chain); err != nil {
		t.Fatal(err)
	}
	if _, err := rankTx.Exec(ctx, `INSERT INTO agent_service.history_text_manifests(
		owner_id,logical_dialog_id,source_stream_id,source_stream_seq,text_id,manifest_hash,attempt_id,complete,size_bytes,text_sha256,chunk_count,payload)
		VALUES($1,$2,$3,8,$4,$5,$6,true,$7,$8,0,'{}')`, owner, secondBinding.LogicalDialogID, secondStreamID,
		toolTextID, chain, toolAttemptID, len(toolText), model.HistoryHashBytes(toolText)); err != nil {
		t.Fatal(err)
	}
	if _, err := rankTx.Exec(ctx, `INSERT INTO agent_service.history_search_tool_sources(
		owner_id,logical_dialog_id,text_id,entry_id,size_bytes,text_sha256)
		VALUES($1,$2,$3,$4,$5,$6)`, owner, secondBinding.LogicalDialogID, toolTextID, toolRankID,
		len(toolText), model.HistoryHashBytes(toolText)); err != nil {
		t.Fatal(err)
	}
	if _, err := rankTx.Exec(ctx, `INSERT INTO agent_service.history_search_tool_segments(
		owner_id,logical_dialog_id,text_id,entry_id,segment_index,safe_text)
		VALUES($1,$2,$3,$4,0,$5)`, owner, secondBinding.LogicalDialogID, toolTextID, toolRankID, string(toolText)); err != nil {
		t.Fatal(err)
	}
	if err := rankTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rankSpec, err := ParseHistorySearchQuery(model.HistorySearchQuery{Q: `"weighted exact phrase"`})
	if err != nil {
		t.Fatal(err)
	}
	ranked, err := database.CreateHistorySearch(ctx, owner, rankSpec, HistorySearchQueryHash(rankSpec), 10)
	if err != nil || len(ranked.Page.Items) != 3 || ranked.Page.Items[0].EntryID != titleRankID ||
		ranked.Page.Items[1].EntryID != messageRankID || ranked.Page.Items[2].EntryID != toolRankID ||
		!(ranked.Page.Items[0].Rank > ranked.Page.Items[1].Rank && ranked.Page.Items[1].Rank > ranked.Page.Items[2].Rank) {
		t.Fatalf("phrase ranking=%+v err=%v", ranked.Page.Items, err)
	}

	largeMessage := "snapshotbounded " + strings.Repeat("я", 30_000)
	largeMessageID := appendSearchEntryText(t, ctx, database, owner, snapshot.Nodes[1].NodeID, secondBinding,
		secondStreamID, 9, base.Add(23*time.Second), largeMessage)
	largeMessageSpec, err := ParseHistorySearchQuery(model.HistorySearchQuery{Q: "snapshotbounded"})
	if err != nil {
		t.Fatal(err)
	}
	largeMessageResult, err := database.CreateHistorySearch(ctx, owner, largeMessageSpec, HistorySearchQueryHash(largeMessageSpec), 10)
	if err != nil || len(largeMessageResult.Page.Items) != 1 || largeMessageResult.Page.Items[0].EntryID != largeMessageID {
		t.Fatalf("large message snapshot=%+v err=%v", largeMessageResult.Page.Items, err)
	}
	assertHistorySearchSnapshotSnippetBound(t, ctx, database, owner, largeMessageResult)

	largeTool := "toolbounded " + strings.Repeat("я", 100_000)
	if _, err := database.pool.Exec(ctx, `UPDATE agent_service.history_search_tool_segments SET safe_text=$4
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND text_id=$3`, owner, secondBinding.LogicalDialogID, toolTextID, largeTool); err != nil {
		t.Fatal(err)
	}
	largeToolSpec, err := ParseHistorySearchQuery(model.HistorySearchQuery{Q: "toolbounded"})
	if err != nil {
		t.Fatal(err)
	}
	largeToolResult, err := database.CreateHistorySearch(ctx, owner, largeToolSpec, HistorySearchQueryHash(largeToolSpec), 10)
	if err != nil || len(largeToolResult.Page.Items) != 1 || largeToolResult.Page.Items[0].EntryID != toolRankID {
		t.Fatalf("large tool snapshot=%+v err=%v", largeToolResult.Page.Items, err)
	}
	assertHistorySearchSnapshotSnippetBound(t, ctx, database, owner, largeToolResult)

	// A completed tail must not revisit unrelated historical manifests. Make an
	// old source intentionally stale: a bounded unrelated-attempt pass leaves it
	// alone, while the matching attempt deterministically repairs (clears) it.
	if _, err := database.pool.Exec(ctx, `UPDATE agent_service.history_text_manifests SET size_bytes=size_bytes+1
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND text_id=$3`, owner, secondBinding.LogicalDialogID, toolTextID); err != nil {
		t.Fatal(err)
	}
	unrelatedAttempt := model.HistoryStableUUID("r14-unrelated-attempt", owner)
	for _, test := range []struct {
		attempts []string
		want     int
	}{{[]string{unrelatedAttempt}, 1}, {[]string{toolAttemptID}, 0}} {
		projectionTx, beginErr := database.pool.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		if projectionErr := indexHistorySafeTextsForDialog(ctx, projectionTx, owner, secondBinding.LogicalDialogID,
			false, test.attempts, []string{}); projectionErr != nil {
			_ = projectionTx.Rollback(ctx)
			t.Fatal(projectionErr)
		}
		if commitErr := projectionTx.Commit(ctx); commitErr != nil {
			t.Fatal(commitErr)
		}
		var sourceCount int
		if countErr := database.pool.QueryRow(ctx, `SELECT count(*) FROM agent_service.history_search_tool_sources
			WHERE owner_id=$1 AND logical_dialog_id=$2 AND text_id=$3`, owner, secondBinding.LogicalDialogID, toolTextID).Scan(&sourceCount); countErr != nil {
			t.Fatal(countErr)
		}
		if sourceCount != test.want {
			t.Fatalf("bounded projection attempts=%v source count=%d want=%d", test.attempts, sourceCount, test.want)
		}
	}
	if _, err := database.ReadHistorySearch(ctx, owner+"-other", spec.Query, first.Position, 2); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-owner snapshot err=%v", err)
	}

	now = now.Add(5 * time.Minute)
	if _, err := database.ReadHistorySearch(ctx, owner, spec.Query, first.Position, 2); !errors.Is(err, ErrHistorySearchExpired) {
		t.Fatalf("expired snapshot err=%v", err)
	}
	if _, err := database.CreateHistorySearch(ctx, owner, spec, HistorySearchQueryHash(spec), 2); err != nil {
		t.Fatal(err)
	}
	var oldSnapshots int
	if err := database.pool.QueryRow(ctx, `SELECT count(*) FROM agent_service.history_search_snapshots
		WHERE owner_id=$1 AND snapshot_id=$2`, owner, first.Position.SnapshotID).Scan(&oldSnapshots); err != nil || oldSnapshots != 0 {
		t.Fatalf("expired snapshot cleanup count=%d err=%v", oldSnapshots, err)
	}
	if _, err := database.ReadHistorySearch(ctx, owner, spec.Query, first.Position, 2); !errors.Is(err, ErrHistorySearchExpired) {
		t.Fatalf("cleaned expired cursor must remain gone, err=%v", err)
	}
}

func assertHistorySearchSnapshotSnippetBound(
	t *testing.T,
	ctx context.Context,
	database *Store,
	owner string,
	result HistorySearchResult,
) {
	t.Helper()
	var maximum int
	if err := database.pool.QueryRow(ctx, `SELECT COALESCE(max(octet_length(snippet)),0)
		FROM agent_service.history_search_snapshot_items WHERE owner_id=$1 AND snapshot_id=$2`,
		owner, result.Position.SnapshotID).Scan(&maximum); err != nil {
		t.Fatal(err)
	}
	if maximum == 0 || maximum > model.HistorySearchMaximumSnippet {
		t.Fatalf("persisted snapshot snippet bytes=%d", maximum)
	}
	for _, item := range result.Page.Items {
		if len([]byte(item.Snippet)) > model.HistorySearchMaximumSnippet {
			t.Fatalf("response snippet bytes=%d", len([]byte(item.Snippet)))
		}
	}
}

func seedSearchDialog(t *testing.T, ctx context.Context, database *Store, owner, nodeID string,
	binding model.DialogMapping, base time.Time, offset time.Duration, count int,
) []string {
	t.Helper()
	streamID := model.HistoryStableUUID("r14-stability-stream", owner+":"+binding.LogicalDialogID)
	chain := strings.Repeat("b", 64)
	checkpoint := []byte(fmt.Sprintf(`{"throughSeq":%d,"chainHash":%q,"ready":true}`, count, chain))
	if _, err := database.pool.Exec(ctx, `INSERT INTO agent_service.history_replica_streams(
		owner_id,stream_id,logical_dialog_id,node_id,node_dialog_id,binding_generation,imported_through,imported_chain_hash,
		imported_checkpoint,source_through,source_chain_hash,source_checkpoint,source_captured_at,observed_at,complete)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$7,$8,$9,$10,$10,true)`, owner, streamID, binding.LogicalDialogID,
		nodeID, binding.NodeDialogID, binding.BindingVersion, count, chain, checkpoint, base); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, count)
	for index := 1; index <= count; index++ {
		ids = append(ids, appendSearchEntry(t, ctx, database, owner, nodeID, binding, streamID, int64(index),
			base.Add(offset+time.Duration(index)*time.Second)))
	}
	return ids
}

func appendSearchEntry(t *testing.T, ctx context.Context, database *Store, owner, nodeID string,
	binding model.DialogMapping, streamID string, sequence int64, created time.Time,
) string {
	return appendSearchEntryText(t, ctx, database, owner, nodeID, binding, streamID, sequence, created, "stable corpus message")
}

func appendSearchEntryText(t *testing.T, ctx context.Context, database *Store, owner, nodeID string,
	binding model.DialogMapping, streamID string, sequence int64, created time.Time, message string,
) string {
	t.Helper()
	entryID := model.HistoryStableUUID("r14-stability-entry", fmt.Sprintf("%s:%s:%d", owner, binding.LogicalDialogID, sequence))
	chain := strings.Repeat("b", 64)
	entry := model.HistoryTranscriptEntry{
		EntryID: entryID,
		Origin: model.HistoryOrigin{NodeID: nodeID, NodeDialogID: binding.NodeDialogID,
			AuthorEventID: fmt.Sprintf("%s:%d", nodeID, sequence)},
		LogicalDialogID: binding.LogicalDialogID, MessageID: entryID, Kind: "message", Role: "assistant",
		Content: json.RawMessage(fmt.Sprintf(
			`{"kind":"inline","content":%q,"redaction":"none","truncated":false}`, message)),
		CreatedAt: created.Format(time.RFC3339Nano), SourceGeneration: 0, ExecutionOrdinal: sequence, ExecutionStatus: "recorded",
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `INSERT INTO agent_service.history_replica_records(
		owner_id,stream_id,stream_seq,record_id,record_type,entity_id,revision,record_hash,prev_hash,chain_hash,record_json)
		VALUES($1,$2,$3,$4,'entry',$5,1,$6,$6,$6,'{}')`, owner, streamID, sequence,
		model.HistoryStableUUID("r14-stability-record", entryID), entryID, chain); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_service.history_entries(
		owner_id,logical_dialog_id,source_stream_id,source_stream_seq,entry_id,entry_hash,origin_node_id,
		origin_node_dialog_id,message_id,role,created_at,execution_ordinal,payload)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$5,'assistant',$9,$4,$10)`, owner, binding.LogicalDialogID,
		streamID, sequence, entryID, chain, nodeID, binding.NodeDialogID, created, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_service.history_search_documents(
		owner_id,logical_dialog_id,entry_id,node_id,node_dialog_id,role,kind,created_at,message_text)
		VALUES($1,$2,$3,$4,$5,'assistant','message',$6,$7)`, owner, binding.LogicalDialogID,
		entryID, nodeID, binding.NodeDialogID, created, message); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return entryID
}

// TestPostgresHistorySearch10kP95LocalSSD is the normative local profile:
// PostgreSQL on localhost, 10,000 committed rows, warm process/connection pool,
// simple dictionary + GIN, 20 fresh snapshots. It is skipped unless the shared
// integration database is explicitly provided.
func TestPostgresHistorySearch10kP95LocalSSD(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set; R14 10k local-Postgres profile NOT_RUN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	database, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	database.now = func() time.Time { return now }
	owner := fmt.Sprintf("hl296-search-%d", time.Now().UnixNano())
	verified, snapshot := inventoryFixture(owner, 1, now)
	if _, err := database.Import(ctx, verified, snapshot); err != nil {
		t.Fatal(err)
	}
	nodeID := snapshot.Nodes[0].NodeID
	bindings, err := database.ListDialogBindings(ctx, owner, nodeID, "", 10)
	if err != nil || len(bindings.Items) != 1 {
		t.Fatalf("bindings=%+v err=%v", bindings, err)
	}
	binding := bindings.Items[0]
	streamID := model.HistoryStableUUID("r14-performance-stream", owner)
	chain := strings.Repeat("a", 64)
	checkpoint := []byte(fmt.Sprintf(`{"throughSeq":10000,"chainHash":%q,"ready":true}`, chain))
	tx, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(ctx, `INSERT INTO agent_service.history_replica_streams(
		owner_id,stream_id,logical_dialog_id,node_id,node_dialog_id,binding_generation,imported_through,imported_chain_hash,
		imported_checkpoint,source_through,source_chain_hash,source_checkpoint,source_captured_at,observed_at,complete)
		VALUES($1,$2,$3,$4,$5,$6,10000,$7,$8,10000,$7,$8,$9,$9,true)`, owner, streamID, binding.LogicalDialogID,
		nodeID, binding.NodeDialogID, binding.BindingVersion, chain, checkpoint, now)
	if err != nil {
		t.Fatal(err)
	}
	recordRows := make([][]any, 0, 10_000)
	entryRows := make([][]any, 0, 10_000)
	for index := 0; index < 10_000; index++ {
		entryID := model.HistoryStableUUID("r14-performance-entry", fmt.Sprintf("%s:%d", owner, index))
		text := fmt.Sprintf("common corpus row %d", index)
		switch index {
		case 9001:
			text += " кириллица уникальнаяошибка"
		case 9100:
			text += " latin uniquefailure"
		case 9200:
			text += " 550e8400-e29b-41d4-a716-446655440000"
		case 9300:
			text += " exact normalized phrase target"
		}
		created := now.Add(time.Duration(index) * time.Millisecond)
		payload := []byte(fmt.Sprintf(`{"entryId":%q,"origin":{"nodeId":%q,"nodeDialogId":%q,"authorEventId":%q},"logicalDialogId":%q,"messageId":%q,"kind":"message","role":"assistant","content":{"kind":"inline","content":%q,"redaction":"none","truncated":false},"createdAt":%q,"sourceGeneration":0,"executionOrdinal":%d,"executionStatus":"recorded"}`,
			entryID, nodeID, binding.NodeDialogID, fmt.Sprintf("%s:%d", nodeID, index+1), binding.LogicalDialogID, entryID,
			text, created.Format(time.RFC3339Nano), index+1))
		recordRows = append(recordRows, []any{owner, streamID, int64(index + 1), model.HistoryStableUUID("r14-performance-record", entryID), "entry", entryID, int64(1), chain, chain, chain, []byte(`{}`)})
		entryRows = append(entryRows, []any{owner, binding.LogicalDialogID, streamID, int64(index + 1), entryID, chain, nodeID, binding.NodeDialogID, entryID, nil, nil, "assistant", created, int64(index + 1), payload})
	}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"agent_service", "history_replica_records"}, []string{"owner_id", "stream_id", "stream_seq", "record_id", "record_type", "entity_id", "revision", "record_hash", "prev_hash", "chain_hash", "record_json"}, pgx.CopyFromRows(recordRows)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.CopyFrom(ctx, pgx.Identifier{"agent_service", "history_entries"}, []string{"owner_id", "logical_dialog_id", "source_stream_id", "source_stream_seq", "entry_id", "entry_hash", "origin_node_id", "origin_node_dialog_id", "message_id", "request_id", "attempt_id", "role", "created_at", "execution_ordinal", "payload"}, pgx.CopyFromRows(entryRows)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO agent_service.history_search_documents(owner_id,logical_dialog_id,entry_id,node_id,node_dialog_id,role,kind,created_at,message_text)
		SELECT owner_id,logical_dialog_id,entry_id,origin_node_id,origin_node_dialog_id,role,payload->>'kind',created_at,payload->'content'->>'content'
		FROM agent_service.history_entries WHERE owner_id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	// Put one verified safe-text projection on a known entry; no raw/private payload is indexed.
	toolEntryID := model.HistoryStableUUID("r14-performance-entry", fmt.Sprintf("%s:%d", owner, 9400))
	toolTextID := model.HistoryStableUUID("r14-performance-tool", owner)
	toolText := []byte("tool safetext evidence")
	toolTextSHA := model.HistoryHashBytes(toolText)
	if _, err = tx.Exec(ctx, `INSERT INTO agent_service.history_text_manifests(
		owner_id,logical_dialog_id,source_stream_id,source_stream_seq,text_id,manifest_hash,attempt_id,complete,size_bytes,text_sha256,chunk_count,payload)
		VALUES($1,$2,$3,9401,$4,$5,$6,true,22,$7,0,'{}')`, owner, binding.LogicalDialogID, streamID, toolTextID, chain,
		model.HistoryStableUUID("r14-performance-attempt", owner), toolTextSHA); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO agent_service.history_search_tool_sources(
		owner_id,logical_dialog_id,text_id,entry_id,size_bytes,text_sha256)
		VALUES($1,$2,$3,$4,$5,$6)`, owner, binding.LogicalDialogID, toolTextID, toolEntryID, len(toolText), toolTextSHA); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO agent_service.history_search_tool_segments(owner_id,logical_dialog_id,text_id,entry_id,segment_index,safe_text)
		VALUES($1,$2,$3,$4,0,'tool safetext evidence')`, owner, binding.LogicalDialogID, toolTextID, toolEntryID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	metadata := model.HistoryDialogMetadata{SchemaID: model.HistoryMetadataSchemaID, NodeID: nodeID, NodeDialogID: binding.NodeDialogID,
		BindingGeneration: binding.BindingVersion, Title: "R14 deterministic title", UpdatedAt: now.Format(time.RFC3339Nano)}
	if _, err = database.UpsertHistoryDialogMetadata(ctx, owner, binding.LogicalDialogID, metadata); err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{"уникальнаяошибка", "uniquefailure", `"550e8400-e29b-41d4-a716-446655440000"`, `"exact normalized phrase"`, "safetext", "deterministic title"} {
		spec, parseErr := ParseHistorySearchQuery(model.HistorySearchQuery{Q: query})
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		result, searchErr := database.CreateHistorySearch(ctx, owner, spec, HistorySearchQueryHash(spec), 50)
		if searchErr != nil || len(result.Page.Items) != 1 {
			t.Fatalf("query=%q items=%d err=%v", query, len(result.Page.Items), searchErr)
		}
	}
	common, _ := ParseHistorySearchQuery(model.HistorySearchQuery{Q: "common"})
	page, err := database.CreateHistorySearch(ctx, owner, common, HistorySearchQueryHash(common), 50)
	if err != nil || page.Page.TotalCount != 10_000 {
		t.Fatalf("common count=%d err=%v", page.Page.TotalCount, err)
	}
	known := model.HistoryStableUUID("r14-performance-entry", fmt.Sprintf("%s:%d", owner, 9001))
	found, pageNumber := false, 1
	for page.HasMore && pageNumber < 250 {
		page, err = database.ReadHistorySearch(ctx, owner, common.Query, page.Position, 50)
		if err != nil {
			t.Fatal(err)
		}
		pageNumber++
		for _, item := range page.Page.Items {
			if item.EntryID == known {
				found = true
			}
		}
		if found {
			break
		}
	}
	if !found || pageNumber == 1 {
		t.Fatalf("known beyond-page-1 hit not found: page=%d", pageNumber)
	}

	durations := make([]time.Duration, 0, 20)
	for range 20 {
		started := time.Now()
		if _, err := database.CreateHistorySearch(ctx, owner, common, HistorySearchQueryHash(common), 50); err != nil {
			t.Fatal(err)
		}
		durations = append(durations, time.Since(started))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[18]
	t.Logf("profile=local-postgres-simple-gin rows=10000 runs=20 p95=%s", p95)
	if p95 > 2*time.Second {
		t.Fatalf("R14 local profile p95=%s exceeds 2s", p95)
	}
}
