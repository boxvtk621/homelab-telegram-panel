package harnessclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessbarrier"
	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	"github.com/boxvtk621/homelab-telegram-panel/internal/logicaldelete"
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

func TestAdministrativeClientBindsProofAndReleaseToLiveIdentity(t *testing.T) {
	identityBody := fixture(t, "read.identity")
	var identity hp.NodeIdentity
	if json.Unmarshal(identityBody, &identity) != nil {
		t.Fatal("decode identity")
	}
	holdRequest := harnessbarrier.InstallRequest{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, OperationID: "client-proof-1",
		NodeID: testNode, ExpectedEpoch: identity.IdentityEpoch, BindingGeneration: identity.RegistryVersion,
		Scope: harnessbarrier.Scope{Kind: "node"}, ExpectedScopeRevision: 0,
	}
	hold := harnessbarrier.HoldReceipt{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, Kind: "hold.installed",
		OperationID: holdRequest.OperationID, ReceiptID: "20000000-0000-4000-8000-000000000011", NodeID: testNode,
		Epoch: identity.IdentityEpoch, BindingGeneration: identity.RegistryVersion, Scope: holdRequest.Scope,
		HoldVersion: 1, ScopeRevision: 1, InstalledAt: "2026-09-16T00:00:00Z",
	}
	proof := harnessbarrier.QuiescenceProof{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, Kind: "scope.parked",
		OperationID: hold.OperationID, NodeID: testNode, Epoch: identity.IdentityEpoch, BindingGeneration: identity.RegistryVersion,
		Scope: hold.Scope, HoldVersion: hold.HoldVersion, ScopeRevision: hold.ScopeRevision, StateVersion: 2, QueueRevision: 1,
		ParkedRequestIDsDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CheckpointStreamID:     testNode, CheckpointSeq: 2, EffectStatus: "known",
	}
	proof.ProofHash = harnessbarrier.ComputeProofHash(proof)
	releaseRequest := harnessbarrier.ReleaseRequest{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, OperationID: hold.OperationID,
		NodeID: testNode, ExpectedEpoch: hold.Epoch, BindingGeneration: hold.BindingGeneration, Scope: hold.Scope,
		HoldVersion: hold.HoldVersion, ExpectedScopeRevision: hold.ScopeRevision, Action: "release",
	}
	release := harnessbarrier.ReleaseReceipt{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, Kind: "hold.released",
		OperationID: hold.OperationID, ReceiptID: "20000000-0000-4000-8000-000000000012", NodeID: testNode,
		Epoch: hold.Epoch, BindingGeneration: hold.BindingGeneration, Scope: hold.Scope, HoldVersion: hold.HoldVersion,
		ScopeRevision: hold.ScopeRevision + 1, ReleasedAt: "2026-09-16T00:00:01Z",
	}
	rig := newRig(t, func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/identity"):
			_, _ = w.Write(identityBody)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/administration/holds"):
			body := new(bytes.Buffer)
			_, _ = body.ReadFrom(request.Body)
			want, _ := json.Marshal(holdRequest)
			if !bytes.Equal(body.Bytes(), want) {
				t.Errorf("install bytes changed: %s", body.Bytes())
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hold)
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/proof"):
			_ = json.NewEncoder(w).Encode(proof)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/release"):
			body := new(bytes.Buffer)
			_, _ = body.ReadFrom(request.Body)
			want, _ := json.Marshal(releaseRequest)
			if !bytes.Equal(body.Bytes(), want) {
				t.Errorf("release bytes changed: %s", body.Bytes())
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(release)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	holdRaw, _ := json.Marshal(holdRequest)
	if response, err := rig.client.InstallHold(context.Background(), testNode, testOwner, holdRaw); err != nil || response.Status != http.StatusCreated {
		t.Fatalf("install failed: response=%+v err=%v", response, err)
	}
	if response, err := rig.client.QuiescenceProof(context.Background(), testNode, testOwner, hold.OperationID); err != nil || response.Status != http.StatusOK {
		t.Fatalf("proof failed: response=%+v err=%v", response, err)
	}
	releaseRaw, _ := json.Marshal(releaseRequest)
	if response, err := rig.client.ReleaseHold(context.Background(), testNode, testOwner, releaseRaw); err != nil || response.Status != http.StatusCreated {
		t.Fatalf("release failed: response=%+v err=%v", response, err)
	}
	staleRequest := holdRequest
	staleRequest.ExpectedEpoch++
	staleRaw, _ := json.Marshal(staleRequest)
	if _, err := rig.client.InstallHold(context.Background(), testNode, testOwner, staleRaw); err == nil {
		t.Fatal("stale install binding reached administrative endpoint")
	}
}

func TestAdministrativeClientRejectsForgedCurrentProof(t *testing.T) {
	identityBody := fixture(t, "read.identity")
	var identity hp.NodeIdentity
	_ = json.Unmarshal(identityBody, &identity)
	proof := harnessbarrier.QuiescenceProof{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, Kind: "scope.parked",
		OperationID: "client-proof-2", NodeID: testNode, Epoch: identity.IdentityEpoch + 1, BindingGeneration: identity.RegistryVersion,
		Scope: harnessbarrier.Scope{Kind: "node"}, HoldVersion: 1, ScopeRevision: 1, StateVersion: 1, QueueRevision: 1,
		ParkedRequestIDsDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CheckpointStreamID: testNode,
		CheckpointSeq: 1, EffectStatus: "known",
	}
	proof.ProofHash = harnessbarrier.ComputeProofHash(proof)
	rig := newRig(t, func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/identity") {
			_, _ = w.Write(identityBody)
			return
		}
		_ = json.NewEncoder(w).Encode(proof)
	})
	if _, err := rig.client.QuiescenceProof(context.Background(), testNode, testOwner, proof.OperationID); err == nil {
		t.Fatal("proof from a different live epoch was trusted")
	}
}

func TestLogicalDeleteClientBindsPrivateReceiptToExactRequest(t *testing.T) {
	identityBody := fixture(t, "read.identity")
	var identity hp.NodeIdentity
	if json.Unmarshal(identityBody, &identity) != nil {
		t.Fatal("decode identity")
	}
	request := logicaldelete.NodeRequest{
		SchemaID: logicaldelete.NodeSchemaID, OperationID: "61000000-0000-4000-8000-000000000001",
		CoordinatorRequestHash: strings.Repeat("a", 64), CommandID: "61000000-0000-4000-8000-000000000002",
		LogicalDialogID: "61000000-0000-4000-8000-000000000003", NodeID: testNode,
		NodeDialogID: "61000000-0000-4000-8000-000000000004", ExpectedEpoch: identity.IdentityEpoch,
		RegistryVersion: identity.RegistryVersion, BindingVersion: 7, ExpectedDialogVersion: 2,
		HoldVersion: 1, HoldScopeRevision: 1,
	}
	requestHash, err := logicaldelete.NodeRequestHash(request)
	if err != nil {
		t.Fatal(err)
	}
	deletedAt := "2026-09-17T10:00:00Z"
	references, _ := json.Marshal(hp.DialogDeleteReferences{DialogID: request.NodeDialogID})
	commandReceipt, _ := json.Marshal(hp.Receipt{
		ProtocolVersion: hp.ProtocolVersion, SchemaID: hp.SchemaID, CommandID: request.CommandID,
		CommandKind: hp.CommandDialogDelete, ReceiptID: "61000000-0000-4000-8000-000000000005",
		AcceptedAt: deletedAt, NodeID: testNode, EventSeq: 9, Result: "deleted", References: references,
	})
	receipt := logicaldelete.NodeReceipt{
		SchemaID: logicaldelete.NodeSchemaID, OperationID: request.OperationID, NodeRequestHash: requestHash,
		CoordinatorRequestHash: request.CoordinatorRequestHash, ReceiptID: "61000000-0000-4000-8000-000000000006",
		CommandID: request.CommandID, LogicalDialogID: request.LogicalDialogID, NodeID: testNode,
		NodeDialogID: request.NodeDialogID, Epoch: identity.IdentityEpoch, RegistryVersion: identity.RegistryVersion,
		BindingVersion: request.BindingVersion, DeletedDialogVersion: request.ExpectedDialogVersion + 1,
		HoldVersion: request.HoldVersion, HoldScopeRevision: request.HoldScopeRevision + 1,
		TombstoneEventSeq: 9, CommandReceipt: commandReceipt, DeletedAt: deletedAt,
	}
	rig := newRig(t, func(w http.ResponseWriter, httpRequest *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(httpRequest.URL.Path, "/identity"):
			_, _ = w.Write(identityBody)
		case httpRequest.Method == http.MethodPost && strings.HasSuffix(httpRequest.URL.Path, "/administration/logical-deletes"):
			body := new(bytes.Buffer)
			_, _ = body.ReadFrom(httpRequest.Body)
			want, _ := json.Marshal(request)
			if !bytes.Equal(body.Bytes(), want) {
				t.Errorf("logical delete bytes changed: %s", body.Bytes())
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(receipt)
		case httpRequest.Method == http.MethodGet && strings.HasSuffix(httpRequest.URL.Path, "/administration/logical-deletes/"+request.OperationID):
			_ = json.NewEncoder(w).Encode(receipt)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if response, err := rig.client.DeleteLogicalDialog(context.Background(), testNode, testOwner, request); err != nil || response.Status != http.StatusCreated {
		t.Fatalf("private delete response=%+v err=%v", response, err)
	}
	if response, err := rig.client.LogicalDeleteStatus(context.Background(), testNode, testOwner, request.OperationID); err != nil || response.Status != http.StatusOK {
		t.Fatalf("private status response=%+v err=%v", response, err)
	}

	forged := receipt
	forged.BindingVersion++
	forgedRig := newRig(t, func(w http.ResponseWriter, httpRequest *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(httpRequest.URL.Path, "/identity") {
			_, _ = w.Write(identityBody)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(forged)
	})
	if _, err := forgedRig.client.DeleteLogicalDialog(context.Background(), testNode, testOwner, request); err == nil {
		t.Fatal("receipt from a different binding was accepted")
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
