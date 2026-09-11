package codex

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessadapter"
)

const adapterHelperEnvironment = "CODEX_ADAPTER_HELPER=1"

func TestAdapterStartResumeAndIndependentDialogs(t *testing.T) {
	adapter := newTestAdapter(t, 2*time.Second)
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first := adapterReference(1, 1, 1)
	second := adapterReference(2, 1, 1)
	assertStartAndTerminal(t, ctx, adapter, first, adapterBoundary(1), "first")
	assertStartAndTerminal(t, ctx, adapter, second, adapterBoundary(2), "second")
	firstDialog, ok := adapter.store.dialog(first.DialogID)
	if !ok {
		t.Fatal("first dialog mapping is missing")
	}
	secondDialog, ok := adapter.store.dialog(second.DialogID)
	if !ok || firstDialog.ThreadID == secondDialog.ThreadID {
		t.Fatalf("dialog mappings are not independent: %#v %#v", firstDialog, secondDialog)
	}

	continued := adapterReference(1, 2, 2)
	result, err := adapter.Resume(ctx, harnessadapter.ResumeInput{
		Attempt: continued, Prompt: "continued", Context: adapterBoundary(3), Policy: adapterPolicy(),
	})
	if err != nil || result.Outcome != harnessadapter.ResumeStarted {
		t.Fatalf("resume = %#v, %v", result, err)
	}
	assertTerminalEvents(t, ctx, adapter, continued)
	updated, ok := adapter.store.dialog(first.DialogID)
	if !ok || updated.ThreadID != firstDialog.ThreadID || updated.Boundary.Sequence != 3 {
		t.Fatalf("resumed dialog = %#v", updated)
	}
}

func TestAdapterSteerAndInterruptUseExactTurnFence(t *testing.T) {
	adapter := newTestAdapter(t, 2*time.Second)
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reference := adapterReference(1, 1, 1)
	result, err := adapter.Start(ctx, harnessadapter.StartInput{
		Attempt: reference, Prompt: "hold", Context: adapterBoundary(1), Policy: adapterPolicy(),
	})
	if err != nil || result.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", result, err)
	}
	steer, err := adapter.Steer(ctx, harnessadapter.SteerInput{Attempt: reference, MessageID: adapterBoundary(2).MessageID, Text: "steer"})
	if err != nil || steer.Outcome != harnessadapter.SteerApplied {
		t.Fatalf("steer = %#v, %v", steer, err)
	}
	cancelResult, err := adapter.Cancel(ctx, harnessadapter.CancelInput{Attempt: reference})
	if err != nil || cancelResult.Outcome != harnessadapter.CancelAcknowledged {
		t.Fatalf("cancel = %#v, %v", cancelResult, err)
	}
	if reconciled, err := adapter.Reconcile(ctx, harnessadapter.ReconcileInput{Attempt: reference}); err != nil || reconciled.Outcome != harnessadapter.ReconcileRunning {
		t.Fatalf("interrupt ACK became terminal: %#v, %v", reconciled, err)
	}
	events := readAdapterEvents(t, ctx, adapter, reference)
	terminal, ok := events[len(events)-1].(harnessadapter.TerminalEvent)
	if !ok || terminal.Outcome != harnessadapter.ReconcileInterrupted {
		t.Fatalf("terminal = %#v", events[len(events)-1])
	}
}

func TestAdapterDeduplicatesExactActiveDispatchAndRejectsConflict(t *testing.T) {
	adapter := newTestAdapter(t, 2*time.Second)
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reference := adapterReference(1, 1, 1)
	input := harnessadapter.StartInput{Attempt: reference, Prompt: "hold", Context: adapterBoundary(1), Policy: adapterPolicy()}
	first, err := adapter.Start(ctx, input)
	if err != nil || first.Outcome != harnessadapter.StartStarted {
		t.Fatalf("first start = %#v, %v", first, err)
	}
	duplicate, err := adapter.Start(ctx, input)
	if err != nil || duplicate.Outcome != harnessadapter.StartStarted {
		t.Fatalf("duplicate start = %#v, %v", duplicate, err)
	}
	input.Prompt = "different"
	conflict, err := adapter.Start(ctx, input)
	if err != nil || conflict.Outcome != harnessadapter.StartUnknown || conflict.Failure == nil || conflict.Failure.Class != harnessadapter.FailureProtocol {
		t.Fatalf("conflicting duplicate = %#v, %v", conflict, err)
	}
	if result, err := adapter.Cancel(ctx, harnessadapter.CancelInput{Attempt: reference}); err != nil || result.Outcome != harnessadapter.CancelAcknowledged {
		t.Fatalf("cancel = %#v, %v", result, err)
	}
	_ = readAdapterEvents(t, ctx, adapter, reference)
}

func TestAdapterMapsOneNonSecretInputRequest(t *testing.T) {
	adapter := newTestAdapter(t, 2*time.Second)
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reference := adapterReference(1, 1, 1)
	result, err := adapter.Start(ctx, harnessadapter.StartInput{
		Attempt: reference, Prompt: "ask-input", Context: adapterBoundary(1), Policy: adapterPolicy(),
	})
	if err != nil || result.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", result, err)
	}
	stream, err := adapter.Events(ctx, harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	if event, err := stream.Next(ctx); err != nil {
		t.Fatal(err)
	} else if _, ok := event.(harnessadapter.StartedEvent); !ok {
		t.Fatalf("first event = %T", event)
	}
	event, err := stream.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	requested, ok := event.(harnessadapter.InputRequestedEvent)
	if !ok || requested.Prompt.Content != "Choose one value" {
		t.Fatalf("input request = %#v", event)
	}
	response, err := adapter.RespondInput(ctx, harnessadapter.RespondInputInput{
		Attempt: reference, InputRequestID: requested.InputRequestID, InputVersion: 2, Text: "value",
	})
	if err != nil || response.Outcome != harnessadapter.ResponseApplied {
		t.Fatalf("input response = %#v, %v", response, err)
	}
	var terminal harnessadapter.TerminalEvent
	for {
		event, err = stream.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := event.(harnessadapter.TerminalEvent); ok {
			terminal = value
		}
	}
	if terminal.Outcome != harnessadapter.ReconcileCompleted || terminal.Output == nil || terminal.Output.Content != "answer:value" {
		t.Fatalf("terminal = %#v", terminal)
	}
}

func TestAdapterRejectsInputResolvedBeforeClientResponse(t *testing.T) {
	adapter := newTestAdapter(t, 2*time.Second)
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reference := adapterReference(1, 1, 1)
	result, err := adapter.Start(ctx, harnessadapter.StartInput{
		Attempt: reference, Prompt: "ask-input-resolved", Context: adapterBoundary(1), Policy: adapterPolicy(),
	})
	if err != nil || result.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", result, err)
	}
	stream, err := adapter.Events(ctx, harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Next(ctx); err != nil {
		t.Fatal(err)
	}
	event, err := stream.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	requested, ok := event.(harnessadapter.InputRequestedEvent)
	if !ok {
		t.Fatalf("input request = %#v", event)
	}
	response, err := adapter.RespondInput(ctx, harnessadapter.RespondInputInput{
		Attempt: reference, InputRequestID: requested.InputRequestID, InputVersion: 2, Text: "late",
	})
	if err != nil || response.Outcome != harnessadapter.ResponseRejected {
		t.Fatalf("stale input response = %#v, %v", response, err)
	}
	if cancelResult, err := adapter.Cancel(ctx, harnessadapter.CancelInput{Attempt: reference}); err != nil || cancelResult.Outcome != harnessadapter.CancelAcknowledged {
		t.Fatalf("cancel = %#v, %v", cancelResult, err)
	}
}

func TestAdapterInputLifecycleResolutionWithoutProgressStaysUnknown(t *testing.T) {
	adapter := newTestAdapter(t, 2*time.Second)
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reference := adapterReference(1, 1, 1)
	result, err := adapter.Start(ctx, harnessadapter.StartInput{
		Attempt: reference, Prompt: "ask-input-cleanup", Context: adapterBoundary(1), Policy: adapterPolicy(),
	})
	if err != nil || result.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", result, err)
	}
	stream, err := adapter.Events(ctx, harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Next(ctx); err != nil {
		t.Fatal(err)
	}
	event, err := stream.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	requested, ok := event.(harnessadapter.InputRequestedEvent)
	if !ok {
		t.Fatalf("input request = %#v", event)
	}
	response, err := adapter.RespondInput(ctx, harnessadapter.RespondInputInput{
		Attempt: reference, InputRequestID: requested.InputRequestID, InputVersion: 2, Text: "value",
	})
	if err != nil || response.Outcome != harnessadapter.ResponseUnknown {
		t.Fatalf("unconfirmed input response = %#v, %v", response, err)
	}
	reconciled, err := adapter.Reconcile(ctx, harnessadapter.ReconcileInput{Attempt: reference})
	if err != nil || reconciled.Outcome != harnessadapter.ReconcileUnknown || reconciled.EffectStatus != "unknown" {
		t.Fatalf("unconfirmed input reconcile = %#v, %v", reconciled, err)
	}
}

func TestAdapterMapsNativeErrorRetrySemantics(t *testing.T) {
	tests := []struct {
		prompt string
		want   harnessadapter.ReconcileOutcome
	}{
		{prompt: "error-no-retry", want: harnessadapter.ReconcileFailed},
		{prompt: "error-retry", want: harnessadapter.ReconcileCompleted},
	}
	for index, test := range tests {
		t.Run(test.prompt, func(t *testing.T) {
			adapter := newTestAdapter(t, 2*time.Second)
			defer adapter.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			reference := adapterReference(int64(index+1), 1, 1)
			result, err := adapter.Start(ctx, harnessadapter.StartInput{
				Attempt: reference, Prompt: test.prompt, Context: adapterBoundary(1), Policy: adapterPolicy(),
			})
			if err != nil || result.Outcome != harnessadapter.StartStarted {
				t.Fatalf("start = %#v, %v", result, err)
			}
			events := readAdapterEvents(t, ctx, adapter, reference)
			terminal, ok := events[len(events)-1].(harnessadapter.TerminalEvent)
			if !ok || terminal.Outcome != test.want {
				t.Fatalf("terminal = %#v", events[len(events)-1])
			}
		})
	}
}

func TestAdapterFailsClosedForEveryUnexpectedNativeItem(t *testing.T) {
	itemTypes := []string{
		"functionCallOutput", "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall",
		"collabAgentToolCall", "subAgentActivity", "webSearch", "imageView", "sleep",
		"imageGeneration", "enteredReviewMode", "exitedReviewMode", "futureToolItem",
	}
	for index, itemType := range itemTypes {
		t.Run(itemType, func(t *testing.T) {
			adapter := newTestAdapter(t, 2*time.Second)
			defer adapter.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			reference := adapterReference(int64(index+1), 1, 1)
			result, err := adapter.Start(ctx, harnessadapter.StartInput{
				Attempt: reference, Prompt: "unexpected-item:" + itemType, Context: adapterBoundary(1), Policy: adapterPolicy(),
			})
			if err != nil || result.Outcome != harnessadapter.StartStarted {
				t.Fatalf("start = %#v, %v", result, err)
			}
			events := readAdapterEvents(t, ctx, adapter, reference)
			unknown, ok := events[len(events)-1].(harnessadapter.UnknownEvent)
			if !ok || unknown.EffectStatus != "unknown" {
				t.Fatalf("fail-closed event = %#v", events[len(events)-1])
			}
			reconciled, err := adapter.Reconcile(ctx, harnessadapter.ReconcileInput{Attempt: reference})
			if err != nil || reconciled.Outcome != harnessadapter.ReconcileUnknown || reconciled.EffectStatus != "unknown" {
				t.Fatalf("fail-closed reconcile = %#v, %v", reconciled, err)
			}
		})
	}
}

func TestAdapterLostTurnAcknowledgementStaysDurablyUnknownWithoutRetry(t *testing.T) {
	adapter := newTestAdapter(t, 2*time.Second)
	adapter.config.OperationTimeout = 50 * time.Millisecond
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reference := adapterReference(1, 1, 1)
	result, err := adapter.Start(ctx, harnessadapter.StartInput{
		Attempt: reference, Prompt: "lose-turn-ack", Context: adapterBoundary(1), Policy: adapterPolicy(),
	})
	if err == nil || result.Outcome != harnessadapter.StartUnknown {
		t.Fatalf("lost ACK = %#v, %v", result, err)
	}
	mapping, ok := adapter.store.attempt(reference)
	if !ok || mapping.State != "turn_dispatching" || mapping.ThreadID == "" || mapping.TurnID != "" {
		t.Fatalf("durable lost ACK mapping = %#v, %v", mapping, ok)
	}
	result, err = adapter.Start(ctx, harnessadapter.StartInput{
		Attempt: reference, Prompt: "lose-turn-ack", Context: adapterBoundary(1), Policy: adapterPolicy(),
	})
	if err != nil || result.Outcome != harnessadapter.StartUnknown {
		t.Fatalf("duplicate dispatch = %#v, %v", result, err)
	}
}

func TestAdapterDoesNotStartTurnUntilNativeFeaturePolicyIsConfirmed(t *testing.T) {
	config := testAdapterConfig(t, 2*time.Second)
	config.Environment = append(config.Environment, "CODEX_FEATURE_SHELL_ENABLED=1")
	adapter, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	reference := adapterReference(1, 1, 1)
	result, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "must-not-run", Context: adapterBoundary(1), Policy: adapterPolicy(),
	})
	if err == nil || result.Outcome != harnessadapter.StartUnknown {
		t.Fatalf("unconfirmed policy = %#v, %v", result, err)
	}
	mapping, ok := adapter.store.attempt(reference)
	if !ok || mapping.State != "thread_acknowledged" || mapping.ThreadID == "" || mapping.TurnID != "" {
		t.Fatalf("policy fence mapping = %#v, %v", mapping, ok)
	}
}

func TestAdapterDoesNotStartTurnWhenNativeMCPServerIsPresent(t *testing.T) {
	config := testAdapterConfig(t, 2*time.Second)
	config.Environment = append(config.Environment, "CODEX_MCP_ENABLED=1")
	adapter, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	reference := adapterReference(1, 1, 1)
	result, err := adapter.Start(context.Background(), harnessadapter.StartInput{
		Attempt: reference, Prompt: "must-not-run", Context: adapterBoundary(1), Policy: adapterPolicy(),
	})
	if err == nil || result.Outcome != harnessadapter.StartUnknown {
		t.Fatalf("unconfirmed MCP isolation = %#v, %v", result, err)
	}
	mapping, ok := adapter.store.attempt(reference)
	if !ok || mapping.State != "thread_acknowledged" || mapping.ThreadID == "" || mapping.TurnID != "" {
		t.Fatalf("MCP isolation fence mapping = %#v, %v", mapping, ok)
	}
}

func TestAdapterRejectsNonEmptyToolsAndProviderKeyEnvironment(t *testing.T) {
	policy := adapterPolicy()
	policy.ToolManifest = []byte(`[{"name":"unsafe"}]`)
	digest := sha256.Sum256(policy.ToolManifest)
	policy.ToolManifestHash = hex.EncodeToString(digest[:])
	policy.EffectiveHash = harnessadapter.EffectivePolicyHash(policy)
	if _, failure := validateDispatch(adapterReference(1, 1, 1), "prompt", adapterBoundary(1), policy); failure == nil || failure.Class != harnessadapter.FailurePolicy {
		t.Fatalf("tool manifest failure = %#v", failure)
	}
	config := testAdapterConfig(t, time.Second)
	config.Environment = append(config.Environment, "OPENAI_API_KEY=forbidden")
	if adapter, err := New(config, nil); err == nil {
		_ = adapter.Close()
		t.Fatal("provider key environment was accepted")
	}
}

func assertStartAndTerminal(t *testing.T, ctx context.Context, adapter *Adapter, reference harnessadapter.AttemptRef, boundary harnessadapter.ContextBoundary, prompt string) {
	t.Helper()
	result, err := adapter.Start(ctx, harnessadapter.StartInput{Attempt: reference, Prompt: prompt, Context: boundary, Policy: adapterPolicy()})
	if err != nil || result.Outcome != harnessadapter.StartStarted {
		t.Fatalf("start = %#v, %v", result, err)
	}
	assertTerminalEvents(t, ctx, adapter, reference)
}

func assertTerminalEvents(t *testing.T, ctx context.Context, adapter *Adapter, reference harnessadapter.AttemptRef) {
	t.Helper()
	events := readAdapterEvents(t, ctx, adapter, reference)
	if len(events) < 4 {
		t.Fatalf("events = %#v", events)
	}
	if _, ok := events[0].(harnessadapter.StartedEvent); !ok {
		t.Fatalf("first event = %T", events[0])
	}
	if _, ok := events[1].(harnessadapter.AssistantDeltaEvent); !ok {
		t.Fatalf("second event = %T", events[1])
	}
	if message, ok := events[2].(harnessadapter.AssistantMessageEvent); !ok || message.Content.Content == "" {
		t.Fatalf("assistant message = %#v", events[2])
	}
	if terminal, ok := events[len(events)-1].(harnessadapter.TerminalEvent); !ok || terminal.Outcome != harnessadapter.ReconcileCompleted || terminal.Usage == nil || terminal.Usage.Source != "per_attempt" || terminal.Usage.TotalTokens >= 100 {
		t.Fatalf("terminal = %#v", events[len(events)-1])
	}
}

func readAdapterEvents(t *testing.T, ctx context.Context, adapter *Adapter, reference harnessadapter.AttemptRef) []harnessadapter.Event {
	t.Helper()
	stream, err := adapter.Events(ctx, harnessadapter.EventsInput{Attempt: reference})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var events []harnessadapter.Event
	for {
		event, err := stream.Next(ctx)
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
}

func newTestAdapter(t *testing.T, timeout time.Duration) *Adapter {
	t.Helper()
	adapter, err := New(testAdapterConfig(t, timeout), nil)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := adapter.Identity(context.Background())
	if err != nil || harnessadapter.ValidateIdentity(identity) != nil || identity.Kind != harnessadapter.KindCodex {
		_ = adapter.Close()
		t.Fatalf("identity = %#v, %v", identity, err)
	}
	return adapter
}

func testAdapterConfig(t *testing.T, timeout time.Duration) Config {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		Executable: executable, Arguments: []string{"-test.run=^TestCodexAdapterHelperProcess$"},
		VersionArguments: []string{"-test.run=^TestCodexVersionHelperProcess$"},
		Environment: []string{
			adapterHelperEnvironment, versionHelperEnvironment, "CODEX_VERSION_OUTPUT=codex-cli " + codexAppServerVersion,
			"HOME=/private/tmp/codex-adapter-home", "CODEX_HOME=/private/tmp/codex-adapter-fixture",
		},
		StateDir: filepath.Join(t.TempDir(), "mapping"), WorkingDir: t.TempDir(), Model: "fixture-model", Effort: defaultEffort,
		OperationTimeout: timeout, MaxFrameBytes: 64 << 10,
	}
}

func adapterPolicy() harnessadapter.PolicySnapshot {
	policy := harnessadapter.PolicySnapshot{
		Revision: "fixture-policy@1", Content: []byte("fixture policy"), ToolManifest: []byte("[]"), ApprovalMode: harnessadapter.ApprovalModeDeny,
	}
	contentHash, manifestHash := sha256.Sum256(policy.Content), sha256.Sum256(policy.ToolManifest)
	policy.ContentHash = hex.EncodeToString(contentHash[:])
	policy.ToolManifestHash = hex.EncodeToString(manifestHash[:])
	policy.EffectiveHash = harnessadapter.EffectivePolicyHash(policy)
	return policy
}

func adapterReference(dialog, request, generation int64) harnessadapter.AttemptRef {
	return harnessadapter.AttemptRef{
		NodeID:    "10000000-0000-4000-8000-000000000001",
		DialogID:  fmt.Sprintf("20000000-0000-4000-8000-%012d", dialog),
		RequestID: fmt.Sprintf("30000000-0000-4000-8000-%012d", request),
		AttemptID: fmt.Sprintf("40000000-0000-4000-8000-%012d", request), Generation: generation,
	}
}

func adapterBoundary(sequence int64) harnessadapter.ContextBoundary {
	return harnessadapter.ContextBoundary{MessageID: fmt.Sprintf("50000000-0000-4000-8000-%012d", sequence), Sequence: sequence}
}

func TestCodexAdapterHelperProcess(t *testing.T) {
	if os.Getenv("CODEX_ADAPTER_HELPER") != "1" {
		return
	}
	os.Exit(runAdapterHelper())
}

func runAdapterHelper() int {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	threads := 0
	turns := 0
	activeThread := ""
	activeTurn := ""
	pendingInput := ""
	for scanner.Scan() {
		var frame rpcFrame
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			return 2
		}
		if frame.Method == "" && len(frame.ID) > 0 {
			if pendingInput == "" {
				return 3
			}
			var response struct {
				Answers map[string]struct {
					Answers []string `json:"answers"`
				} `json:"answers"`
			}
			if json.Unmarshal(frame.Result, &response) != nil || len(response.Answers["question-1"].Answers) != 1 {
				return 4
			}
			answer := response.Answers["question-1"].Answers[0]
			mode := pendingInput
			pendingInput = ""
			if mode == "resolve" {
				emitResolved(encoder, activeThread, frame.ID)
				emitAssistant(encoder, activeThread, activeTurn, "answer:"+answer)
				emitTerminal(encoder, activeThread, activeTurn, "completed")
			} else if mode == "cleanup" {
				emitResolved(encoder, activeThread, frame.ID)
				emitTerminal(encoder, activeThread, activeTurn, "completed")
			}
			continue
		}
		if len(frame.ID) == 0 {
			if frame.Method != "initialized" {
				return 5
			}
			continue
		}
		switch frame.Method {
		case "initialize":
			_ = encoder.Encode(map[string]any{"id": frame.ID, "result": initializeResponse{
				UserAgent: validSessionUserAgent(), CodexHome: "/private/tmp/codex-adapter-fixture", PlatformFamily: "unix", PlatformOS: "test",
			}})
		case "thread/start":
			var params nativeThreadOptions
			if json.Unmarshal(frame.Params, &params) != nil || !validHelperPolicy(params) || params.ThreadID != "" {
				return 6
			}
			threads++
			activeThread = fmt.Sprintf("thread-%d", threads)
			emitThreadResponse(encoder, frame.ID, activeThread)
		case "thread/resume":
			var params nativeThreadOptions
			if json.Unmarshal(frame.Params, &params) != nil || !validHelperPolicy(params) || params.ThreadID == "" {
				return 7
			}
			activeThread = params.ThreadID
			emitThreadResponse(encoder, frame.ID, activeThread)
		case "experimentalFeature/list":
			var params struct {
				ThreadID string `json:"threadId"`
				Limit    int    `json:"limit"`
			}
			if json.Unmarshal(frame.Params, &params) != nil || params.ThreadID != activeThread || params.Limit != 100 {
				return 12
			}
			features := make([]map[string]any, 0, len(deniedNativeFeatures))
			for _, name := range deniedNativeFeatures {
				features = append(features, map[string]any{"name": name, "enabled": name == "shell_tool" && os.Getenv("CODEX_FEATURE_SHELL_ENABLED") == "1"})
			}
			_ = encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]any{"data": features, "nextCursor": nil}})
		case "mcpServerStatus/list":
			var params struct {
				ThreadID string `json:"threadId"`
				Limit    int    `json:"limit"`
				Detail   string `json:"detail"`
			}
			if json.Unmarshal(frame.Params, &params) != nil || params.ThreadID != activeThread || params.Limit != 100 || params.Detail != "toolsAndAuthOnly" {
				return 13
			}
			servers := []map[string]any{}
			if os.Getenv("CODEX_MCP_ENABLED") == "1" {
				servers = append(servers, map[string]any{"name": "unexpected"})
			}
			_ = encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]any{"data": servers, "nextCursor": nil}})
		case "turn/start":
			var params nativeTurnParams
			if json.Unmarshal(frame.Params, &params) != nil || params.ThreadID != activeThread || len(params.Input) != 1 || params.Input[0].Type != "text" || params.ClientUserMessageID == "" || params.ApprovalPolicy != "never" || params.SandboxPolicy.Type != "readOnly" || params.SandboxPolicy.NetworkAccess {
				return 8
			}
			turns++
			activeTurn = fmt.Sprintf("turn-%d", turns)
			_ = encoder.Encode(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": activeThread, "turn": map[string]string{"id": activeTurn, "status": "inProgress"}}})
			if params.Input[0].Text == "lose-turn-ack" {
				continue
			}
			if params.Input[0].Text == "ask-input" || params.Input[0].Text == "ask-input-cleanup" || params.Input[0].Text == "ask-input-resolved" {
				pendingInput = map[string]string{"ask-input": "resolve", "ask-input-cleanup": "cleanup", "ask-input-resolved": "resolved"}[params.Input[0].Text]
				_ = encoder.Encode(map[string]any{"id": "input-request-1", "method": "item/tool/requestUserInput", "params": map[string]any{
					"threadId": activeThread, "turnId": activeTurn, "itemId": "input-item-1", "isBlocking": true,
					"questions": []map[string]any{{"id": "question-1", "header": "Choice", "question": "Choose one value", "isSecret": false}},
				}})
				if pendingInput == "resolved" {
					emitResolved(encoder, activeThread, json.RawMessage(`"input-request-1"`))
					pendingInput = ""
				}
			}
			_ = encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]any{"turn": map[string]string{"id": activeTurn, "status": "inProgress"}}})
			switch params.Input[0].Text {
			case "error-no-retry":
				emitError(encoder, activeThread, activeTurn, false)
			case "error-retry":
				emitError(encoder, activeThread, activeTurn, true)
				emitAssistant(encoder, activeThread, activeTurn, "answer:retry")
				emitUsage(encoder, activeThread, activeTurn, turns)
				emitTerminal(encoder, activeThread, activeTurn, "completed")
			default:
				if itemType, ok := strings.CutPrefix(params.Input[0].Text, "unexpected-item:"); ok {
					emitUnexpectedItem(encoder, activeThread, activeTurn, itemType)
					emitTerminal(encoder, activeThread, activeTurn, "completed")
				} else if params.Input[0].Text != "hold" && !strings.HasPrefix(params.Input[0].Text, "ask-input") {
					emitAssistant(encoder, activeThread, activeTurn, "answer:"+params.Input[0].Text)
					emitUsage(encoder, activeThread, activeTurn, turns)
					emitTerminal(encoder, activeThread, activeTurn, "completed")
				}
			}
		case "turn/steer":
			var params struct {
				ThreadID       string `json:"threadId"`
				ExpectedTurnID string `json:"expectedTurnId"`
			}
			if json.Unmarshal(frame.Params, &params) != nil || params.ThreadID != activeThread || params.ExpectedTurnID != activeTurn {
				return 9
			}
			_ = encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]string{"turnId": activeTurn}})
		case "turn/interrupt":
			var params struct{ ThreadID, TurnID string }
			if json.Unmarshal(frame.Params, &params) != nil || params.ThreadID != activeThread || params.TurnID != activeTurn {
				return 10
			}
			_ = encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]any{}})
			time.Sleep(150 * time.Millisecond)
			emitTerminal(encoder, activeThread, activeTurn, "interrupted")
		default:
			_ = encoder.Encode(map[string]any{"id": frame.ID, "error": map[string]any{"code": -32601, "message": "unsupported"}})
		}
	}
	if scanner.Err() != nil {
		return 11
	}
	return 0
}

func validHelperPolicy(params nativeThreadOptions) bool {
	features, ok := params.Config["features"].(map[string]any)
	mcpServers, mcpOK := params.Config["mcp_servers"].(map[string]any)
	if params.Model != "fixture-model" || params.CWD == "" || params.ApprovalPolicy != "never" || params.Sandbox != "read-only" || params.DeveloperInstructions != "fixture policy" || !ok || len(features) != len(deniedNativeFeatures) || !mcpOK || len(mcpServers) != 0 {
		return false
	}
	for _, name := range deniedNativeFeatures {
		if features[name] != false {
			return false
		}
	}
	return true
}

func emitThreadResponse(encoder *json.Encoder, id json.RawMessage, threadID string) {
	_ = encoder.Encode(map[string]any{"id": id, "result": map[string]any{
		"thread": map[string]string{"id": threadID}, "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false},
	}})
}

func emitAssistant(encoder *json.Encoder, threadID, turnID, text string) {
	itemID := "message-" + turnID
	_ = encoder.Encode(map[string]any{"method": "item/agentMessage/delta", "params": map[string]string{"threadId": threadID, "turnId": turnID, "itemId": itemID, "delta": text}})
	_ = encoder.Encode(map[string]any{"method": "item/completed", "params": map[string]any{
		"threadId": threadID, "turnId": turnID, "completedAtMs": 1, "item": map[string]string{"id": itemID, "type": "agentMessage", "text": text},
	}})
}

func emitTerminal(encoder *json.Encoder, threadID, turnID, status string) {
	_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": status, "items": []any{}}}})
}

func emitResolved(encoder *json.Encoder, threadID string, requestID json.RawMessage) {
	_ = encoder.Encode(map[string]any{"method": "serverRequest/resolved", "params": map[string]any{"threadId": threadID, "requestId": requestID}})
}

func emitError(encoder *json.Encoder, threadID, turnID string, willRetry bool) {
	_ = encoder.Encode(map[string]any{"method": "error", "params": map[string]any{
		"threadId": threadID, "turnId": turnID, "willRetry": willRetry, "error": map[string]string{"message": "fixture failure"},
	}})
}

func emitUsage(encoder *json.Encoder, threadID, turnID string, turns int) {
	_ = encoder.Encode(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{
		"threadId": threadID, "turnId": turnID, "tokenUsage": map[string]any{
			"last":  map[string]int{"inputTokens": turns, "outputTokens": turns, "totalTokens": turns * 2},
			"total": map[string]int{"inputTokens": 100 + turns, "outputTokens": 100 + turns, "totalTokens": 200 + turns*2},
		},
	}})
}

func emitUnexpectedItem(encoder *json.Encoder, threadID, turnID, itemType string) {
	item := map[string]string{"id": "unexpected-" + turnID, "type": itemType, "status": "completed"}
	if itemType == "mcpToolCall" {
		item["server"], item["tool"] = "fixture", "tool"
	}
	if itemType == "dynamicToolCall" {
		item["tool"] = "fixture"
	}
	_ = encoder.Encode(map[string]any{"method": "item/started", "params": map[string]any{"threadId": threadID, "turnId": turnID, "item": item}})
}
