package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

var (
	ErrLogicalDeleteConflict = errors.New("logical delete conflict")
	ErrLogicalDeleteNotReady = errors.New("logical delete source is not ready")
	ErrLogicalDeleteState    = errors.New("logical delete state is invalid")
)

func (s *Store) BeginLogicalDelete(ctx context.Context, owner string, request model.LogicalDeleteRequest) (model.LogicalDeleteStatus, error) {
	requestHash, err := model.LogicalDeleteRequestHash(request)
	if err != nil || !model.ValidActor(owner) {
		return model.LogicalDeleteStatus{}, ErrLogicalDeleteState
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.LogicalDeleteStatus{}, errors.New("logical delete transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "logical-dialog:"+owner+":"+request.LogicalDialogID); err != nil {
		return model.LogicalDeleteStatus{}, errors.New("logical delete lock unavailable")
	}
	if current, found, readErr := logicalDeleteStatusTx(ctx, tx, owner, request.OperationID, true); readErr != nil {
		return model.LogicalDeleteStatus{}, readErr
	} else if found {
		if current.RequestHash != requestHash {
			return model.LogicalDeleteStatus{}, ErrLogicalDeleteConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return model.LogicalDeleteStatus{}, errors.New("logical delete readback unavailable")
		}
		return current, nil
	}

	var deletedAt *time.Time
	var holdScopeRevision int64
	if err := tx.QueryRow(ctx, `SELECT deleted_at,hold_scope_revision
		FROM agent_service.logical_dialogs WHERE owner_id=$1 AND logical_dialog_id=$2 FOR UPDATE`,
		owner, request.LogicalDialogID).Scan(&deletedAt, &holdScopeRevision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.LogicalDeleteStatus{}, pgx.ErrNoRows
		}
		return model.LogicalDeleteStatus{}, errors.New("logical dialog unavailable")
	}
	if deletedAt != nil {
		return model.LogicalDeleteStatus{}, pgx.ErrNoRows
	}
	var nodeID, nodeDialogID, bindingState string
	var bindingVersion, registryVersion, identityEpoch int64
	err = tx.QueryRow(ctx, `SELECT b.node_id::text,b.node_dialog_id::text,b.binding_version,b.state,
		i.registration_revision,i.registration_epoch
		FROM agent_service.dialog_bindings b
		JOIN agent_service.instances i ON i.owner_id=b.owner_id AND i.node_id=b.node_id
		WHERE b.owner_id=$1 AND b.logical_dialog_id=$2 AND b.state IN ('active','deleting')
		FOR UPDATE OF b,i`, owner, request.LogicalDialogID).Scan(
		&nodeID, &nodeDialogID, &bindingVersion, &bindingState, &registryVersion, &identityEpoch)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.LogicalDeleteStatus{}, pgx.ErrNoRows
		}
		return model.LogicalDeleteStatus{}, errors.New("dialog binding unavailable")
	}
	if bindingState != "active" || bindingVersion != request.ExpectedBindingVersion {
		return model.LogicalDeleteStatus{}, ErrLogicalDeleteConflict
	}

	checkpoint, checkpointHash, err := exactDeleteCheckpoint(ctx, tx, owner, request, nodeID, nodeDialogID)
	if err != nil {
		return model.LogicalDeleteStatus{}, err
	}
	archived, err := exactArchiveClosure(ctx, tx, owner, request, nodeID, nodeDialogID, checkpoint, checkpointHash)
	if err != nil {
		return model.LogicalDeleteStatus{}, err
	}
	now := s.now().UTC()
	if _, err := tx.Exec(ctx, `INSERT INTO agent_service.dialog_operation_reservations(
		owner_id,operation_id,logical_dialog_id,operation_kind,request_hash,state,created_at,updated_at)
		VALUES($1,$2,$3,'delete',$4,'active',$5,$5)`, owner, request.OperationID, request.LogicalDialogID, requestHash, now); err != nil {
		return model.LogicalDeleteStatus{}, ErrLogicalDeleteConflict
	}
	phase, effectState, operationVersion := "accepted", "not_sent", int64(1)
	if archived {
		phase, effectState, operationVersion = "succeeded", "reconciled", 2
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_service.logical_dialog_delete_operations(
		owner_id,operation_id,request_hash,command_id,logical_dialog_id,expected_binding_version,
		expected_dialog_version,node_id,node_dialog_id,registry_version,identity_epoch,hold_scope_revision,
		phase,effect_state,operation_version,archived,result_code,accepted_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$18)`,
		owner, request.OperationID, requestHash, request.CommandID, request.LogicalDialogID,
		request.ExpectedBindingVersion, request.ExpectedDialogVersion, nodeID, nodeDialogID,
		registryVersion, identityEpoch, holdScopeRevision, phase, effectState, operationVersion, archived,
		func() any {
			if archived {
				return "archive_tombstoned"
			}
			return nil
		}(), now); err != nil {
		return model.LogicalDeleteStatus{}, errors.New("logical delete reservation unavailable")
	}
	if archived {
		if err := tombstoneLogicalDialog(ctx, tx, owner, request.OperationID, requestHash, request.LogicalDialogID,
			request.ExpectedBindingVersion, nodeID, nodeDialogID, request.ExpectedDialogVersion, "archive_closure", nil, now); err != nil {
			return model.LogicalDeleteStatus{}, err
		}
	} else {
		result, err := tx.Exec(ctx, `UPDATE agent_service.dialog_bindings SET state='deleting',updated_at=$4
			WHERE owner_id=$1 AND logical_dialog_id=$2 AND binding_version=$3 AND state='active'`,
			owner, request.LogicalDialogID, request.ExpectedBindingVersion, now)
		if err != nil || result.RowsAffected() != 1 {
			return model.LogicalDeleteStatus{}, ErrLogicalDeleteConflict
		}
	}
	status, found, err := logicalDeleteStatusTx(ctx, tx, owner, request.OperationID, false)
	if err != nil || !found {
		return model.LogicalDeleteStatus{}, errors.New("logical delete readback unavailable")
	}
	if err := tx.Commit(ctx); err != nil {
		return model.LogicalDeleteStatus{}, errors.New("logical delete commit unavailable")
	}
	return status, nil
}

func (s *Store) GetLogicalDelete(ctx context.Context, owner, operationID string) (model.LogicalDeleteStatus, error) {
	if !model.ValidActor(owner) || !model.ValidUUID(operationID) {
		return model.LogicalDeleteStatus{}, ErrLogicalDeleteState
	}
	status, found, err := logicalDeleteStatusQuery(ctx, s.pool, owner, operationID, false)
	if err != nil {
		return model.LogicalDeleteStatus{}, err
	}
	if !found {
		return model.LogicalDeleteStatus{}, pgx.ErrNoRows
	}
	return status, nil
}

func (s *Store) AdvanceLogicalDelete(ctx context.Context, owner string, advance model.LogicalDeleteAdvance) (model.LogicalDeleteStatus, error) {
	if !model.ValidActor(owner) || model.ValidateLogicalDeleteAdvance(advance) != nil {
		return model.LogicalDeleteStatus{}, ErrLogicalDeleteState
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.LogicalDeleteStatus{}, errors.New("logical delete transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	current, found, err := logicalDeleteStatusTx(ctx, tx, owner, advance.OperationID, true)
	if err != nil {
		return model.LogicalDeleteStatus{}, err
	}
	if !found {
		return model.LogicalDeleteStatus{}, pgx.ErrNoRows
	}
	if current.RequestHash != advance.RequestHash {
		return model.LogicalDeleteStatus{}, ErrLogicalDeleteConflict
	}
	if current.Phase == "succeeded" || current.Phase == "failed" {
		if !terminalAdvanceMatches(current, advance) {
			return model.LogicalDeleteStatus{}, ErrLogicalDeleteConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return model.LogicalDeleteStatus{}, errors.New("logical delete readback unavailable")
		}
		return current, nil
	}
	if current.OperationVersion != advance.ExpectedOperationVersion {
		return model.LogicalDeleteStatus{}, ErrLogicalDeleteConflict
	}
	now := s.now().UTC()
	switch advance.Action {
	case "hold":
		if current.Phase != "accepted" || advance.ObservedHoldScopeRevision != current.HoldScopeRevision+1 {
			return model.LogicalDeleteStatus{}, ErrLogicalDeleteConflict
		}
		err = logicalDeleteExecOne(ctx, tx, `UPDATE agent_service.logical_dialog_delete_operations SET
			hold_version=$4,hold_scope_revision=$5,phase='holding',effect_state='sent',operation_version=operation_version+1,updated_at=$6
			WHERE owner_id=$1 AND operation_id=$2 AND operation_version=$3`, owner, advance.OperationID,
			advance.ExpectedOperationVersion, advance.HoldVersion, advance.ObservedHoldScopeRevision, now)
	case "unknown":
		if current.Phase != "holding" && current.Phase != "reconciling" {
			return model.LogicalDeleteStatus{}, ErrLogicalDeleteConflict
		}
		err = logicalDeleteExecOne(ctx, tx, `UPDATE agent_service.logical_dialog_delete_operations SET
			phase='reconciling',effect_state='unknown',operation_version=operation_version+1,updated_at=$4
			WHERE owner_id=$1 AND operation_id=$2 AND operation_version=$3`, owner, advance.OperationID,
			advance.ExpectedOperationVersion, now)
	case "complete":
		if current.Phase != "holding" && current.Phase != "reconciling" {
			return model.LogicalDeleteStatus{}, ErrLogicalDeleteConflict
		}
		receipt := *advance.NodeReceipt
		if !nodeReceiptMatches(current, receipt) || receipt.HoldScopeRevision != current.HoldScopeRevision+1 {
			return model.LogicalDeleteStatus{}, ErrLogicalDeleteConflict
		}
		receiptJSON, _ := json.Marshal(receipt)
		if err = tombstoneLogicalDialog(ctx, tx, owner, current.OperationID, current.RequestHash, current.LogicalDialogID,
			current.ExpectedBindingVersion, current.NodeID, current.NodeDialogID, current.ExpectedDialogVersion,
			"active_node_receipt", receiptJSON, now); err == nil {
			err = logicalDeleteExecOne(ctx, tx, `UPDATE agent_service.logical_dialogs SET hold_scope_revision=$3
				WHERE owner_id=$1 AND logical_dialog_id=$2 AND hold_scope_revision=$4`, owner, current.LogicalDialogID,
				receipt.HoldScopeRevision, current.HoldScopeRevision-1)
		}
		if err == nil {
			err = logicalDeleteExecOne(ctx, tx, `UPDATE agent_service.logical_dialog_delete_operations SET
				hold_scope_revision=$4,phase='succeeded',effect_state='reconciled',operation_version=operation_version+1,
				result_code='node_tombstoned',node_receipt=$5::jsonb,updated_at=$6
				WHERE owner_id=$1 AND operation_id=$2 AND operation_version=$3`, owner, advance.OperationID,
				advance.ExpectedOperationVersion, receipt.HoldScopeRevision, receiptJSON, now)
		}
	case "reject":
		expectedLogicalRevision := current.HoldScopeRevision
		if current.Phase == "holding" {
			if advance.ObservedHoldScopeRevision != current.HoldScopeRevision+1 {
				return model.LogicalDeleteStatus{}, ErrLogicalDeleteConflict
			}
			expectedLogicalRevision = current.HoldScopeRevision - 1
		} else if current.Phase != "accepted" || advance.ObservedHoldScopeRevision != current.HoldScopeRevision {
			return model.LogicalDeleteStatus{}, ErrLogicalDeleteConflict
		}
		if err = logicalDeleteExecOne(ctx, tx, `UPDATE agent_service.logical_dialogs SET hold_scope_revision=$3
			WHERE owner_id=$1 AND logical_dialog_id=$2 AND deleted_at IS NULL AND hold_scope_revision=$4`,
			owner, current.LogicalDialogID, advance.ObservedHoldScopeRevision, expectedLogicalRevision); err == nil {
			err = logicalDeleteExecOne(ctx, tx, `UPDATE agent_service.dialog_bindings SET state='active',updated_at=$4
				WHERE owner_id=$1 AND logical_dialog_id=$2 AND binding_version=$3 AND state='deleting'`,
				owner, current.LogicalDialogID, current.ExpectedBindingVersion, now)
		}
		if err == nil {
			err = logicalDeleteExecOne(ctx, tx, `UPDATE agent_service.dialog_operation_reservations SET state='failed',updated_at=$3
				WHERE owner_id=$1 AND operation_id=$2 AND state='active'`, owner, advance.OperationID, now)
		}
		if err == nil {
			err = logicalDeleteExecOne(ctx, tx, `UPDATE agent_service.logical_dialog_delete_operations SET
				hold_scope_revision=$4,phase='failed',effect_state='failed',operation_version=operation_version+1,
				result_code=$5,updated_at=$6 WHERE owner_id=$1 AND operation_id=$2 AND operation_version=$3`,
				owner, advance.OperationID, advance.ExpectedOperationVersion, advance.ObservedHoldScopeRevision,
				advance.ResultCode, now)
		}
	}
	if err != nil {
		return model.LogicalDeleteStatus{}, errors.New("logical delete transition unavailable")
	}
	status, found, err := logicalDeleteStatusTx(ctx, tx, owner, advance.OperationID, false)
	if err != nil || !found {
		return model.LogicalDeleteStatus{}, errors.New("logical delete readback unavailable")
	}
	if err := tx.Commit(ctx); err != nil {
		return model.LogicalDeleteStatus{}, errors.New("logical delete commit unavailable")
	}
	return status, nil
}

func (s *Store) RecordDialogClosure(ctx context.Context, owner string, descriptor model.LogicalDialogClosure) error {
	if !model.ValidActor(owner) || model.ValidateLogicalDialogClosure(descriptor) != nil {
		return ErrLogicalDeleteState
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO agent_service.dialog_closure_descriptors(
		owner_id,logical_dialog_id,binding_version,node_id,node_dialog_id,dialog_version,descriptor_version,
		state,zero_pending,final_checkpoint,final_checkpoint_sha256,verified_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,'verified',true,$8::jsonb,$9,$10)`, owner, descriptor.LogicalDialogID,
		descriptor.BindingVersion, descriptor.NodeID, descriptor.NodeDialogID, descriptor.DialogVersion,
		descriptor.DescriptorVersion, descriptor.FinalCheckpoint, descriptor.FinalCheckpointSHA256, descriptor.VerifiedAt)
	if err != nil {
		return ErrLogicalDeleteConflict
	}
	return nil
}

func exactDeleteCheckpoint(ctx context.Context, tx pgx.Tx, owner string, request model.LogicalDeleteRequest, nodeID, nodeDialogID string) (model.HistoryCheckpoint, string, error) {
	var raw []byte
	var importedThrough, sourceThrough int64
	var complete bool
	err := tx.QueryRow(ctx, `SELECT source_checkpoint,imported_through,source_through,complete
		FROM agent_service.history_replica_streams
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND node_id=$3 AND node_dialog_id=$4 AND binding_generation=$5
		ORDER BY observed_at DESC,stream_id DESC LIMIT 1 FOR UPDATE`, owner, request.LogicalDialogID, nodeID,
		nodeDialogID, request.ExpectedBindingVersion).Scan(&raw, &importedThrough, &sourceThrough, &complete)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.HistoryCheckpoint{}, "", ErrLogicalDeleteNotReady
		}
		return model.HistoryCheckpoint{}, "", errors.New("logical delete checkpoint unavailable")
	}
	var checkpoint model.HistoryCheckpoint
	if json.Unmarshal(raw, &checkpoint) != nil || !complete || !checkpoint.Ready || importedThrough != sourceThrough ||
		checkpoint.ThroughSeq != sourceThrough || checkpoint.DialogVersion != request.ExpectedDialogVersion {
		return model.HistoryCheckpoint{}, "", ErrLogicalDeleteNotReady
	}
	canonical, err := json.Marshal(checkpoint)
	if err != nil {
		return model.HistoryCheckpoint{}, "", ErrLogicalDeleteNotReady
	}
	digest := sha256.Sum256(canonical)
	return checkpoint, hex.EncodeToString(digest[:]), nil
}

func exactArchiveClosure(ctx context.Context, tx pgx.Tx, owner string, request model.LogicalDeleteRequest, nodeID, nodeDialogID string,
	checkpoint model.HistoryCheckpoint, checkpointHash string) (bool, error) {
	var dialogVersion int64
	var zeroPending bool
	var hash string
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT dialog_version,zero_pending,final_checkpoint_sha256,final_checkpoint
		FROM agent_service.dialog_closure_descriptors
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND binding_version=$3 AND node_id=$4 AND node_dialog_id=$5
			AND state='verified' ORDER BY descriptor_version DESC LIMIT 1`, owner, request.LogicalDialogID,
		request.ExpectedBindingVersion, nodeID, nodeDialogID).Scan(&dialogVersion, &zeroPending, &hash, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("logical dialog closure unavailable")
	}
	var closed model.HistoryCheckpoint
	if !zeroPending || dialogVersion != request.ExpectedDialogVersion || hash != checkpointHash ||
		json.Unmarshal(raw, &closed) != nil || closed != checkpoint {
		return false, ErrLogicalDeleteNotReady
	}
	return true, nil
}

func tombstoneLogicalDialog(ctx context.Context, tx pgx.Tx, owner, operationID, requestHash, logicalDialogID string,
	bindingVersion int64, nodeID, nodeDialogID string, dialogVersion int64, source string, receipt []byte, now time.Time) error {
	event := struct {
		SchemaID        string `json:"schemaId"`
		OperationID     string `json:"operationId"`
		RequestHash     string `json:"requestHash"`
		LogicalDialogID string `json:"logicalDialogId"`
		BindingVersion  int64  `json:"bindingVersion"`
		DialogVersion   int64  `json:"dialogVersion"`
		Source          string `json:"source"`
		DeletedAt       string `json:"deletedAt"`
	}{"logical-dialog-tombstone-v1", operationID, requestHash, logicalDialogID, bindingVersion, dialogVersion, source, now.Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(event)
	digest := sha256.Sum256(raw)
	if _, err := tx.Exec(ctx, `INSERT INTO agent_service.logical_dialog_tombstones(
		owner_id,logical_dialog_id,operation_id,binding_version,node_id,node_dialog_id,dialog_version,source,
		event_version,event_sha256,receipt,deleted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,1,$9,$10::jsonb,$11)`,
		owner, logicalDialogID, operationID, bindingVersion, nodeID, nodeDialogID, dialogVersion, source,
		hex.EncodeToString(digest[:]), nullableJSON(receipt), now); err != nil {
		return errors.New("logical dialog tombstone unavailable")
	}
	if result, err := tx.Exec(ctx, `UPDATE agent_service.logical_dialogs SET deleted_at=$3
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND deleted_at IS NULL`, owner, logicalDialogID, now); err != nil || result.RowsAffected() != 1 {
		return ErrLogicalDeleteConflict
	}
	if result, err := tx.Exec(ctx, `UPDATE agent_service.dialog_bindings SET state='tombstoned',updated_at=$4
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND binding_version=$3 AND state IN ('active','deleting')`,
		owner, logicalDialogID, bindingVersion, now); err != nil || result.RowsAffected() != 1 {
		return ErrLogicalDeleteConflict
	}
	if result, err := tx.Exec(ctx, `UPDATE agent_service.dialog_operation_reservations SET state='succeeded',updated_at=$3
		WHERE owner_id=$1 AND operation_id=$2 AND state='active'`, owner, operationID, now); err != nil || result.RowsAffected() != 1 {
		return ErrLogicalDeleteConflict
	}
	return nil
}

func nullableJSON(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func logicalDeleteExecOne(ctx context.Context, tx pgx.Tx, statement string, arguments ...any) error {
	result, err := tx.Exec(ctx, statement, arguments...)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrLogicalDeleteConflict
	}
	return nil
}

func nodeReceiptMatches(status model.LogicalDeleteStatus, receipt model.LogicalDeleteNodeReceipt) bool {
	return model.ValidateLogicalDeleteNodeReceipt(receipt) == nil && receipt.OperationID == status.OperationID &&
		receipt.CoordinatorRequestHash == status.RequestHash && receipt.CommandID == status.CommandID &&
		receipt.LogicalDialogID == status.LogicalDialogID && receipt.NodeID == status.NodeID &&
		receipt.NodeDialogID == status.NodeDialogID && receipt.Epoch == status.IdentityEpoch &&
		receipt.RegistryVersion == status.RegistryVersion && receipt.BindingVersion == status.ExpectedBindingVersion &&
		receipt.DeletedDialogVersion == status.ExpectedDialogVersion+1 && receipt.HoldVersion == status.HoldVersion
}

func terminalAdvanceMatches(status model.LogicalDeleteStatus, advance model.LogicalDeleteAdvance) bool {
	switch status.Phase {
	case "succeeded":
		if status.Archived || advance.Action != "complete" || status.NodeReceipt == nil || advance.NodeReceipt == nil {
			return false
		}
		return logicalDeleteNodeReceiptsEqual(*status.NodeReceipt, *advance.NodeReceipt)
	case "failed":
		return advance.Action == "reject" && advance.ResultCode == status.ResultCode &&
			advance.ObservedHoldScopeRevision == status.HoldScopeRevision
	default:
		return false
	}
}

func logicalDeleteNodeReceiptsEqual(stored, replayed model.LogicalDeleteNodeReceipt) bool {
	storedCommand, replayedCommand := stored.CommandReceipt, replayed.CommandReceipt
	stored.CommandReceipt, replayed.CommandReceipt = nil, nil
	if !reflect.DeepEqual(stored, replayed) {
		return false
	}
	var storedValue, replayedValue any
	return json.Unmarshal(storedCommand, &storedValue) == nil && json.Unmarshal(replayedCommand, &replayedValue) == nil &&
		reflect.DeepEqual(storedValue, replayedValue)
}

type logicalDeleteQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func logicalDeleteStatusQuery(ctx context.Context, query logicalDeleteQuery, owner, operationID string, lock bool) (model.LogicalDeleteStatus, bool, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	var status model.LogicalDeleteStatus
	var receiptRaw []byte
	var updated time.Time
	err := query.QueryRow(ctx, `SELECT operation_id::text,request_hash,command_id::text,logical_dialog_id::text,
		expected_binding_version,expected_dialog_version,node_id::text,node_dialog_id::text,registry_version,
		identity_epoch,hold_scope_revision,COALESCE(hold_version,0),phase,effect_state,operation_version,archived,
		COALESCE(result_code,''),node_receipt,updated_at
		FROM agent_service.logical_dialog_delete_operations WHERE owner_id=$1 AND operation_id=$2`+suffix,
		owner, operationID).Scan(&status.OperationID, &status.RequestHash, &status.CommandID, &status.LogicalDialogID,
		&status.ExpectedBindingVersion, &status.ExpectedDialogVersion, &status.NodeID, &status.NodeDialogID,
		&status.RegistryVersion, &status.IdentityEpoch, &status.HoldScopeRevision, &status.HoldVersion, &status.Phase,
		&status.EffectState, &status.OperationVersion, &status.Archived, &status.ResultCode, &receiptRaw, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.LogicalDeleteStatus{}, false, nil
	}
	if err != nil {
		return model.LogicalDeleteStatus{}, false, errors.New("logical delete status unavailable")
	}
	status.SchemaID = model.LogicalDeleteSchemaID
	status.UpdatedAt = updated.UTC().Format(time.RFC3339Nano)
	if len(receiptRaw) != 0 {
		var receipt model.LogicalDeleteNodeReceipt
		if json.Unmarshal(receiptRaw, &receipt) != nil {
			return model.LogicalDeleteStatus{}, false, errors.New("logical delete receipt unavailable")
		}
		status.NodeReceipt = &receipt
	}
	if model.ValidateLogicalDeleteStatus(status) != nil {
		return model.LogicalDeleteStatus{}, false, errors.New("logical delete status is invalid")
	}
	return status, true, nil
}

func logicalDeleteStatusTx(ctx context.Context, tx pgx.Tx, owner, operationID string, lock bool) (model.LogicalDeleteStatus, bool, error) {
	return logicalDeleteStatusQuery(ctx, tx, owner, operationID, lock)
}
