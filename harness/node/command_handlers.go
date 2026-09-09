package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

func decodeParts[T any, E any, P any](envelope harnessprotocol.CommandEnvelope) (T, E, P, error) {
	var target T
	var expected E
	var payload P
	if err := json.Unmarshal(envelope.Target, &target); err != nil {
		return target, expected, payload, err
	}
	if err := json.Unmarshal(envelope.Expected, &expected); err != nil {
		return target, expected, payload, err
	}
	err := json.Unmarshal(envelope.Payload, &payload)
	return target, expected, payload, err
}

func (node *Node) applyDialogCreate(ctx context.Context, tx *sql.Tx, state *durableState, envelope harnessprotocol.CommandEnvelope) (any, string, string, postCommitAction, error) {
	_, expected, payload, err := decodeParts[harnessprotocol.NodeTarget, harnessprotocol.RegistryExpected, harnessprotocol.DialogCreatePayload](envelope)
	if err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	if expected.RegistryVersion != state.RegistryVersion {
		return nil, "", "", postCommitAction{}, stale(state.RegistryVersion, "registry")
	}
	dialogID, err := node.newID()
	if err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	now := timestamp(node.config.Clock())
	if _, err := tx.ExecContext(ctx, `INSERT INTO dialogs(dialog_id,node_id,owner_id,version,title,created_at) VALUES(?,?,?,?,?,?)`, dialogID, state.NodeID, state.OwnerID, 1, nullText(payload.Title), now); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	state.StateVersion++
	if err := node.appendNodeEvent(ctx, tx, state); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	return harnessprotocol.DialogCreateReferences{DialogID: dialogID}, "admitted", node.blockingReason(*state), postCommitAction{}, nil
}

func (node *Node) applyMessageEnqueue(ctx context.Context, tx *sql.Tx, state *durableState, envelope harnessprotocol.CommandEnvelope) (any, string, string, postCommitAction, error) {
	target, expected, payload, err := decodeParts[harnessprotocol.DialogTarget, harnessprotocol.DialogExpected, harnessprotocol.MessagePayload](envelope)
	if err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	var dialogVersion int64
	if err := tx.QueryRowContext(ctx, "SELECT version FROM dialogs WHERE dialog_id=?", target.DialogID).Scan(&dialogVersion); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	if expected.DialogVersion != dialogVersion {
		return nil, "", "", postCommitAction{}, stale(dialogVersion, "dialog")
	}
	if state.PendingCount >= QueueCapacity {
		return nil, "", "", postCommitAction{}, reject(http.StatusTooManyRequests, "queue_full", "pending queue is full")
	}
	messageID, err := node.newID()
	if err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	requestID, err := node.newID()
	if err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	now := timestamp(node.config.Clock())
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages(message_id,dialog_id,sequence,version,role,text,disposition,command_id,request_id,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, messageID, target.DialogID, state.NextMessageSequence, 1, "user", payload.Text, "queued", envelope.CommandID, requestID, now); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO requests(request_id,dialog_id,input_message_id,queue_sequence,version,status,created_at,updated_at)
		VALUES(?,?,?,?,1,'queued',?,?)`, requestID, target.DialogID, messageID, state.NextQueueSequence, now, now); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE dialogs SET version=version+1 WHERE dialog_id=?", target.DialogID); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	sequence := state.NextMessageSequence
	state.NextMessageSequence++
	state.NextQueueSequence++
	state.PendingCount++
	state.QueueVersion++
	state.StateVersion++
	if _, err := node.appendEvent(ctx, tx, state, "message.accepted", messageID, 1, "", "", harnessprotocol.MessageAcceptedPayload{
		DialogID: target.DialogID, MessageID: messageID, RequestID: requestID, Sequence: sequence, Disposition: "queued",
	}, false); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	if err := node.appendQueueEvent(ctx, tx, state); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	if err := node.appendNodeEvent(ctx, tx, state); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	return harnessprotocol.MessageEnqueueReferences{DialogID: target.DialogID, MessageID: messageID, RequestID: requestID}, "admitted", node.blockingReason(*state), postCommitAction{}, nil
}

func (node *Node) applyRequestCancel(ctx context.Context, tx *sql.Tx, state *durableState, envelope harnessprotocol.CommandEnvelope) (any, string, string, postCommitAction, error) {
	target, expected, _, err := decodeParts[harnessprotocol.RequestTarget, harnessprotocol.RequestExpected, harnessprotocol.EmptyPayload](envelope)
	if err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	var version int64
	var status, messageID, dialogID string
	if err := tx.QueryRowContext(ctx, "SELECT version,status,input_message_id,dialog_id FROM requests WHERE request_id=?", target.RequestID).Scan(&version, &status, &messageID, &dialogID); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	if expected.RequestVersion != version {
		return nil, "", "", postCommitAction{}, stale(version, status)
	}
	if status != "queued" {
		return nil, "", "", postCommitAction{}, stale(version, status)
	}
	now := timestamp(node.config.Clock())
	if _, err := tx.ExecContext(ctx, "UPDATE requests SET version=version+1,status='cancelled',updated_at=? WHERE request_id=?", now, target.RequestID); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE messages SET version=version+1,disposition='cancelled' WHERE message_id=?", messageID); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	state.PendingCount--
	state.QueueVersion++
	state.StateVersion++
	if _, err := node.appendEvent(ctx, tx, state, "message.disposition_changed", messageID, 2, "", "", harnessprotocol.MessageDispositionPayload{
		MessageID: messageID, From: "queued", To: "cancelled", ReasonCode: "request_cancelled",
	}, false); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	if err := node.appendQueueEvent(ctx, tx, state); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	if err := node.appendNodeEvent(ctx, tx, state); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	return harnessprotocol.RequestCancelReferences{RequestID: target.RequestID}, "admitted", "", postCommitAction{}, nil
}

func (node *Node) applyQueueResume(ctx context.Context, tx *sql.Tx, state *durableState, envelope harnessprotocol.CommandEnvelope) (any, string, string, postCommitAction, error) {
	_, expected, _, err := decodeParts[harnessprotocol.NodeTarget, harnessprotocol.QueueExpected, harnessprotocol.EmptyPayload](envelope)
	if err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	if expected.QueueVersion != state.QueueVersion {
		return nil, "", "", postCommitAction{}, stale(state.QueueVersion, "queue")
	}
	state.QueuePaused = false
	state.BlockedReasons = removeReason(state.BlockedReasons, "operator_pause")
	state.QueueVersion++
	state.StateVersion++
	if err := node.appendQueueEvent(ctx, tx, state); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	if err := node.appendNodeEvent(ctx, tx, state); err != nil {
		return nil, "", "", postCommitAction{}, err
	}
	return harnessprotocol.QueueResumeReferences{NodeID: state.NodeID}, "applied", node.blockingReason(*state), postCommitAction{}, nil
}

func (node *Node) blockingReason(state durableState) string {
	if state.QueuePaused {
		return "operator_pause"
	}
	if len(state.BlockedReasons) > 0 {
		return state.BlockedReasons[0]
	}
	if state.EngineReadiness != "ready" {
		return "engine_unavailable"
	}
	return ""
}

func addReason(reasons []string, reason string) []string {
	if slices.Contains(reasons, reason) {
		return reasons
	}
	return append(reasons, reason)
}

func removeReason(reasons []string, reason string) []string {
	return slices.DeleteFunc(reasons, func(candidate string) bool { return candidate == reason })
}
