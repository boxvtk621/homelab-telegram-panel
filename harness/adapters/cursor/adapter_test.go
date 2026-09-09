package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessadapter"
)

func TestStartResumeAndDurablePrivateMapping(t *testing.T) {
	config := fakeConfig(t)
	first, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	reference := testReference(1)
	start, err := first.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "first", Policy: denyPolicy(), Context: testBoundary(1),
	})
	if err != nil || start.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", start, err)
	}
	stream, err := first.Events(context.Background(), harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, stream, 3)
	if _, ok := events[0].(harnessadapter.StartedEvent); !ok {
		t.Fatalf("first event = %T", events[0])
	}
	message, ok := events[1].(harnessadapter.AssistantMessageEvent)
	if !ok || message.Content.Content != "reply:first" || message.FinishReason != "complete" {
		t.Fatalf("assistant event = %#v", events[1])
	}
	terminal, ok := events[2].(harnessadapter.TerminalEvent)
	if !ok || terminal.Outcome != harnessadapter.ReconcileCompleted {
		t.Fatalf("terminal event = %#v", events[2])
	}
	mapping, err := os.ReadFile(filepath.Join(config.StateDir, "native-mapping.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(mapping) == "" || contains(mapping, []byte(config.APIKey)) {
		t.Fatal("native mapping is empty or contains the API key")
	}
	if err := first.Close(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		// An explicit Close kills the long-lived worker; its wait status is not a
		// provider outcome and is intentionally ignored by callers as well.
	}

	second, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	resumedReference := testReference(2)
	resume, err := second.Resume(context.Background(), harnessadapter.ResumeInput{
		Attempt: resumedReference, Prompt: "resume", Policy: denyPolicy(), Context: testBoundary(2),
	})
	if err != nil || resume.Outcome != harnessadapter.ResumeStarted {
		t.Fatalf("resume = %#v, %v", resume, err)
	}
	resumed, err := second.Events(context.Background(), harnessadapter.EventsInput{Attempt: resumedReference})
	if err != nil {
		t.Fatal(err)
	}
	resumeEvents := readEvents(t, resumed, 3)
	if message, ok := resumeEvents[1].(harnessadapter.AssistantMessageEvent); !ok || message.Content.Content != "reply:resume:agent-1" {
		t.Fatalf("resumed assistant event = %#v", resumeEvents[1])
	}
}

func TestDispatchIsAsyncAndCancelHasSeparateTerminal(t *testing.T) {
	adapter, err := New(fakeConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	reference := testReference(1)
	startedAt := time.Now()
	result, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "long", Policy: denyPolicy(), Context: testBoundary(1),
	})
	if err != nil || result.Outcome != harnessadapter.StartStarted || time.Since(startedAt) > time.Second {
		t.Fatalf("asynchronous start = %#v, %v, elapsed %s", result, err, time.Since(startedAt))
	}
	stream, err := adapter.Events(context.Background(), harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := first.(harnessadapter.StartedEvent); !ok {
		t.Fatalf("first event = %T", first)
	}
	cancelled, err := adapter.Cancel(context.Background(), harnessadapter.CancelInput{Attempt: reference})
	if err != nil || cancelled.Outcome != harnessadapter.CancelAcknowledged {
		t.Fatalf("cancel = %#v, %v", cancelled, err)
	}
	event, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	terminal, ok := event.(harnessadapter.TerminalEvent)
	if !ok || terminal.Outcome != harnessadapter.ReconcileInterrupted || terminal.EffectStatus != "known" {
		t.Fatalf("cancel terminal = %#v", event)
	}
}

func TestSteerToolLifecycleAndPolicyRejection(t *testing.T) {
	adapter, err := New(fakeConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	identity, err := adapter.Identity(context.Background())
	if err != nil || harnessadapter.ValidateIdentity(identity) != nil {
		t.Fatalf("identity = %#v, %v", identity, err)
	}
	reference := testReference(1)
	result, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "tool", Policy: denyPolicy(), Context: testBoundary(1),
	})
	if err != nil || result.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", result, err)
	}
	steered, err := adapter.Steer(context.Background(), harnessadapter.SteerInput{
		Attempt: reference, MessageID: "50000000-0000-4000-8000-000000000001", Text: "continue",
	})
	if err != nil || steered.Outcome != harnessadapter.SteerApplied {
		t.Fatalf("steer = %#v, %v", steered, err)
	}
	stream, err := adapter.Events(context.Background(), harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, stream, 5)
	started, ok := events[1].(harnessadapter.ToolStartedEvent)
	if !ok || started.ToolName != "fixture" || len(started.ActionHash) != 64 || started.Input.Kind != "unavailable" {
		t.Fatalf("tool start = %#v", events[1])
	}
	completed, ok := events[2].(harnessadapter.ToolCompletedEvent)
	if !ok || completed.CallID != started.CallID || completed.Status != "succeeded" || completed.Result.Kind != "unavailable" {
		t.Fatalf("tool completion = %#v", events[2])
	}

	unsupported := denyPolicy()
	unsupported.ToolManifest = []byte(`[{"name":"shell"}]`)
	sum := sha256.Sum256(unsupported.ToolManifest)
	unsupported.ToolManifestHash = hex.EncodeToString(sum[:])
	unsupported.EffectiveHash = harnessadapter.EffectivePolicyHash(unsupported)
	rejected, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: testReference(2), Prompt: "must reject", Policy: unsupported, Context: testBoundary(2),
	})
	if err != nil || rejected.Outcome != harnessadapter.StartRejected || rejected.Failure == nil || rejected.Failure.Class != harnessadapter.FailurePolicy {
		t.Fatalf("unsupported policy = %#v, %v", rejected, err)
	}
}

func TestLostAcknowledgementIsNotBlindlyRedispatched(t *testing.T) {
	config := fakeConfig(t)
	config.OperationTimeout = 40 * time.Millisecond
	adapter, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	reference := testReference(1)
	input := harnessadapter.StartInput{Attempt: reference, Prompt: "lost", Policy: denyPolicy(), Context: testBoundary(1)}
	first, err := adapter.Start(context.Background(), input)
	if err == nil || first.Outcome != harnessadapter.StartUnknown {
		t.Fatalf("lost acknowledgement = %#v, %v", first, err)
	}
	second, err := adapter.Start(context.Background(), input)
	if err != nil || second.Outcome != harnessadapter.StartUnknown || second.Failure == nil {
		t.Fatalf("repeat after lost acknowledgement = %#v, %v", second, err)
	}
}

func readEvents(t *testing.T, stream harnessadapter.EventStream, count int) []harnessadapter.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	events := make([]harnessadapter.Event, 0, count)
	for len(events) < count {
		event, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("read event %d: %v", len(events), err)
		}
		events = append(events, event)
	}
	return events
}

func denyPolicy() harnessadapter.PolicySnapshot {
	content := []byte("deny all tools")
	tools := []byte(`[]`)
	contentSum := sha256.Sum256(content)
	toolsSum := sha256.Sum256(tools)
	policy := harnessadapter.PolicySnapshot{
		Revision: "cursor-alpha@1", Content: content, ContentHash: hex.EncodeToString(contentSum[:]),
		ToolManifest: tools, ToolManifestHash: hex.EncodeToString(toolsSum[:]), ApprovalMode: harnessadapter.ApprovalModeDeny,
	}
	policy.EffectiveHash = harnessadapter.EffectivePolicyHash(policy)
	return policy
}

func testReference(generation int64) harnessadapter.AttemptRef {
	last := byte('0' + generation)
	return harnessadapter.AttemptRef{
		NodeID: "10000000-0000-4000-8000-000000000001", DialogID: "20000000-0000-4000-8000-000000000001",
		RequestID: "30000000-0000-4000-8000-00000000000" + string(last), AttemptID: "40000000-0000-4000-8000-00000000000" + string(last), Generation: generation,
	}
}

func testBoundary(sequence int64) harnessadapter.ContextBoundary {
	return harnessadapter.ContextBoundary{MessageID: "50000000-0000-4000-8000-00000000000" + string(byte('0'+sequence)), Sequence: sequence}
}

func fakeConfig(t *testing.T) Config {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node executable is unavailable")
	}
	workerPath := filepath.Join(t.TempDir(), "fake-worker.mjs")
	if err := os.WriteFile(workerPath, []byte(fakeWorker), 0o600); err != nil {
		t.Fatal(err)
	}
	return Config{
		NodeExecutable: nodePath, WorkerEntrypoint: workerPath, StateDir: filepath.Join(t.TempDir(), "state"),
		APIKey: "test-key-never-provider", OperationTimeout: time.Second, MaxFrameBytes: 64 * 1024,
	}
}

func contains(value, part []byte) bool {
	if len(part) == 0 || len(part) > len(value) {
		return false
	}
	for index := 0; index <= len(value)-len(part); index++ {
		if string(value[index:index+len(part)]) == string(part) {
			return true
		}
	}
	return false
}

const fakeWorker = `
import readline from 'node:readline';
const lines = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
const active = new Map();
const send = (value) => process.stdout.write(JSON.stringify(value) + '\n');
for await (const line of lines) {
  const frame = JSON.parse(line);
  const payload = frame.payload || {};
  if (frame.operation === 'init') {
    send({ type: 'response', id: frame.id, ok: true, result: { version: '1.0.31' } });
  } else if (frame.operation === 'dispatch') {
    if (payload.prompt === 'lost') continue;
    const agentId = payload.resumeAgentId || 'agent-1';
    active.set(payload.attemptKey, { runId: 'run-' + payload.attemptKey, prompt: payload.prompt, agentId });
    send({ type: 'response', id: frame.id, ok: true, result: { agentId, runId: 'run-' + payload.attemptKey } });
    if (payload.prompt === 'tool') {
      send({ type: 'event', attemptKey: payload.attemptKey, event: 'tool', callId: 'private-call', name: 'fixture', status: 'running' });
      send({ type: 'event', attemptKey: payload.attemptKey, event: 'tool', callId: 'private-call', name: 'fixture', status: 'completed' });
      setTimeout(() => send({ type: 'event', attemptKey: payload.attemptKey, event: 'terminal', status: 'finished', text: 'reply:tool' }), 30);
    } else if (payload.prompt !== 'long') {
      setTimeout(() => send({ type: 'event', attemptKey: payload.attemptKey, event: 'terminal', status: 'finished', text: 'reply:' + payload.prompt + (payload.resumeAgentId ? ':' + payload.resumeAgentId : ''), usage: { inputTokens: 2, outputTokens: 1, totalTokens: 3 } }), 10);
    }
  } else if (frame.operation === 'steer') {
    const run = active.get(payload.attemptKey);
    send(run && run.runId === payload.runId ? { type: 'response', id: frame.id, ok: true, result: { status: 'complete_delivered' } } : { type: 'response', id: frame.id, ok: false, code: 'rejected' });
  } else if (frame.operation === 'cancel') {
    const run = active.get(payload.attemptKey);
    if (!run || run.runId !== payload.runId) send({ type: 'response', id: frame.id, ok: false, code: 'rejected' });
    else {
      send({ type: 'response', id: frame.id, ok: true, result: {} });
      send({ type: 'event', attemptKey: payload.attemptKey, event: 'terminal', status: 'cancelled' });
      active.delete(payload.attemptKey);
    }
  }
}
`
