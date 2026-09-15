package harnessclient

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessbarrier"
	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

func TestBarrierRejectionConsumerBindsNodeAndCommand(t *testing.T) {
	receipt := harnessbarrier.RejectionReceipt{
		ProtocolVersion: 1, SchemaID: harnessbarrier.SchemaID, Kind: "command.rejected",
		CommandID: "10000000-0000-4000-8000-000000000001", CommandKind: "message.enqueue",
		ReceiptID: "20000000-0000-4000-8000-000000000001", NodeID: "30000000-0000-4000-8000-000000000001",
		Epoch: 1, CanonicalPayloadHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		RejectedAt: "2026-09-16T00:00:00Z", Reason: "administrative_hold", HoldOperationID: "hold-1",
		HoldVersion: 1, Scope: harnessbarrier.Scope{Kind: "node"}, ScopeRevision: 1,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if !validBarrierRejection(http.StatusConflict, raw, receipt.NodeID, receipt.CommandID, receipt.Epoch, "message.enqueue", receipt.CanonicalPayloadHash) {
		t.Fatal("valid barrier rejection was not accepted")
	}
	if validBarrierRejection(http.StatusConflict, raw, "30000000-0000-4000-8000-000000000002", receipt.CommandID, receipt.Epoch, "message.enqueue", receipt.CanonicalPayloadHash) ||
		validBarrierRejection(http.StatusConflict, raw, receipt.NodeID, "10000000-0000-4000-8000-000000000002", receipt.Epoch, "message.enqueue", receipt.CanonicalPayloadHash) ||
		validBarrierRejection(http.StatusConflict, raw, receipt.NodeID, receipt.CommandID, receipt.Epoch+1, "message.enqueue", receipt.CanonicalPayloadHash) ||
		validBarrierRejection(http.StatusConflict, raw, receipt.NodeID, receipt.CommandID, receipt.Epoch, "attempt.retry", receipt.CanonicalPayloadHash) ||
		validBarrierRejection(http.StatusConflict, raw, receipt.NodeID, receipt.CommandID, receipt.Epoch, "message.enqueue", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb") ||
		validBarrierRejection(http.StatusOK, raw, receipt.NodeID, receipt.CommandID, receipt.Epoch, "message.enqueue", receipt.CanonicalPayloadHash) {
		t.Fatal("barrier rejection escaped response scope")
	}
}

func TestBarrierRejectionCommandBindsExactIntentAndEpoch(t *testing.T) {
	identityBody := fixture(t, "read.identity")
	commandBody := fixture(t, "command.2.message.enqueue")
	var identity hp.NodeIdentity
	var command hp.CommandEnvelope
	if json.Unmarshal(identityBody, &identity) != nil || json.Unmarshal(commandBody, &command) != nil {
		t.Fatal("decode fixtures")
	}
	_, commandHash, err := hp.CanonicalCommand(commandBody)
	if err != nil {
		t.Fatal(err)
	}
	base := harnessbarrier.RejectionReceipt{
		ProtocolVersion: 1, SchemaID: harnessbarrier.SchemaID, Kind: "command.rejected",
		CommandID: command.CommandID, CommandKind: string(command.Kind),
		ReceiptID: "20000000-0000-4000-8000-000000000001", NodeID: testNode,
		Epoch: identity.IdentityEpoch, CanonicalPayloadHash: commandHash,
		RejectedAt: "2026-09-16T00:00:00Z", Reason: "administrative_hold", HoldOperationID: "hold-1",
		HoldVersion: 1, Scope: harnessbarrier.Scope{Kind: "node"}, ScopeRevision: 1,
	}
	for _, test := range []struct {
		name   string
		mutate func(*harnessbarrier.RejectionReceipt)
		valid  bool
	}{
		{name: "exact", mutate: func(*harnessbarrier.RejectionReceipt) {}, valid: true},
		{name: "epoch", mutate: func(receipt *harnessbarrier.RejectionReceipt) { receipt.Epoch++ }},
		{name: "kind", mutate: func(receipt *harnessbarrier.RejectionReceipt) { receipt.CommandKind = "attempt.retry" }},
		{name: "payload", mutate: func(receipt *harnessbarrier.RejectionReceipt) {
			receipt.CanonicalPayloadHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt := base
			test.mutate(&receipt)
			rejectionBody, marshalErr := json.Marshal(receipt)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			rig := newRig(t, func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if request.Method == http.MethodGet {
					_, _ = w.Write(identityBody)
					return
				}
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write(rejectionBody)
			})
			response, commandErr := rig.client.Command(context.Background(), testNode, testOwner, commandBody)
			if test.valid {
				if commandErr != nil || response.Status != http.StatusConflict {
					t.Fatalf("exact rejection was not returned: status=%d err=%v", response.Status, commandErr)
				}
				return
			}
			if commandErr == nil {
				t.Fatalf("mismatched rejection was trusted: status=%d body=%s", response.Status, response.Body)
			}
		})
	}
}
