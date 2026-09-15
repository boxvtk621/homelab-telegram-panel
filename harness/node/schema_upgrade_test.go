package node_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/harness/fixture"
	"github.com/boxvtk621/homelab-telegram-panel/harness/node"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessadapter"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessbarrier"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	_ "modernc.org/sqlite"
)

const (
	legacySchemaFingerprintV1 = "2ef224cb3489121c3b8fb21f38bba849b2a7eb36383eb2fae1a5e255099b987d"
	legacySchemaFingerprintV2 = "5ae1b8abce397d2cb3e5221757c3069529b302d7842ab791dc430442bcc101df"
)

var (
	currentSchemaTokenForTest = []byte(`"schemaId":"harness-wire-v2"`)
	legacySchemaTokenForTest  = []byte(`"schemaId":"harness-wire-v1"`)
)

type legacyVolume struct {
	path            string
	replayCommand   []byte
	replayReceipt   []byte
	replayCommandID string
	attemptID       string
	eventCount      int
}

func prepareLegacyV1Volume(t *testing.T) legacyVolume {
	t.Helper()
	ctx := context.Background()
	path := t.TempDir()
	config := testConfig(path)
	config.Policies = fixture.NewPolicySource()
	opened, err := node.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}

	create := command(t, "31000000-0000-4000-8000-000000000001", "dialog.create",
		map[string]any{"nodeId": testNodeID}, map[string]any{"registryVersion": 1}, map[string]any{"title": "legacy"})
	createReceipt := decodeReceipt(t, opened.SubmitCommand(ctx, nodeTrust(), create), 202)
	var dialog harnessprotocol.DialogCreateReferences
	if err := json.Unmarshal(createReceipt.References, &dialog); err != nil {
		t.Fatal(err)
	}
	replayCommandID := "31000000-0000-4000-8000-000000000002"
	replayCommand := command(t, replayCommandID, "message.enqueue",
		map[string]any{"nodeId": testNodeID, "dialogId": dialog.DialogID}, map[string]any{"dialogVersion": 1}, map[string]any{"text": "legacy durable message"})
	replayResult := opened.SubmitCommand(ctx, nodeTrust(), replayCommand)
	replayReceipt := append([]byte(nil), replayResult.Body...)
	messageReceipt := decodeReceipt(t, replayResult, 202)
	var message harnessprotocol.MessageEnqueueReferences
	if err := json.Unmarshal(messageReceipt.References, &message); err != nil {
		t.Fatal(err)
	}
	dispatched := dispatch(t, ctx, opened)
	waitFor(t, func() bool {
		active := currentSnapshot(t, ctx, opened).ActiveAttempt
		return active != nil && active.AttemptID == dispatched.AttemptID && active.State == "running"
	})
	reference := harnessadapter.AttemptRef{
		NodeID: testNodeID, DialogID: dialog.DialogID, RequestID: message.RequestID,
		AttemptID: dispatched.AttemptID, Generation: 1,
	}
	if err := opened.ObserveAdapterEvent(ctx, reference, harnessadapter.TerminalEvent{
		EventBase: harnessadapter.EventBase{Attempt: reference}, Outcome: harnessadapter.ReconcileCompleted, EffectStatus: "none",
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return currentSnapshot(t, ctx, opened).ActiveAttempt == nil })
	replay, failure, ok := opened.ReplayEvents(ctx, nodeTrust(), 0, 100)
	if !ok || failure.HTTPStatus != 0 || len(replay.Events) == 0 {
		t.Fatalf("v2 setup replay failed: ok=%v failure=%+v replay=%+v", ok, failure, replay)
	}
	if result := opened.AttemptEvents(ctx, nodeTrust(), dispatched.AttemptID, 0, 100); result.HTTPStatus != 200 {
		t.Fatalf("v2 setup attempt events status=%d body=%s", result.HTTPStatus, result.Body)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeVolumeToLegacyV1(t, path)
	return legacyVolume{
		path: path, replayCommand: replayCommand, replayReceipt: replayReceipt,
		replayCommandID: replayCommandID, attemptID: dispatched.AttemptID, eventCount: len(replay.Events),
	}
}

func downgradeVolumeToLegacyV1(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(path, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	rows, err := tx.Query("SELECT command_id,canonical_json,receipt_json FROM commands ORDER BY command_id")
	if err != nil {
		t.Fatal(err)
	}
	type commandRow struct {
		id, canonical, receipt string
	}
	commands := make([]commandRow, 0)
	for rows.Next() {
		var row commandRow
		if err := rows.Scan(&row.id, &row.canonical, &row.receipt); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		commands = append(commands, row)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, row := range commands {
		canonical := downgradeWireBytes(t, []byte(row.canonical))
		receipt := downgradeWireBytes(t, []byte(row.receipt))
		digest := sha256.Sum256(canonical)
		if _, err := tx.Exec("UPDATE commands SET canonical_json=?,canonical_payload_hash=?,receipt_json=? WHERE command_id=?",
			canonical, hex.EncodeToString(digest[:]), receipt, row.id); err != nil {
			t.Fatal(err)
		}
	}

	rows, err = tx.Query("SELECT rowid,event_json FROM events ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	type eventRow struct {
		rowID int64
		wire  string
	}
	events := make([]eventRow, 0)
	for rows.Next() {
		var row eventRow
		if err := rows.Scan(&row.rowID, &row.wire); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		events = append(events, row)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, row := range events {
		if _, err := tx.Exec("UPDATE events SET event_json=? WHERE rowid=?", downgradeWireBytes(t, []byte(row.wire)), row.rowID); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		"DROP TABLE command_rejections",
		"DROP INDEX one_dialog_hold",
		"DROP INDEX one_node_hold",
		"DROP TABLE administrative_holds",
		"DROP TABLE hold_scope_revisions",
		"DROP TABLE hold_clock",
	} {
		if _, err := tx.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec("UPDATE schema_meta SET fingerprint=? WHERE singleton=1", legacySchemaFingerprintV1); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func downgradeWireBytes(t *testing.T, wire []byte) []byte {
	t.Helper()
	if bytes.Count(wire, currentSchemaTokenForTest) != 1 {
		t.Fatalf("expected one v2 schema token in %s", wire)
	}
	return bytes.Replace(wire, currentSchemaTokenForTest, legacySchemaTokenForTest, 1)
}

func assertLegacyV1Store(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(path, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	var fingerprint string
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT fingerprint FROM schema_meta WHERE singleton=1").Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	if version != 1 || fingerprint != legacySchemaFingerprintV1 {
		t.Fatalf("legacy metadata changed: version=%d fingerprint=%s", version, fingerprint)
	}
	for _, query := range []string{
		"SELECT COUNT(*) FROM commands WHERE instr(CAST(canonical_json AS TEXT),'harness-wire-v2')>0 OR instr(CAST(receipt_json AS TEXT),'harness-wire-v2')>0",
		"SELECT COUNT(*) FROM events WHERE instr(CAST(event_json AS TEXT),'harness-wire-v2')>0",
	} {
		var count int
		if err := db.QueryRow(query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("v2 wire bytes survived rollback: count=%d", count)
		}
	}
}

func TestSchemaV1UpgradePreservesReplayStatusAndAttemptEvents(t *testing.T) {
	legacy := prepareLegacyV1Volume(t)
	opened, err := node.Open(context.Background(), testConfig(legacy.path))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if runtime := opened.Runtime(); runtime.SchemaVersion != node.SchemaVersion || runtime.SchemaFingerprint == legacySchemaFingerprintV1 {
		t.Fatalf("volume was not upgraded: %+v", runtime)
	}

	replayed := opened.SubmitCommand(context.Background(), nodeTrust(), legacy.replayCommand)
	if replayed.HTTPStatus != 200 || !bytes.Equal(replayed.Body, legacy.replayReceipt) {
		t.Fatalf("same-command replay changed: status=%d body=%s want=%s", replayed.HTTPStatus, replayed.Body, legacy.replayReceipt)
	}
	canonical, digest, err := node.CanonicalCommand(legacy.replayCommand)
	if err != nil || len(canonical) == 0 {
		t.Fatal(err)
	}
	status := opened.CommandStatus(context.Background(), nodeTrust(), legacy.replayCommandID)
	if status.HTTPStatus != 200 || harnessprotocol.Validate("commandStatus", status.Body) != nil {
		t.Fatalf("command status invalid: status=%d body=%s", status.HTTPStatus, status.Body)
	}
	var decodedStatus harnessprotocol.CommandStatus
	if err := json.Unmarshal(status.Body, &decodedStatus); err != nil || decodedStatus.SchemaID != harnessprotocol.SchemaID || decodedStatus.CanonicalPayloadHash != digest || !bytes.Equal(decodedStatus.Receipt.References, mustReceiptReferences(t, legacy.replayReceipt)) {
		t.Fatalf("command status changed: %+v err=%v", decodedStatus, err)
	}

	replay, failure, ok := opened.ReplayEvents(context.Background(), nodeTrust(), 0, 100)
	if !ok || failure.HTTPStatus != 0 || len(replay.Events) != legacy.eventCount {
		t.Fatalf("event replay changed: ok=%v failure=%+v events=%d want=%d", ok, failure, len(replay.Events), legacy.eventCount)
	}
	for index, event := range replay.Events {
		if err := harnessprotocol.Validate("event", event); err != nil || !bytes.Contains(event, currentSchemaTokenForTest) || bytes.Contains(event, legacySchemaTokenForTest) {
			t.Fatalf("event %d was not migrated: %v %s", index, err, event)
		}
	}
	attemptEvents := opened.AttemptEvents(context.Background(), nodeTrust(), legacy.attemptID, 0, 100)
	if attemptEvents.HTTPStatus != 200 || harnessprotocol.Validate("eventPage", attemptEvents.Body) != nil || bytes.Contains(attemptEvents.Body, legacySchemaTokenForTest) {
		t.Fatalf("attempt events were not migrated: status=%d body=%s", attemptEvents.HTTPStatus, attemptEvents.Body)
	}
}

func mustReceiptReferences(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var receipt harnessprotocol.Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt.References
}

func TestSchemaV1UpgradeFaultRollsBackAndReopens(t *testing.T) {
	legacy := prepareLegacyV1Volume(t)
	config := testConfig(legacy.path)
	config.StartupFault = func(point node.StartupPoint) error {
		if point == node.StartupDuringMigration {
			return errors.New("synthetic migration crash")
		}
		return nil
	}
	if _, err := node.Open(context.Background(), config); err == nil || !strings.Contains(err.Error(), "synthetic migration crash") {
		t.Fatalf("migration fault was not returned: %v", err)
	}
	assertLegacyV1Store(t, legacy.path)
	reopened, err := node.Open(context.Background(), testConfig(legacy.path))
	if err != nil {
		t.Fatalf("rollback volume did not reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Runtime().SchemaVersion != node.SchemaVersion {
		t.Fatalf("retry did not migrate: %+v", reopened.Runtime())
	}
}

func TestSchemaV2UpgradeAddsDurableBarrierAndRollsBackAtomically(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	opened, err := node.Open(ctx, testConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	create := command(t, "31500000-0000-4000-8000-000000000001", "dialog.create",
		map[string]any{"nodeId": testNodeID}, map[string]any{"registryVersion": 1}, map[string]any{})
	accepted := opened.SubmitCommand(ctx, nodeTrust(), create)
	decodeReceipt(t, accepted, 202)
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeVolumeToV2(t, path)

	faulted := testConfig(path)
	faulted.StartupFault = func(point node.StartupPoint) error {
		if point == node.StartupDuringMigration {
			return errors.New("synthetic v2 migration interruption")
		}
		return nil
	}
	if _, err := node.Open(ctx, faulted); err == nil || !strings.Contains(err.Error(), "synthetic v2 migration interruption") {
		t.Fatalf("v2 migration fault was not returned: %v", err)
	}
	assertSchemaVersionAndObjects(t, path, 2, legacySchemaFingerprintV2, false)

	reopened, err := node.Open(ctx, testConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if runtime := reopened.Runtime(); runtime.SchemaVersion != node.SchemaVersion || runtime.SchemaFingerprint == legacySchemaFingerprintV2 {
		t.Fatalf("v2 volume was not upgraded: %+v", runtime)
	}
	if replay := reopened.SubmitCommand(ctx, nodeTrust(), create); replay.HTTPStatus != 200 || !bytes.Equal(replay.Body, accepted.Body) {
		t.Fatalf("v2 replay changed during migration: status=%d body=%s", replay.HTTPStatus, replay.Body)
	}
	snapshot := currentSnapshot(t, ctx, reopened)
	hold := installHold(t, reopened, holdRequest(t, "v2-migrated-hold", harnessbarrier.Scope{Kind: "node"}, snapshot.Epoch, 0), 201)
	if hold.HoldVersion != 1 || hold.ScopeRevision != 1 {
		t.Fatalf("migrated hold clock/revision invalid: %+v", hold)
	}
}

func TestSchemaV1UpgradeRejectsInvalidWireAndRollsBack(t *testing.T) {
	legacy := prepareLegacyV1Volume(t)
	execLegacyMutation(t, legacy.path, `UPDATE commands
		SET receipt_json=CAST(json_set(CAST(receipt_json AS TEXT),'$.result','invalid') AS BLOB)
		WHERE command_id='31000000-0000-4000-8000-000000000002'`)
	if _, err := node.Open(context.Background(), testConfig(legacy.path)); err == nil || !strings.Contains(err.Error(), "migrated receipt does not validate") {
		t.Fatalf("invalid durable wire was accepted: %v", err)
	}
	assertLegacyV1Store(t, legacy.path)
}

func TestSchemaV1PreflightRejectsFingerprintDDLIdentityAndIntegrityDrift(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		mutate    func(*testing.T, string)
		configure func(*node.Config)
		want      string
	}{
		{name: "fingerprint", mutate: func(t *testing.T, path string) {
			execLegacyMutation(t, path, "UPDATE schema_meta SET fingerprint='wrong' WHERE singleton=1")
		}, want: "fingerprint"},
		{name: "ddl", mutate: func(t *testing.T, path string) {
			execLegacyMutation(t, path, "CREATE TABLE injected(value TEXT) STRICT")
		}, want: "object count"},
		{name: "identity", configure: func(config *node.Config) { config.OwnerID = "1-2" }, want: "durable volume"},
		{name: "foreign key integrity", mutate: func(t *testing.T, path string) {
			execLegacyMutation(t, path, `PRAGMA foreign_keys=OFF;
				INSERT INTO messages(message_id,dialog_id,sequence,version,role,text,created_at)
				VALUES('41000000-0000-4000-8000-000000000099','31000000-0000-4000-8000-000000000099',99,1,'user','broken','2026-09-09T00:00:00Z')`)
		}, want: "foreign key"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			legacy := prepareLegacyV1Volume(t)
			if testCase.mutate != nil {
				testCase.mutate(t, legacy.path)
			}
			config := testConfig(legacy.path)
			if testCase.configure != nil {
				testCase.configure(&config)
			}
			if _, err := node.Open(context.Background(), config); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("unsafe v1 volume was accepted: err=%v want=%q", err, testCase.want)
			}
			var version int
			db, err := sql.Open("sqlite", filepath.Join(legacy.path, "harness.db"))
			if err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
				db.Close()
				t.Fatal(err)
			}
			db.Close()
			if version != 1 {
				t.Fatalf("rejected preflight changed user_version=%d", version)
			}
		})
	}
}

func execLegacyMutation(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(path, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(fmt.Errorf("legacy mutation: %w", err))
	}
}

func downgradeVolumeToV2(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(path, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		"DROP TABLE command_rejections",
		"DROP INDEX one_dialog_hold",
		"DROP INDEX one_node_hold",
		"DROP TABLE administrative_holds",
		"DROP TABLE hold_scope_revisions",
		"DROP TABLE hold_clock",
	} {
		if _, err := tx.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec("UPDATE schema_meta SET fingerprint=? WHERE singleton=1", legacySchemaFingerprintV2); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func assertSchemaVersionAndObjects(t *testing.T, path string, version int, fingerprint string, hasBarrier bool) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(path, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var actualVersion int
	var actualFingerprint string
	if err := db.QueryRow("PRAGMA user_version").Scan(&actualVersion); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT fingerprint FROM schema_meta WHERE singleton=1").Scan(&actualFingerprint); err != nil {
		t.Fatal(err)
	}
	if actualVersion != version || actualFingerprint != fingerprint {
		t.Fatalf("schema metadata changed: version=%d fingerprint=%s", actualVersion, actualFingerprint)
	}
	var objects int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE name='administrative_holds'").Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if (objects == 1) != hasBarrier {
		t.Fatalf("barrier schema presence=%d want=%v", objects, hasBarrier)
	}
}
