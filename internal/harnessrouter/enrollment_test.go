package harnessrouter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

func completeAdmission(ownerID, nodeID string, revision, epoch int64, adapter hp.AdapterIdentity) hp.AdmissionProfile {
	return hp.AdmissionProfile{
		SchemaID: hp.AdmissionSchemaID, OwnerID: ownerID, NodeID: nodeID,
		RegistrationRevision: revision, IdentityEpoch: epoch, WireSchemaSHA256: hp.SchemaSHA256,
		Adapter: adapter, Readiness: "ready",
		Capabilities: hp.AdmissionCapabilities{
			Profile: true, NativeEpoch: true, PolicyEnforcement: true, History: true, Facts: true,
			DurableReceipts: true, ReplicaExport: true, ReplicaImport: true, AssetExport: true,
			AssetImport: true, ScopedQuiesce: true, OwnershipRelease: true, TargetReservation: true,
		},
	}
}

func TestAdmissionProfileRejectsMissingOwnerIdentityCapabilityAndReadiness(t *testing.T) {
	adapter := hp.AdapterIdentity{Kind: "codex", Version: "0.153.4"}
	node := registryNode(2, "codex")
	node.RegistrationRevision, node.RegistrationEpoch, node.Compatibility = 1, 1, "compatible"
	registry := harnessclient.RoutingRegistry{
		RegistryVersion: 2, OwnerID: "owner-1", Mode: "fixture", SchemaID: harnessclient.RouterRegistrySchemaID,
		WireSchemaSHA256: hp.SchemaSHA256, Nodes: []harnessclient.RoutingNode{{
			NodeID: node.NodeID, Adapter: node.Adapter, RegistrationRevision: 1, RegistrationEpoch: 1, Compatibility: "compatible",
		}},
	}
	base := completeAdmission(registry.OwnerID, node.NodeID, 1, 1, adapter)
	tests := []struct {
		name    string
		profile *hp.AdmissionProfile
		mutate  func(*hp.AdmissionProfile)
		code    string
	}{
		{name: "missing", code: "admission_profile_missing"},
		{name: "owner", profile: &base, mutate: func(value *hp.AdmissionProfile) { value.OwnerID = "owner-2" }, code: "admission_owner_mismatch"},
		{name: "identity", profile: &base, mutate: func(value *hp.AdmissionProfile) { value.IdentityEpoch = 2 }, code: "admission_identity_mismatch"},
		{name: "capability", profile: &base, mutate: func(value *hp.AdmissionProfile) { value.Capabilities.AssetImport = false }, code: "admission_capability_missing"},
		{name: "readiness", profile: &base, mutate: func(value *hp.AdmissionProfile) { value.Readiness = "blocked" }, code: "admission_unready"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var profile *hp.AdmissionProfile
			if test.profile != nil {
				copy := *test.profile
				test.mutate(&copy)
				profile = &copy
			}
			backend := &readyBackend{
				registry: registry, adapters: map[string]hp.AdapterIdentity{node.NodeID: adapter},
				epochs: map[string]int64{node.NodeID: 1}, admission: profile,
			}
			_, err := observeNode(context.Background(), backend, registry, node.NodeID, "codex", "", 1, false, true)
			var fault *admissionFault
			if !errors.As(err, &fault) || fault.code != test.code {
				t.Fatalf("fault=%v, want %s", err, test.code)
			}
		})
	}
}

func TestEnrollmentDuplicateAndLostAcknowledgementProjectOnceWithoutLifecycle(t *testing.T) {
	trust := newRegistryTrust(t)
	first := registryNode(1, "cursor")
	first.RegistrationRevision, first.RegistrationEpoch, first.Compatibility = 1, 7, "compatible"
	currentClient, currentRaw := registryClient(t, trust, dynamicManifest(1, first))
	current := currentClient.RoutingRegistry()
	state := State{
		Schema: ProjectionStateSchema, OwnerID: current.OwnerID, RegistryVersion: current.RegistryVersion,
		RegistrySHA256: current.ManifestSHA256, RegistryOperationID: "r02-seed",
		RegistryRequestSHA256: strings.Repeat("a", 64), RegistryEnvelope: currentRaw,
		Nodes: map[string]NodeState{
			first.NodeID: {Mode: ModeEligible, StateVersion: 2, Generation: 1, RegistrationRevision: 1, IdentityEpoch: 7, Compatibility: "compatible", AdapterKind: "cursor", AdapterVersion: "1.0.31"},
		},
	}
	second := registryNode(2, "codex")
	second.RegistrationRevision, second.RegistrationEpoch, second.Compatibility = 1, 1, "compatible"
	candidateManifest := dynamicManifest(2, first, second)
	candidateClient, candidateRaw := registryClient(t, trust, candidateManifest)
	candidateRegistry := candidateClient.RoutingRegistry()
	candidateClient.Close()
	adapter := hp.AdapterIdentity{Kind: "codex", Version: "0.153.4"}
	admission := completeAdmission(current.OwnerID, second.NodeID, 1, 1, adapter)
	candidateBackend := &readyBackend{
		registry: candidateRegistry, adapters: map[string]hp.AdapterIdentity{second.NodeID: adapter},
		epochs: map[string]int64{second.NodeID: 1}, admission: &admission,
	}
	factoryCalls := 0
	router, _, _ := managedRegistryRouter(t, currentClient, state, func(raw []byte) (Backend, error) {
		factoryCalls++
		if string(raw) != string(candidateRaw) {
			t.Fatal("unexpected candidate registry")
		}
		return candidateBackend, nil
	})
	defer router.Close()
	input := EnrollmentRegistryInput{
		OperationID: "r10-enroll", NodeID: second.NodeID,
		ExpectedRegistryVersion: current.RegistryVersion, ExpectedRegistrySHA256: current.ManifestSHA256,
		Registry: candidateRaw,
	}
	if _, err := router.InstallEnrollmentRegistry(input); err != nil {
		t.Fatal(err)
	}
	if _, err := router.InstallEnrollmentRegistry(input); err != nil {
		t.Fatal("lost-ack retry must reconcile", err)
	}
	conflict := input
	conflict.ExpectedRegistrySHA256 = strings.Repeat("f", 64)
	if _, err := router.InstallEnrollmentRegistry(conflict); err == nil {
		t.Fatal("same operation id with a different exact request was accepted")
	}
	if factoryCalls != 3 {
		t.Fatalf("candidate validated %d times", factoryCalls)
	}
	node, err := router.ActivateEnrollment(context.Background(), input.OperationID, input.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if node.Mode != ModeEligible || !node.AdmissionRequired || node.EnrollmentOperationID != input.OperationID {
		t.Fatalf("unexpected activated state: %+v", node)
	}
	if _, err := router.ActivateEnrollment(context.Background(), input.OperationID, input.NodeID); err != nil {
		t.Fatal("activation retry must be idempotent", err)
	}
	if candidateBackend.commandCalls != 0 {
		t.Fatalf("enrollment invoked %d lifecycle/command calls", candidateBackend.commandCalls)
	}
}

func TestEnrollmentIncompatibilityLeavesNodeSealed(t *testing.T) {
	trust := newRegistryTrust(t)
	first := registryNode(1, "cursor")
	first.RegistrationRevision, first.RegistrationEpoch, first.Compatibility = 1, 7, "compatible"
	currentClient, currentRaw := registryClient(t, trust, dynamicManifest(1, first))
	current := currentClient.RoutingRegistry()
	state := State{
		Schema: ProjectionStateSchema, OwnerID: current.OwnerID, RegistryVersion: current.RegistryVersion,
		RegistrySHA256: current.ManifestSHA256, RegistryOperationID: "r02-seed",
		RegistryRequestSHA256: strings.Repeat("a", 64), RegistryEnvelope: currentRaw,
		Nodes: map[string]NodeState{
			first.NodeID: {Mode: ModeEligible, StateVersion: 2, Generation: 1, RegistrationRevision: 1, IdentityEpoch: 7, Compatibility: "compatible", AdapterKind: "cursor", AdapterVersion: "1.0.31"},
		},
	}
	second := registryNode(2, "codex")
	second.RegistrationRevision, second.RegistrationEpoch, second.Compatibility = 1, 1, "compatible"
	candidateClient, candidateRaw := registryClient(t, trust, dynamicManifest(2, first, second))
	candidateRegistry := candidateClient.RoutingRegistry()
	candidateClient.Close()
	adapter := hp.AdapterIdentity{Kind: "codex", Version: "0.153.4"}
	admission := completeAdmission(current.OwnerID, second.NodeID, 1, 1, adapter)
	admission.Capabilities.TargetReservation = false
	backend := &readyBackend{registry: candidateRegistry, adapters: map[string]hp.AdapterIdentity{second.NodeID: adapter}, epochs: map[string]int64{second.NodeID: 1}, admission: &admission}
	router, _, _ := managedRegistryRouter(t, currentClient, state, func([]byte) (Backend, error) { return backend, nil })
	defer router.Close()
	input := EnrollmentRegistryInput{OperationID: "r10-incompatible", NodeID: second.NodeID, ExpectedRegistryVersion: current.RegistryVersion, ExpectedRegistrySHA256: current.ManifestSHA256, Registry: json.RawMessage(candidateRaw)}
	if _, err := router.InstallEnrollmentRegistry(input); err != nil {
		t.Fatal(err)
	}
	_, err := router.ActivateEnrollment(context.Background(), input.OperationID, input.NodeID)
	var fault *EnrollmentFault
	if !errors.As(err, &fault) || fault.Status != http.StatusUnprocessableEntity || fault.Code != "admission_capability_missing" {
		t.Fatalf("fault=%v", err)
	}
	if got := router.snapshot().Nodes[second.NodeID]; got.Mode != ModeSealed {
		t.Fatalf("incompatible node escaped seal: %+v", got)
	}
}
