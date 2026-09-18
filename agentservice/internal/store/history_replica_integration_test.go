package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

func TestPostgresHistoryReplicaAtomicDedupeRecoveryAndOfflineRead(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx); err != nil {
		database.Close()
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 17, 2, 0, 2, 0, time.UTC)
	database.now = func() time.Time { return now }
	owner := os.Getenv("HL294_HISTORY_TEST_OWNER")
	if owner == "" {
		owner = fmt.Sprintf("hl294-%d", time.Now().UnixNano())
	}
	verified, snapshot := inventoryFixture(owner, 1, now.Add(-2*time.Second))
	if _, err := database.Import(ctx, verified, snapshot); err != nil {
		database.Close()
		t.Fatal(err)
	}
	nodeID := snapshot.Nodes[0].NodeID
	binding := listAllBindings(t, ctx, database, owner, nodeID, 10)[0]
	identity := model.HistoryStreamIdentity{
		OwnerID: owner, LogicalDialogID: binding.LogicalDialogID, NodeID: nodeID,
		NodeDialogID: binding.NodeDialogID, BindingGeneration: binding.BindingVersion,
	}
	page := historyPageFixture(t, identity, now.Add(-2*time.Second), "hello")
	first, second := splitHistoryPage(t, page, 2)
	result, err := database.ApplyHistoryReplicaPage(ctx, owner, first)
	if err != nil || result.Duplicate || result.Complete || result.ImportedThrough != *first.NextAfter ||
		result.SourceThrough != page.Checkpoint.ThroughSeq {
		database.Close()
		t.Fatalf("partial import=%+v err=%v", result, err)
	}
	assertHistoryReplicaCounts(t, ctx, database, owner, page.StreamID, int64(len(first.Records)))
	database.Close()

	database, err = Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx); err != nil {
		database.Close()
		t.Fatal(err)
	}
	database.now = func() time.Time { return now }
	partialReplay, err := database.ApplyHistoryReplicaPage(ctx, owner, first)
	if err != nil || !partialReplay.Duplicate || partialReplay.Complete || partialReplay.ImportedThrough != *first.NextAfter {
		database.Close()
		t.Fatalf("partial recovery replay=%+v err=%v", partialReplay, err)
	}
	result, err = database.ApplyHistoryReplicaPage(ctx, owner, second)
	if err != nil || result.Duplicate || !result.Complete || result.ImportedThrough != page.Checkpoint.ThroughSeq {
		database.Close()
		t.Fatalf("recovered import=%+v err=%v", result, err)
	}
	replay, err := database.ApplyHistoryReplicaPage(ctx, owner, second)
	if err != nil || !replay.Duplicate || !replay.Complete || replay.ImportedThrough != result.ImportedThrough {
		database.Close()
		t.Fatalf("duplicate import=%+v err=%v", replay, err)
	}
	assertHistoryReplicaCounts(t, ctx, database, owner, page.StreamID, int64(len(page.Records)))
	assertHistoryReplicaRead(t, ctx, database, owner, identity, page)

	gapRecord := historyRecordFixture(t, page.StreamID, model.HistoryRecordFact,
		model.HistoryStableUUID("history-record-fact-v1", "gap"), model.HistoryStableUUID("history-execution-fact-v1", "gap"), 1,
		model.HistoryExecutionFact{
			FactID:          model.HistoryStableUUID("history-execution-fact-v1", "gap"),
			Origin:          model.HistoryOrigin{NodeID: nodeID, NodeDialogID: binding.NodeDialogID, AuthorEventID: nodeID + ":99"},
			LogicalDialogID: binding.LogicalDialogID, Kind: "attempt.completed", Status: "attempt.completed", EventSeq: 99,
			OccurredAt: now.Format(time.RFC3339Nano), Event: json.RawMessage(`{"type":"attempt.completed"}`),
		}, page.Checkpoint.ThroughSeq+1, page.Checkpoint.ChainHash)
	gapCheckpoint := page.Checkpoint
	foldHistoryCheckpoint(&gapCheckpoint, gapRecord)
	gapPage := model.HistoryExportPage{
		SchemaID: model.HistoryExportSchemaID, SchemaSHA256: model.HistorySchemaSHA256,
		StreamID: page.StreamID, Identity: identity, AfterSeq: gapCheckpoint.ThroughSeq,
		Records: []model.HistoryRecord{}, Checkpoint: gapCheckpoint,
	}
	if _, err := database.ApplyHistoryReplicaPage(ctx, owner, gapPage); !errors.Is(err, ErrHistoryReplicaGap) {
		database.Close()
		t.Fatalf("gap result=%v", err)
	}
	conflict := historyPageFixture(t, identity, now.Add(-2*time.Second), "changed")
	if _, err := database.ApplyHistoryReplicaPage(ctx, owner, conflict); !errors.Is(err, ErrHistoryReplicaConflict) {
		database.Close()
		t.Fatalf("hash conflict result=%v", err)
	}
	assertHistoryReplicaCounts(t, ctx, database, owner, page.StreamID, int64(len(page.Records)))

	extended := page
	for index := 0; index < 2; index++ {
		factID := model.HistoryStableUUID("history-pagination-fact", fmt.Sprintf("%s-%d", owner, index))
		extended = appendHistoryFixtureRecord(t, extended, model.HistoryRecordFact,
			model.HistoryStableUUID("history-record-fact-v1", factID), factID, 1, model.HistoryExecutionFact{
				FactID: factID, Origin: model.HistoryOrigin{NodeID: nodeID, NodeDialogID: binding.NodeDialogID,
					AuthorEventID: fmt.Sprintf("%s:%d", nodeID, index+10)},
				LogicalDialogID: binding.LogicalDialogID, Kind: "attempt.completed", Status: "attempt.completed",
				EventSeq: int64(index + 10), OccurredAt: now.Add(time.Duration(index+10) * time.Second).Format(time.RFC3339Nano),
				Event: json.RawMessage(`{"type":"attempt.completed"}`),
			})
		commandID := model.HistoryStableUUID("history-pagination-command", fmt.Sprintf("%s-%d", owner, index))
		receiptID := model.HistoryStableUUID("history-pagination-receipt", commandID)
		extended = appendHistoryFixtureRecord(t, extended, model.HistoryRecordReceipt, receiptID, receiptID, 1,
			model.HistoryReceiptRevision{
				ReceiptRevisionID: receiptID,
				Origin: model.HistoryOrigin{NodeID: nodeID, NodeDialogID: binding.NodeDialogID,
					AuthorEventID: fmt.Sprintf("%s:%d", nodeID, index+20)},
				LogicalDialogID: binding.LogicalDialogID, CommandID: commandID, CommandKind: "message.enqueue",
				Fingerprint: model.HistoryHashBytes([]byte(commandID)), Revision: 1,
				PredecessorHash: model.HistoryGenesisHash, Admission: "accepted", Outcome: "accepted",
				Receipt:    json.RawMessage(`{"result":"accepted"}`),
				AcceptedAt: now.Add(time.Duration(index+20) * time.Second).Format(time.RFC3339Nano),
			})
	}
	textBytes := []byte("durable full text")
	textID := model.HistoryStableUUID("history-positive-text", owner)
	extended = appendHistoryFixtureRecord(t, extended, model.HistoryRecordTextManifest,
		model.HistoryStableUUID("history-record-text-manifest-v1", textID), textID, 1, model.HistoryTextManifest{
			TextID: textID, Origin: model.HistoryOrigin{NodeID: nodeID, NodeDialogID: binding.NodeDialogID, AuthorEventID: "text:" + textID},
			LogicalDialogID: binding.LogicalDialogID, AttemptID: "71000000-0000-4000-8000-000000000006",
			Source: json.RawMessage(`{"kind":"assistant_message"}`), Preview: string(textBytes), Redaction: "none", Complete: true,
			SizeBytes: int64(len(textBytes)), SHA256: model.HistoryHashBytes(textBytes), ChunkCount: 1,
		})
	chunkEntity := model.HistoryStableUUID("history-text-chunk-v1", textID+"\x000")
	extended = appendHistoryFixtureRecord(t, extended, model.HistoryRecordTextChunk,
		model.HistoryStableUUID("history-record-text-chunk-v1", chunkEntity+"\x00"+model.HistoryHashBytes(textBytes)), chunkEntity, 1,
		model.HistoryTextChunk{TextID: textID, LogicalDialogID: binding.LogicalDialogID, ChunkIndex: 0,
			OffsetBytes: 0, SizeBytes: int64(len(textBytes)), SHA256: model.HistoryHashBytes(textBytes), Bytes: textBytes})
	tail := historyTailPage(t, page, extended)
	if imported, err := database.ApplyHistoryReplicaPage(ctx, owner, tail); err != nil || !imported.Complete {
		database.Close()
		t.Fatalf("pagination tail import=%+v err=%v", imported, err)
	}
	manifest, err := database.HistoryTextManifest(ctx, owner, identity.LogicalDialogID, textID)
	if err != nil || !manifest.Complete || manifest.ChunkCount != 1 || manifest.SHA256 != model.HistoryHashBytes(textBytes) {
		database.Close()
		t.Fatalf("full-text manifest=%+v err=%v", manifest, err)
	}
	chunk, err := database.HistoryTextChunk(ctx, owner, identity.LogicalDialogID, textID, 0)
	if err != nil || string(chunk.Bytes) != string(textBytes) || chunk.SHA256 != manifest.SHA256 {
		database.Close()
		t.Fatalf("full-text chunk=%+v err=%v", chunk, err)
	}
	readSnapshot, err := database.ReadHistoryReplica(ctx, owner, identity.LogicalDialogID, HistoryReadPosition{}, 1)
	if err != nil || !readSnapshot.HasMore {
		database.Close()
		t.Fatalf("first paginated snapshot=%+v err=%v", readSnapshot, err)
	}
	earlyID := model.HistoryStableUUID("history-backfill-entry", owner)
	withBackfill := appendHistoryFixtureRecord(t, extended, model.HistoryRecordEntry,
		model.HistoryStableUUID("history-record-entry-v1", earlyID), earlyID, 1, model.HistoryTranscriptEntry{
			EntryID: earlyID, Origin: model.HistoryOrigin{NodeID: nodeID, NodeDialogID: binding.NodeDialogID, AuthorEventID: nodeID + ":30"},
			LogicalDialogID: binding.LogicalDialogID, MessageID: earlyID,
			AttemptID: "71000000-0000-4000-8000-000000000006", Kind: "message", Role: "user",
			Content:   json.RawMessage(`{"kind":"inline","content":"late backfill"}`),
			CreatedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), SourceGeneration: 0,
			ExecutionOrdinal: 30, ExecutionStatus: "recorded",
		})
	if imported, err := database.ApplyHistoryReplicaPage(ctx, owner, historyTailPage(t, extended, withBackfill)); err != nil || !imported.Complete {
		database.Close()
		t.Fatalf("backfill import=%+v err=%v", imported, err)
	}
	toolSpec, err := ParseHistorySearchQuery(model.HistorySearchQuery{Q: "durable full text"})
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	toolSearch, err := database.CreateHistorySearch(ctx, owner, toolSpec, HistorySearchQueryHash(toolSpec), 10)
	if err != nil || len(toolSearch.Page.Items) != 1 || toolSearch.Page.Items[0].EntryID != earlyID {
		database.Close()
		t.Fatalf("verified tool text was not projected after its target entry: page=%+v err=%v", toolSearch.Page, err)
	}
	entries, facts, receipts := append([]model.HistoryTranscriptEntry(nil), readSnapshot.Page.Entries...),
		append([]model.HistoryExecutionFact(nil), readSnapshot.Page.Facts...),
		append([]model.HistoryReceiptRevision(nil), readSnapshot.Page.Receipts...)
	for readSnapshot.HasMore {
		readSnapshot, err = database.ReadHistoryReplica(ctx, owner, identity.LogicalDialogID, readSnapshot.Position, 1)
		if err != nil {
			database.Close()
			t.Fatal(err)
		}
		entries, facts, receipts = append(entries, readSnapshot.Page.Entries...), append(facts, readSnapshot.Page.Facts...), append(receipts, readSnapshot.Page.Receipts...)
	}
	if len(entries) != 1 || len(facts) != 3 || len(receipts) != 3 || entries[0].EntryID == earlyID {
		database.Close()
		t.Fatalf("pinned pagination lost or leaked rows: entries=%d facts=%d receipts=%d", len(entries), len(facts), len(receipts))
	}
	fresh, err := database.ReadHistoryReplica(ctx, owner, identity.LogicalDialogID, HistoryReadPosition{}, 10)
	if err != nil || len(fresh.Page.Entries) != 2 || fresh.Page.Entries[0].EntryID != earlyID || len(fresh.Page.Facts) != 3 || len(fresh.Page.Receipts) != 3 {
		database.Close()
		t.Fatalf("fresh snapshot missed backfill: %+v err=%v", fresh, err)
	}
	stale := historyPrefixPage(t, withBackfill, 3)
	if _, err := database.ApplyHistoryReplicaPage(ctx, owner, stale); !errors.Is(err, ErrHistoryReplicaConflict) {
		database.Close()
		t.Fatalf("stale source checkpoint result=%v", err)
	}
	wrongBinding := identity
	wrongBinding.BindingGeneration++
	if _, err := database.ApplyHistoryReplicaPage(ctx, owner, historyPageFixture(t, wrongBinding, now, "wrong binding")); !errors.Is(err, ErrHistoryReplicaScope) {
		database.Close()
		t.Fatalf("mismatched binding result=%v", err)
	}
	now = now.Add(HistoryReplicaFreshnessWindow + time.Second)
	offline, err := database.ReadHistoryReplica(ctx, owner, identity.LogicalDialogID, HistoryReadPosition{}, 10)
	if err != nil || !offline.Page.Incomplete || offline.Page.SyncedThrough.IncompleteReason != "source_unreachable" {
		database.Close()
		t.Fatalf("stale source was reported complete: %+v err=%v", offline, err)
	}
	if replayed, err := database.ApplyHistoryReplicaPage(ctx, owner, withBackfill); err != nil || !replayed.Duplicate || !replayed.Complete {
		database.Close()
		t.Fatalf("fresh exact replay=%+v err=%v", replayed, err)
	}
	refreshed, err := database.ReadHistoryReplica(ctx, owner, identity.LogicalDialogID, HistoryReadPosition{}, 10)
	if err != nil || refreshed.Page.Incomplete {
		database.Close()
		t.Fatalf("successful refresh remained incomplete: %+v err=%v", refreshed, err)
	}
	if _, err := database.ReadHistoryReplica(ctx, owner+"-other", identity.LogicalDialogID, HistoryReadPosition{}, 10); err == nil {
		database.Close()
		t.Fatal("foreign owner read crossed isolation boundary")
	}
	database.Close()

	reopened, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	reopened.now = func() time.Time { return now }
	assertHistoryReplicaRead(t, ctx, reopened, owner, identity, withBackfill)
	opaqueID := model.HistoryStableUUID("history-opaque-entry", owner)
	withOpaque := appendHistoryFixtureRecord(t, withBackfill, model.HistoryRecordEntry,
		model.HistoryStableUUID("history-record-entry-v1", opaqueID), opaqueID, 1, model.HistoryTranscriptEntry{
			EntryID:         opaqueID,
			Origin:          model.HistoryOrigin{NodeID: nodeID, NodeDialogID: binding.NodeDialogID, AuthorEventID: nodeID + ":opaque"},
			LogicalDialogID: binding.LogicalDialogID, MessageID: opaqueID, Kind: "message", Role: "assistant",
			Content:   json.RawMessage(`{"kind":"native","provider":{"private":"opaque"}}`),
			CreatedAt: now.Add(time.Minute).Format(time.RFC3339Nano), SourceGeneration: 0,
			ExecutionOrdinal: 31, ExecutionStatus: "recorded",
		})
	if imported, err := reopened.ApplyHistoryReplicaPage(ctx, owner, historyTailPage(t, withBackfill, withOpaque)); err != nil || !imported.Complete {
		t.Fatalf("opaque R12 entry import=%+v err=%v", imported, err)
	}
	var opaqueReplicaRows, opaqueSearchRows int
	if err := reopened.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM agent_service.history_entries WHERE owner_id=$1 AND entry_id=$2),
		(SELECT count(*) FROM agent_service.history_search_documents WHERE owner_id=$1 AND entry_id=$2)`, owner, opaqueID).
		Scan(&opaqueReplicaRows, &opaqueSearchRows); err != nil || opaqueReplicaRows != 1 || opaqueSearchRows != 0 {
		t.Fatalf("opaque projection rows replica=%d search=%d err=%v", opaqueReplicaRows, opaqueSearchRows, err)
	}
}

func TestPostgresHistoryReplicaHashInputSemanticBindingAndRepairRollback(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 18, 12, 45, 0, 0, time.UTC)
	database.now = func() time.Time { return now }
	seed := func(owner string) (model.HistoryStreamIdentity, model.HistoryExportPage) {
		t.Helper()
		verified, snapshot := inventoryFixture(owner, 1, now)
		if _, err := database.Import(ctx, verified, snapshot); err != nil {
			t.Fatal(err)
		}
		binding := listAllBindings(t, ctx, database, owner, snapshot.Nodes[0].NodeID, 10)[0]
		identity := model.HistoryStreamIdentity{OwnerID: owner, LogicalDialogID: binding.LogicalDialogID,
			NodeID: snapshot.Nodes[0].NodeID, NodeDialogID: binding.NodeDialogID, BindingGeneration: binding.BindingVersion}
		return identity, historyPageFixture(t, identity, now, "hash-input-binding")
	}

	tamperOwner := fmt.Sprintf("hl294-hash-binding-%d", time.Now().UnixNano())
	_, tamperPage := seed(tamperOwner)
	if imported, err := database.ApplyHistoryReplicaPage(ctx, tamperOwner, tamperPage); err != nil || !imported.Complete {
		t.Fatalf("tamper baseline import=%+v err=%v", imported, err)
	}
	tag, err := database.pool.Exec(ctx, `UPDATE agent_service.history_replica_records SET record_type='execution_fact'
		WHERE owner_id=$1 AND stream_id=$2 AND stream_seq=1`, tamperOwner, tamperPage.StreamID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("relational-column tamper rows=%d err=%v", tag.RowsAffected(), err)
	}
	if _, err := database.ApplyHistoryReplicaPage(ctx, tamperOwner, tamperPage); !errors.Is(err, ErrHistoryReplicaConflict) {
		t.Fatalf("relational-column tamper exact replay result=%v", err)
	}
	if tag, err := database.pool.Exec(ctx, `UPDATE agent_service.history_replica_records SET record_type='entry'
		WHERE owner_id=$1 AND stream_id=$2 AND stream_seq=1`, tamperOwner, tamperPage.StreamID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("relational-column tamper restore rows=%d err=%v", tag.RowsAffected(), err)
	}
	tag, err = database.pool.Exec(ctx, `UPDATE agent_service.history_replica_records
		SET record_json=jsonb_set(record_json,'{payload,content,content}',to_jsonb($3::text),false),record_hash_input=NULL
		WHERE owner_id=$1 AND stream_id=$2 AND stream_seq=1`, tamperOwner, tamperPage.StreamID, "tampered")
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("semantic tamper rows=%d err=%v", tag.RowsAffected(), err)
	}
	if _, err := database.ApplyHistoryReplicaPage(ctx, tamperOwner, tamperPage); !errors.Is(err, ErrHistoryReplicaConflict) {
		t.Fatalf("semantic tamper exact replay result=%v", err)
	}
	var hashInputMissing bool
	var storedContent string
	if err := database.pool.QueryRow(ctx, `SELECT record_hash_input IS NULL,record_json->'payload'->'content'->>'content'
		FROM agent_service.history_replica_records WHERE owner_id=$1 AND stream_id=$2 AND stream_seq=1`,
		tamperOwner, tamperPage.StreamID).Scan(&hashInputMissing, &storedContent); err != nil || !hashInputMissing || storedContent != "tampered" {
		t.Fatalf("semantic tamper repair state missing=%t content=%q err=%v", hashInputMissing, storedContent, err)
	}

	rollbackOwner := fmt.Sprintf("hl294-hash-rollback-%d", time.Now().UnixNano())
	rollbackIdentity, rollbackPage := seed(rollbackOwner)
	first, _ := splitHistoryPage(t, rollbackPage, 2)
	if imported, err := database.ApplyHistoryReplicaPage(ctx, rollbackOwner, first); err != nil || imported.Complete {
		t.Fatalf("rollback partial import=%+v err=%v", imported, err)
	}
	if tag, err := database.pool.Exec(ctx, `UPDATE agent_service.history_replica_records SET record_hash_input=NULL
		WHERE owner_id=$1 AND stream_id=$2`, rollbackOwner, rollbackPage.StreamID); err != nil || tag.RowsAffected() != 2 {
		t.Fatalf("rollback legacy setup rows=%d err=%v", tag.RowsAffected(), err)
	}
	textID := model.HistoryStableUUID("history-rollback-missing-text", rollbackOwner)
	invalidReady := appendHistoryFixtureRecord(t, rollbackPage, model.HistoryRecordTextManifest,
		model.HistoryStableUUID("history-record-text-manifest-v1", textID), textID, 1, model.HistoryTextManifest{
			TextID: textID,
			Origin: model.HistoryOrigin{NodeID: rollbackIdentity.NodeID, NodeDialogID: rollbackIdentity.NodeDialogID,
				AuthorEventID: "text:" + textID},
			LogicalDialogID: rollbackIdentity.LogicalDialogID, AttemptID: "71000000-0000-4000-8000-000000000006",
			Source: json.RawMessage(`{"kind":"assistant_message"}`), Preview: "missing", Redaction: "none", Complete: true,
			SizeBytes: 7, SHA256: model.HistoryHashBytes([]byte("missing")), ChunkCount: 1,
		})
	if _, err := database.ApplyHistoryReplicaPage(ctx, rollbackOwner, invalidReady); !errors.Is(err, ErrHistoryReplicaConflict) {
		t.Fatalf("invalid ready replay result=%v", err)
	}
	var importedThrough, sourceThrough, records, missingInputs int64
	var complete bool
	if err := database.pool.QueryRow(ctx, `SELECT imported_through,source_through,complete,
		(SELECT count(*) FROM agent_service.history_replica_records r WHERE r.owner_id=s.owner_id AND r.stream_id=s.stream_id),
		(SELECT count(*) FROM agent_service.history_replica_records r WHERE r.owner_id=s.owner_id AND r.stream_id=s.stream_id AND r.record_hash_input IS NULL)
		FROM agent_service.history_replica_streams s WHERE owner_id=$1 AND stream_id=$2`, rollbackOwner, rollbackPage.StreamID).
		Scan(&importedThrough, &sourceThrough, &complete, &records, &missingInputs); err != nil ||
		importedThrough != 2 || sourceThrough != 4 || complete || records != 2 || missingInputs != 2 {
		t.Fatalf("failed repair rollback imported=%d source=%d complete=%t records=%d missing=%d err=%v",
			importedThrough, sourceThrough, complete, records, missingInputs, err)
	}
	if imported, err := database.ApplyHistoryReplicaPage(ctx, rollbackOwner, rollbackPage); err != nil || !imported.Complete {
		t.Fatalf("post-rollback exact repair=%+v err=%v", imported, err)
	}
	if err := database.pool.QueryRow(ctx, `SELECT count(*) FROM agent_service.history_replica_records
		WHERE owner_id=$1 AND stream_id=$2 AND record_hash_input IS NULL`, rollbackOwner, rollbackPage.StreamID).
		Scan(&missingInputs); err != nil || missingInputs != 0 {
		t.Fatalf("post-rollback exact repair missing=%d err=%v", missingInputs, err)
	}
}

func TestPostgresHistoryReplicaServerRestartBackfill(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	phase := os.Getenv("HL294_HISTORY_RESTART_PHASE")
	if phase == "" {
		t.Skip("HL294_HISTORY_RESTART_PHASE is not set")
	}
	if phase != "setup" && phase != "verify-server-restart" {
		t.Fatalf("unsupported restart phase %q", phase)
	}
	owner := os.Getenv("HL294_HISTORY_RESTART_OWNER")
	runID := os.Getenv("HL294_HISTORY_RESTART_RUN_ID")
	if owner == "" || runID == "" {
		t.Fatal("HL294_HISTORY_RESTART_OWNER and HL294_HISTORY_RESTART_RUN_ID are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 18, 11, 50, 0, 0, time.UTC)
	database.now = func() time.Time { return now }
	nodeID := fixtureUUID("20000000", 1)
	systemIdentifier, postmasterStartedAt := postgresSystemIdentity(t, ctx, database)
	if phase == "setup" {
		var existing int
		if err := database.pool.QueryRow(ctx, `SELECT count(*) FROM agent_service.logical_dialogs WHERE owner_id=$1`, owner).Scan(&existing); err != nil {
			t.Fatal(err)
		}
		if existing != 0 {
			t.Fatalf("restart fixture owner is not clean: owner=%s rows=%d", owner, existing)
		}
		verified, snapshot := inventoryFixture(owner, 1, now.Add(-2*time.Second))
		if _, err := database.Import(ctx, verified, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	bindings := listAllBindings(t, ctx, database, owner, nodeID, 10)
	if len(bindings) != 1 {
		t.Fatalf("restart fixture binding count=%d", len(bindings))
	}
	identity := model.HistoryStreamIdentity{
		OwnerID: owner, LogicalDialogID: bindings[0].LogicalDialogID, NodeID: nodeID,
		NodeDialogID: bindings[0].NodeDialogID, BindingGeneration: bindings[0].BindingVersion,
	}
	type restartMarker struct {
		RunID               string `json:"runId"`
		SystemIdentifier    string `json:"systemIdentifier"`
		PostmasterStartedAt string `json:"postmasterStartedAt"`
	}
	marker := restartMarker{RunID: runID, SystemIdentifier: systemIdentifier,
		PostmasterStartedAt: postmasterStartedAt.UTC().Format(time.RFC3339Nano)}
	if phase == "verify-server-restart" {
		var persistedMarker string
		if err := database.pool.QueryRow(ctx, `SELECT record_json->'payload'->'content'->>'content'
			FROM agent_service.history_replica_records WHERE owner_id=$1 AND stream_id=$2 AND stream_seq=1`,
			owner, model.HistoryStreamID(identity)).Scan(&persistedMarker); err != nil {
			t.Fatal(err)
		}
		if json.Unmarshal([]byte(persistedMarker), &marker) != nil || marker.RunID != runID ||
			marker.SystemIdentifier != systemIdentifier {
			t.Fatalf("restart fixture marker does not match run/cluster: %+v", marker)
		}
		previousStart, parseErr := time.Parse(time.RFC3339Nano, marker.PostmasterStartedAt)
		if parseErr != nil || !postmasterStartedAt.After(previousStart) {
			t.Fatalf("PostgreSQL postmaster did not restart: before=%s after=%s err=%v",
				marker.PostmasterStartedAt, postmasterStartedAt.UTC().Format(time.RFC3339Nano), parseErr)
		}
	}
	markerJSON, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	page := historyPageFixture(t, identity, now.Add(-2*time.Second), string(markerJSON))
	first, second := splitHistoryPage(t, page, 2)

	if phase == "setup" {
		result, err := database.ApplyHistoryReplicaPage(ctx, owner, first)
		if err != nil || result.Duplicate || result.Complete || result.ImportedThrough != *first.NextAfter ||
			result.SourceThrough != page.Checkpoint.ThroughSeq {
			t.Fatalf("pre-restart partial import=%+v err=%v", result, err)
		}
		tag, err := database.pool.Exec(ctx, `UPDATE agent_service.history_replica_records SET record_hash_input=NULL
			WHERE owner_id=$1 AND stream_id=$2`, owner, page.StreamID)
		if err != nil || tag.RowsAffected() != int64(len(first.Records)) {
			t.Fatalf("legacy hash-input simulation rows=%d want=%d err=%v", tag.RowsAffected(), len(first.Records), err)
		}
		assertHistoryReplicaCounts(t, ctx, database, owner, page.StreamID, int64(len(first.Records)))
		t.Logf("restart_marker run=%s system_identifier=%s postmaster_started_at=%s legacy_hash_inputs=%d", runID,
			systemIdentifier, postmasterStartedAt.UTC().Format(time.RFC3339Nano), tag.RowsAffected())
		return
	}

	assertHistoryReplicaCounts(t, ctx, database, owner, page.StreamID, int64(len(first.Records)))
	partialReplay, err := database.ApplyHistoryReplicaPage(ctx, owner, first)
	if err != nil || !partialReplay.Duplicate || partialReplay.Complete || partialReplay.ImportedThrough != *first.NextAfter {
		t.Fatalf("post-restart partial replay=%+v err=%v", partialReplay, err)
	}
	result, err := database.ApplyHistoryReplicaPage(ctx, owner, second)
	if err != nil || result.Duplicate || !result.Complete || result.ImportedThrough != page.Checkpoint.ThroughSeq {
		t.Fatalf("post-restart backfill=%+v err=%v", result, err)
	}
	assertHistoryReplicaCounts(t, ctx, database, owner, page.StreamID, int64(len(page.Records)))
	assertHistoryReplicaRead(t, ctx, database, owner, identity, page)
	rows, err := database.pool.Query(ctx, `SELECT stream_seq,record_id::text,record_type,entity_id::text,revision,
		record_hash,prev_hash,chain_hash,record_json,record_hash_input
		FROM agent_service.history_replica_records
		WHERE owner_id=$1 AND stream_id=$2 ORDER BY stream_seq`, owner, page.StreamID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var sequence, revision int64
		var recordID, recordType, entityID, recordHash, previousHash, chainHash string
		var recordJSON, hashInput []byte
		if rows.Scan(&sequence, &recordID, &recordType, &entityID, &revision, &recordHash, &previousHash,
			&chainHash, &recordJSON, &hashInput) != nil || index >= len(page.Records) {
			t.Fatalf("post-restart record scan failed at index=%d", index)
		}
		want := page.Records[index]
		wantHashInput, hashErr := model.HistoryRecordHashInput(want)
		var persisted model.HistoryRecord
		if hashErr != nil || json.Unmarshal(recordJSON, &persisted) != nil || sequence != want.StreamSeq ||
			recordID != want.RecordID || recordType != want.Type || entityID != want.EntityID || revision != want.Revision ||
			recordHash != want.RecordHash || previousHash != want.PrevHash || chainHash != want.ChainHash ||
			!bytes.Equal(hashInput, wantHashInput) || model.HistoryHashBytes(hashInput) != recordHash ||
			persisted.StreamSeq != want.StreamSeq || persisted.Type != want.Type || persisted.RecordID != want.RecordID ||
			persisted.EntityID != want.EntityID || persisted.Revision != want.Revision || persisted.RecordHash != want.RecordHash ||
			persisted.PrevHash != want.PrevHash || persisted.ChainHash != want.ChainHash ||
			!model.HistoryRecordHashInputMatches(persisted, hashInput) {
			t.Fatalf("post-restart record/hash-input mismatch at index=%d seq=%d hash=%s", index, sequence, recordHash)
		}
		index++
	}
	if rows.Err() != nil || index != len(page.Records) {
		t.Fatalf("post-restart immutable hash rows=%d want=%d err=%v", index, len(page.Records), rows.Err())
	}
}

func postgresSystemIdentity(t *testing.T, ctx context.Context, database *Store) (string, time.Time) {
	t.Helper()
	var systemIdentifier string
	var postmasterStartedAt time.Time
	if err := database.pool.QueryRow(ctx, `SELECT system_identifier::text,pg_postmaster_start_time() FROM pg_control_system()`).
		Scan(&systemIdentifier, &postmasterStartedAt); err != nil {
		t.Fatal(err)
	}
	return systemIdentifier, postmasterStartedAt
}

func TestPostgresHistoryReplicaHarnessComponentSeed(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	owner := os.Getenv("HL294_HARNESS_COMPONENT_REPLICA_OWNER")
	if owner == "" {
		t.Skip("HL294_HARNESS_COMPONENT_REPLICA_OWNER is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	verified, snapshot := inventoryFixture(owner, 1, time.Date(2026, 9, 18, 11, 55, 0, 0, time.UTC))
	result, err := database.Import(ctx, verified, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	bindings := listAllBindings(t, ctx, database, owner, snapshot.Nodes[0].NodeID, 10)
	if result.NodesSeen != 1 || result.DialogsSeen != 1 || len(bindings) != 1 ||
		bindings[0].NodeDialogID != snapshot.Nodes[0].Dialogs[0].NodeDialogID {
		t.Fatalf("Harness component coordinator seed=%+v bindings=%+v", result, bindings)
	}
	t.Logf("owner=%s node=%s nodeDialog=%s logicalDialog=%s bindingVersion=%d", owner,
		snapshot.Nodes[0].NodeID, bindings[0].NodeDialogID, bindings[0].LogicalDialogID, bindings[0].BindingVersion)
}

func TestPostgresHistoryReplicaReadyTransitionRepairsStaleToolTargetAcrossPages(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	owner := fmt.Sprintf("hl296-cross-page-%d", time.Now().UnixNano())
	verified, snapshot := inventoryFixture(owner, 1, now)
	if _, err := database.Import(ctx, verified, snapshot); err != nil {
		t.Fatal(err)
	}
	nodeID := snapshot.Nodes[0].NodeID
	binding := listAllBindings(t, ctx, database, owner, nodeID, 10)[0]
	identity := model.HistoryStreamIdentity{OwnerID: owner, LogicalDialogID: binding.LogicalDialogID,
		NodeID: nodeID, NodeDialogID: binding.NodeDialogID, BindingGeneration: binding.BindingVersion}
	base := historyPageFixture(t, identity, now, "base")
	if imported, err := database.ApplyHistoryReplicaPage(ctx, owner, base); err != nil || !imported.Complete {
		t.Fatalf("base import=%+v err=%v", imported, err)
	}

	attemptID := model.HistoryStableUUID("history-cross-page-attempt", owner)
	oldEntryID := model.HistoryStableUUID("history-cross-page-old-entry", owner)
	withOldTarget := appendHistoryFixtureRecord(t, base, model.HistoryRecordEntry,
		model.HistoryStableUUID("history-record-entry-v1", oldEntryID), oldEntryID, 1, model.HistoryTranscriptEntry{
			EntryID: oldEntryID,
			Origin: model.HistoryOrigin{NodeID: nodeID, NodeDialogID: binding.NodeDialogID,
				AuthorEventID: nodeID + ":cross-page-old"},
			LogicalDialogID: binding.LogicalDialogID, MessageID: oldEntryID, AttemptID: attemptID,
			Kind: "message", Role: "assistant",
			Content:          json.RawMessage(`{"kind":"inline","content":"old tool target","redaction":"none","truncated":false}`),
			CreatedAt:        now.Add(time.Second).Format(time.RFC3339Nano),
			SourceGeneration: 0, ExecutionOrdinal: 100, ExecutionStatus: "recorded",
		})
	text := []byte("cross page tool evidence")
	textID := model.HistoryStableUUID("history-cross-page-text", owner)
	withManifest := appendHistoryFixtureRecord(t, withOldTarget, model.HistoryRecordTextManifest,
		model.HistoryStableUUID("history-record-text-manifest-v1", textID), textID, 1, model.HistoryTextManifest{
			TextID: textID,
			Origin: model.HistoryOrigin{NodeID: nodeID, NodeDialogID: binding.NodeDialogID,
				AuthorEventID: "text:" + textID},
			LogicalDialogID: binding.LogicalDialogID, AttemptID: attemptID,
			Source: json.RawMessage(`{"kind":"tool_output"}`), Preview: string(text), Redaction: "none", Complete: true,
			SizeBytes: int64(len(text)), SHA256: model.HistoryHashBytes(text), ChunkCount: 1,
		})
	chunkEntity := model.HistoryStableUUID("history-text-chunk-v1", textID+"\x000")
	withManifest = appendHistoryFixtureRecord(t, withManifest, model.HistoryRecordTextChunk,
		model.HistoryStableUUID("history-record-text-chunk-v1", chunkEntity), chunkEntity, 1,
		model.HistoryTextChunk{TextID: textID, LogicalDialogID: binding.LogicalDialogID, ChunkIndex: 0,
			OffsetBytes: 0, SizeBytes: int64(len(text)), SHA256: model.HistoryHashBytes(text), Bytes: text})
	if imported, err := database.ApplyHistoryReplicaPage(ctx, owner, historyTailPage(t, base, withManifest)); err != nil || !imported.Complete {
		t.Fatalf("old target import=%+v err=%v", imported, err)
	}
	var projectedEntryID string
	if err := database.pool.QueryRow(ctx, `SELECT entry_id::text FROM agent_service.history_search_tool_sources
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND text_id=$3`, owner, binding.LogicalDialogID, textID).Scan(&projectedEntryID); err != nil || projectedEntryID != oldEntryID {
		t.Fatalf("initial projected entry=%q err=%v", projectedEntryID, err)
	}

	newEntryID := model.HistoryStableUUID("history-cross-page-new-entry", owner)
	withNewTarget := appendHistoryFixtureRecord(t, withManifest, model.HistoryRecordEntry,
		model.HistoryStableUUID("history-record-entry-v1", newEntryID), newEntryID, 1, model.HistoryTranscriptEntry{
			EntryID: newEntryID,
			Origin: model.HistoryOrigin{NodeID: nodeID, NodeDialogID: binding.NodeDialogID,
				AuthorEventID: nodeID + ":cross-page-new"},
			LogicalDialogID: binding.LogicalDialogID, MessageID: newEntryID, AttemptID: attemptID,
			Kind: "message", Role: "assistant",
			Content:          json.RawMessage(`{"kind":"inline","content":"new tool target","redaction":"none","truncated":false}`),
			CreatedAt:        now.Add(2 * time.Second).Format(time.RFC3339Nano),
			SourceGeneration: 0, ExecutionOrdinal: 101, ExecutionStatus: "recorded",
		})
	factID := model.HistoryStableUUID("history-cross-page-filler", owner)
	withReadyTail := appendHistoryFixtureRecord(t, withNewTarget, model.HistoryRecordFact,
		model.HistoryStableUUID("history-record-fact-v1", factID), factID, 1, model.HistoryExecutionFact{
			FactID: factID,
			Origin: model.HistoryOrigin{NodeID: nodeID, NodeDialogID: binding.NodeDialogID,
				AuthorEventID: nodeID + ":cross-page-filler"},
			LogicalDialogID: binding.LogicalDialogID, AttemptID: attemptID,
			Kind: "attempt.completed", Status: "attempt.completed", EventSeq: 102,
			OccurredAt: now.Add(3 * time.Second).Format(time.RFC3339Nano), Event: json.RawMessage(`{"type":"attempt.completed"}`),
		})
	firstTail, finalTail := splitHistoryPage(t, historyTailPage(t, withManifest, withReadyTail), 1)
	if imported, err := database.ApplyHistoryReplicaPage(ctx, owner, firstTail); err != nil || imported.Complete {
		t.Fatalf("intermediate late target import=%+v err=%v", imported, err)
	}
	if imported, err := database.ApplyHistoryReplicaPage(ctx, owner, finalTail); err != nil || !imported.Complete {
		t.Fatalf("final Ready tail import=%+v err=%v", imported, err)
	}
	if err := database.pool.QueryRow(ctx, `SELECT entry_id::text FROM agent_service.history_search_tool_sources
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND text_id=$3`, owner, binding.LogicalDialogID, textID).Scan(&projectedEntryID); err != nil || projectedEntryID != newEntryID {
		t.Fatalf("repaired projected entry=%q want=%q err=%v", projectedEntryID, newEntryID, err)
	}
	searchSpec, err := ParseHistorySearchQuery(model.HistorySearchQuery{Q: "cross page tool evidence"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := database.CreateHistorySearch(ctx, owner, searchSpec, HistorySearchQueryHash(searchSpec), 10)
	if err != nil || len(result.Page.Items) != 1 || result.Page.Items[0].EntryID != newEntryID {
		t.Fatalf("cross-page search=%+v err=%v", result.Page.Items, err)
	}
}

func TestPostgresHistoryReplicaRejectsIncompleteReadyAtomically(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 17, 3, 0, 0, 0, time.UTC)
	database.now = func() time.Time { return now }
	owner := fmt.Sprintf("hl294-incomplete-%d", time.Now().UnixNano())
	verified, snapshot := inventoryFixture(owner, 1, now)
	if _, err := database.Import(ctx, verified, snapshot); err != nil {
		t.Fatal(err)
	}
	binding := listAllBindings(t, ctx, database, owner, snapshot.Nodes[0].NodeID, 10)[0]
	identity := model.HistoryStreamIdentity{OwnerID: owner, LogicalDialogID: binding.LogicalDialogID,
		NodeID: snapshot.Nodes[0].NodeID, NodeDialogID: binding.NodeDialogID, BindingGeneration: binding.BindingVersion}
	page := historyPageFixture(t, identity, now, "atomic")
	textID := model.HistoryStableUUID("history-missing-text", owner)
	page = appendHistoryFixtureRecord(t, page, model.HistoryRecordTextManifest,
		model.HistoryStableUUID("history-record-text-manifest-v1", textID), textID, 1, model.HistoryTextManifest{
			TextID: textID, Origin: model.HistoryOrigin{NodeID: identity.NodeID, NodeDialogID: identity.NodeDialogID, AuthorEventID: "text:" + textID},
			LogicalDialogID: identity.LogicalDialogID, AttemptID: "71000000-0000-4000-8000-000000000006",
			Source: json.RawMessage(`{"kind":"assistant_message"}`), Preview: "hello", Redaction: "none", Complete: true,
			SizeBytes: 5, SHA256: model.HistoryHashBytes([]byte("hello")), ChunkCount: 1,
		})
	if _, err := database.ApplyHistoryReplicaPage(ctx, owner, page); !errors.Is(err, ErrHistoryReplicaConflict) {
		t.Fatalf("missing chunk result=%v", err)
	}
	var streams, records int64
	if err := database.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM agent_service.history_replica_streams WHERE owner_id=$1),
		(SELECT count(*) FROM agent_service.history_replica_records WHERE owner_id=$1)`, owner).Scan(&streams, &records); err != nil {
		t.Fatal(err)
	}
	if streams != 0 || records != 0 {
		t.Fatalf("failed ready batch was partially committed: streams=%d records=%d", streams, records)
	}
	incompleteTextID := model.HistoryStableUUID("history-incomplete-text", owner)
	page = historyPageFixture(t, identity, now, "atomic")
	page = appendHistoryFixtureRecord(t, page, model.HistoryRecordTextManifest,
		model.HistoryStableUUID("history-record-text-manifest-v1", incompleteTextID), incompleteTextID, 1, model.HistoryTextManifest{
			TextID: incompleteTextID,
			Origin: model.HistoryOrigin{NodeID: identity.NodeID, NodeDialogID: identity.NodeDialogID,
				AuthorEventID: "text:" + incompleteTextID},
			LogicalDialogID: identity.LogicalDialogID, AttemptID: "71000000-0000-4000-8000-000000000006",
			Source: json.RawMessage(`{"kind":"assistant_message"}`), Preview: "hello", Redaction: "none",
			Complete: false, Reason: "output_limit_exceeded", SizeBytes: 5,
			SHA256: model.HistoryHashBytes([]byte("hello")), ChunkCount: 0,
		})
	if _, err := database.ApplyHistoryReplicaPage(ctx, owner, page); !errors.Is(err, ErrHistoryReplicaConflict) {
		t.Fatalf("ready checkpoint accepted incomplete text manifest: %v", err)
	}
	if err := database.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM agent_service.history_replica_streams WHERE owner_id=$1),
		(SELECT count(*) FROM agent_service.history_replica_records WHERE owner_id=$1)`, owner).Scan(&streams, &records); err != nil {
		t.Fatal(err)
	}
	if streams != 0 || records != 0 {
		t.Fatalf("incomplete ready batch was partially committed: streams=%d records=%d", streams, records)
	}
	emptyReady := model.HistoryExportPage{
		SchemaID: model.HistoryExportSchemaID, SchemaSHA256: model.HistorySchemaSHA256,
		StreamID: model.HistoryStreamID(identity), Identity: identity, Records: []model.HistoryRecord{},
		Checkpoint: model.HistoryCheckpoint{
			ChainHash: model.HistoryChainGenesis(model.HistoryStreamID(identity)), DialogVersion: 1, QueueRevision: 1,
			EntriesHash: model.HistoryGenesisHash, FactsHash: model.HistoryGenesisHash,
			ReceiptsHash: model.HistoryGenesisHash, TextHash: model.HistoryGenesisHash,
			AssetManifestHash: model.HistoryGenesisHash, Ready: true, CapturedAt: now.Format(time.RFC3339Nano),
		},
	}
	if model.ValidateHistoryExportPage(emptyReady) != nil {
		t.Fatal("empty ready fixture is not contract-valid")
	}
	if _, err := database.ApplyHistoryReplicaPage(ctx, owner, emptyReady); !errors.Is(err, ErrHistoryReplicaConflict) {
		t.Fatalf("ready checkpoint accepted missing asset declaration: %v", err)
	}
	// R12 permits opaque binary chunks. A fully proved non-UTF8 value remains
	// durable, but R14 must skip it as text without rejecting the replica page.
	binaryAttemptID := model.HistoryStableUUID("history-binary-attempt", owner)
	binaryEntryID := model.HistoryStableUUID("history-binary-entry", owner)
	binaryTextID := model.HistoryStableUUID("history-binary-text", owner)
	binaryBytes := []byte{0xff, 0xfe, 0x00}
	binaryPage := historyPageFixture(t, identity, now, "binary-safe-entry")
	binaryPage = appendHistoryFixtureRecord(t, binaryPage, model.HistoryRecordEntry,
		model.HistoryStableUUID("history-record-entry-v1", binaryEntryID), binaryEntryID, 1, model.HistoryTranscriptEntry{
			EntryID:         binaryEntryID,
			Origin:          model.HistoryOrigin{NodeID: identity.NodeID, NodeDialogID: identity.NodeDialogID, AuthorEventID: identity.NodeID + ":binary"},
			LogicalDialogID: identity.LogicalDialogID, MessageID: binaryEntryID, AttemptID: binaryAttemptID,
			Kind: "message", Role: "assistant",
			Content:   json.RawMessage(`{"kind":"inline","content":"binary target","redaction":"none","truncated":false}`),
			CreatedAt: now.Add(time.Second).Format(time.RFC3339Nano), ExecutionOrdinal: 2, ExecutionStatus: "recorded",
		})
	binaryPage = appendHistoryFixtureRecord(t, binaryPage, model.HistoryRecordTextManifest,
		model.HistoryStableUUID("history-record-text-manifest-v1", binaryTextID), binaryTextID, 1, model.HistoryTextManifest{
			TextID:          binaryTextID,
			Origin:          model.HistoryOrigin{NodeID: identity.NodeID, NodeDialogID: identity.NodeDialogID, AuthorEventID: "text:" + binaryTextID},
			LogicalDialogID: identity.LogicalDialogID, AttemptID: binaryAttemptID,
			Source: json.RawMessage(`{"kind":"tool_output"}`), Preview: "binary", Redaction: "none", Complete: true,
			SizeBytes: int64(len(binaryBytes)), SHA256: model.HistoryHashBytes(binaryBytes), ChunkCount: 1,
		})
	binaryChunkID := model.HistoryStableUUID("history-text-chunk-v1", binaryTextID+"\x000")
	binaryPage = appendHistoryFixtureRecord(t, binaryPage, model.HistoryRecordTextChunk,
		model.HistoryStableUUID("history-record-text-chunk-v1", binaryChunkID), binaryChunkID, 1, model.HistoryTextChunk{
			TextID: binaryTextID, LogicalDialogID: identity.LogicalDialogID, ChunkIndex: 0, OffsetBytes: 0,
			SizeBytes: int64(len(binaryBytes)), SHA256: model.HistoryHashBytes(binaryBytes), Bytes: binaryBytes,
		})
	if imported, err := database.ApplyHistoryReplicaPage(ctx, owner, binaryPage); err != nil || !imported.Complete {
		t.Fatalf("proved binary text import=%+v err=%v", imported, err)
	}
	var binaryChunks, binarySearchSegments int
	if err := database.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM agent_service.history_text_chunks WHERE owner_id=$1 AND text_id=$2),
		(SELECT count(*) FROM agent_service.history_search_tool_segments WHERE owner_id=$1 AND text_id=$2)`, owner, binaryTextID).
		Scan(&binaryChunks, &binarySearchSegments); err != nil || binaryChunks != 1 || binarySearchSegments != 0 {
		t.Fatalf("binary projection chunks=%d searchSegments=%d err=%v", binaryChunks, binarySearchSegments, err)
	}
}

func TestPostgresHistoryReplicaConcurrentLostACK(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 17, 4, 0, 0, 0, time.UTC)
	database.now = func() time.Time { return now }
	owner := fmt.Sprintf("hl294-concurrent-%d", time.Now().UnixNano())
	verified, snapshot := inventoryFixture(owner, 1, now)
	if _, err := database.Import(ctx, verified, snapshot); err != nil {
		t.Fatal(err)
	}
	binding := listAllBindings(t, ctx, database, owner, snapshot.Nodes[0].NodeID, 10)[0]
	identity := model.HistoryStreamIdentity{OwnerID: owner, LogicalDialogID: binding.LogicalDialogID,
		NodeID: snapshot.Nodes[0].NodeID, NodeDialogID: binding.NodeDialogID, BindingGeneration: binding.BindingVersion}
	page := historyPageFixture(t, identity, now, "concurrent")
	type outcome struct {
		result model.HistoryImportResult
		err    error
	}
	results := make(chan outcome, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	for index := 0; index < 2; index++ {
		go func() {
			ready.Done()
			<-start
			result, err := database.ApplyHistoryReplicaPage(ctx, owner, page)
			results <- outcome{result: result, err: err}
		}()
	}
	ready.Wait()
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.result.Duplicate == second.result.Duplicate ||
		!first.result.Complete || !second.result.Complete {
		t.Fatalf("concurrent lost-ACK results: first=%+v second=%+v", first, second)
	}
	assertHistoryReplicaCounts(t, ctx, database, owner, page.StreamID, int64(len(page.Records)))
}

func TestPostgresHistoryReplicaPreservesTransferredOrigin(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 17, 5, 0, 0, 0, time.UTC)
	database.now = func() time.Time { return now }
	owner := fmt.Sprintf("hl294-origin-%d", time.Now().UnixNano())
	verified, snapshot := inventoryFixture(owner, 1, now)
	if _, err := database.Import(ctx, verified, snapshot); err != nil {
		t.Fatal(err)
	}
	binding := listAllBindings(t, ctx, database, owner, snapshot.Nodes[0].NodeID, 10)[0]
	identity := model.HistoryStreamIdentity{OwnerID: owner, LogicalDialogID: binding.LogicalDialogID,
		NodeID: snapshot.Nodes[0].NodeID, NodeDialogID: binding.NodeDialogID, BindingGeneration: binding.BindingVersion}
	page := historyPageFixture(t, identity, now, "transferred")
	prior := model.HistoryOrigin{
		NodeID: "72000000-0000-4000-8000-000000000001", NodeDialogID: "72000000-0000-4000-8000-000000000002",
		AuthorEventID: "72000000-0000-4000-8000-000000000001:7",
	}
	page = rewriteHistoryFixtureOrigin(t, page, prior)
	if result, err := database.ApplyHistoryReplicaPage(ctx, owner, page); err != nil || !result.Complete {
		t.Fatalf("transferred-origin import=%+v err=%v", result, err)
	}
	read, err := database.ReadHistoryReplica(ctx, owner, identity.LogicalDialogID, HistoryReadPosition{}, 10)
	if err != nil || len(read.Page.Entries) != 1 || read.Page.Entries[0].Origin.NodeID != prior.NodeID {
		t.Fatalf("transferred origin was rewritten: %+v err=%v", read, err)
	}
	lookup, err := database.HistoryReceiptByOrigin(ctx, owner, prior.NodeID, read.Page.Receipts[0].CommandID)
	if err != nil || lookup.Receipt.Origin.NodeDialogID != prior.NodeDialogID || lookup.Checkpoint != page.Checkpoint {
		t.Fatalf("transferred receipt origin lookup=%+v err=%v", lookup, err)
	}
}

func appendHistoryFixtureRecord(t *testing.T, page model.HistoryExportPage, recordType, recordID, entityID string,
	revision int64, payload any) model.HistoryExportPage {
	t.Helper()
	record := historyRecordFixture(t, page.StreamID, recordType, recordID, entityID, revision, payload,
		page.Checkpoint.ThroughSeq+1, page.Checkpoint.ChainHash)
	page.Records = append(append([]model.HistoryRecord(nil), page.Records...), record)
	foldHistoryCheckpoint(&page.Checkpoint, record)
	page.Checkpoint.Ready = true
	page.Checkpoint.IncompleteReason = ""
	page.AfterSeq = 0
	page.NextAfter = nil
	if model.ValidateHistoryExportPage(page) != nil {
		t.Fatal("extended history fixture is invalid")
	}
	return page
}

func historyTailPage(t *testing.T, before, after model.HistoryExportPage) model.HistoryExportPage {
	t.Helper()
	if before.StreamID != after.StreamID || before.Checkpoint.ThroughSeq >= after.Checkpoint.ThroughSeq {
		t.Fatal("invalid history tail fixtures")
	}
	result := after
	result.AfterSeq = before.Checkpoint.ThroughSeq
	result.Records = append([]model.HistoryRecord(nil), after.Records[len(before.Records):]...)
	if model.ValidateHistoryExportPage(result) != nil {
		t.Fatal("history tail fixture is invalid")
	}
	return result
}

func historyPrefixPage(t *testing.T, source model.HistoryExportPage, count int) model.HistoryExportPage {
	t.Helper()
	if count < 1 || count >= len(source.Records) {
		t.Fatal("invalid history prefix count")
	}
	checkpoint := model.HistoryCheckpoint{
		ChainHash: model.HistoryChainGenesis(source.StreamID), DialogVersion: source.Checkpoint.DialogVersion,
		QueueRevision: source.Checkpoint.QueueRevision, EntriesHash: model.HistoryGenesisHash,
		FactsHash: model.HistoryGenesisHash, ReceiptsHash: model.HistoryGenesisHash,
		TextHash: model.HistoryGenesisHash, AssetManifestHash: model.HistoryGenesisHash,
		Ready: false, IncompleteReason: "replica_lag", CapturedAt: source.Checkpoint.CapturedAt,
	}
	result := source
	result.Records = append([]model.HistoryRecord(nil), source.Records[:count]...)
	result.NextAfter = nil
	result.AfterSeq = 0
	for _, record := range result.Records {
		foldHistoryCheckpoint(&checkpoint, record)
	}
	result.Checkpoint = checkpoint
	if model.ValidateHistoryExportPage(result) != nil {
		t.Fatal("history prefix fixture is invalid")
	}
	return result
}

func rewriteHistoryFixtureOrigin(t *testing.T, source model.HistoryExportPage, origin model.HistoryOrigin) model.HistoryExportPage {
	t.Helper()
	checkpoint := model.HistoryCheckpoint{
		ChainHash: model.HistoryChainGenesis(source.StreamID), DialogVersion: source.Checkpoint.DialogVersion,
		QueueRevision: source.Checkpoint.QueueRevision, EntriesHash: model.HistoryGenesisHash,
		FactsHash: model.HistoryGenesisHash, ReceiptsHash: model.HistoryGenesisHash,
		TextHash: model.HistoryGenesisHash, AssetManifestHash: model.HistoryGenesisHash,
		Ready: true, CapturedAt: source.Checkpoint.CapturedAt,
	}
	result := source
	result.Records = nil
	previous := checkpoint.ChainHash
	for index, sourceRecord := range source.Records {
		payload, err := model.DecodeHistoryPayload(sourceRecord)
		if err != nil {
			t.Fatal(err)
		}
		switch value := payload.(type) {
		case *model.HistoryTranscriptEntry:
			value.Origin = origin
		case *model.HistoryExecutionFact:
			value.Origin = origin
		case *model.HistoryReceiptRevision:
			value.Origin = origin
		case *model.HistoryTextManifest:
			value.Origin = origin
		}
		record := historyRecordFixture(t, source.StreamID, sourceRecord.Type, sourceRecord.RecordID,
			sourceRecord.EntityID, sourceRecord.Revision, payload, int64(index+1), previous)
		result.Records = append(result.Records, record)
		previous = record.ChainHash
		foldHistoryCheckpoint(&checkpoint, record)
	}
	result.Checkpoint = checkpoint
	if model.ValidateHistoryExportPage(result) != nil {
		t.Fatal("transferred-origin fixture is invalid")
	}
	return result
}

func splitHistoryPage(t *testing.T, page model.HistoryExportPage, count int) (model.HistoryExportPage, model.HistoryExportPage) {
	t.Helper()
	if count < 1 || count >= len(page.Records) {
		t.Fatal("invalid history split")
	}
	boundary := page.Records[count-1].StreamSeq
	first := page
	first.Records = append([]model.HistoryRecord(nil), page.Records[:count]...)
	first.NextAfter = &boundary
	second := page
	second.AfterSeq = boundary
	second.Records = append([]model.HistoryRecord(nil), page.Records[count:]...)
	if model.ValidateHistoryExportPage(first) != nil || model.ValidateHistoryExportPage(second) != nil {
		t.Fatal("split history fixture is invalid")
	}
	return first, second
}

func historyPageFixture(t *testing.T, identity model.HistoryStreamIdentity, captured time.Time, text string) model.HistoryExportPage {
	t.Helper()
	entryID := "71000000-0000-4000-8000-000000000001"
	factID := "71000000-0000-4000-8000-000000000002"
	receiptID := "71000000-0000-4000-8000-000000000003"
	commandID := "71000000-0000-4000-8000-000000000004"
	entry := model.HistoryTranscriptEntry{
		EntryID: entryID, Origin: model.HistoryOrigin{NodeID: identity.NodeID, NodeDialogID: identity.NodeDialogID, AuthorEventID: identity.NodeID + ":1"},
		LogicalDialogID: identity.LogicalDialogID, MessageID: entryID, RequestID: "71000000-0000-4000-8000-000000000005",
		Kind: "message", Role: "user", Content: json.RawMessage(fmt.Sprintf(`{"kind":"inline","content":%q}`, text)),
		CreatedAt: captured.Format(time.RFC3339Nano), SourceGeneration: 0, ExecutionOrdinal: 1, ExecutionStatus: "recorded",
	}
	fact := model.HistoryExecutionFact{
		FactID: factID, Origin: model.HistoryOrigin{NodeID: identity.NodeID, NodeDialogID: identity.NodeDialogID, AuthorEventID: identity.NodeID + ":2"},
		LogicalDialogID: identity.LogicalDialogID, RequestID: entry.RequestID, AttemptID: "71000000-0000-4000-8000-000000000006",
		Kind: "attempt.completed", Status: "attempt.completed", EventSeq: 2, OccurredAt: captured.Add(time.Second).Format(time.RFC3339Nano),
		Event: json.RawMessage(`{"type":"attempt.completed","payload":{"output":{"kind":"empty"}}}`),
	}
	receipt := model.HistoryReceiptRevision{
		ReceiptRevisionID: receiptID,
		Origin:            model.HistoryOrigin{NodeID: identity.NodeID, NodeDialogID: identity.NodeDialogID, AuthorEventID: identity.NodeID + ":1"},
		LogicalDialogID:   identity.LogicalDialogID, CommandID: commandID, CommandKind: "message.enqueue",
		Fingerprint: model.HistoryHashBytes([]byte("command")), RequestID: entry.RequestID, Revision: 1,
		PredecessorHash: model.HistoryGenesisHash, Admission: "accepted", Outcome: "accepted",
		Receipt: json.RawMessage(`{"result":"accepted"}`), AcceptedAt: captured.Format(time.RFC3339Nano),
	}
	assetEntity := model.HistoryStableUUID("history-asset-manifest-entity-v1", identity.NodeDialogID)
	values := []struct {
		typeName, recordID, entityID string
		payload                      any
	}{
		{model.HistoryRecordEntry, model.HistoryStableUUID("history-record-entry-v1", entryID), entryID, entry},
		{model.HistoryRecordFact, model.HistoryStableUUID("history-record-fact-v1", factID), factID, fact},
		{model.HistoryRecordReceipt, receiptID, receiptID, receipt},
		{model.HistoryRecordAssetManifest, model.HistoryStableUUID("history-record-asset-manifest-v1", assetEntity+"\x00empty"), assetEntity,
			model.HistoryAssetManifest{LogicalDialogID: identity.LogicalDialogID, Assets: []model.HistoryAsset{}}},
	}
	checkpoint := model.HistoryCheckpoint{
		ChainHash: model.HistoryChainGenesis(model.HistoryStreamID(identity)), DialogVersion: 2, QueueRevision: 3,
		EntriesHash: model.HistoryGenesisHash, FactsHash: model.HistoryGenesisHash,
		ReceiptsHash: model.HistoryGenesisHash, TextHash: model.HistoryGenesisHash,
		AssetManifestHash: model.HistoryGenesisHash, CapturedAt: captured.Add(2 * time.Second).Format(time.RFC3339Nano),
	}
	records := make([]model.HistoryRecord, 0, len(values))
	previous := model.HistoryChainGenesis(model.HistoryStreamID(identity))
	for index, value := range values {
		record := historyRecordFixture(t, model.HistoryStreamID(identity), value.typeName, value.recordID, value.entityID, 1, value.payload, int64(index+1), previous)
		records = append(records, record)
		previous = record.ChainHash
		foldHistoryCheckpoint(&checkpoint, record)
	}
	checkpoint.Ready = true
	return model.HistoryExportPage{
		SchemaID: model.HistoryExportSchemaID, SchemaSHA256: model.HistorySchemaSHA256,
		StreamID: model.HistoryStreamID(identity), Identity: identity, Records: records, Checkpoint: checkpoint,
	}
}

func historyRecordFixture(t *testing.T, streamID, recordType, recordID, entityID string, revision int64, payload any, sequence int64, previous string) model.HistoryRecord {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	core := struct {
		Type     string          `json:"type"`
		RecordID string          `json:"recordId"`
		EntityID string          `json:"entityId"`
		Revision int64           `json:"revision"`
		Payload  json.RawMessage `json:"payload"`
	}{recordType, recordID, entityID, revision, raw}
	canonical, err := json.Marshal(core)
	if err != nil {
		t.Fatal(err)
	}
	recordHash := model.HistoryHashBytes(canonical)
	return model.HistoryRecord{
		StreamSeq: sequence, Type: recordType, RecordID: recordID, EntityID: entityID, Revision: revision, Payload: raw,
		RecordHash: recordHash, PrevHash: previous,
		ChainHash: model.HistoryHashBytes([]byte(fmt.Sprintf("history-chain-v1\x00%s\x00%d\x00%s\x00%s", streamID, sequence, previous, recordHash))),
	}
}

func assertHistoryReplicaCounts(t *testing.T, ctx context.Context, database *Store, owner, streamID string, want int64) {
	t.Helper()
	var records, entries, facts, receipts, manifests int64
	if err := database.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM agent_service.history_replica_records WHERE owner_id=$1 AND stream_id=$2),
		(SELECT count(*) FROM agent_service.history_entries WHERE owner_id=$1),
		(SELECT count(*) FROM agent_service.history_execution_facts WHERE owner_id=$1),
		(SELECT count(*) FROM agent_service.history_receipt_revisions WHERE owner_id=$1),
		(SELECT count(*) FROM agent_service.history_asset_manifests WHERE owner_id=$1)`, owner, streamID).
		Scan(&records, &entries, &facts, &receipts, &manifests); err != nil {
		t.Fatal(err)
	}
	wantEntries, wantFacts, wantReceipts, wantManifests := int64(0), int64(0), int64(0), int64(0)
	if want >= 1 {
		wantEntries = 1
	}
	if want >= 2 {
		wantFacts = 1
	}
	if want >= 3 {
		wantReceipts = 1
	}
	if want >= 4 {
		wantManifests = 1
	}
	if records != want || entries != wantEntries || facts != wantFacts || receipts != wantReceipts || manifests != wantManifests {
		t.Fatalf("history rows records=%d entries=%d facts=%d receipts=%d manifests=%d", records, entries, facts, receipts, manifests)
	}
}

func assertHistoryReplicaRead(t *testing.T, ctx context.Context, database *Store, owner string, identity model.HistoryStreamIdentity, source model.HistoryExportPage) {
	t.Helper()
	read, err := database.ReadHistoryReplica(ctx, owner, identity.LogicalDialogID, HistoryReadPosition{}, 10)
	if err != nil || read.Page.Incomplete || int64(len(read.Page.Entries)) != source.Checkpoint.EffectsSummary.EntryCount ||
		int64(len(read.Page.Facts)) != source.Checkpoint.EffectsSummary.FactCount ||
		int64(len(read.Page.Receipts)) != source.Checkpoint.EffectsSummary.ReceiptRevisionCount ||
		read.Page.SyncedThrough != source.Checkpoint {
		t.Fatalf("offline history=%+v err=%v", read, err)
	}
	receipt, err := database.HistoryReceiptByOrigin(ctx, owner, identity.NodeID, read.Page.Receipts[0].CommandID)
	if err != nil || receipt.Receipt.Origin.NodeID != identity.NodeID || receipt.Checkpoint != source.Checkpoint {
		t.Fatalf("origin receipt=%+v err=%v", receipt, err)
	}
}
