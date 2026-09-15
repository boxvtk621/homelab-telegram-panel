// Command homelab-docker-adapter is the separate private R02 adapter process.
// R02 executes only a durable local fixture effect; real Docker effects are
// introduced by later roadmap stages.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/dockeradapter"
	"github.com/boxvtk621/homelab-telegram-panel/internal/operationclient"
)

var version = "dev"

func main() {
	os.Exit(execute(os.Args[1:], os.LookupEnv, os.Stdout))
}

func execute(args []string, lookup func(string) (string, bool), output io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(output, "COMMAND_INVALID")
		return 2
	}
	if args[0] == "version" {
		_, _ = fmt.Fprintln(output, "homelab-docker-adapter", version)
		return 0
	}
	if args[0] != "journal-init" && args[0] != "journal-check" && args[0] != "fixture-once" {
		_, _ = fmt.Fprintln(output, "COMMAND_INVALID")
		return 2
	}
	directory, directoryOK := lookup("DOCKER_ADAPTER_STATE_DIR")
	daemonID, daemonOK := lookup("DOCKER_ADAPTER_DAEMON_ID")
	instanceID, instanceOK := lookup("DOCKER_ADAPTER_INSTANCE_ID")
	if !directoryOK || !daemonOK || !instanceOK {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	if args[0] == "fixture-once" {
		return executeFixture(lookup, output, directory, daemonID, instanceID)
	}
	if args[0] == "journal-init" {
		executor, err := dockeradapter.Initialize(directory, daemonID, instanceID)
		if err != nil {
			_, _ = fmt.Fprintln(output, "JOURNAL_INITIALIZATION_REJECTED")
			return 1
		}
		executor.Close()
		_, _ = fmt.Fprintln(output, "JOURNAL_INITIALIZED")
		return 0
	}
	executor, err := dockeradapter.Open(directory, daemonID, instanceID)
	if err != nil {
		_, _ = fmt.Fprintln(output, "JOURNAL_UNAVAILABLE")
		return 1
	}
	executor.Close()
	_, _ = fmt.Fprintln(output, "JOURNAL_OK")
	return 0
}

func executeFixture(lookup func(string) (string, bool), output io.Writer, directory, daemonID, instanceID string) int {
	socket, socketOK := lookup("AGENT_SERVICE_SOCKET")
	ownerID, ownerOK := lookup("AGENT_SERVICE_OWNER")
	workerToken, workerTokenOK := lookup("AGENT_SERVICE_WORKER_TOKEN")
	workerID, workerOK := lookup("DOCKER_ADAPTER_WORKER_ID")
	loseAck, loseAckSet := lookup("DOCKER_ADAPTER_FIXTURE_LOSE_ACK")
	if !socketOK || !ownerOK || !workerTokenOK || !workerOK || loseAckSet && loseAck != "1" {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	executor, err := dockeradapter.Open(directory, daemonID, instanceID)
	if err != nil {
		_, _ = fmt.Fprintln(output, "JOURNAL_UNAVAILABLE")
		return 1
	}
	defer executor.Close()
	backend, err := dockeradapter.OpenFixtureBackend(directory, daemonID, instanceID, len(executor.Snapshot()) == 0)
	if err != nil {
		_, _ = fmt.Fprintln(output, "FIXTURE_STATE_UNAVAILABLE")
		return 1
	}
	if loseAckSet {
		backend.LoseNextAcknowledgement()
	}
	client, err := operationclient.NewWorker(socket, workerToken)
	if err != nil {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	work, err := client.Claim(ctx, ownerID, instanceID, workerID, 30*time.Second)
	if err != nil {
		var fault *operationclient.Fault
		if errors.As(err, &fault) && fault.Status == 404 {
			_, _ = fmt.Fprintln(output, "NO_OPERATION")
			return 0
		}
		_, _ = fmt.Fprintln(output, "CLAIM_UNAVAILABLE")
		return 1
	}
	if work.Intent.Target.HostID != daemonID || work.Intent.Target.NodeID != instanceID ||
		work.Intent.Kind != "adapter.fixture" || work.Intent.Step.Action != "adapter.fixture.apply" {
		_, _ = fmt.Fprintln(output, "WORK_REJECTED")
		return 1
	}
	request, err := operationclient.AdapterRequest(work)
	if err != nil {
		_, _ = fmt.Fprintln(output, "WORK_REJECTED")
		return 1
	}
	authority, err := operationclient.NewRemoteAuthority(client, ownerID, work)
	if err != nil {
		_, _ = fmt.Fprintln(output, "WORK_REJECTED")
		return 1
	}
	execution, executeErr := executeClaimedWork(ctx, executor, authority, operationclient.AdapterProof(work.Proof), backend, request, work.EffectState)
	proof := authority.Proof()
	if executeErr != nil && execution.JournalState == "sent" && execution.Proof.OperationID == work.Intent.OperationID {
		code := "effect_unknown"
		if _, err := client.Advance(ctx, ownerID, proof, "reconciling", "unknown", &code); err != nil {
			_, _ = fmt.Fprintln(output, "RESULT_UNKNOWN")
			return 1
		}
		_, _ = fmt.Fprintln(output, "OPERATION_UNKNOWN")
		return 1
	}
	if execution.JournalState == "failed" && execution.Proof.OperationID == work.Intent.OperationID {
		code := execution.Result.ReceiptID
		if _, err := client.Advance(ctx, ownerID, proof, "failed", "failed", &code); err != nil {
			_, _ = fmt.Fprintln(output, "RESULT_UNKNOWN")
			return 1
		}
		_, _ = fmt.Fprintln(output, "OPERATION_FAILED")
		return 1
	}
	if executeErr != nil {
		_, _ = fmt.Fprintln(output, "OPERATION_FAILED")
		return 1
	}
	effectState, known := completionEffectState(work.EffectState, execution)
	if !known {
		_, _ = fmt.Fprintln(output, "RESULT_UNKNOWN")
		return 1
	}
	code := execution.Result.ReceiptID
	if execution.Result.Outcome != "applied" {
		if _, err := client.Advance(ctx, ownerID, proof, "failed", "failed", &code); err != nil {
			_, _ = fmt.Fprintln(output, "RESULT_UNKNOWN")
			return 1
		}
		_, _ = fmt.Fprintln(output, "OPERATION_FAILED")
		return 1
	}
	status, err := client.Advance(ctx, ownerID, proof, "succeeded", effectState, &code)
	if err != nil || status.Phase != "succeeded" || status.EffectState != effectState {
		_, _ = fmt.Fprintln(output, "RESULT_UNKNOWN")
		return 1
	}
	_, _ = fmt.Fprintf(output, "OPERATION_OK id=%s state=%s\n", work.Intent.OperationID, effectState)
	return 0
}

func executeClaimedWork(ctx context.Context, executor *dockeradapter.Executor, authority dockeradapter.Authority, proof dockeradapter.AuthorityProof, backend dockeradapter.Backend, request dockeradapter.Request, effectState string) (dockeradapter.Execution, error) {
	if effectState == "not_sent" {
		return executor.Execute(ctx, authority, proof, backend, request)
	}
	return executor.Recover(ctx, authority, proof, backend, request)
}

func completionEffectState(claimedEffectState string, execution dockeradapter.Execution) (string, bool) {
	effectState := execution.JournalState
	if execution.Replayed && claimedEffectState == "unknown" && effectState == "acknowledged" {
		// The exact acknowledged journal survived an ambiguous local completion,
		// while Agent Service conservatively retained unknown. Publishing it as
		// reconciled preserves the generic prohibition on unknown -> acknowledged.
		effectState = "reconciled"
	}
	return effectState, effectState == "acknowledged" || effectState == "reconciled"
}
