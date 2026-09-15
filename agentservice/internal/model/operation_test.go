package model

import "testing"

func operationFixture() OperationIntent {
	return OperationIntent{
		SchemaID: OperationSchemaID, OperationID: "op-1", Kind: "adapter.fixture",
		Target: OperationTarget{
			NodeID:               "20000000-0000-4000-8000-000000000001",
			HostID:               "10000000-0000-4000-8000-000000000001",
			RegistrationRevision: 1, RegistrationEpoch: 1, Generation: 0,
		},
		Step: OperationStepIntent{
			StepID: "apply-1", Action: "adapter.fixture.apply",
			ResourceIDs: []string{"container:agent-1", "volume:agent-1"},
		},
	}
}

func TestOperationRequestHashIsNormalizedAndClosed(t *testing.T) {
	intent := operationFixture()
	first, err := OperationRequestHash(intent)
	if err != nil || len(first) != 64 {
		t.Fatal(first, err)
	}
	second, err := OperationRequestHash(intent)
	if err != nil || second != first {
		t.Fatal("stable operation hash changed", first, second, err)
	}
	intent.Step.ResourceIDs = []string{"volume:agent-1", "container:agent-1"}
	if _, err := OperationRequestHash(intent); err == nil {
		t.Fatal("non-canonical resource order accepted")
	}
	intent = operationFixture()
	intent.Step.Action = "router.registry.install"
	if _, err := OperationRequestHash(intent); err == nil {
		t.Fatal("mismatched kind and effect accepted")
	}
}

func TestOperationWorkProofBindsExactIntent(t *testing.T) {
	intent := operationFixture()
	requestHash, err := OperationRequestHash(intent)
	if err != nil {
		t.Fatal(err)
	}
	proof := OperationClaimProof{
		SchemaID: OperationProofSchemaID, OperationID: intent.OperationID, RequestHash: requestHash,
		NodeID: intent.Target.NodeID, Generation: intent.Target.Generation + 1,
		WorkerID: "worker-1", WorkerToken: "30000000-0000-4000-8000-000000000001",
		OperationVersion: 2, LeaseExpiresAt: "2026-09-14T14:00:00.123456Z",
	}
	work := OperationWork{SchemaID: OperationWorkSchemaID, Intent: intent, Proof: proof, EffectState: "not_sent"}
	if err := ValidateOperationWork(work); err != nil {
		t.Fatal("exact work proof was rejected", err)
	}
	work.Intent.Step.ResourceIDs = []string{"container:agent-2"}
	if err := ValidateOperationWork(work); err == nil {
		t.Fatal("changed intent retained the original proof authority")
	}
}

func TestUnknownOperationCanOnlyReconcile(t *testing.T) {
	if !ValidOperationTransition("applying", "sent", "reconciling", "unknown") {
		t.Fatal("unknown effect could not enter reconciliation")
	}
	for _, phase := range []string{"succeeded", "failed", "waiting"} {
		if ValidOperationTransition("applying", "sent", phase, "unknown") {
			t.Fatal("unknown effect reached", phase)
		}
	}
	if ValidOperationTransition("reconciling", "unknown", "succeeded", "unknown") {
		t.Fatal("unknown effect became success")
	}
	for _, effect := range []string{"not_sent", "sent", "acknowledged"} {
		if ValidOperationTransition("reconciling", "unknown", "reconciling", effect) {
			t.Fatal("unknown effect regressed to", effect)
		}
	}
	if !ValidOperationTransition("verifying", "sent", "succeeded", "acknowledged") ||
		!ValidOperationTransition("reconciling", "unknown", "succeeded", "reconciled") {
		t.Fatal("known success transition rejected")
	}
}
