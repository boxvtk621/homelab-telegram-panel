package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessbarrier"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

// OperatorTrustContext is intentionally separate from the browser command
// TrustContext. R06 may expose this seam through an authenticated administrative
// endpoint; R05 only establishes the node-side authority.
type OperatorTrustContext struct {
	ActorID         string
	TransportNodeID string
	PeerVerified    bool
}

type durableHold struct {
	OperationID   string
	ScopeKind     string
	DialogID      sql.NullString
	HoldVersion   int64
	ScopeRevision int64
}

func (hold durableHold) scope() harnessbarrier.Scope {
	return harnessbarrier.Scope{Kind: hold.ScopeKind, DialogID: hold.DialogID.String}
}

type storedCommandOutcome struct {
	hash     string
	body     []byte
	status   int
	accepted bool
}

// CanonicalInstallRequest validates and canonicalizes the additive barrier
// request without changing harness-wire-v2.
func CanonicalInstallRequest(raw []byte) ([]byte, string, error) {
	if err := harnessbarrier.Validate("installRequest", raw); err != nil {
		return nil, "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, "", err
	}
	canonical, err := appendCanonical(make([]byte, 0, len(raw)), value)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(canonical)
	return canonical, hex.EncodeToString(digest[:]), nil
}

// InstallHold durably installs one node- or dialog-scoped admission/dispatch
// barrier. startGate is acquired before mu so the commit cannot overtake an
// Adapter.Start/Resume call that has already crossed its final gate.
func (node *Node) InstallHold(ctx context.Context, trust OperatorTrustContext, raw []byte) Result {
	if !trust.PeerVerified || trust.ActorID == "" || trust.ActorID != node.config.OwnerID {
		return node.errorResult(http.StatusForbidden, "forbidden", "trusted operator is not allowed", "", nil, "")
	}
	if trust.TransportNodeID != node.config.NodeID {
		return node.errorResult(http.StatusNotFound, "not_found", "node was not found", "", nil, "")
	}
	canonical, digest, err := CanonicalInstallRequest(raw)
	if err != nil {
		return node.errorResult(http.StatusBadRequest, "invalid", "hold request does not match harness-barrier-v1", "", nil, "")
	}
	var request harnessbarrier.InstallRequest
	if json.Unmarshal(raw, &request) != nil {
		return node.errorResult(http.StatusBadRequest, "invalid", "hold request cannot be decoded", "", nil, "")
	}
	if request.NodeID != node.config.NodeID {
		return node.errorResult(http.StatusNotFound, "not_found", "node was not found", "", nil, "")
	}

	node.startGate.Lock()
	defer node.startGate.Unlock()
	node.mu.Lock()
	defer node.mu.Unlock()
	reserveReleased := false
	defer func() { node.replenishControlReserve(reserveReleased) }()

	tx, err := node.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "durable store is unavailable", "", nil, "")
	}
	defer tx.Rollback()
	state, err := loadState(ctx, tx)
	if err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "durable state is unavailable", "", nil, "")
	}
	var storedHash string
	var storedReceipt []byte
	err = tx.QueryRowContext(ctx, "SELECT canonical_payload_hash,receipt_json FROM administrative_holds WHERE operation_id=?", request.OperationID).Scan(&storedHash, &storedReceipt)
	if err == nil {
		if storedHash != digest {
			return node.errorResult(http.StatusConflict, "id_conflict", "operationId already names different bytes", "", nil, "")
		}
		return Result{HTTPStatus: http.StatusOK, Body: storedReceipt}
	}
	if !isNoRows(err) {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "hold lookup failed", "", nil, "")
	}
	if request.ExpectedEpoch != state.Epoch || request.BindingGeneration != state.RegistryVersion {
		return node.errorResult(http.StatusConflict, "stale", "node binding changed", "", &state.Epoch, "blocked")
	}
	if request.Scope.Kind == "dialog" {
		var ownerID string
		err := tx.QueryRowContext(ctx, `SELECT owner_id FROM dialogs WHERE dialog_id=? AND node_id=? AND NOT EXISTS (
			SELECT 1 FROM events deleted WHERE deleted.dialog_id=dialogs.dialog_id AND deleted.projection_key='dialog.deleted')`, request.Scope.DialogID, state.NodeID).Scan(&ownerID)
		if isNoRows(err) || ownerID != state.OwnerID {
			return node.errorResult(http.StatusNotFound, "not_found", "dialog was not found", "", nil, "")
		}
		if err != nil {
			return node.errorResult(http.StatusServiceUnavailable, "not_durable", "dialog lookup failed", "", nil, "")
		}
	}
	var preStart int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts a JOIN control_actions c ON c.attempt_id=a.attempt_id
		WHERE a.node_id=? AND a.state='dispatching' AND a.started_at IS NULL AND c.kind=? AND c.status IN('pending','inflight')
		AND (?='node' OR a.dialog_id=?)`, state.NodeID, actionDispatchStart, request.Scope.Kind, request.Scope.DialogID).Scan(&preStart); err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "dispatch boundary lookup failed", "", nil, "")
	}
	if preStart != 0 {
		return node.errorResult(http.StatusConflict, "stale", "dispatch start is already in progress", "", nil, "active")
	}
	var conflicts int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM administrative_holds WHERE node_id=? AND
		(scope='node' OR ?='node' OR (scope='dialog' AND dialog_id=?))`, state.NodeID, request.Scope.Kind, request.Scope.DialogID).Scan(&conflicts); err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "hold conflict lookup failed", "", nil, "")
	}
	if conflicts != 0 {
		return node.errorResult(http.StatusConflict, "stale", "scope is already held", "", nil, "blocked")
	}
	scopeKey := request.Scope.Kind
	if request.Scope.Kind == "dialog" {
		scopeKey += ":" + request.Scope.DialogID
	}
	currentRevision := int64(0)
	err = tx.QueryRowContext(ctx, "SELECT revision FROM hold_scope_revisions WHERE scope_key=?", scopeKey).Scan(&currentRevision)
	if err != nil && !isNoRows(err) {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "hold revision lookup failed", "", nil, "")
	}
	if request.ExpectedScopeRevision != currentRevision {
		return node.errorResult(http.StatusConflict, "stale", "hold scope revision changed", "", &currentRevision, "blocked")
	}
	var holdVersion int64
	if err := tx.QueryRowContext(ctx, "SELECT next_version FROM hold_clock WHERE singleton=1").Scan(&holdVersion); err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "hold clock is unavailable", "", nil, "")
	}
	receiptID, err := node.newID()
	if err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "hold receipt identity is unavailable", "", nil, "")
	}
	installedAt := timestamp(node.config.Clock())
	receipt := harnessbarrier.HoldReceipt{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, Kind: "hold.installed",
		OperationID: request.OperationID, ReceiptID: receiptID, NodeID: state.NodeID, Epoch: state.Epoch,
		BindingGeneration: state.RegistryVersion, Scope: request.Scope, HoldVersion: holdVersion,
		ScopeRevision: currentRevision + 1, InstalledAt: installedAt,
	}
	receiptJSON, err := json.Marshal(receipt)
	if err != nil || harnessbarrier.Validate("holdReceipt", receiptJSON) != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "internal hold receipt is invalid", "", nil, "")
	}
	reserveReleased, err = node.releaseControlReserve()
	if err != nil || !reserveReleased {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "control reserve is unavailable", "", nil, "")
	}
	if _, err := tx.ExecContext(ctx, "UPDATE hold_clock SET next_version=next_version+1 WHERE singleton=1 AND next_version=?", holdVersion); err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "hold clock commit failed", "", nil, "")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO hold_scope_revisions(scope_key,revision) VALUES(?,?)
		ON CONFLICT(scope_key) DO UPDATE SET revision=excluded.revision`, scopeKey, currentRevision+1); err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "hold revision commit failed", "", nil, "")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO administrative_holds(
		operation_id,node_id,node_epoch,binding_generation,scope,dialog_id,hold_version,scope_revision,
		canonical_json,canonical_payload_hash,receipt_json,installed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		request.OperationID, state.NodeID, state.Epoch, state.RegistryVersion, request.Scope.Kind, nullText(request.Scope.DialogID),
		holdVersion, currentRevision+1, canonical, digest, receiptJSON, installedAt); err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "hold commit failed", "", nil, "")
	}
	if err := node.checkFault(FaultBeforeHoldCommit); err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "hold commit was not attempted", "", nil, "")
	}
	if err := tx.Commit(); err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "atomic hold commit failed", "", nil, "")
	}
	node.afterCommit(context.Background(), postCommitAction{})
	if err := node.checkFault(FaultAfterHoldCommit); err != nil {
		return Result{HTTPStatus: http.StatusServiceUnavailable, Body: node.errorResult(http.StatusServiceUnavailable, "node_unavailable", "hold receipt delivery was interrupted", "", nil, "").Body, Committed: true}
	}
	return Result{HTTPStatus: http.StatusCreated, Body: receiptJSON, Committed: true}
}

func (node *Node) HoldStatus(ctx context.Context, trust OperatorTrustContext, operationID string) Result {
	if !trust.PeerVerified || trust.ActorID == "" || trust.ActorID != node.config.OwnerID {
		return node.errorResult(http.StatusForbidden, "forbidden", "trusted operator is not allowed", "", nil, "")
	}
	if trust.TransportNodeID != node.config.NodeID {
		return node.errorResult(http.StatusNotFound, "not_found", "node was not found", "", nil, "")
	}
	if len(operationID) == 0 || len(operationID) > 128 {
		return node.errorResult(http.StatusBadRequest, "invalid", "operation id is invalid", "", nil, "")
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	var receipt []byte
	if err := node.db.QueryRowContext(ctx, "SELECT receipt_json FROM administrative_holds WHERE operation_id=?", operationID).Scan(&receipt); err != nil {
		if isNoRows(err) {
			return node.errorResult(http.StatusNotFound, "not_found", "hold was not found", "", nil, "")
		}
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "hold status is unavailable", "", nil, "")
	}
	if harnessbarrier.Validate("holdReceipt", receipt) != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "hold status is invalid", "", nil, "")
	}
	return Result{HTTPStatus: http.StatusOK, Body: receipt}
}

func loadCommandOutcome(ctx context.Context, tx *sql.Tx, commandID string) (storedCommandOutcome, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT canonical_payload_hash,receipt_json,http_status,accepted FROM (
		SELECT canonical_payload_hash,receipt_json,200 AS http_status,1 AS accepted FROM commands WHERE command_id=?
		UNION ALL
		SELECT canonical_payload_hash,receipt_json,http_status,0 AS accepted FROM command_rejections WHERE command_id=?
	)`, commandID, commandID)
	if err != nil {
		return storedCommandOutcome{}, false, err
	}
	defer rows.Close()
	var outcome storedCommandOutcome
	count := 0
	for rows.Next() {
		var accepted int
		if err := rows.Scan(&outcome.hash, &outcome.body, &outcome.status, &accepted); err != nil {
			return storedCommandOutcome{}, false, err
		}
		outcome.accepted = accepted == 1
		count++
	}
	if err := rows.Err(); err != nil {
		return storedCommandOutcome{}, false, err
	}
	if count > 1 {
		return storedCommandOutcome{}, false, errors.New("command id has multiple durable outcomes")
	}
	return outcome, count == 1, nil
}

func (node *Node) coveringHoldForCommand(ctx context.Context, tx *sql.Tx, envelope harnessprotocol.CommandEnvelope) (*durableHold, error) {
	dialogID := ""
	blockable := true
	switch envelope.Kind {
	case harnessprotocol.CommandDialogCreate, harnessprotocol.CommandQueueResume:
		// Only a node hold covers node-wide admission/resume.
	case harnessprotocol.CommandDialogDelete, harnessprotocol.CommandMessageEnqueue:
		var target harnessprotocol.DialogTarget
		if err := json.Unmarshal(envelope.Target, &target); err != nil {
			return nil, err
		}
		dialogID = target.DialogID
	case harnessprotocol.CommandAttemptRetry:
		var target harnessprotocol.AttemptTarget
		if err := json.Unmarshal(envelope.Target, &target); err != nil {
			return nil, err
		}
		if err := tx.QueryRowContext(ctx, "SELECT dialog_id FROM attempts WHERE attempt_id=?", target.AttemptID).Scan(&dialogID); err != nil {
			return nil, err
		}
	default:
		blockable = false
	}
	if !blockable {
		return nil, nil
	}
	query := `SELECT operation_id,scope,dialog_id,hold_version,scope_revision FROM administrative_holds
		WHERE node_id=? AND scope='node' ORDER BY hold_version LIMIT 1`
	arguments := []any{node.config.NodeID}
	if dialogID != "" {
		query = `SELECT operation_id,scope,dialog_id,hold_version,scope_revision FROM administrative_holds
			WHERE node_id=? AND (scope='node' OR (scope='dialog' AND dialog_id=?)) ORDER BY CASE scope WHEN 'node' THEN 0 ELSE 1 END,hold_version LIMIT 1`
		arguments = append(arguments, dialogID)
	}
	var hold durableHold
	if err := tx.QueryRowContext(ctx, query, arguments...).Scan(&hold.OperationID, &hold.ScopeKind, &hold.DialogID, &hold.HoldVersion, &hold.ScopeRevision); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &hold, nil
}

func (node *Node) commitCommandRejection(ctx context.Context, tx *sql.Tx, state durableState, trust TrustContext,
	envelope harnessprotocol.CommandEnvelope, canonical []byte, digest string, hold durableHold) Result {
	receiptID, err := node.newID()
	if err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "rejection receipt identity is unavailable", envelope.CommandID, nil, "")
	}
	rejectedAt := timestamp(node.config.Clock())
	receipt := harnessbarrier.RejectionReceipt{
		ProtocolVersion: harnessbarrier.ProtocolVersion, SchemaID: harnessbarrier.SchemaID, Kind: "command.rejected",
		CommandID: envelope.CommandID, CommandKind: string(envelope.Kind), ReceiptID: receiptID, NodeID: state.NodeID,
		Epoch: state.Epoch, CanonicalPayloadHash: digest, RejectedAt: rejectedAt, Reason: "administrative_hold",
		HoldOperationID: hold.OperationID, HoldVersion: hold.HoldVersion, Scope: hold.scope(), ScopeRevision: hold.ScopeRevision,
	}
	receiptJSON, err := json.Marshal(receipt)
	if err != nil || harnessbarrier.Validate("rejectionReceipt", receiptJSON) != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "internal rejection receipt is invalid", envelope.CommandID, nil, "")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO command_rejections(
		command_id,actor_id,node_id,kind,canonical_json,canonical_payload_hash,receipt_json,http_status,rejected_at,hold_operation_id)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, envelope.CommandID, trust.ActorID, state.NodeID, envelope.Kind, canonical, digest,
		receiptJSON, http.StatusConflict, rejectedAt, hold.OperationID); err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "rejection commit failed", envelope.CommandID, nil, "")
	}
	if err := node.checkFault(FaultBeforeRejectionCommit); err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "rejection commit was not attempted", envelope.CommandID, nil, "")
	}
	if err := tx.Commit(); err != nil {
		return node.errorResult(http.StatusServiceUnavailable, "not_durable", "atomic rejection commit failed", envelope.CommandID, nil, "")
	}
	if err := node.checkFault(FaultAfterRejectionCommit); err != nil {
		return Result{HTTPStatus: http.StatusServiceUnavailable, Body: node.errorResult(http.StatusServiceUnavailable, "node_unavailable", "rejection receipt delivery was interrupted", envelope.CommandID, nil, "").Body, Committed: true}
	}
	return Result{HTTPStatus: http.StatusConflict, Body: receiptJSON, Committed: true}
}
