package codex

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessadapter"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

type nativeItem struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Text   string `json:"text"`
	Status string `json:"status"`
	Server string `json:"server"`
	Tool   string `json:"tool"`
}

type nativeItemParams struct {
	ThreadID string     `json:"threadId"`
	TurnID   string     `json:"turnId"`
	Item     nativeItem `json:"item"`
}

type nativeTurnNotice struct {
	ThreadID string `json:"threadId"`
	Turn     struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"turn"`
}

type nativeDeltaParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type nativeUsageParams struct {
	ThreadID   string `json:"threadId"`
	TurnID     string `json:"turnId"`
	TokenUsage struct {
		Last  nativeUsage `json:"last"`
		Total nativeUsage `json:"total"`
	} `json:"tokenUsage"`
}

type nativeRequestBase struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
}

type nativeInputQuestion struct {
	ID       string `json:"id"`
	Header   string `json:"header"`
	Question string `json:"question"`
	IsSecret bool   `json:"isSecret"`
}

type nativeInputRequest struct {
	nativeRequestBase
	IsBlocking bool                  `json:"isBlocking"`
	Questions  []nativeInputQuestion `json:"questions"`
}

type nativeRequestResolved struct {
	ThreadID  string          `json:"threadId"`
	RequestID json.RawMessage `json:"requestId"`
}

type nativeErrorNotice struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	WillRetry bool   `json:"willRetry"`
	Error     struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (adapter *Adapter) handleNotification(notification rpcNotification) {
	switch notification.Method {
	case "turn/started":
		var params nativeTurnNotice
		if json.Unmarshal(notification.Params, &params) != nil || !boundedNativeID(params.ThreadID) || !boundedNativeID(params.Turn.ID) || params.Turn.Status != "inProgress" {
			adapter.failActive("adapter_protocol")
			return
		}
		adapter.mu.Lock()
		native := adapter.byThread[params.ThreadID]
		adapter.mu.Unlock()
		if native != nil && !adapter.bindTurn(native, params.Turn.ID) {
			native.runtime.failUnknown("adapter_protocol")
		}
	case "item/agentMessage/delta":
		var params nativeDeltaParams
		if json.Unmarshal(notification.Params, &params) != nil || !boundedNativeID(params.ItemID) || !utf8.ValidString(params.Delta) {
			adapter.failActive("adapter_protocol")
			return
		}
		native := adapter.nativeFor(params.ThreadID, params.TurnID)
		if native == nil {
			return
		}
		adapter.confirmAttemptInputs(native)
		native.mu.Lock()
		index := native.deltas[params.ItemID]
		native.deltas[params.ItemID] = index + 1
		native.mu.Unlock()
		native.runtime.push(harnessadapter.AssistantDeltaEvent{
			EventBase: harnessadapter.EventBase{Attempt: native.reference},
			MessageID: derivedUUID("codex-message", params.ItemID), DeltaIndex: index, Content: safeInline(params.Delta),
		})
	case "item/started", "item/completed":
		var params nativeItemParams
		if json.Unmarshal(notification.Params, &params) != nil || !boundedNativeID(params.Item.ID) {
			adapter.failActive("adapter_protocol")
			return
		}
		native := adapter.nativeFor(params.ThreadID, params.TurnID)
		if native != nil {
			adapter.confirmAttemptInputs(native)
			adapter.handleItem(native, notification.Method, params.Item)
		}
	case "thread/tokenUsage/updated":
		var params nativeUsageParams
		if json.Unmarshal(notification.Params, &params) != nil {
			adapter.failActive("adapter_protocol")
			return
		}
		native := adapter.nativeFor(params.ThreadID, params.TurnID)
		if native != nil {
			adapter.confirmAttemptInputs(native)
			adapter.handleUsage(native, params.TokenUsage.Last)
		}
	case "turn/completed":
		var params nativeTurnNotice
		if json.Unmarshal(notification.Params, &params) != nil || !boundedNativeID(params.Turn.ID) {
			adapter.failActive("adapter_protocol")
			return
		}
		native := adapter.nativeFor(params.ThreadID, params.Turn.ID)
		if native != nil {
			adapter.finish(native, params.Turn.Status)
		}
	case "serverRequest/resolved":
		var params nativeRequestResolved
		id, err := parseResolvedRequest(notification.Params, &params)
		if err != nil {
			adapter.failActive("adapter_protocol")
			return
		}
		adapter.resolveNativeInput(params.ThreadID, id)
	case "error":
		var params nativeErrorNotice
		if json.Unmarshal(notification.Params, &params) != nil || !boundedNativeID(params.ThreadID) || !boundedNativeID(params.TurnID) ||
			params.Error.Message == "" || len(params.Error.Message) > 64<<10 || !utf8.ValidString(params.Error.Message) {
			adapter.failActive("adapter_protocol")
			return
		}
		if native := adapter.nativeFor(params.ThreadID, params.TurnID); native != nil && !params.WillRetry {
			adapter.finish(native, "failed")
		}
	case "item/plan/delta", "item/reasoning/summaryPartAdded", "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
		// Passive display-only deltas are intentionally not projected.
	default:
		if strings.HasPrefix(notification.Method, "item/") {
			adapter.failActive("adapter_protocol")
		}
	}
}

func parseResolvedRequest(encoded json.RawMessage, params *nativeRequestResolved) (rpcID, error) {
	if json.Unmarshal(encoded, params) != nil || !boundedNativeID(params.ThreadID) {
		return rpcID{}, errors.New("codex resolved request is invalid")
	}
	return parseRPCID(params.RequestID)
}

func (adapter *Adapter) handleRequest(request rpcServerRequest) {
	switch request.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		var params nativeRequestBase
		if json.Unmarshal(request.Params, &params) != nil || adapter.nativeFor(params.ThreadID, params.TurnID) == nil {
			if err := adapter.session.Reject(request.ID, -32602); err != nil {
				adapter.failActive("provider_state")
			}
			return
		}
		// The current production policy is explicit deny with an empty manifest.
		// A native request is answered once and never exposed as an allow action.
		if err := adapter.session.Respond(request.ID, map[string]string{"decision": "decline"}); err != nil {
			if native := adapter.nativeFor(params.ThreadID, params.TurnID); native != nil {
				native.runtime.failUnknown("provider_state")
			}
		}
	case "item/tool/requestUserInput":
		var params nativeInputRequest
		if json.Unmarshal(request.Params, &params) != nil || !params.IsBlocking || len(params.Questions) != 1 {
			if err := adapter.session.Reject(request.ID, -32602); err != nil {
				adapter.failActive("provider_state")
			}
			return
		}
		native := adapter.nativeFor(params.ThreadID, params.TurnID)
		question := params.Questions[0]
		if native == nil || question.IsSecret || !boundedQuestion(question.ID) || !boundedText(question.Question) {
			if err := adapter.session.Reject(request.ID, -32602); err != nil {
				adapter.failActive("provider_state")
			}
			return
		}
		inputID := derivedUUID("codex-input", attemptKey(native.reference)+"\x00"+request.ID.key)
		adapter.mu.Lock()
		if _, exists := adapter.inputs[inputID]; exists {
			adapter.mu.Unlock()
			if err := adapter.session.Reject(request.ID, -32602); err != nil {
				adapter.failActive("provider_state")
			}
			return
		}
		if _, exists := adapter.inputByRPC[request.ID.key]; exists {
			adapter.mu.Unlock()
			if err := adapter.session.Reject(request.ID, -32602); err != nil {
				adapter.failActive("provider_state")
			}
			return
		}
		adapter.inputs[inputID] = &pendingInput{id: request.ID, attempt: native, questionID: question.ID, result: make(chan bool, 1)}
		adapter.inputByRPC[request.ID.key] = inputID
		adapter.mu.Unlock()
		native.runtime.push(harnessadapter.InputRequestedEvent{
			EventBase: harnessadapter.EventBase{Attempt: native.reference}, InputRequestID: inputID, Prompt: safeInline(question.Question),
		})
	default:
		if err := adapter.session.Reject(request.ID, -32601); err != nil {
			adapter.failActive("provider_state")
		}
	}
}

func (adapter *Adapter) resolveNativeInput(threadID string, id rpcID) {
	adapter.mu.Lock()
	inputID := adapter.inputByRPC[id.key]
	pending := adapter.inputs[inputID]
	if pending == nil || pending.attempt.threadID != threadID {
		adapter.mu.Unlock()
		_ = adapter.session.ResolveInbound(id)
		return
	}
	delete(adapter.inputByRPC, id.key)
	if pending.responding {
		pending.resolved = true
		adapter.mu.Unlock()
		_ = adapter.session.ResolveInbound(id)
		return
	}
	delete(adapter.inputs, inputID)
	adapter.mu.Unlock()
	_ = adapter.session.ResolveInbound(id)
	pending.result <- false
}

func (adapter *Adapter) resolvePendingInput(inputID string, applied bool) {
	adapter.mu.Lock()
	pending := adapter.inputs[inputID]
	if pending != nil {
		delete(adapter.inputs, inputID)
		delete(adapter.inputByRPC, pending.id.key)
	}
	adapter.mu.Unlock()
	if pending != nil {
		_ = adapter.session.ResolveInbound(pending.id)
		pending.result <- applied
	}
}

func (adapter *Adapter) resolveAttemptInputs(native *nativeAttempt, applied bool) bool {
	adapter.mu.Lock()
	ids := make([]string, 0)
	responseUnconfirmed := false
	for inputID, pending := range adapter.inputs {
		if pending.attempt == native {
			ids = append(ids, inputID)
			responseUnconfirmed = responseUnconfirmed || pending.responding
		}
	}
	adapter.mu.Unlock()
	for _, inputID := range ids {
		adapter.resolvePendingInput(inputID, applied)
	}
	return responseUnconfirmed
}

func (adapter *Adapter) confirmAttemptInputs(native *nativeAttempt) {
	adapter.mu.Lock()
	ids := make([]string, 0)
	for inputID, pending := range adapter.inputs {
		if pending.attempt == native && pending.responding && pending.resolved {
			ids = append(ids, inputID)
		}
	}
	adapter.mu.Unlock()
	for _, inputID := range ids {
		adapter.resolvePendingInput(inputID, true)
	}
}

func (adapter *Adapter) nativeFor(threadID, turnID string) *nativeAttempt {
	if !boundedNativeID(threadID) || !boundedNativeID(turnID) {
		return nil
	}
	adapter.mu.Lock()
	native := adapter.byTurn[turnKey(threadID, turnID)]
	adapter.mu.Unlock()
	return native
}

func (adapter *Adapter) handleItem(native *nativeAttempt, method string, item nativeItem) {
	base := harnessadapter.EventBase{Attempt: native.reference}
	switch item.Type {
	case "agentMessage":
		if method != "item/completed" || !utf8.ValidString(item.Text) {
			return
		}
		content := safeInline(item.Text)
		native.mu.Lock()
		copy := content
		native.output = &copy
		native.mu.Unlock()
		native.runtime.push(harnessadapter.AssistantMessageEvent{
			EventBase: base, MessageID: derivedUUID("codex-message", item.ID), Content: content, FinishReason: finishReason(content),
		})
		return
	case "userMessage", "hookPrompt", "plan", "reasoning", "contextCompaction":
		// These are provider conversation bookkeeping only. They do not
		// represent a tool or an external effect in the deny-only slice.
		return
	}
	toolName, ok := nativeToolName(item)
	if ok {
		native.mu.Lock()
		state := native.tools[item.ID]
		if state.callID == "" {
			state.callID = derivedUUID("codex-tool", item.ID)
		}
		events := make([]harnessadapter.Event, 0, 2)
		if !state.started {
			state.started = true
			events = append(events, harnessadapter.ToolStartedEvent{
				EventBase: base, CallID: state.callID, ToolName: toolName,
				ActionHash: digestString("codex-tool-v1\x00" + item.Type + "\x00" + item.ID), Input: unavailableContent("provider_redacted"),
			})
		}
		if method == "item/completed" && !state.done {
			state.done = true
			status := "unknown"
			switch item.Status {
			case "completed":
				status = "succeeded"
			case "failed", "declined", "interrupted":
				status = "failed"
			}
			events = append(events, harnessadapter.ToolCompletedEvent{
				EventBase: base, CallID: state.callID, Status: status,
				Result: unavailableContent("provider_redacted"), EffectStatus: "unknown",
			})
		}
		native.tools[item.ID] = state
		native.mu.Unlock()
		for _, event := range events {
			native.runtime.push(event)
		}
	}
	// Harness supplied an empty manifest and native execution was configured
	// read-only/no-network. Any tool-shaped, state-changing, or future item is
	// therefore a policy violation with an unknown effect. Once unknown, a
	// later successful turn notification cannot make the attempt known again.
	native.runtime.failUnknown("adapter_protocol")
}

func nativeToolName(item nativeItem) (string, bool) {
	var value string
	switch item.Type {
	case "commandExecution":
		value = "codex.command"
	case "fileChange":
		value = "codex.file_change"
	case "mcpToolCall":
		if item.Server == "" || item.Tool == "" {
			return "", false
		}
		value = "mcp." + item.Server + "." + item.Tool
	case "dynamicToolCall":
		if item.Tool == "" {
			return "", false
		}
		value = "codex." + item.Tool
	default:
		return "", false
	}
	return value, utf8.ValidString(value) && len(value) <= 200 && !strings.ContainsAny(value, "\x00\r\n")
}

func (adapter *Adapter) handleUsage(native *nativeAttempt, usage nativeUsage) {
	if !validNativeUsage(usage) {
		native.runtime.failUnknown("adapter_protocol")
		return
	}
	// app-server's tokenUsage.last is the most recent request/turn usage;
	// tokenUsage.total is lifetime thread usage and would double count on resume.
	mapped := &harnessprotocol.Usage{Source: "per_attempt", InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens}
	native.mu.Lock()
	native.usage = mapped
	native.mu.Unlock()
}

func (adapter *Adapter) finish(native *nativeAttempt, status string) {
	if adapter.resolveAttemptInputs(native, false) {
		native.runtime.failUnknown("provider_state")
		return
	}
	native.mu.Lock()
	output, usage := native.output, native.usage
	if output != nil {
		copy := *output
		output = &copy
	}
	if usage != nil {
		copy := *usage
		usage = &copy
	}
	native.mu.Unlock()
	base := harnessadapter.EventBase{Attempt: native.reference}
	switch status {
	case "completed":
		result := harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileCompleted, Output: output, Usage: usage, EffectStatus: "known"}
		native.runtime.finish(result, harnessadapter.TerminalEvent{EventBase: base, Outcome: result.Outcome, Output: output, Usage: usage, EffectStatus: result.EffectStatus})
	case "interrupted":
		result := harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileInterrupted, Usage: usage, EffectStatus: "known"}
		native.runtime.finish(result, harnessadapter.TerminalEvent{EventBase: base, Outcome: result.Outcome, Usage: usage, EffectStatus: result.EffectStatus})
	case "failed":
		failure := taskFailure("codex_turn_failed", "codex turn failed")
		result := harnessadapter.ReconcileResult{Outcome: harnessadapter.ReconcileFailed, Usage: usage, Failure: failure, EffectStatus: "unknown"}
		native.runtime.finish(result, harnessadapter.TerminalEvent{EventBase: base, Outcome: result.Outcome, Usage: usage, Failure: failure, EffectStatus: result.EffectStatus})
	default:
		native.runtime.failUnknown("provider_state")
	}
	_ = adapter.store.terminal(native.reference)
}

func (adapter *Adapter) handleExit() { adapter.failActive("provider_state") }

func (adapter *Adapter) failActive(reason string) {
	adapter.mu.Lock()
	attempts := make([]*nativeAttempt, 0, len(adapter.attempts))
	for _, native := range adapter.attempts {
		attempts = append(attempts, native)
	}
	adapter.mu.Unlock()
	for _, native := range attempts {
		adapter.resolveAttemptInputs(native, false)
		if native.runtime.reconcile().Outcome == harnessadapter.ReconcileRunning {
			native.runtime.failUnknown(reason)
		}
	}
}

func boundedQuestion(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}
