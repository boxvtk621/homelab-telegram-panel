package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/dockeradapter"
)

func TestJournalInitializationIsExplicitAndOneTime(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"DOCKER_ADAPTER_STATE_DIR":   directory,
		"DOCKER_ADAPTER_DAEMON_ID":   "fixture-daemon",
		"DOCKER_ADAPTER_INSTANCE_ID": "fixture-instance",
	}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	var output bytes.Buffer
	if code := execute([]string{"journal-check"}, lookup, &output); code != 1 || output.String() != "JOURNAL_UNAVAILABLE\n" {
		t.Fatalf("uninitialized check code=%d output=%q", code, output.String())
	}
	output.Reset()
	if code := execute([]string{"journal-init"}, lookup, &output); code != 0 || output.String() != "JOURNAL_INITIALIZED\n" {
		t.Fatalf("init code=%d output=%q", code, output.String())
	}
	output.Reset()
	if code := execute([]string{"journal-init"}, lookup, &output); code != 1 || output.String() != "JOURNAL_INITIALIZATION_REJECTED\n" {
		t.Fatalf("reinitialize code=%d output=%q", code, output.String())
	}
	output.Reset()
	if code := execute([]string{"journal-check"}, lookup, &output); code != 0 || output.String() != "JOURNAL_OK\n" {
		t.Fatalf("reopen code=%d output=%q", code, output.String())
	}
}

func TestUnknownDBStateUsesOnlyExactAcknowledgedReplayAsReconciled(t *testing.T) {
	for name, fixture := range map[string]struct {
		claimed   string
		execution dockeradapter.Execution
		want      string
		known     bool
	}{
		"fresh acknowledged":          {claimed: "not_sent", execution: dockeradapter.Execution{JournalState: "acknowledged"}, want: "acknowledged", known: true},
		"unknown acknowledged replay": {claimed: "unknown", execution: dockeradapter.Execution{JournalState: "acknowledged", Replayed: true}, want: "reconciled", known: true},
		"unknown sent replay":         {claimed: "unknown", execution: dockeradapter.Execution{JournalState: "sent", Replayed: true}, want: "sent", known: false},
	} {
		t.Run(name, func(t *testing.T) {
			got, known := completionEffectState(fixture.claimed, fixture.execution)
			if got != fixture.want || known != fixture.known {
				t.Fatalf("state=%s known=%v", got, known)
			}
		})
	}
}

func TestClaimedUnknownWithoutExactJournalNeverCallsApply(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	executor, err := dockeradapter.Initialize(directory, "fixture-daemon", "fixture-instance")
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	request := dockeradapter.Request{
		SchemaID: dockeradapter.RequestSchemaID, OperationID: "op-unknown-missing", StepID: "apply-1",
		Generation: 1, Action: "fixture.apply", ResourceIDs: []string{"container:agent-1"},
	}
	proof := dockeradapter.AuthorityProof{
		OperationID: request.OperationID, Generation: request.Generation, WorkerID: "worker-1",
		WorkerToken: "worker-token-1", OperationVersion: 1,
	}
	authority := dockeradapter.NewFixtureAuthority()
	if err := authority.Grant("fixture-daemon", "fixture-instance", request, proof); err != nil {
		t.Fatal(err)
	}
	backend := dockeradapter.NewFixtureBackend()
	if _, err := executeClaimedWork(context.Background(), executor, authority, proof, backend, request, "unknown"); !errors.Is(err, dockeradapter.ErrReconciliationRequired) {
		t.Fatal("unknown work without exact journal was not blocked", err)
	}
	if backend.ApplyCalls() != 0 || len(executor.Snapshot()) != 0 {
		t.Fatalf("unknown work crossed effect boundary: applyCalls=%d journal=%+v", backend.ApplyCalls(), executor.Snapshot())
	}
}
