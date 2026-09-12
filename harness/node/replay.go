package node

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

// EventReplay is a bounded durable SSE read. The caller must request again
// after LastEventSeq to follow new commits; no database lock is held while the
// network client is idle.
type EventReplay struct {
	Epoch        int64
	LastEventSeq int64
	Events       []json.RawMessage
}

func (node *Node) ReplayEvents(ctx context.Context, trust TrustContext, after int64, limit int) (EventReplay, Result, bool) {
	if denied := node.authorizeRead(trust); denied != nil {
		return EventReplay{}, *denied, false
	}
	if after < 0 || limit < 1 || limit > 100 {
		return EventReplay{}, node.Invalid("event cursor is invalid"), false
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	tx, err := node.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EventReplay{}, node.errorResult(503, "not_durable", "events are unavailable", "", nil, ""), false
	}
	defer tx.Rollback()
	state, err := loadState(ctx, tx)
	if err != nil {
		return EventReplay{}, node.errorResult(503, "not_durable", "events are unavailable", "", nil, ""), false
	}
	if after > state.LastEventSeq {
		return EventReplay{}, node.errorResult(409, "stale", "event cursor is ahead of durable history", "", nil, ""), false
	}
	deletedDialogs, deletedMessages, err := deletedEventScopes(ctx, tx)
	if err != nil {
		return EventReplay{}, node.errorResult(503, "not_durable", "events are unavailable", "", nil, ""), false
	}
	rows, err := tx.QueryContext(ctx, `SELECT event_json FROM events WHERE node_id=? AND seq>? ORDER BY seq LIMIT ?`, state.NodeID, after, limit)
	if err != nil {
		return EventReplay{}, node.errorResult(503, "not_durable", "events are unavailable", "", nil, ""), false
	}
	defer rows.Close()
	events := make([]json.RawMessage, 0, limit)
	totalBytes := 0
	expected := after + 1
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return EventReplay{}, node.errorResult(503, "not_durable", "events are unavailable", "", nil, ""), false
		}
		var envelope harnessprotocol.EventEnvelope
		if harnessprotocol.Validate("event", raw) != nil || json.Unmarshal(raw, &envelope) != nil || envelope.NodeID != state.NodeID || envelope.Epoch != state.Epoch || envelope.Seq != expected {
			return EventReplay{}, node.errorResult(409, "stale", "durable event history has a gap", "", nil, ""), false
		}
		if eventBelongsToDeletedDialog(envelope, deletedDialogs, deletedMessages) {
			return EventReplay{}, node.errorResult(409, "stale", "event history requires snapshot resynchronization", "", nil, ""), false
		}
		if totalBytes+len(raw) > harnessprotocol.MaximumWireBytes-1024 {
			break
		}
		events = append(events, json.RawMessage(raw))
		totalBytes += len(raw)
		expected++
	}
	if err := rows.Err(); err != nil {
		return EventReplay{}, node.errorResult(503, "not_durable", "events are unavailable", "", nil, ""), false
	}
	return EventReplay{Epoch: state.Epoch, LastEventSeq: state.LastEventSeq, Events: events}, Result{}, true
}

func deletedEventScopes(ctx context.Context, tx *sql.Tx) (map[string]bool, map[string]bool, error) {
	dialogs := map[string]bool{}
	rows, err := tx.QueryContext(ctx, "SELECT dialog_id FROM events WHERE projection_key='dialog.deleted' AND dialog_id IS NOT NULL")
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var dialogID string
		if err := rows.Scan(&dialogID); err != nil {
			rows.Close()
			return nil, nil, err
		}
		dialogs[dialogID] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	messages := map[string]bool{}
	rows, err = tx.QueryContext(ctx, `SELECT m.message_id FROM messages m JOIN events deleted ON deleted.dialog_id=m.dialog_id AND deleted.projection_key='dialog.deleted'`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID string
		if err := rows.Scan(&messageID); err != nil {
			return nil, nil, err
		}
		messages[messageID] = true
	}
	return dialogs, messages, rows.Err()
}

func eventBelongsToDeletedDialog(envelope harnessprotocol.EventEnvelope, dialogs, messages map[string]bool) bool {
	if envelope.Type == "dialog.deleted" {
		return false
	}
	if dialogs[envelope.DialogID] || (envelope.Type == "message.disposition_changed" && messages[envelope.EntityID]) {
		return true
	}
	if envelope.Type == "message.accepted" {
		var payload harnessprotocol.MessageAcceptedPayload
		return json.Unmarshal(envelope.Payload, &payload) == nil && dialogs[payload.DialogID]
	}
	return false
}
