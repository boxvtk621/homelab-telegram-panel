package node

import (
	"context"
	"database/sql"
	"errors"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessadapter"
)

func (node *Node) recoverStartup(ctx context.Context) error {
	node.mu.Lock()
	defer node.mu.Unlock()
	tx, err := node.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, err := loadState(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE control_actions SET status='unknown' WHERE status IN('pending','inflight')"); err != nil {
		return err
	}
	if !state.ActiveAttemptID.Valid {
		return tx.Commit()
	}
	var reference harnessadapter.AttemptRef
	var attemptState string
	reference.NodeID = state.NodeID
	err = tx.QueryRowContext(ctx, `SELECT dialog_id,request_id,attempt_id,generation,state FROM attempts WHERE attempt_id=?`, state.ActiveAttemptID.String).Scan(
		&reference.DialogID, &reference.RequestID, &reference.AttemptID, &reference.Generation, &attemptState)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("node active attempt reference is missing")
	}
	if err != nil {
		return err
	}
	if attemptState == "unknown" {
		return tx.Commit()
	}
	if attemptState != "dispatching" && attemptState != "running" && attemptState != "waiting_input" && attemptState != "stopping" {
		return errors.New("node active attempt is not active")
	}
	if err := node.setAttemptUnknown(ctx, tx, &state, reference, "dispatch_uncertain", "unknown"); err != nil {
		return err
	}
	if err := saveState(ctx, tx, state); err != nil {
		return err
	}
	return tx.Commit()
}
