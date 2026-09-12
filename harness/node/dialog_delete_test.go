package node_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/harness/node"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	_ "modernc.org/sqlite"
)

func TestDialogDeleteIsDurableRecoverableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	opened, err := node.Open(ctx, testConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	dialogID := createDialog(t, ctx, opened, "41000000-0000-4000-8000-000000000001")
	message := enqueue(t, ctx, opened, "41000000-0000-4000-8000-000000000002", dialogID, "retained", 1)
	cancel := command(t, "41000000-0000-4000-8000-000000000003", "request.cancel", map[string]any{"nodeId": testNodeID, "requestId": message.RequestID}, map[string]any{"requestVersion": 1}, map[string]any{})
	decodeReceipt(t, opened.SubmitCommand(ctx, nodeTrust(), cancel), 202)

	deleteCommand := command(t, "41000000-0000-4000-8000-000000000004", "dialog.delete", map[string]any{"nodeId": testNodeID, "dialogId": dialogID}, map[string]any{"dialogVersion": 2}, map[string]any{})
	deleted := opened.SubmitCommand(ctx, nodeTrust(), deleteCommand)
	receipt := decodeReceipt(t, deleted, 202)
	if receipt.Result != "deleted" || harnessprotocol.Validate("receipt", receiptBody(t, receipt)) != nil {
		t.Fatalf("invalid delete receipt: %+v", receipt)
	}
	var references harnessprotocol.DialogDeleteReferences
	if err := json.Unmarshal(receipt.References, &references); err != nil || references.DialogID != dialogID {
		t.Fatalf("delete references=%+v err=%v", references, err)
	}

	replayed := opened.SubmitCommand(ctx, nodeTrust(), deleteCommand)
	if replayed.HTTPStatus != 200 || string(replayed.Body) != string(deleted.Body) {
		t.Fatalf("delete replay status=%d body=%s", replayed.HTTPStatus, replayed.Body)
	}
	assertDeletedDialogReads(t, ctx, opened, dialogID, message.RequestID)
	if result := opened.SubmitCommand(ctx, nodeTrust(), command(t, "41000000-0000-4000-8000-000000000005", "message.enqueue", map[string]any{"nodeId": testNodeID, "dialogId": dialogID}, map[string]any{"dialogVersion": 3}, map[string]any{"text": "hidden"})); result.HTTPStatus != 404 {
		t.Fatalf("deleted dialog accepted message: status=%d body=%s", result.HTTPStatus, result.Body)
	}
	if result := opened.SubmitCommand(ctx, nodeTrust(), command(t, "41000000-0000-4000-8000-000000000006", "request.cancel", map[string]any{"nodeId": testNodeID, "requestId": message.RequestID}, map[string]any{"requestVersion": 2}, map[string]any{})); result.HTTPStatus != 404 {
		t.Fatalf("deleted dialog accepted request command: status=%d body=%s", result.HTTPStatus, result.Body)
	}

	afterDelete := currentSnapshot(t, ctx, opened)
	secondDelete := opened.SubmitCommand(ctx, nodeTrust(), command(t, "41000000-0000-4000-8000-000000000007", "dialog.delete", map[string]any{"nodeId": testNodeID, "dialogId": dialogID}, map[string]any{"dialogVersion": 3}, map[string]any{}))
	if secondDelete.HTTPStatus != 404 {
		t.Fatalf("second delete status=%d body=%s", secondDelete.HTTPStatus, secondDelete.Body)
	}
	if status := opened.CommandStatus(ctx, nodeTrust(), "41000000-0000-4000-8000-000000000007"); status.HTTPStatus != 404 {
		t.Fatalf("rejected delete persisted command: status=%d body=%s", status.HTTPStatus, status.Body)
	}
	afterRejected := currentSnapshot(t, ctx, opened)
	if afterRejected.StateVersion != afterDelete.StateVersion || afterRejected.LastEventSeq != afterDelete.LastEventSeq {
		t.Fatalf("rejected delete mutated state: before=%+v after=%+v", afterDelete, afterRejected)
	}

	replay, failure, ok := opened.ReplayEvents(ctx, nodeTrust(), receipt.EventSeq-1, 100)
	if !ok || len(replay.Events) != 1 {
		t.Fatalf("delete event replay failed: ok=%v status=%d events=%d body=%s", ok, failure.HTTPStatus, len(replay.Events), failure.Body)
	}
	var event harnessprotocol.EventEnvelope
	if err := json.Unmarshal(replay.Events[0], &event); err != nil || event.Type != "dialog.deleted" || event.DialogID != dialogID {
		t.Fatalf("delete event=%+v err=%v", event, err)
	}
	if _, failure, ok := opened.ReplayEvents(ctx, nodeTrust(), 0, 100); ok || failure.HTTPStatus != 409 {
		t.Fatalf("historical dialog events replayed: ok=%v status=%d body=%s", ok, failure.HTTPStatus, failure.Body)
	}

	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", filepath.Join(path, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	var dialogs, messages, requests, tombstones int
	if err := database.QueryRow(`SELECT
		(SELECT COUNT(*) FROM dialogs),(SELECT COUNT(*) FROM messages),(SELECT COUNT(*) FROM requests),
		(SELECT COUNT(*) FROM events WHERE projection_key='dialog.deleted')`).Scan(&dialogs, &messages, &requests, &tombstones); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if dialogs != 1 || messages != 1 || requests != 1 || tombstones != 1 {
		t.Fatalf("delete physically removed durable data: dialogs=%d messages=%d requests=%d tombstones=%d", dialogs, messages, requests, tombstones)
	}
	reopened, err := node.Open(ctx, testConfig(path))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertDeletedDialogReads(t, ctx, reopened, dialogID, message.RequestID)
	if _, failure, ok := reopened.ReplayEvents(ctx, nodeTrust(), 0, 100); ok || failure.HTTPStatus != 409 {
		t.Fatalf("reopened node replayed deleted history: ok=%v status=%d body=%s", ok, failure.HTTPStatus, failure.Body)
	}
}

func TestDialogDeleteLostACKRecoversReceipt(t *testing.T) {
	ctx := context.Background()
	opened, err := node.Open(ctx, testConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	dialogID := createDialog(t, ctx, opened, "41000000-0000-4000-8000-000000000020")
	deleteCommand := command(t, "41000000-0000-4000-8000-000000000021", "dialog.delete", map[string]any{"nodeId": testNodeID, "dialogId": dialogID}, map[string]any{"dialogVersion": 1}, map[string]any{})

	opened.SetFaultInjector(func(point node.FaultPoint) error {
		if point == node.FaultAfterCommit {
			return errors.New("synthetic lost delete receipt")
		}
		return nil
	})
	lost := opened.SubmitCommand(ctx, nodeTrust(), deleteCommand)
	if lost.HTTPStatus != 503 || !lost.Committed {
		t.Fatalf("lost ACK status=%d committed=%v body=%s", lost.HTTPStatus, lost.Committed, lost.Body)
	}
	opened.SetFaultInjector(nil)

	status := opened.CommandStatus(ctx, nodeTrust(), "41000000-0000-4000-8000-000000000021")
	if status.HTTPStatus != 200 {
		t.Fatalf("command status=%d body=%s", status.HTTPStatus, status.Body)
	}
	var commandStatus harnessprotocol.CommandStatus
	if err := json.Unmarshal(status.Body, &commandStatus); err != nil {
		t.Fatal(err)
	}
	if commandStatus.Status != "accepted" || commandStatus.Receipt.Result != "deleted" {
		t.Fatalf("delete command status=%+v", commandStatus)
	}
	wantReceipt := receiptBody(t, commandStatus.Receipt)
	replay := opened.SubmitCommand(ctx, nodeTrust(), deleteCommand)
	if replay.HTTPStatus != 200 || string(replay.Body) != string(wantReceipt) {
		t.Fatalf("delete replay status=%d body=%s want=%s", replay.HTTPStatus, replay.Body, wantReceipt)
	}
	if result := opened.History(ctx, nodeTrust(), dialogID, "", 100); result.HTTPStatus != 404 {
		t.Fatalf("lost ACK delete did not hide dialog: status=%d body=%s", result.HTTPStatus, result.Body)
	}
}

func receiptBody(t *testing.T, receipt harnessprotocol.Receipt) []byte {
	t.Helper()
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func assertDeletedDialogReads(t *testing.T, ctx context.Context, opened *node.Node, dialogID, requestID string) {
	t.Helper()
	var dialogs harnessprotocol.Page[harnessprotocol.DialogSummary]
	result := opened.Dialogs(ctx, nodeTrust(), "", 100)
	if result.HTTPStatus != 200 || json.Unmarshal(result.Body, &dialogs) != nil || len(dialogs.Items) != 0 {
		t.Fatalf("deleted dialog remains listed: status=%d body=%s", result.HTTPStatus, result.Body)
	}
	if result := opened.History(ctx, nodeTrust(), dialogID, "", 100); result.HTTPStatus != 404 {
		t.Fatalf("deleted history status=%d body=%s", result.HTTPStatus, result.Body)
	}
	var requests harnessprotocol.Page[harnessprotocol.Request]
	result = opened.Requests(ctx, nodeTrust(), "", "", 100)
	if result.HTTPStatus != 200 || json.Unmarshal(result.Body, &requests) != nil || len(requests.Items) != 0 {
		t.Fatalf("deleted requests remain listed: status=%d body=%s", result.HTTPStatus, result.Body)
	}
	if result := opened.Attempts(ctx, nodeTrust(), requestID, "", 100); result.HTTPStatus != 404 {
		t.Fatalf("deleted request attempts status=%d body=%s", result.HTTPStatus, result.Body)
	}
}
