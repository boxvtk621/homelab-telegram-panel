// Package codex implements the private Codex app-server adapter for Harness.
package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/boxvtk621/homelab-telegram-panel/harness/node"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessadapter"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

const (
	defaultOperationTimeout = 30 * time.Second
	defaultMaximumFrame     = 8 << 20
	defaultEffort           = "medium"
	maximumFeaturePages     = 10
)

// codexAppServerVersion is pinned, so this list is an exact deny fence for the
// model-facing native surfaces present in that binary. The empty Harness tool
// manifest is not assumed to be a native Codex allowlist.
var deniedNativeFeatures = []string{
	"apps",
	"artifact",
	"auth_elicitation",
	"browser_use",
	"browser_use_external",
	"browser_use_full_cdp_access",
	"code_mode",
	"code_mode_host",
	"computer_use",
	"deferred_executor",
	"enable_mcp_apps",
	"exec_permission_approvals",
	"external_agent_memory_import",
	"goals",
	"guardian_approval",
	"hooks",
	"image_generation",
	"in_app_browser",
	"in_app_local_automation",
	"memories",
	"multi_agent",
	"multi_agent_v2",
	"network_proxy",
	"plugins",
	"psp",
	"realtime_conversation",
	"remote_plugin",
	"request_permissions_tool",
	"shell_snapshot",
	"shell_snapshot_v2",
	"shell_tool",
	"skill_mcp_dependency_install",
	"skill_search",
	"sleep_tool",
	"standalone_web_search",
	"tool_call_mcp_elicitation",
	"tool_suggest",
	"view_image",
	"workspace_dependencies",
	"write_stdin_approval",
}

// Config contains only private app-server process settings. Authentication is
// owned by the dedicated CODEX_HOME supplied in Environment; no API token is
// accepted by this adapter.
type Config struct {
	Executable       string
	Arguments        []string
	VersionArguments []string
	Environment      []string
	StateDir         string
	WorkingDir       string
	Model            string
	Effort           string
	OperationTimeout time.Duration
	MaxFrameBytes    int
}

type nativeAttempt struct {
	mu        sync.Mutex
	runtime   *attemptRuntime
	reference harnessadapter.AttemptRef
	threadID  string
	turnID    string
	deltas    map[string]int64
	tools     map[string]nativeTool
	output    *harnessprotocol.SafeContent
	usage     *harnessprotocol.Usage
}

type nativeTool struct {
	callID  string
	started bool
	done    bool
}

type pendingInput struct {
	id         rpcID
	attempt    *nativeAttempt
	questionID string
	result     chan bool
	responding bool
	resolved   bool
}

// Adapter owns all native thread, turn, item and request identifiers.
type Adapter struct {
	config    Config
	artifacts node.ArtifactSink
	store     *mappingStore
	session   *nativeSession

	dispatchMu sync.Mutex
	mu         sync.Mutex
	attempts   map[string]*nativeAttempt
	byThread   map[string]*nativeAttempt
	byTurn     map[string]*nativeAttempt
	inputs     map[string]*pendingInput
	inputByRPC map[string]string
	closed     bool
}

var _ harnessadapter.Adapter = (*Adapter)(nil)

func New(config Config, artifacts node.ArtifactSink) (*Adapter, error) {
	home, homeOK := exactEnvironmentPath(config.Environment, "HOME")
	codexHome, codexHomeOK := exactEnvironmentPath(config.Environment, "CODEX_HOME")
	if config.Executable == "" || strings.ContainsAny(config.Executable, "\x00\r\n") ||
		config.StateDir == "" || !filepath.IsAbs(config.StateDir) || config.WorkingDir == "" || !filepath.IsAbs(config.WorkingDir) ||
		!boundedText(config.Model) || !validEffort(config.Effort) || invalidEnvironment(config.Environment) ||
		!homeOK || !codexHomeOK || home == codexHome {
		return nil, errors.New("codex adapter config is incomplete")
	}
	if len(config.Arguments) == 0 {
		config.Arguments = []string{"app-server", "--listen", "stdio://"}
	}
	if config.OperationTimeout == 0 {
		config.OperationTimeout = defaultOperationTimeout
	}
	if config.OperationTimeout < 0 {
		return nil, errors.New("codex operation timeout is invalid")
	}
	if config.MaxFrameBytes == 0 {
		config.MaxFrameBytes = defaultMaximumFrame
	}
	if config.MaxFrameBytes < 4096 || config.MaxFrameBytes > harnessprotocol.MaximumWireBytes {
		return nil, errors.New("codex app-server frame limit is invalid")
	}
	store, err := openMappingStore(config.StateDir)
	if err != nil {
		return nil, err
	}
	adapter := &Adapter{
		config: config, artifacts: artifacts, store: store,
		attempts: make(map[string]*nativeAttempt), byThread: make(map[string]*nativeAttempt),
		byTurn: make(map[string]*nativeAttempt), inputs: make(map[string]*pendingInput), inputByRPC: make(map[string]string),
	}
	ctx, cancel := context.WithTimeout(context.Background(), config.OperationTimeout)
	defer cancel()
	session, err := startNativeSession(ctx, bridgeConfig{
		Executable: config.Executable, Arguments: append([]string(nil), config.Arguments...),
		VersionArguments: append([]string(nil), config.VersionArguments...), Environment: append([]string(nil), config.Environment...),
		WorkingDir: config.WorkingDir, MaxFrameBytes: config.MaxFrameBytes,
	}, store, sessionHandlers{
		Notification: adapter.handleNotification, Request: adapter.handleRequest, Exit: adapter.handleExit,
		Ready: func(session *nativeSession) { adapter.session = session },
	})
	if err != nil {
		return nil, err
	}
	adapter.session = session
	return adapter, nil
}

func (adapter *Adapter) Identity(context.Context) (harnessadapter.Identity, error) {
	declared := make(map[harnessadapter.Capability]bool)
	verified := make(map[harnessadapter.Capability]bool)
	for _, capability := range []harnessadapter.Capability{
		harnessadapter.CapabilityChat, harnessadapter.CapabilityEvents,
		harnessadapter.CapabilityToolResults, harnessadapter.CapabilityCancel,
		harnessadapter.CapabilitySteerAttached, harnessadapter.CapabilitySessionResume,
		harnessadapter.CapabilityPolicyEnforcement,
	} {
		declared[capability] = true
		verified[capability] = true
	}
	return harnessadapter.Identity{
		Kind: harnessadapter.KindCodex, Version: harnessadapter.CodexAppServerVersion,
		ProtocolVersion: harnessprotocol.ProtocolVersion, SchemaID: harnessprotocol.SchemaID,
		SchemaSHA256: harnessprotocol.SchemaSHA256, Declared: declared, Verified: verified,
	}, nil
}

func (adapter *Adapter) Start(ctx context.Context, input harnessadapter.StartInput) (harnessadapter.StartResult, error) {
	policy, failure := validateDispatch(input.Attempt, input.Prompt, input.Context, input.Policy)
	if failure != nil {
		return harnessadapter.StartResult{Outcome: harnessadapter.StartRejected, Failure: failure}, nil
	}
	if _, attempted := adapter.store.attempt(input.Attempt); attempted {
		failure, err := adapter.dispatch(ctx, "start", input.Attempt, input.Prompt, input.Context, policy, "")
		if err != nil || failure != nil {
			if failure == nil {
				failure = nodeFailure("codex_dispatch_unknown", "codex dispatch acknowledgement is unknown", true)
			}
			return harnessadapter.StartResult{Outcome: harnessadapter.StartUnknown, Failure: failure}, err
		}
		return harnessadapter.StartResult{Outcome: harnessadapter.StartStarted}, nil
	}
	if _, exists := adapter.store.dialog(input.Attempt.DialogID); exists {
		return harnessadapter.StartResult{Outcome: harnessadapter.StartRejected, Failure: taskFailure("codex_dialog_exists", "codex dialog already has native context")}, nil
	}
	failure, err := adapter.dispatch(ctx, "start", input.Attempt, input.Prompt, input.Context, policy, "")
	if err != nil || failure != nil {
		if failure == nil {
			failure = nodeFailure("codex_dispatch_unknown", "codex dispatch acknowledgement is unknown", true)
		}
		return harnessadapter.StartResult{Outcome: harnessadapter.StartUnknown, Failure: failure}, err
	}
	return harnessadapter.StartResult{Outcome: harnessadapter.StartStarted}, nil
}

func (adapter *Adapter) Resume(ctx context.Context, input harnessadapter.ResumeInput) (harnessadapter.ResumeResult, error) {
	policy, failure := validateDispatch(input.Attempt, input.Prompt, input.Context, input.Policy)
	if failure != nil {
		return harnessadapter.ResumeResult{Outcome: harnessadapter.ResumeRejected, Failure: failure}, nil
	}
	if _, attempted := adapter.store.attempt(input.Attempt); attempted {
		failure, err := adapter.dispatch(ctx, "resume", input.Attempt, input.Prompt, input.Context, policy, "")
		if err != nil || failure != nil {
			if failure == nil {
				failure = nodeFailure("codex_dispatch_unknown", "codex dispatch acknowledgement is unknown", true)
			}
			return harnessadapter.ResumeResult{Outcome: harnessadapter.ResumeUnknown, Failure: failure}, err
		}
		return harnessadapter.ResumeResult{Outcome: harnessadapter.ResumeStarted}, nil
	}
	dialog, exists := adapter.store.dialog(input.Attempt.DialogID)
	if !exists || !boundedNativeID(dialog.ThreadID) || input.Context.Sequence <= dialog.Boundary.Sequence {
		return harnessadapter.ResumeResult{Outcome: harnessadapter.ResumeContextMissing, Failure: taskFailure("codex_context_missing", "codex dialog context is unavailable")}, nil
	}
	failure, err := adapter.dispatch(ctx, "resume", input.Attempt, input.Prompt, input.Context, policy, dialog.ThreadID)
	if err != nil || failure != nil {
		if failure == nil {
			failure = nodeFailure("codex_dispatch_unknown", "codex dispatch acknowledgement is unknown", true)
		}
		return harnessadapter.ResumeResult{Outcome: harnessadapter.ResumeUnknown, Failure: failure}, err
	}
	return harnessadapter.ResumeResult{Outcome: harnessadapter.ResumeStarted}, nil
}

func (adapter *Adapter) dispatch(ctx context.Context, kind string, reference harnessadapter.AttemptRef, prompt string, boundary harnessadapter.ContextBoundary, policy harnessadapter.PolicySnapshot, resumeThreadID string) (*harnessadapter.Failure, error) {
	adapter.dispatchMu.Lock()
	defer adapter.dispatchMu.Unlock()
	if previous, exists := adapter.store.attempt(reference); exists {
		if previous.DispatchKind != kind || previous.Context != boundary || previous.PolicyHash != policy.EffectiveHash || previous.PromptHash != digestString(prompt) {
			return protocolFailure("codex_dispatch_conflict", "codex dispatch does not match the durable attempt"), nil
		}
		adapter.mu.Lock()
		live := adapter.attempts[attemptKey(reference)]
		adapter.mu.Unlock()
		if previous.State == "active" && live != nil && live.runtime.reconcile().Outcome == harnessadapter.ReconcileRunning {
			return nil, nil
		}
		return nodeFailure("codex_dispatch_unknown", "codex dispatch was previously attempted", true), nil
	}
	if err := adapter.store.putIntent(kind, reference, boundary, policy.EffectiveHash, digestString(prompt), resumeThreadID); err != nil {
		return nil, err
	}
	native := &nativeAttempt{runtime: newAttemptRuntime(reference), reference: reference, deltas: make(map[string]int64), tools: make(map[string]nativeTool)}
	key := attemptKey(reference)
	adapter.mu.Lock()
	if adapter.closed {
		adapter.mu.Unlock()
		return nil, errors.New("codex adapter is closed")
	}
	adapter.attempts[key] = native
	adapter.mu.Unlock()

	options := adapter.threadOptions(policy)
	var threadResponse nativeThreadResponse
	operationCtx, cancel := adapter.operationContext(ctx)
	defer cancel()
	if kind == "start" {
		if err := adapter.session.Call(operationCtx, "thread/start", options, &threadResponse); err != nil {
			native.runtime.failUnknown("dispatch_uncertain")
			return nil, err
		}
	} else {
		params := options
		params.ThreadID = resumeThreadID
		if err := adapter.session.Call(operationCtx, "thread/resume", params, &threadResponse); err != nil {
			native.runtime.failUnknown("dispatch_uncertain")
			return nil, err
		}
	}
	threadID, err := validateThreadResponse(threadResponse, resumeThreadID)
	if err != nil {
		native.runtime.failUnknown("adapter_protocol")
		return nil, err
	}
	if err := adapter.store.acknowledgeThread(reference, threadID, adapter.session.ProcessGeneration()); err != nil {
		native.runtime.failUnknown("dispatch_uncertain")
		return nil, err
	}
	adapter.bindThread(native, threadID)
	if err := adapter.verifyThreadFeatures(operationCtx, threadID); err != nil {
		native.runtime.failUnknown("policy_unconfirmed")
		return nil, err
	}
	if err := adapter.verifyNoMCPServers(operationCtx, threadID); err != nil {
		native.runtime.failUnknown("policy_unconfirmed")
		return nil, err
	}
	if err := adapter.store.beginTurn(reference); err != nil {
		native.runtime.failUnknown("dispatch_uncertain")
		return nil, err
	}
	var turnResponse nativeTurnResponse
	if err := adapter.session.Call(operationCtx, "turn/start", nativeTurnParams{
		ThreadID: threadID, Input: []nativeUserInput{{Type: "text", Text: prompt}}, ClientUserMessageID: boundary.MessageID,
		ApprovalPolicy: "never", SandboxPolicy: nativeSandboxPolicy{Type: "readOnly", NetworkAccess: false},
		Model: adapter.config.Model, Effort: adapter.config.Effort,
	}, &turnResponse); err != nil {
		native.runtime.failUnknown("dispatch_uncertain")
		return nil, err
	}
	turnID := turnResponse.Turn.ID
	if !boundedNativeID(turnID) || turnResponse.Turn.Status != "inProgress" || !adapter.bindTurn(native, turnID) {
		native.runtime.failUnknown("adapter_protocol")
		return nil, errors.New("codex turn acknowledgement is invalid")
	}
	if err := adapter.store.activate(reference, turnID, adapter.session.ProcessGeneration()); err != nil {
		native.runtime.failUnknown("dispatch_uncertain")
		return nil, err
	}
	native.runtime.activate()
	if native.runtime.reconcile().Outcome != harnessadapter.ReconcileRunning {
		_ = adapter.store.terminal(reference)
	}
	return nil, nil
}

type nativeFeature struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type nativeFeaturePage struct {
	Data       []nativeFeature `json:"data"`
	NextCursor *string         `json:"nextCursor"`
}

type nativeMCPPage struct {
	Data       []json.RawMessage `json:"data"`
	NextCursor *string           `json:"nextCursor"`
}

func (adapter *Adapter) verifyThreadFeatures(ctx context.Context, threadID string) error {
	wanted := deniedFeatureOverrides()
	observed := make(map[string]bool, len(wanted))
	seenCursors := make(map[string]bool)
	var cursor string
	for page := 0; page < maximumFeaturePages; page++ {
		params := map[string]any{"threadId": threadID, "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var response nativeFeaturePage
		if err := adapter.session.Call(ctx, "experimentalFeature/list", params, &response); err != nil {
			return fmt.Errorf("verify codex thread policy: %w", err)
		}
		if response.Data == nil {
			return errors.New("codex feature policy response is invalid")
		}
		for _, feature := range response.Data {
			if _, required := wanted[feature.Name]; !required {
				continue
			}
			if previous, exists := observed[feature.Name]; exists && previous != feature.Enabled {
				return errors.New("codex feature policy is conflicting")
			}
			observed[feature.Name] = feature.Enabled
		}
		if response.NextCursor == nil {
			if len(observed) != len(wanted) {
				return errors.New("codex feature policy is incomplete")
			}
			for name := range wanted {
				if observed[name] {
					return errors.New("codex feature policy is not enforced")
				}
			}
			if len(wanted) == 0 {
				return errors.New("codex feature policy is not enforced")
			}
			return nil
		}
		cursor = *response.NextCursor
		if !boundedSessionText(cursor, 4096) || seenCursors[cursor] {
			return errors.New("codex feature policy cursor is invalid")
		}
		seenCursors[cursor] = true
	}
	return errors.New("codex feature policy exceeds page limit")
}

func (adapter *Adapter) verifyNoMCPServers(ctx context.Context, threadID string) error {
	seenCursors := make(map[string]bool)
	var cursor string
	for page := 0; page < maximumFeaturePages; page++ {
		params := map[string]any{"threadId": threadID, "limit": 100, "detail": "toolsAndAuthOnly"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var response nativeMCPPage
		if err := adapter.session.Call(ctx, "mcpServerStatus/list", params, &response); err != nil {
			return fmt.Errorf("verify codex MCP isolation: %w", err)
		}
		if response.Data == nil || len(response.Data) != 0 {
			return errors.New("codex MCP isolation is not enforced")
		}
		if response.NextCursor == nil {
			return nil
		}
		cursor = *response.NextCursor
		if !boundedSessionText(cursor, 4096) || seenCursors[cursor] {
			return errors.New("codex MCP status cursor is invalid")
		}
		seenCursors[cursor] = true
	}
	return errors.New("codex MCP status exceeds page limit")
}

func (adapter *Adapter) Events(_ context.Context, input harnessadapter.EventsInput) (harnessadapter.EventStream, error) {
	adapter.mu.Lock()
	native := adapter.attempts[attemptKey(input.Attempt)]
	adapter.mu.Unlock()
	if native == nil {
		return nil, errors.New("codex event stream is unavailable for attempt")
	}
	return native.runtime.claim()
}

func (adapter *Adapter) Steer(ctx context.Context, input harnessadapter.SteerInput) (harnessadapter.SteerResult, error) {
	if !validReference(input.Attempt) || !uuidPattern.MatchString(input.MessageID) || !boundedText(input.Text) {
		return harnessadapter.SteerResult{Outcome: harnessadapter.SteerRejected, Failure: protocolFailure("codex_steer_invalid", "codex steer input is invalid")}, nil
	}
	mapping, native := adapter.active(input.Attempt)
	if native == nil {
		return harnessadapter.SteerResult{Outcome: harnessadapter.SteerUnknown, Failure: nodeFailure("codex_turn_unavailable", "codex turn is unavailable", true)}, nil
	}
	var response struct {
		TurnID string `json:"turnId"`
	}
	operationCtx, cancel := adapter.operationContext(ctx)
	defer cancel()
	err := adapter.session.Call(operationCtx, "turn/steer", map[string]any{
		"threadId": mapping.ThreadID, "expectedTurnId": mapping.TurnID,
		"clientUserMessageId": input.MessageID, "input": []nativeUserInput{{Type: "text", Text: input.Text}},
	}, &response)
	if err != nil || response.TurnID != mapping.TurnID {
		return harnessadapter.SteerResult{Outcome: harnessadapter.SteerUnknown, Failure: nodeFailure("codex_steer_unknown", "codex steer acknowledgement is unknown", true)}, err
	}
	return harnessadapter.SteerResult{Outcome: harnessadapter.SteerApplied}, nil
}

func (adapter *Adapter) Cancel(ctx context.Context, input harnessadapter.CancelInput) (harnessadapter.CancelResult, error) {
	if !validReference(input.Attempt) {
		return harnessadapter.CancelResult{Outcome: harnessadapter.CancelRejected, Failure: protocolFailure("codex_cancel_invalid", "codex cancel input is invalid")}, nil
	}
	mapping, native := adapter.active(input.Attempt)
	if native == nil {
		return harnessadapter.CancelResult{Outcome: harnessadapter.CancelUnknown, Failure: nodeFailure("codex_turn_unavailable", "codex turn is unavailable", true)}, nil
	}
	operationCtx, cancel := adapter.operationContext(ctx)
	defer cancel()
	err := adapter.session.Call(operationCtx, "turn/interrupt", map[string]string{"threadId": mapping.ThreadID, "turnId": mapping.TurnID}, &struct{}{})
	if err != nil {
		return harnessadapter.CancelResult{Outcome: harnessadapter.CancelUnknown, Failure: nodeFailure("codex_cancel_unknown", "codex interrupt acknowledgement is unknown", true)}, err
	}
	return harnessadapter.CancelResult{Outcome: harnessadapter.CancelAcknowledged}, nil
}

func (adapter *Adapter) RespondApproval(context.Context, harnessadapter.RespondApprovalInput) (harnessadapter.ResponseResult, error) {
	return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseRejected, Failure: policyFailure("codex_approval_denied", "codex approvals are disabled by node policy")}, nil
}

func (adapter *Adapter) RespondInput(ctx context.Context, input harnessadapter.RespondInputInput) (harnessadapter.ResponseResult, error) {
	if !validReference(input.Attempt) || !uuidPattern.MatchString(input.InputRequestID) || input.InputVersion != 2 || !boundedText(input.Text) {
		return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseRejected, Failure: protocolFailure("codex_input_invalid", "codex input response is invalid")}, nil
	}
	adapter.mu.Lock()
	pending, ok := adapter.inputs[input.InputRequestID]
	if !ok || pending.attempt.reference != input.Attempt {
		ok = false
	} else if pending.responding {
		adapter.mu.Unlock()
		return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseUnknown, Failure: nodeFailure("codex_input_unknown", "codex input response acknowledgement is unknown", true)}, nil
	} else {
		pending.responding = true
	}
	adapter.mu.Unlock()
	if !ok {
		return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseRejected, Failure: taskFailure("codex_input_stale", "codex input request is no longer pending")}, nil
	}
	err := adapter.session.Respond(pending.id, map[string]any{"answers": map[string]any{
		pending.questionID: map[string]any{"answers": []string{input.Text}},
	}})
	if err != nil {
		adapter.resolvePendingInput(input.InputRequestID, false)
		pending.attempt.runtime.failUnknown("provider_state")
		return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseUnknown, Failure: nodeFailure("codex_input_unknown", "codex input response acknowledgement is unknown", true)}, err
	}
	select {
	case applied := <-pending.result:
		if applied {
			return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseApplied}, nil
		}
		return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseUnknown, Failure: nodeFailure("codex_input_unknown", "codex input request ended before acknowledgement", true)}, nil
	case <-ctx.Done():
		pending.attempt.runtime.failUnknown("provider_state")
		return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseUnknown, Failure: nodeFailure("codex_input_unknown", "codex input response acknowledgement is unknown", true)}, ctx.Err()
	case <-adapter.session.Done():
		pending.attempt.runtime.failUnknown("provider_state")
		return harnessadapter.ResponseResult{Outcome: harnessadapter.ResponseUnknown, Failure: nodeFailure("codex_input_unknown", "codex input response acknowledgement is unknown", true)}, errBridgeClosed
	}
}

func (adapter *Adapter) Reconcile(_ context.Context, input harnessadapter.ReconcileInput) (harnessadapter.ReconcileResult, error) {
	if !validReference(input.Attempt) {
		return harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileUnknown, EffectStatus: "unknown", Failure: protocolFailure("codex_reconcile_invalid", "codex reconcile input is invalid")}, nil
	}
	adapter.mu.Lock()
	native := adapter.attempts[attemptKey(input.Attempt)]
	adapter.mu.Unlock()
	if native != nil {
		return native.runtime.reconcile(), nil
	}
	if _, exists := adapter.store.attempt(input.Attempt); exists {
		return harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileUnknown, EffectStatus: "unknown", Failure: nodeFailure("codex_provider_state_unknown", "codex provider state is unknown after restart", true)}, nil
	}
	return harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileUnknown, EffectStatus: "none", Failure: taskFailure("codex_attempt_missing", "codex attempt mapping is unavailable")}, nil
}

func (adapter *Adapter) Close() error {
	adapter.mu.Lock()
	if adapter.closed {
		adapter.mu.Unlock()
		return nil
	}
	adapter.closed = true
	adapter.mu.Unlock()
	if err := adapter.session.Close(); err != nil {
		return err
	}
	_ = adapter.session.Wait()
	return nil
}

type nativeThreadOptions struct {
	ThreadID              string         `json:"threadId,omitempty"`
	Model                 string         `json:"model"`
	CWD                   string         `json:"cwd"`
	ApprovalPolicy        string         `json:"approvalPolicy"`
	Sandbox               string         `json:"sandbox"`
	DeveloperInstructions string         `json:"developerInstructions"`
	Config                map[string]any `json:"config"`
}

type nativeThreadResponse struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
	ApprovalPolicy json.RawMessage `json:"approvalPolicy"`
	Sandbox        struct {
		Type          string `json:"type"`
		NetworkAccess bool   `json:"networkAccess"`
	} `json:"sandbox"`
}

type nativeUserInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type nativeSandboxPolicy struct {
	Type          string `json:"type"`
	NetworkAccess bool   `json:"networkAccess"`
}

type nativeTurnParams struct {
	ThreadID            string              `json:"threadId"`
	Input               []nativeUserInput   `json:"input"`
	ClientUserMessageID string              `json:"clientUserMessageId"`
	ApprovalPolicy      string              `json:"approvalPolicy"`
	SandboxPolicy       nativeSandboxPolicy `json:"sandboxPolicy"`
	Model               string              `json:"model"`
	Effort              string              `json:"effort"`
}

type nativeTurnResponse struct {
	Turn struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"turn"`
}

func (adapter *Adapter) threadOptions(policy harnessadapter.PolicySnapshot) nativeThreadOptions {
	return nativeThreadOptions{
		Model: adapter.config.Model, CWD: adapter.config.WorkingDir, ApprovalPolicy: "never", Sandbox: "read-only",
		DeveloperInstructions: string(policy.Content),
		Config: map[string]any{
			"features": deniedFeatureOverrides(), "mcp_servers": map[string]any{},
			"model_reasoning_effort": adapter.config.Effort, "web_search": "disabled",
		},
	}
}

func deniedFeatureOverrides() map[string]bool {
	features := make(map[string]bool, len(deniedNativeFeatures))
	for _, name := range deniedNativeFeatures {
		features[name] = false
	}
	return features
}

func validateThreadResponse(response nativeThreadResponse, expectedThreadID string) (string, error) {
	threadID := response.Thread.ID
	var approval string
	if !boundedNativeID(threadID) || (expectedThreadID != "" && threadID != expectedThreadID) || json.Unmarshal(response.ApprovalPolicy, &approval) != nil || approval != "never" || response.Sandbox.Type != "readOnly" || response.Sandbox.NetworkAccess {
		return "", errors.New("codex thread acknowledgement is invalid")
	}
	return threadID, nil
}

func (adapter *Adapter) bindThread(native *nativeAttempt, threadID string) {
	native.mu.Lock()
	native.threadID = threadID
	native.mu.Unlock()
	adapter.mu.Lock()
	adapter.byThread[threadID] = native
	adapter.mu.Unlock()
}

func turnKey(threadID, turnID string) string { return threadID + "\x00" + turnID }

func (adapter *Adapter) bindTurn(native *nativeAttempt, turnID string) bool {
	native.mu.Lock()
	if native.turnID != "" && native.turnID != turnID {
		native.mu.Unlock()
		return false
	}
	native.turnID = turnID
	threadID := native.threadID
	native.mu.Unlock()
	if !boundedNativeID(threadID) || !boundedNativeID(turnID) {
		return false
	}
	adapter.mu.Lock()
	key := turnKey(threadID, turnID)
	if existing := adapter.byTurn[key]; existing != nil && existing != native {
		adapter.mu.Unlock()
		return false
	}
	adapter.byTurn[key] = native
	adapter.mu.Unlock()
	return true
}

func (adapter *Adapter) active(reference harnessadapter.AttemptRef) (persistedAttempt, *nativeAttempt) {
	mapping, exists := adapter.store.attempt(reference)
	if !exists || mapping.State != "active" || !boundedNativeID(mapping.ThreadID) || !boundedNativeID(mapping.TurnID) {
		return persistedAttempt{}, nil
	}
	adapter.mu.Lock()
	native := adapter.attempts[attemptKey(reference)]
	adapter.mu.Unlock()
	if native == nil || native.runtime.reconcile().Outcome != harnessadapter.ReconcileRunning {
		return persistedAttempt{}, nil
	}
	return mapping, native
}

func (adapter *Adapter) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, adapter.config.OperationTimeout)
}

func validateDispatch(reference harnessadapter.AttemptRef, prompt string, boundary harnessadapter.ContextBoundary, policy harnessadapter.PolicySnapshot) (harnessadapter.PolicySnapshot, *harnessadapter.Failure) {
	if !validReference(reference) || !boundedText(prompt) || !validBoundary(boundary) {
		return harnessadapter.PolicySnapshot{}, protocolFailure("codex_dispatch_invalid", "codex dispatch input is invalid")
	}
	prepared, err := harnessadapter.PreparePolicySnapshot(policy)
	if err != nil {
		return harnessadapter.PolicySnapshot{}, protocolFailure("codex_policy_invalid", "codex policy snapshot is invalid")
	}
	var tools []json.RawMessage
	if prepared.ApprovalMode != harnessadapter.ApprovalModeDeny || json.Unmarshal(prepared.ToolManifest, &tools) != nil || tools == nil || len(tools) != 0 {
		return harnessadapter.PolicySnapshot{}, policyFailure("codex_policy_unsupported", "codex node requires deny policy with an empty tool manifest")
	}
	if !boundedText(string(prepared.Content)) || strings.TrimSpace(string(prepared.Content)) == "" {
		return harnessadapter.PolicySnapshot{}, policyFailure("codex_policy_unsupported", "codex developer instructions are unavailable")
	}
	return prepared, nil
}

func invalidEnvironment(environment []string) bool {
	if len(environment) == 0 || len(environment) > 64 {
		return true
	}
	seen := make(map[string]bool, len(environment))
	for _, value := range environment {
		name, _, ok := strings.Cut(value, "=")
		if !ok || name == "" || seen[name] || strings.ContainsAny(value, "\x00\r\n") || name == "OPENAI_API_KEY" || name == "CODEX_API_KEY" || name == "ANTHROPIC_API_KEY" || name == "CURSOR_API_KEY" {
			return true
		}
		seen[name] = true
	}
	return false
}

func validEffort(value string) bool {
	switch value {
	case "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func boundedText(value string) bool {
	return value != "" && utf8.ValidString(value) && len(value) <= harnessprotocol.MaximumMessageBytes
}

func taskFailure(code, message string) *harnessadapter.Failure {
	return &harnessadapter.Failure{Class: harnessadapter.FailureTask, Code: code, SafeMessage: message}
}

func nodeFailure(code, message string, retryable bool) *harnessadapter.Failure {
	return &harnessadapter.Failure{Class: harnessadapter.FailureNode, Code: code, SafeMessage: message, Retryable: retryable}
}

func policyFailure(code, message string) *harnessadapter.Failure {
	return &harnessadapter.Failure{Class: harnessadapter.FailurePolicy, Code: code, SafeMessage: message}
}

func protocolFailure(code, message string) *harnessadapter.Failure {
	return &harnessadapter.Failure{Class: harnessadapter.FailureProtocol, Code: code, SafeMessage: message}
}

func safeInline(value string) harnessprotocol.SafeContent {
	if !utf8.ValidString(value) {
		return unavailableContent("provider_redacted")
	}
	truncated := false
	if len(value) > harnessprotocol.MaximumMessageBytes {
		value = value[:harnessprotocol.MaximumMessageBytes]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
		truncated = true
	}
	return harnessprotocol.SafeContent{Kind: "inline", Content: value, Redaction: "none", Truncated: truncated}
}

func unavailableContent(reason string) harnessprotocol.SafeContent {
	return harnessprotocol.SafeContent{Kind: "unavailable", Reason: reason, Redaction: "unknown"}
}

func finishReason(content harnessprotocol.SafeContent) string {
	if content.Truncated {
		return "length"
	}
	return "complete"
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func derivedUUID(domain, value string) string {
	sum := sha256.Sum256([]byte(domain + "\x00" + value))
	raw := sum[:16]
	raw[6] = raw[6]&0x0f | 0x50
	raw[8] = raw[8]&0x3f | 0x80
	hexValue := hex.EncodeToString(raw)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexValue[:8], hexValue[8:12], hexValue[12:16], hexValue[16:20], hexValue[20:])
}
