package harnessrouter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessbarrier"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
)

const proofDialogID = "30000000-0000-4000-8000-000000000001"

type proofBackend struct {
	readyBackend
	proof        harnessbarrier.QuiescenceProof
	proofStatus  int
	proofErr     error
	release      harnessbarrier.ReleaseReceipt
	releaseCalls int
}

func (backend *proofBackend) InstallHold(context.Context, string, string, []byte) (harnessclient.Response, error) {
	return harnessclient.Response{}, errors.New("not used")
}

func (backend *proofBackend) QuiescenceProof(_ context.Context, nodeID, owner, operationID string) (harnessclient.Response, error) {
	if backend.proofErr != nil {
		return harnessclient.Response{}, backend.proofErr
	}
	if nodeID != routerNodeID || owner != "1-1" || operationID != backend.proof.OperationID {
		return harnessclient.Response{Status: http.StatusNotFound}, nil
	}
	body, _ := json.Marshal(backend.proof)
	status := backend.proofStatus
	if status == 0 {
		status = http.StatusOK
	}
	return harnessclient.Response{Status: status, Body: body}, nil
}

func (backend *proofBackend) ReleaseHold(_ context.Context, nodeID, owner string, raw []byte) (harnessclient.Response, error) {
	backend.releaseCalls++
	var request harnessbarrier.ReleaseRequest
	if nodeID != routerNodeID || owner != "1-1" || json.Unmarshal(raw, &request) != nil || request.OperationID != backend.release.OperationID {
		return harnessclient.Response{Status: http.StatusConflict}, nil
	}
	body, _ := json.Marshal(backend.release)
	return harnessclient.Response{Status: http.StatusCreated, Body: body}, nil
}

func proofRegistry() harnessclient.RoutingRegistry {
	return harnessclient.RoutingRegistry{
		SchemaID: harnessclient.RouterRegistrySchemaID, RegistryVersion: 11, OwnerID: "1-1",
		ManifestSHA256: "a8f9da106644f67ec9d26a36d20ad15aa90ad499c22f1156d8ec037f9a42cf38",
		Nodes: []harnessclient.RoutingNode{{
			NodeID: routerNodeID, Name: "Node", Adapter: "cursor", RegistrationRevision: 11,
			RegistrationEpoch: 7, Compatibility: "compatible",
		}},
	}
}

func currentProof(scope harnessbarrier.Scope) harnessbarrier.QuiescenceProof {
	proof := harnessbarrier.QuiescenceProof{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, Kind: "scope.parked",
		OperationID: "quiesce-1", NodeID: routerNodeID, Epoch: 7, BindingGeneration: 11,
		Scope: scope, HoldVersion: 3, ScopeRevision: 5, StateVersion: 17, QueueRevision: 13,
		ParkedRequestIDsDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CheckpointStreamID:     routerNodeID, CheckpointSeq: 17, EffectStatus: "known",
	}
	if scope.Kind == "dialog" {
		proof.CheckpointStreamID = scope.DialogID
	}
	proof.ProofHash = harnessbarrier.ComputeProofHash(proof)
	return proof
}

func proofRouter(t *testing.T, proof harnessbarrier.QuiescenceProof) (*Router, *proofBackend) {
	t.Helper()
	registry := proofRegistry()
	backend := &proofBackend{readyBackend: readyBackend{registry: registry}, proof: proof}
	router := &Router{
		backend: backend, registry: registry, registryOperationID: "registry-1",
		registryRequestSHA256: registry.ManifestSHA256, registryEnvelope: json.RawMessage(`{"schemaId":"fixture"}`),
		nodes: map[string]*nodeRoute{routerNodeID: {state: NodeState{
			Mode: ModeEligible, StateVersion: 1, Generation: 1, RegistrationRevision: 11,
			IdentityEpoch: 7, Compatibility: "compatible", AdapterKind: "cursor", AdapterVersion: "1.0.31",
		}}},
		persist: func(string, State, bool, func() error) (bool, error) { return true, nil },
	}
	return router, backend
}

func TestRouterSealRequiresExactCurrentAuthenticatedProof(t *testing.T) {
	base := currentProof(harnessbarrier.Scope{Kind: "node"})
	router, _ := proofRouter(t, base)
	if _, err := router.transition(context.Background(), routerNodeID, "seal", transitionRequest{
		OperationID: base.OperationID, Expected: expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1},
	}); err == nil || router.nodes[routerNodeID].state.Mode != ModeEligible {
		t.Fatalf("proofless R06 seal was accepted: err=%v state=%+v", err, router.nodes[routerNodeID].state)
	}
	for _, test := range []struct {
		name   string
		mutate func(*harnessbarrier.QuiescenceProof)
	}{
		{name: "operation", mutate: func(value *harnessbarrier.QuiescenceProof) { value.OperationID = "quiesce-other" }},
		{name: "epoch", mutate: func(value *harnessbarrier.QuiescenceProof) { value.Epoch++ }},
		{name: "binding", mutate: func(value *harnessbarrier.QuiescenceProof) { value.BindingGeneration++ }},
		{name: "scope", mutate: func(value *harnessbarrier.QuiescenceProof) {
			value.Scope = harnessbarrier.Scope{Kind: "dialog", DialogID: proofDialogID}
			value.CheckpointStreamID = proofDialogID
		}},
		{name: "hold version", mutate: func(value *harnessbarrier.QuiescenceProof) { value.HoldVersion++ }},
		{name: "scope revision", mutate: func(value *harnessbarrier.QuiescenceProof) { value.ScopeRevision++ }},
		{name: "state version", mutate: func(value *harnessbarrier.QuiescenceProof) { value.StateVersion++; value.CheckpointSeq++ }},
		{name: "queue revision", mutate: func(value *harnessbarrier.QuiescenceProof) { value.QueueRevision++ }},
		{name: "digest", mutate: func(value *harnessbarrier.QuiescenceProof) {
			value.ParkedRequestIDsDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			router, backend := proofRouter(t, base)
			candidate := base
			test.mutate(&candidate)
			candidate.ProofHash = harnessbarrier.ComputeProofHash(candidate)
			_, err := router.transition(context.Background(), routerNodeID, "seal", transitionRequest{
				OperationID: "quiesce-1", Expected: expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1}, Proof: &candidate,
			})
			var fault *controlFault
			if !errors.As(err, &fault) || fault.status != http.StatusConflict || router.nodes[routerNodeID].state.Mode != ModeEligible || backend.releaseCalls != 0 {
				t.Fatalf("mismatched proof was used: err=%v state=%+v", err, router.nodes[routerNodeID].state)
			}
		})
	}
	router, backend := proofRouter(t, base)
	sealed, err := router.transition(context.Background(), routerNodeID, "seal", transitionRequest{
		OperationID: base.OperationID, Expected: expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1}, Proof: &base,
	})
	if err != nil || sealed.Mode != ModeSealed || sealed.SealScope == nil || sealed.SealedProofHash != base.ProofHash || sealed.SealHoldVersion != base.HoldVersion {
		t.Fatalf("exact current proof did not seal: state=%+v err=%v", sealed, err)
	}
	backend.proofStatus = http.StatusConflict
	if _, err := router.transition(context.Background(), routerNodeID, "seal", transitionRequest{OperationID: base.OperationID, Expected: expectedState{Mode: ModeSealed, StateVersion: 2, Generation: 1}, Proof: &base}); err == nil {
		t.Fatal("stale authenticated readback was accepted")
	}
}

func TestRouterReleaseRecoversAfterPersistFailureAndClearsProjection(t *testing.T) {
	proof := currentProof(harnessbarrier.Scope{Kind: "node"})
	router, backend := proofRouter(t, proof)
	sealed, err := router.transition(context.Background(), routerNodeID, "seal", transitionRequest{
		OperationID: proof.OperationID, Expected: expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1}, Proof: &proof,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := harnessbarrier.ReleaseRequest{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, OperationID: proof.OperationID,
		NodeID: routerNodeID, ExpectedEpoch: proof.Epoch, BindingGeneration: proof.BindingGeneration, Scope: proof.Scope,
		HoldVersion: proof.HoldVersion, ExpectedScopeRevision: proof.ScopeRevision, Action: "release",
	}
	backend.release = harnessbarrier.ReleaseReceipt{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, Kind: "hold.released",
		OperationID: proof.OperationID, ReceiptID: "90000000-0000-4000-8000-000000000002", NodeID: routerNodeID,
		Epoch: proof.Epoch, BindingGeneration: proof.BindingGeneration, Scope: proof.Scope, HoldVersion: proof.HoldVersion,
		ScopeRevision: proof.ScopeRevision + 1, ManualPause: true, ReleasedAt: "2026-09-16T00:00:00Z",
	}
	originalPersist := router.persist
	router.persist = func(string, State, bool, func() error) (bool, error) {
		return false, errors.New("synthetic pre-rename persistence failure")
	}
	input := transitionRequest{
		OperationID: proof.OperationID, Expected: expectedState{Mode: ModeSealed, StateVersion: sealed.StateVersion, Generation: sealed.Generation},
		IdentityEpoch: 7, AdapterVersion: "1.0.31", Release: &request,
	}
	if _, err := router.transition(context.Background(), routerNodeID, "release", input); err == nil ||
		router.nodes[routerNodeID].state.Mode != ModeSealed || backend.releaseCalls != 1 {
		t.Fatalf("failed Router persistence lost the seal: err=%v calls=%d state=%+v", err, backend.releaseCalls, router.nodes[routerNodeID].state)
	}
	router.persist = originalPersist
	eligible, err := router.transition(context.Background(), routerNodeID, "release", input)
	if err != nil || eligible.Mode != ModeEligible || eligible.OperationID != "" || eligible.Generation != sealed.Generation+1 ||
		hasSealProjection(eligible) || backend.releaseCalls != 2 {
		t.Fatalf("idempotent release did not recover: state=%+v calls=%d err=%v", eligible, backend.releaseCalls, err)
	}
}

func TestRouterProofProjectionSurvivesRestartAndReleasesExactHold(t *testing.T) {
	proof := currentProof(harnessbarrier.Scope{Kind: "dialog", DialogID: proofDialogID})
	registry := proofRegistry()
	backend := &proofBackend{readyBackend: readyBackend{registry: registry}, proof: proof}
	state := State{
		Schema: ProjectionStateSchema, OwnerID: registry.OwnerID, RegistryVersion: registry.RegistryVersion,
		RegistrySHA256: registry.ManifestSHA256, RegistryOperationID: "registry-1", RegistryRequestSHA256: registry.ManifestSHA256,
		RegistryEnvelope: json.RawMessage(`{"schemaId":"fixture"}`),
		Nodes: map[string]NodeState{routerNodeID: {
			Mode: ModeEligible, StateVersion: 1, Generation: 1, RegistrationRevision: 11,
			IdentityEpoch: 7, Compatibility: "compatible", AdapterKind: "cursor", AdapterVersion: "1.0.31",
		}},
	}
	router, statePath, socketPath := managedFixture(t, backend, state)
	sealed, err := router.transition(context.Background(), routerNodeID, "seal", transitionRequest{
		OperationID: proof.OperationID, Expected: expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1}, Proof: &proof,
	})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, err := decodeState(statePath); err != nil || persisted.Nodes[routerNodeID].SealedProofHash != proof.ProofHash {
		t.Fatalf("proof projection was not persisted: state=%+v err=%v", persisted, err)
	}
	router.Close()
	reopened, err := newManaged(backend, statePath, socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	request := harnessbarrier.ReleaseRequest{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, OperationID: proof.OperationID,
		NodeID: routerNodeID, ExpectedEpoch: proof.Epoch, BindingGeneration: proof.BindingGeneration, Scope: proof.Scope,
		HoldVersion: proof.HoldVersion, ExpectedScopeRevision: proof.ScopeRevision, Action: "release",
	}
	backend.release = harnessbarrier.ReleaseReceipt{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, Kind: "hold.released",
		OperationID: proof.OperationID, ReceiptID: "90000000-0000-4000-8000-000000000003", NodeID: routerNodeID,
		Epoch: proof.Epoch, BindingGeneration: proof.BindingGeneration, Scope: proof.Scope, HoldVersion: proof.HoldVersion,
		ScopeRevision: proof.ScopeRevision + 1, ManualPause: true, ReleasedAt: "2026-09-16T00:00:00Z",
	}
	eligible, err := reopened.transition(context.Background(), routerNodeID, "release", transitionRequest{
		OperationID: proof.OperationID, Expected: expectedState{Mode: ModeSealed, StateVersion: sealed.StateVersion, Generation: sealed.Generation},
		IdentityEpoch: 7, AdapterVersion: "1.0.31", Release: &request,
	})
	if err != nil || eligible.Mode != ModeEligible || hasSealProjection(eligible) {
		t.Fatalf("restarted Router did not release exact projected hold: state=%+v err=%v", eligible, err)
	}
	if persisted, err := decodeState(statePath); err != nil || persisted.Nodes[routerNodeID] != eligible {
		t.Fatalf("released route readback changed: state=%+v err=%v", persisted, err)
	}
}

func TestRouterProofSealKeepsControlsAndSiblingDialogAvailable(t *testing.T) {
	proof := currentProof(harnessbarrier.Scope{Kind: "dialog", DialogID: proofDialogID})
	router, backend := proofRouter(t, proof)
	_, err := router.transition(context.Background(), routerNodeID, "seal", transitionRequest{
		OperationID: proof.OperationID, Expected: expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1}, Proof: &proof,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Command(context.Background(), routerNodeID, "1-1", commandFixture(t, "command.2.message.enqueue")); err == nil {
		t.Fatal("selected dialog admission crossed Router seal")
	}
	sibling := commandFixture(t, "command.2.message.enqueue")
	sibling = []byte(string(sibling))
	var envelope map[string]any
	if json.Unmarshal(sibling, &envelope) != nil {
		t.Fatal("decode command fixture")
	}
	target := envelope["target"].(map[string]any)
	target["dialogId"] = "30000000-0000-4000-8000-000000000002"
	sibling, _ = json.Marshal(envelope)
	if _, err := router.Command(context.Background(), routerNodeID, "1-1", sibling); err != nil {
		t.Fatalf("sibling dialog was coupled to seal: %v", err)
	}
	for _, name := range []string{"command.3.message.steer", "command.4.request.cancel", "command.5.attempt.stop", "command.8.approval.respond", "command.9.input.respond"} {
		if _, err := router.Command(context.Background(), routerNodeID, "1-1", commandFixture(t, name)); err != nil {
			t.Fatalf("owning control %s was blocked: %v", name, err)
		}
	}
	if backend.commandCalls != 6 {
		t.Fatalf("forwarded commands=%d want=6", backend.commandCalls)
	}
}

func TestRouterAbortCancelsOnlyProjectedHoldBeforeUnsealing(t *testing.T) {
	proof := currentProof(harnessbarrier.Scope{Kind: "node"})
	router, backend := proofRouter(t, proof)
	sealed, err := router.transition(context.Background(), routerNodeID, "seal", transitionRequest{
		OperationID: proof.OperationID, Expected: expectedState{Mode: ModeEligible, StateVersion: 1, Generation: 1}, Proof: &proof,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := harnessbarrier.ReleaseRequest{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, OperationID: proof.OperationID,
		NodeID: routerNodeID, ExpectedEpoch: proof.Epoch, BindingGeneration: proof.BindingGeneration, Scope: proof.Scope,
		HoldVersion: proof.HoldVersion, ExpectedScopeRevision: proof.ScopeRevision, Action: "cancel",
	}
	backend.release = harnessbarrier.ReleaseReceipt{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, Kind: "hold.cancelled",
		OperationID: proof.OperationID, ReceiptID: "90000000-0000-4000-8000-000000000001", NodeID: routerNodeID,
		Epoch: proof.Epoch, BindingGeneration: proof.BindingGeneration, Scope: proof.Scope, HoldVersion: proof.HoldVersion,
		ScopeRevision: proof.ScopeRevision + 1, ManualPause: true, ReleasedAt: "2026-09-16T00:00:00Z",
	}
	stale := request
	stale.HoldVersion++
	if _, err := router.transition(context.Background(), routerNodeID, "abort", transitionRequest{
		OperationID: proof.OperationID, Expected: expectedState{Mode: ModeSealed, StateVersion: sealed.StateVersion, Generation: sealed.Generation},
		IdentityEpoch: 7, AdapterVersion: "1.0.31", Release: &stale,
	}); err == nil || backend.releaseCalls != 0 || router.nodes[routerNodeID].state.Mode != ModeSealed {
		t.Fatalf("stale release changed seal: err=%v calls=%d", err, backend.releaseCalls)
	}
	eligible, err := router.transition(context.Background(), routerNodeID, "abort", transitionRequest{
		OperationID: proof.OperationID, Expected: expectedState{Mode: ModeSealed, StateVersion: sealed.StateVersion, Generation: sealed.Generation},
		IdentityEpoch: 7, AdapterVersion: "1.0.31", Release: &request,
	})
	if err != nil || eligible.Mode != ModeEligible || eligible.OperationID != "" || hasSealProjection(eligible) || backend.releaseCalls != 1 {
		t.Fatalf("exact cancel did not restore route: state=%+v calls=%d err=%v", eligible, backend.releaseCalls, err)
	}
}
