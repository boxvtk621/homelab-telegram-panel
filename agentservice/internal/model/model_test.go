package model

import (
	"strings"
	"testing"
	"time"
)

func TestUnavailableHostObservationUsesContractFieldLimits(t *testing.T) {
	base := HostObservation{
		SchemaID: HostObservationSchemaID, HostID: "20000000-0000-4000-8000-000000000001", HostVersion: 1,
		ObservedAt: "2026-09-16T10:00:00Z", Availability: "unavailable",
		FailureStage: "daemon_info", FailureCode: "docker_response_invalid", NextAction: "Check Docker API.",
		DockerContextRef: "desktop-linux", Capabilities: []string{}, RegistryAvailability: "not_checked",
	}
	for name, mutate := range map[string]func(*HostObservation){
		"daemon id":        func(value *HostObservation) { value.DaemonID = strings.Repeat("d", 129) },
		"context endpoint": func(value *HostObservation) { value.ContextEndpoint = strings.Repeat("e", 81) },
		"api version":      func(value *HostObservation) { value.APIVersion = strings.Repeat("a", 33) },
		"engine version":   func(value *HostObservation) { value.EngineVersion = strings.Repeat("v", 65) },
		"host key":         func(value *HostObservation) { value.HostKeySHA256 = "not-a-fingerprint" },
	} {
		t.Run(name, func(t *testing.T) {
			value := base
			mutate(&value)
			if ValidateHostObservation(value) == nil {
				t.Fatalf("oversized or malformed %s accepted", name)
			}
		})
	}
	base.DaemonID = strings.Repeat("d", 128)
	base.ContextEndpoint = strings.Repeat("e", 80)
	base.APIVersion = strings.Repeat("a", 32)
	base.EngineVersion = strings.Repeat("v", 64)
	if err := ValidateHostObservation(base); err != nil {
		t.Fatalf("contract boundary rejected: %v", err)
	}
}

func TestDeriveStatusDistinguishesR01States(t *testing.T) {
	now := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Second)
	stale := now.Add(-StaleAfter - time.Nanosecond)
	source := "fixture"
	tests := []struct {
		name         string
		registration string
		observation  StoredObservation
		want         string
	}{
		{name: "readonly", registration: "legacy_readonly", want: "readonly"},
		{name: "online", registration: "compatible", observation: StoredObservation{Process: "running", Connection: "online", Readiness: "ready", Occupancy: "idle", ObservedAt: &fresh, Source: &source}, want: "online"},
		{name: "unready", registration: "compatible", observation: StoredObservation{Process: "running", Connection: "online", Readiness: "unready", Occupancy: "idle", ObservedAt: &fresh, Source: &source}, want: "unready"},
		{name: "busy", registration: "compatible", observation: StoredObservation{Process: "running", Connection: "online", Readiness: "ready", Occupancy: "busy", ObservedAt: &fresh, Source: &source}, want: "busy"},
		{name: "stale", registration: "compatible", observation: StoredObservation{Process: "running", Connection: "online", Readiness: "ready", Occupancy: "idle", ObservedAt: &stale, Source: &source}, want: "stale"},
		{name: "stopped", registration: "compatible", observation: StoredObservation{Process: "stopped", Connection: "offline", Readiness: "unknown", Occupancy: "unknown", ObservedAt: &fresh, Source: &source}, want: "stopped"},
		{name: "old stopped is stale", registration: "compatible", observation: StoredObservation{Process: "stopped", Connection: "offline", Readiness: "unknown", Occupancy: "unknown", ObservedAt: &stale, Source: &source}, want: "stale"},
		{name: "conflicting stopped axes", registration: "compatible", observation: StoredObservation{Process: "stopped", Connection: "online", Readiness: "ready", Occupancy: "idle", ObservedAt: &fresh, Source: &source}, want: "unknown"},
		{name: "missing observation", registration: "compatible", observation: StoredObservation{Process: "unknown", Connection: "unknown", Readiness: "unknown", Occupancy: "unknown"}, want: "unknown"},
		{name: "offline is not stopped", registration: "compatible", observation: StoredObservation{Process: "running", Connection: "offline", Readiness: "unknown", Occupancy: "unknown", ObservedAt: &fresh, Source: &source}, want: "unknown"},
	}
	seen := map[string]bool{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := DeriveStatus(now, test.registration, test.observation)
			if got != test.want {
				t.Fatalf("status=%q want=%q", got, test.want)
			}
			seen[got] = true
		})
	}
	for _, status := range []string{"online", "unready", "busy", "stale", "stopped", "unknown", "readonly"} {
		if !seen[status] {
			t.Errorf("status %q was not exercised", status)
		}
	}
}

func TestUnknownMetricIsNotSynthesized(t *testing.T) {
	actions := ActionsFor("unknown")
	if actions.SendMessage.Allowed || actions.SendMessage.Reason != "state_unknown" || actions.SendMessage.NextAction == "" {
		t.Fatalf("unexpected unknown actions: %#v", actions)
	}
	if !actions.OpenWorkspace.Allowed {
		t.Fatal("existing workspace action must remain available")
	}
}

func TestValidateSnapshotRequiresCompleteExactInventory(t *testing.T) {
	manifest := RegistryManifest{Nodes: []RegistryNode{{NodeID: "10000000-0000-4000-8000-000000000001"}}}
	valid := ImportSnapshot{
		SchemaID: ImportSchemaID, Complete: true,
		Hosts: []HostSeed{{HostID: "20000000-0000-4000-8000-000000000001", Name: "Host"}},
		Nodes: []NodeSeed{{NodeID: manifest.Nodes[0].NodeID, HostID: "20000000-0000-4000-8000-000000000001", RegistrationMode: "legacy_readonly", Dialogs: []DialogSeed{}}},
	}
	if err := ValidateSnapshot(manifest, valid); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.Complete = false
	if ValidateSnapshot(manifest, invalid) == nil {
		t.Fatal("partial snapshot accepted")
	}
	invalid = valid
	invalid.Nodes = append([]NodeSeed(nil), valid.Nodes...)
	invalid.Nodes[0].NodeID = "10000000-0000-4000-8000-000000000002"
	if ValidateSnapshot(manifest, invalid) == nil {
		t.Fatal("unknown node accepted")
	}
	invalid = valid
	invalid.Nodes = append([]NodeSeed(nil), valid.Nodes...)
	invalid.Nodes[0].Observation = &ObservationSeed{
		Process: "stopped", Connection: "online", Readiness: "ready", Occupancy: "idle",
		ObservedAt: "2026-09-14T09:00:00Z", Source: "fixture",
	}
	if ValidateSnapshot(manifest, invalid) == nil {
		t.Fatal("conflicting observation accepted")
	}
	unsafe := MaximumSafeInt + 1
	invalid = valid
	invalid.Nodes = append([]NodeSeed(nil), valid.Nodes...)
	invalid.Nodes[0].Observation = &ObservationSeed{
		Process: "running", Connection: "online", Readiness: "ready", Occupancy: "idle",
		ObservedAt: "2026-09-14T09:00:00Z", Source: "fixture", PendingCount: &unsafe,
	}
	if ValidateSnapshot(manifest, invalid) == nil {
		t.Fatal("unsafe JSON integer accepted")
	}
}

func TestRetirementDescriptorVerifiedProofs(t *testing.T) {
	exit := true
	proof := "receipt:ownership-release"
	base := RetirementDescriptor{SchemaID: RetirementSchema, DescriptorVersion: 1, State: "verified", ObservedAt: "2026-09-14T09:00:00Z"}
	managed := base
	managed.Kind = "managed_stopped"
	managed.ProcessExitObserved = &exit
	if err := ValidateRetirementDescriptor(managed); err != nil {
		t.Fatal(err)
	}
	external := base
	external.Kind = "external_detached"
	external.OwnershipReleaseProof = &proof
	if err := ValidateRetirementDescriptor(external); err != nil {
		t.Fatal(err)
	}
	external.ProcessExitObserved = &exit
	if ValidateRetirementDescriptor(external) == nil {
		t.Fatal("external descriptor may not imitate process exit")
	}
}
