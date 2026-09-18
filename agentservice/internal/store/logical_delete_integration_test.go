package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

type logicalDeleteFixture struct {
	owner    string
	nodeID   string
	binding  model.DialogMapping
	identity model.HistoryStreamIdentity
	page     model.HistoryExportPage
	textID   string
}

func prepareLogicalDeleteFixture(t *testing.T, ctx context.Context, database *Store, owner string, now time.Time, withText bool) logicalDeleteFixture {
	t.Helper()
	verified, snapshot := inventoryFixture(owner, 1, now)
	if _, err := database.Import(ctx, verified, snapshot); err != nil {
		t.Fatal(err)
	}
	nodeID := snapshot.Nodes[0].NodeID
	binding := listAllBindings(t, ctx, database, owner, nodeID, 10)[0]
	identity := model.HistoryStreamIdentity{
		OwnerID: owner, LogicalDialogID: binding.LogicalDialogID, NodeID: nodeID,
		NodeDialogID: binding.NodeDialogID, BindingGeneration: binding.BindingVersion,
	}
	page := historyPageFixture(t, identity, now, "delete fixture")
	textID := ""
	if withText {
		content := []byte("durable deleted text")
		textID = model.HistoryStableUUID("logical-delete-text", owner)
		manifest := model.HistoryTextManifest{
			TextID:          textID,
			Origin:          model.HistoryOrigin{NodeID: nodeID, NodeDialogID: binding.NodeDialogID, AuthorEventID: nodeID + ":5"},
			LogicalDialogID: binding.LogicalDialogID, AttemptID: "71000000-0000-4000-8000-000000000006",
			Source: json.RawMessage(`{"kind":"assistant_message"}`), Preview: string(content), Redaction: "none",
			Complete: true, SizeBytes: int64(len(content)), SHA256: model.HistoryHashBytes(content), ChunkCount: 1,
		}
		page = appendHistoryFixtureRecord(t, page, model.HistoryRecordTextManifest,
			model.HistoryStableUUID("history-record-text-manifest-v1", textID), textID, 1, manifest)
		chunkEntity := model.HistoryStableUUID("history-text-chunk-v1", textID+"\x000")
		page = appendHistoryFixtureRecord(t, page, model.HistoryRecordTextChunk,
			model.HistoryStableUUID("history-record-text-chunk-v1", chunkEntity+"\x00"+model.HistoryHashBytes(content)),
			chunkEntity, 1, model.HistoryTextChunk{
				TextID: textID, LogicalDialogID: binding.LogicalDialogID, ChunkIndex: 0,
				OffsetBytes: 0, SizeBytes: int64(len(content)), SHA256: model.HistoryHashBytes(content), Bytes: content,
			})
	}
	if imported, err := database.ApplyHistoryReplicaPage(ctx, owner, page); err != nil || !imported.Complete {
		t.Fatalf("history fixture import=%+v err=%v", imported, err)
	}
	return logicalDeleteFixture{owner: owner, nodeID: nodeID, binding: binding, identity: identity, page: page, textID: textID}
}

func TestPostgresLogicalDeleteActiveCASAndTombstoneVisibility(t *testing.T) {
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
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	database.now = func() time.Time { return now }
	fixture := prepareLogicalDeleteFixture(t, ctx, database, fmt.Sprintf("hl295-delete-%d", time.Now().UnixNano()), now, true)
	oldRead, err := database.ReadHistoryReplica(ctx, fixture.owner, fixture.binding.LogicalDialogID, HistoryReadPosition{}, 1)
	if err != nil || len(oldRead.Position.Horizons) != 1 {
		t.Fatalf("old cursor fixture=%+v err=%v", oldRead, err)
	}
	if _, err := database.HistoryTextManifest(ctx, fixture.owner, fixture.binding.LogicalDialogID, fixture.textID); err != nil {
		t.Fatal(err)
	}
	searchSpec, err := ParseHistorySearchQuery(model.HistorySearchQuery{Q: "delete fixture"})
	if err != nil {
		t.Fatal(err)
	}
	preDeleteSearch, err := database.CreateHistorySearch(ctx, fixture.owner, searchSpec, HistorySearchQueryHash(searchSpec), 10)
	if err != nil || preDeleteSearch.Page.TotalCount != 1 || len(preDeleteSearch.Page.Items) != 1 {
		t.Fatalf("pre-delete search=%+v err=%v", preDeleteSearch, err)
	}
	deletedEntryID := preDeleteSearch.Page.Items[0].EntryID
	if lookup, err := database.HistoryEntryByID(ctx, fixture.owner, fixture.binding.LogicalDialogID, deletedEntryID); err != nil ||
		lookup.Entry.EntryID != deletedEntryID {
		t.Fatalf("pre-delete exact entry=%+v err=%v", lookup, err)
	}
	request := model.LogicalDeleteRequest{
		SchemaID: model.LogicalDeleteSchemaID, OperationID: "81000000-0000-4000-8000-000000000001",
		CommandID: "81000000-0000-4000-8000-000000000002", LogicalDialogID: fixture.binding.LogicalDialogID,
		ExpectedBindingVersion: fixture.binding.BindingVersion, ExpectedDialogVersion: fixture.page.Checkpoint.DialogVersion,
	}
	accepted, err := database.BeginLogicalDelete(ctx, fixture.owner, request)
	if err != nil || accepted.Phase != "accepted" || accepted.EffectState != "not_sent" || accepted.OperationVersion != 1 {
		t.Fatalf("begin=%+v err=%v", accepted, err)
	}
	if replay, replayErr := database.BeginLogicalDelete(ctx, fixture.owner, request); replayErr != nil || replay != accepted {
		t.Fatalf("begin replay=%+v err=%v want=%+v", replay, replayErr, accepted)
	}
	if bindings := listAllBindings(t, ctx, database, fixture.owner, fixture.nodeID, 10); len(bindings) != 0 {
		t.Fatalf("deleting binding remained public: %+v", bindings)
	}
	if _, err := database.ApplyHistoryReplicaPage(ctx, fixture.owner, fixture.page); !errors.Is(err, ErrHistoryReplicaScope) {
		t.Fatalf("late sync crossed delete barrier: %v", err)
	}
	holding, err := database.AdvanceLogicalDelete(ctx, fixture.owner, model.LogicalDeleteAdvance{
		SchemaID: model.LogicalDeleteAdvanceSchemaID, OperationID: accepted.OperationID, RequestHash: accepted.RequestHash,
		ExpectedOperationVersion: accepted.OperationVersion, Action: "hold", HoldVersion: 1,
		ObservedHoldScopeRevision: accepted.HoldScopeRevision + 1,
	})
	if err != nil || holding.Phase != "holding" || holding.OperationVersion != 2 {
		t.Fatalf("holding=%+v err=%v", holding, err)
	}
	receipt := model.LogicalDeleteNodeReceipt{
		SchemaID: model.LogicalDeleteNodeSchemaID, OperationID: holding.OperationID,
		NodeRequestHash: strings.Repeat("b", 64), CoordinatorRequestHash: holding.RequestHash,
		ReceiptID: "81000000-0000-4000-8000-000000000003", CommandID: holding.CommandID,
		LogicalDialogID: holding.LogicalDialogID, NodeID: holding.NodeID, NodeDialogID: holding.NodeDialogID,
		Epoch: holding.IdentityEpoch, RegistryVersion: holding.RegistryVersion, BindingVersion: holding.ExpectedBindingVersion,
		DeletedDialogVersion: holding.ExpectedDialogVersion + 1, HoldVersion: holding.HoldVersion,
		HoldScopeRevision: holding.HoldScopeRevision + 1, TombstoneEventSeq: 9,
		CommandReceipt: json.RawMessage(`{"protocolVersion":1,"schemaId":"harness-wire-v2","commandId":"81000000-0000-4000-8000-000000000002","commandKind":"dialog.delete","receiptId":"81000000-0000-4000-8000-000000000004","acceptedAt":"2026-09-17T10:00:00Z","nodeId":"20000000-0000-4000-8000-000000000001","eventSeq":9,"result":"deleted","references":{"dialogId":"30000000-0000-4000-8000-000000000001"}}`),
		DeletedAt:      now.Format(time.RFC3339Nano),
	}
	advance := model.LogicalDeleteAdvance{
		SchemaID: model.LogicalDeleteAdvanceSchemaID, OperationID: holding.OperationID, RequestHash: holding.RequestHash,
		ExpectedOperationVersion: holding.OperationVersion, Action: "complete", NodeReceipt: &receipt,
	}
	completed, err := database.AdvanceLogicalDelete(ctx, fixture.owner, advance)
	if err != nil || completed.Phase != "succeeded" || completed.EffectState != "reconciled" || completed.Archived {
		t.Fatalf("complete=%+v err=%v", completed, err)
	}
	if replay, replayErr := database.AdvanceLogicalDelete(ctx, fixture.owner, advance); replayErr != nil || replay.OperationVersion != completed.OperationVersion {
		t.Fatalf("lost-ACK completion replay=%+v err=%v want=%+v", replay, replayErr, completed)
	}
	if _, err := database.ReadHistoryReplica(ctx, fixture.owner, fixture.binding.LogicalDialogID, oldRead.Position, 1); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old cursor survived tombstone: %v", err)
	}
	if _, err := database.ApplyHistoryReplicaPage(ctx, fixture.owner, fixture.page); !errors.Is(err, ErrHistoryReplicaScope) {
		t.Fatalf("post-tombstone sync crossed barrier: %v", err)
	}
	postDeleteSnapshot, err := database.ReadHistorySearch(ctx, fixture.owner, searchSpec.Query, preDeleteSearch.Position, 10)
	if err != nil || postDeleteSnapshot.Page.TotalCount != 0 || len(postDeleteSnapshot.Page.Items) != 0 {
		t.Fatalf("old search snapshot survived tombstone: %+v err=%v", postDeleteSnapshot, err)
	}
	postDeleteSearch, err := database.CreateHistorySearch(ctx, fixture.owner, searchSpec, HistorySearchQueryHash(searchSpec), 10)
	if err != nil || postDeleteSearch.Page.TotalCount != 0 || len(postDeleteSearch.Page.Items) != 0 {
		t.Fatalf("fresh search survived tombstone: %+v err=%v", postDeleteSearch, err)
	}
	if _, err := database.HistoryEntryByID(ctx, fixture.owner, fixture.binding.LogicalDialogID, deletedEntryID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("exact search entry survived tombstone: %v", err)
	}
	if _, err := database.HistoryReceiptByOrigin(ctx, fixture.owner, fixture.nodeID, "71000000-0000-4000-8000-000000000004"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old receipt survived tombstone: %v", err)
	}
	if _, err := database.HistoryTextManifest(ctx, fixture.owner, fixture.binding.LogicalDialogID, fixture.textID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old text manifest survived tombstone: %v", err)
	}
	if _, err := database.HistoryTextChunk(ctx, fixture.owner, fixture.binding.LogicalDialogID, fixture.textID, 0); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old text chunk survived tombstone: %v", err)
	}
	var logicalRows, bindingRows, historyRows, tombstones int
	if err := database.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM agent_service.logical_dialogs WHERE owner_id=$1 AND logical_dialog_id=$2),
		(SELECT count(*) FROM agent_service.dialog_bindings WHERE owner_id=$1 AND logical_dialog_id=$2),
		(SELECT count(*) FROM agent_service.history_replica_records WHERE owner_id=$1 AND stream_id=$3),
		(SELECT count(*) FROM agent_service.logical_dialog_tombstones WHERE owner_id=$1 AND logical_dialog_id=$2)`,
		fixture.owner, fixture.binding.LogicalDialogID, fixture.page.StreamID).Scan(&logicalRows, &bindingRows, &historyRows, &tombstones); err != nil {
		t.Fatal(err)
	}
	if logicalRows != 1 || bindingRows != 1 || historyRows != len(fixture.page.Records) || tombstones != 1 {
		t.Fatalf("logical delete physically removed data: logical=%d binding=%d history=%d tombstones=%d", logicalRows, bindingRows, historyRows, tombstones)
	}
	t.Logf("r13_r14_cross_seam owner=%s snapshot=%s entry=%s before=%d stale_after=%d fresh_after=%d retained_records=%d tombstones=%d",
		fixture.owner, preDeleteSearch.Position.SnapshotID, deletedEntryID, preDeleteSearch.Page.TotalCount,
		postDeleteSnapshot.Page.TotalCount, postDeleteSearch.Page.TotalCount, historyRows, tombstones)
}

func TestPostgresLogicalDeleteArchiveAndSharedReservationCAS(t *testing.T) {
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
	now := time.Date(2026, 9, 17, 11, 0, 0, 0, time.UTC)
	database.now = func() time.Time { return now }

	archive := prepareLogicalDeleteFixture(t, ctx, database, fmt.Sprintf("hl295-archive-%d", time.Now().UnixNano()), now, false)
	checkpoint, err := json.Marshal(archive.page.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(checkpoint)
	if err := database.RecordDialogClosure(ctx, archive.owner, model.LogicalDialogClosure{
		LogicalDialogID: archive.binding.LogicalDialogID, BindingVersion: archive.binding.BindingVersion,
		NodeID: archive.nodeID, NodeDialogID: archive.binding.NodeDialogID,
		DialogVersion: archive.page.Checkpoint.DialogVersion, DescriptorVersion: 1, ZeroPending: true,
		FinalCheckpoint: checkpoint, FinalCheckpointSHA256: hex.EncodeToString(digest[:]), VerifiedAt: now.Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	archived, err := database.BeginLogicalDelete(ctx, archive.owner, model.LogicalDeleteRequest{
		SchemaID: model.LogicalDeleteSchemaID, OperationID: "82000000-0000-4000-8000-000000000001",
		CommandID: "82000000-0000-4000-8000-000000000002", LogicalDialogID: archive.binding.LogicalDialogID,
		ExpectedBindingVersion: archive.binding.BindingVersion, ExpectedDialogVersion: archive.page.Checkpoint.DialogVersion,
	})
	if err != nil || !archived.Archived || archived.Phase != "succeeded" || archived.NodeReceipt != nil {
		t.Fatalf("archive delete=%+v err=%v", archived, err)
	}
	if _, err := database.ReadHistoryReplica(ctx, archive.owner, archive.binding.LogicalDialogID, HistoryReadPosition{}, 10); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("archive history remained visible: %v", err)
	}

	race := prepareLogicalDeleteFixture(t, ctx, database, fmt.Sprintf("hl295-race-%d", time.Now().UnixNano()), now, false)
	deleteRequest := model.LogicalDeleteRequest{
		SchemaID: model.LogicalDeleteSchemaID, OperationID: "82000000-0000-4000-8000-000000000003",
		CommandID: "82000000-0000-4000-8000-000000000004", LogicalDialogID: race.binding.LogicalDialogID,
		ExpectedBindingVersion: race.binding.BindingVersion, ExpectedDialogVersion: race.page.Checkpoint.DialogVersion,
	}
	start := make(chan struct{})
	results := make(chan bool, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, beginErr := database.BeginLogicalDelete(ctx, race.owner, deleteRequest)
		results <- beginErr == nil
	}()
	go func() {
		defer wait.Done()
		<-start
		_, reserveErr := database.pool.Exec(ctx, `INSERT INTO agent_service.dialog_operation_reservations(
			owner_id,operation_id,logical_dialog_id,operation_kind,request_hash,state,created_at,updated_at)
			VALUES($1,$2,$3,'transfer',$4,'active',$5,$5)`, race.owner,
			"82000000-0000-4000-8000-000000000005", race.binding.LogicalDialogID, strings.Repeat("c", 64), now)
		results <- reserveErr == nil
	}()
	close(start)
	wait.Wait()
	close(results)
	winners := 0
	for succeeded := range results {
		if succeeded {
			winners++
		}
	}
	var active int
	if err := database.pool.QueryRow(ctx, `SELECT count(*) FROM agent_service.dialog_operation_reservations
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND state='active'`, race.owner, race.binding.LogicalDialogID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if winners != 1 || active != 1 {
		t.Fatalf("shared operation CAS winners=%d active=%d", winners, active)
	}
}

func TestPostgresLogicalDeleteRejectionRestoresBindingAndOwnReservation(t *testing.T) {
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
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	database.now = func() time.Time { return now }
	fixture := prepareLogicalDeleteFixture(t, ctx, database, fmt.Sprintf("hl295-reject-%d", time.Now().UnixNano()), now, false)
	request := model.LogicalDeleteRequest{
		SchemaID: model.LogicalDeleteSchemaID, OperationID: "83000000-0000-4000-8000-000000000001",
		CommandID: "83000000-0000-4000-8000-000000000002", LogicalDialogID: fixture.binding.LogicalDialogID,
		ExpectedBindingVersion: fixture.binding.BindingVersion, ExpectedDialogVersion: fixture.page.Checkpoint.DialogVersion,
	}
	accepted, err := database.BeginLogicalDelete(ctx, fixture.owner, request)
	if err != nil {
		t.Fatal(err)
	}
	holding, err := database.AdvanceLogicalDelete(ctx, fixture.owner, model.LogicalDeleteAdvance{
		SchemaID: model.LogicalDeleteAdvanceSchemaID, OperationID: accepted.OperationID, RequestHash: accepted.RequestHash,
		ExpectedOperationVersion: accepted.OperationVersion, Action: "hold", HoldVersion: 3,
		ObservedHoldScopeRevision: accepted.HoldScopeRevision + 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	reject := model.LogicalDeleteAdvance{
		SchemaID: model.LogicalDeleteAdvanceSchemaID, OperationID: holding.OperationID, RequestHash: holding.RequestHash,
		ExpectedOperationVersion: holding.OperationVersion, Action: "reject", ResultCode: "node_guard_rejected",
		ObservedHoldScopeRevision: holding.HoldScopeRevision + 1,
	}
	rejected, err := database.AdvanceLogicalDelete(ctx, fixture.owner, reject)
	if err != nil || rejected.Phase != "failed" || rejected.EffectState != "failed" || rejected.ResultCode != reject.ResultCode {
		t.Fatalf("reject=%+v err=%v", rejected, err)
	}
	if replay, replayErr := database.AdvanceLogicalDelete(ctx, fixture.owner, reject); replayErr != nil || replay != rejected {
		t.Fatalf("reject replay=%+v err=%v want=%+v", replay, replayErr, rejected)
	}
	bindings := listAllBindings(t, ctx, database, fixture.owner, fixture.nodeID, 10)
	if len(bindings) != 1 || bindings[0] != fixture.binding {
		t.Fatalf("rejected delete did not restore exact binding: %+v", bindings)
	}
	var reservationState string
	var holdScopeRevision int64
	var tombstones int
	if err := database.pool.QueryRow(ctx, `SELECT
		(SELECT state FROM agent_service.dialog_operation_reservations WHERE owner_id=$1 AND operation_id=$2),
		(SELECT hold_scope_revision FROM agent_service.logical_dialogs WHERE owner_id=$1 AND logical_dialog_id=$3),
		(SELECT count(*) FROM agent_service.logical_dialog_tombstones WHERE owner_id=$1 AND logical_dialog_id=$3)`,
		fixture.owner, request.OperationID, request.LogicalDialogID).Scan(&reservationState, &holdScopeRevision, &tombstones); err != nil {
		t.Fatal(err)
	}
	if reservationState != "failed" || holdScopeRevision != reject.ObservedHoldScopeRevision || tombstones != 0 {
		t.Fatalf("reject durable state reservation=%s holdRevision=%d tombstones=%d", reservationState, holdScopeRevision, tombstones)
	}
	if _, err := database.ApplyHistoryReplicaPage(ctx, fixture.owner, fixture.page); err != nil {
		t.Fatalf("replication remained blocked after rejection: %v", err)
	}

	preHold := prepareLogicalDeleteFixture(t, ctx, database, fmt.Sprintf("hl295-pre-hold-reject-%d", time.Now().UnixNano()), now, false)
	preHoldRequest := model.LogicalDeleteRequest{
		SchemaID: model.LogicalDeleteSchemaID, OperationID: "83000000-0000-4000-8000-000000000003",
		CommandID: "83000000-0000-4000-8000-000000000004", LogicalDialogID: preHold.binding.LogicalDialogID,
		ExpectedBindingVersion: preHold.binding.BindingVersion, ExpectedDialogVersion: preHold.page.Checkpoint.DialogVersion,
	}
	preHoldAccepted, err := database.BeginLogicalDelete(ctx, preHold.owner, preHoldRequest)
	if err != nil {
		t.Fatal(err)
	}
	preHoldReject := model.LogicalDeleteAdvance{
		SchemaID: model.LogicalDeleteAdvanceSchemaID, OperationID: preHoldAccepted.OperationID,
		RequestHash: preHoldAccepted.RequestHash, ExpectedOperationVersion: preHoldAccepted.OperationVersion,
		Action: "reject", ResultCode: "hold_rejected", ObservedHoldScopeRevision: preHoldAccepted.HoldScopeRevision,
	}
	preHoldFailed, err := database.AdvanceLogicalDelete(ctx, preHold.owner, preHoldReject)
	if err != nil || preHoldFailed.Phase != "failed" || preHoldFailed.HoldVersion != 0 ||
		preHoldFailed.HoldScopeRevision != preHoldAccepted.HoldScopeRevision {
		t.Fatalf("pre-hold reject=%+v err=%v", preHoldFailed, err)
	}
	if replay, replayErr := database.AdvanceLogicalDelete(ctx, preHold.owner, preHoldReject); replayErr != nil || replay != preHoldFailed {
		t.Fatalf("pre-hold reject replay=%+v err=%v want=%+v", replay, replayErr, preHoldFailed)
	}
	preHoldBindings := listAllBindings(t, ctx, database, preHold.owner, preHold.nodeID, 10)
	if len(preHoldBindings) != 1 || preHoldBindings[0] != preHold.binding {
		t.Fatalf("pre-hold rejection did not restore exact binding: %+v", preHoldBindings)
	}
	var preHoldReservation string
	if err := database.pool.QueryRow(ctx, `SELECT state FROM agent_service.dialog_operation_reservations
		WHERE owner_id=$1 AND operation_id=$2`, preHold.owner, preHoldRequest.OperationID).Scan(&preHoldReservation); err != nil {
		t.Fatal(err)
	}
	if preHoldReservation != "failed" {
		t.Fatalf("pre-hold reservation state=%s", preHoldReservation)
	}
}
