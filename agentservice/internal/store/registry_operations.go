package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/registry"
)

type RegistryReserveResult struct {
	Status   model.RegistryOperationStatus
	Existing bool
}

func (s *Store) GetRegistryEnvelope(ctx context.Context, ownerID string) ([]byte, int64, string, error) {
	if !model.ValidActor(ownerID) {
		return nil, 0, "", errors.New("invalid registry owner")
	}
	var raw string
	var version int64
	var digest string
	err := s.pool.QueryRow(ctx, `
		SELECT registry_envelope::text,registry_version,manifest_sha256
		FROM agent_service.registry_state WHERE owner_id=$1 AND registry_envelope IS NOT NULL`, ownerID,
	).Scan(&raw, &version, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, "", ErrOperationNotFound
	}
	if err != nil || len(raw) == 0 || len(raw) > 256<<10 {
		return nil, 0, "", errors.New("registry state unavailable")
	}
	return []byte(raw), version, digest, nil
}

func (s *Store) ReserveRegistryOperation(
	ctx context.Context,
	ownerID string,
	intent model.RegistryOperationIntent,
	candidate registry.Verified,
	affectedNodeIDs []string,
	newNodeID string,
) (RegistryReserveResult, error) {
	if !model.ValidActor(ownerID) || model.ValidateRegistryOperationIntent(intent) != nil || candidate.Manifest.OwnerID != ownerID ||
		candidate.Manifest.RegistryVersion != intent.Expected.RegistryVersion+1 || len(affectedNodeIDs) == 0 ||
		len(affectedNodeIDs) > 1000 || !slices.IsSorted(affectedNodeIDs) ||
		(newNodeID == "") != (intent.NewNodeHostID == nil) {
		return RegistryReserveResult{}, errors.New("invalid registry operation")
	}
	for index, nodeID := range affectedNodeIDs {
		if !model.ValidUUID(nodeID) || index > 0 && nodeID == affectedNodeIDs[index-1] {
			return RegistryReserveResult{}, errors.New("invalid registry operation target")
		}
	}
	if newNodeID != "" && (!model.ValidUUID(newNodeID) || !slices.Contains(affectedNodeIDs, newNodeID)) {
		return RegistryReserveResult{}, errors.New("invalid added registry node")
	}
	requestHash, err := model.RegistryOperationRequestHash(intent)
	if err != nil {
		return RegistryReserveResult{}, err
	}
	candidateIntent := intent
	candidateIntent.Registry = candidate.Envelope
	candidateRequestHash, err := model.RegistryOperationRequestHash(candidateIntent)
	if err != nil || candidateRequestHash != requestHash {
		return RegistryReserveResult{}, errors.New("registry operation envelope mismatch")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RegistryReserveResult{}, errors.New("registry operation transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "agent-service-import:"+ownerID); err != nil {
		return RegistryReserveResult{}, errors.New("registry operation owner lock unavailable")
	}
	var storedKind, storedHash string
	err = tx.QueryRow(ctx, `
		SELECT operation_kind,request_hash FROM agent_service.operation_ids
		WHERE owner_id=$1 AND operation_id=$2`, ownerID, intent.OperationID,
	).Scan(&storedKind, &storedHash)
	if err == nil {
		if storedKind != "registry.install" || storedHash != requestHash {
			return RegistryReserveResult{}, ErrOperationConflict
		}
		status, found, readErr := readRegistryOperationStatus(ctx, tx, ownerID, intent.OperationID)
		if readErr != nil || !found || status.Receipt.RequestHash != requestHash {
			return RegistryReserveResult{}, errors.New("registry operation identity unavailable")
		}
		return RegistryReserveResult{Status: status, Existing: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RegistryReserveResult{}, errors.New("registry operation identity unavailable")
	}
	var currentVersion int64
	var currentHash string
	err = tx.QueryRow(ctx, `
		SELECT registry_version,manifest_sha256
		FROM agent_service.registry_state
		WHERE owner_id=$1 AND registry_envelope IS NOT NULL FOR UPDATE`, ownerID,
	).Scan(&currentVersion, &currentHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegistryReserveResult{}, ErrOperationNotFound
	}
	if err != nil {
		return RegistryReserveResult{}, errors.New("registry state unavailable")
	}
	if currentVersion != intent.Expected.RegistryVersion || currentHash != intent.Expected.RegistrySHA256 {
		return RegistryReserveResult{}, ErrOperationConflict
	}
	var existingTargets int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM agent_service.instances
		WHERE owner_id=$1 AND node_id=ANY($2::uuid[])`, ownerID, affectedNodeIDs,
	).Scan(&existingTargets); err != nil || existingTargets != len(affectedNodeIDs)-boolInt(newNodeID != "") {
		return RegistryReserveResult{}, ErrOperationConflict
	}
	if newNodeID != "" {
		var hostExists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM agent_service.hosts WHERE owner_id=$1 AND host_id=$2::uuid)`,
			ownerID, *intent.NewNodeHostID,
		).Scan(&hostExists); err != nil || !hostExists {
			return RegistryReserveResult{}, ErrOperationConflict
		}
	}
	var active bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM agent_service.operations
			WHERE owner_id=$1 AND node_id=ANY($2::uuid[]) AND phase NOT IN ('succeeded','failed')
		)`, ownerID, affectedNodeIDs).Scan(&active); err != nil || active {
		return RegistryReserveResult{}, ErrOperationConflict
	}
	acceptedAt := s.now().UTC().Truncate(time.Microsecond)
	intentJSON, err := json.Marshal(intent)
	if err != nil {
		return RegistryReserveResult{}, errors.New("registry operation intent unavailable")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_service.operation_ids(owner_id,operation_id,operation_kind,request_hash)
		VALUES($1,$2,'registry.install',$3)`, ownerID, intent.OperationID, requestHash,
	); err != nil {
		if uniqueViolation(err) {
			return RegistryReserveResult{}, ErrOperationConflict
		}
		return RegistryReserveResult{}, errors.New("registry operation identity unavailable")
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO agent_service.registry_operations(
			owner_id,operation_id,request_hash,intent,expected_registry_version,expected_registry_sha256,
			candidate_registry_version,candidate_registry_sha256,affected_node_ids,new_node_host_id,accepted_at,updated_at
		) VALUES($1,$2,$3,$4::json,$5,$6,$7,$8,$9::uuid[],$10::uuid,$11,$11)`,
		ownerID, intent.OperationID, requestHash, intentJSON, intent.Expected.RegistryVersion, intent.Expected.RegistrySHA256,
		candidate.Manifest.RegistryVersion, candidate.ManifestSHA256, affectedNodeIDs, intent.NewNodeHostID, acceptedAt,
	)
	if err != nil {
		if uniqueViolation(err) {
			return RegistryReserveResult{}, ErrOperationConflict
		}
		return RegistryReserveResult{}, errors.New("registry operation intent unavailable")
	}
	if err := tx.Commit(ctx); err != nil {
		return RegistryReserveResult{}, errors.New("registry operation commit unknown")
	}
	status := registryOperationStatus(intent.OperationID, requestHash, intent.Expected, candidate, affectedNodeIDs, acceptedAt)
	return RegistryReserveResult{Status: status}, nil
}

func (s *Store) GetRegistryOperation(ctx context.Context, ownerID, operationID string) (model.RegistryOperationStatus, error) {
	if !model.ValidActor(ownerID) || !model.ValidActor(operationID) {
		return model.RegistryOperationStatus{}, errors.New("invalid registry operation query")
	}
	status, found, err := readRegistryOperationStatus(ctx, s.pool, ownerID, operationID)
	if err != nil {
		return model.RegistryOperationStatus{}, err
	}
	if !found {
		return model.RegistryOperationStatus{}, ErrOperationNotFound
	}
	return status, nil
}

func (s *Store) GetRegistryOperationIntent(ctx context.Context, ownerID, operationID string) (model.RegistryOperationIntent, error) {
	if !model.ValidActor(ownerID) || !model.ValidActor(operationID) {
		return model.RegistryOperationIntent{}, errors.New("invalid registry operation query")
	}
	var raw []byte
	if err := s.pool.QueryRow(ctx, `
		SELECT intent::text FROM agent_service.registry_operations
		WHERE owner_id=$1 AND operation_id=$2`, ownerID, operationID,
	).Scan(&raw); errors.Is(err, pgx.ErrNoRows) {
		return model.RegistryOperationIntent{}, ErrOperationNotFound
	} else if err != nil {
		return model.RegistryOperationIntent{}, errors.New("registry operation unavailable")
	}
	var intent model.RegistryOperationIntent
	if json.Unmarshal(raw, &intent) != nil || model.ValidateRegistryOperationIntent(intent) != nil {
		return model.RegistryOperationIntent{}, errors.New("registry operation intent unavailable")
	}
	requestHash, err := model.RegistryOperationRequestHash(intent)
	if err != nil {
		return model.RegistryOperationIntent{}, errors.New("registry operation intent unavailable")
	}
	var storedHash string
	if err := s.pool.QueryRow(ctx, `
		SELECT request_hash FROM agent_service.operation_ids
		WHERE owner_id=$1 AND operation_id=$2 AND operation_kind='registry.install'`, ownerID, operationID,
	).Scan(&storedHash); err != nil || storedHash != requestHash {
		return model.RegistryOperationIntent{}, errors.New("registry operation intent unavailable")
	}
	return intent, nil
}

func (s *Store) MarkRegistryOperationSent(ctx context.Context, ownerID string, command model.RegistryOperationCommand) (model.RegistryOperationStatus, error) {
	return s.transitionRegistryOperation(ctx, ownerID, command, "sent")
}

func (s *Store) MarkRegistryOperationUnknown(ctx context.Context, ownerID string, command model.RegistryOperationCommand) (model.RegistryOperationStatus, error) {
	return s.transitionRegistryOperation(ctx, ownerID, command, "unknown")
}

func (s *Store) FailRegistryOperation(ctx context.Context, ownerID string, failure model.RegistryOperationFailure) (model.RegistryOperationStatus, error) {
	if !model.ValidActor(ownerID) || model.ValidateRegistryOperationFailure(failure) != nil {
		return model.RegistryOperationStatus{}, errors.New("invalid registry operation failure")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.RegistryOperationStatus{}, errors.New("registry operation transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "agent-service-import:"+ownerID); err != nil {
		return model.RegistryOperationStatus{}, errors.New("registry operation owner lock unavailable")
	}
	status, found, err := readRegistryOperationStatusForUpdate(ctx, tx, ownerID, failure.OperationID)
	if err != nil {
		return model.RegistryOperationStatus{}, err
	}
	if !found {
		return model.RegistryOperationStatus{}, ErrOperationNotFound
	}
	if status.Receipt.RequestHash != failure.RequestHash {
		return model.RegistryOperationStatus{}, ErrOperationConflict
	}
	if status.Phase == "failed" {
		return status, nil
	}
	if status.Phase == "succeeded" || status.EffectState != "sent" ||
		status.OperationVersion != failure.OperationVersion || status.OperationVersion >= model.MaximumSafeInt {
		return model.RegistryOperationStatus{}, ErrOperationConflict
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	if _, err := tx.Exec(ctx, `
		UPDATE agent_service.registry_operations
		SET phase='failed',effect_state='failed',operation_version=operation_version+1,result_code=$3,updated_at=$4
		WHERE owner_id=$1 AND operation_id=$2`, ownerID, failure.OperationID, failure.ResultCode, now,
	); err != nil {
		return model.RegistryOperationStatus{}, errors.New("registry operation failure unavailable")
	}
	status.Phase, status.EffectState, status.OperationVersion = "failed", "failed", status.OperationVersion+1
	status.ResultCode, status.UpdatedAt = &failure.ResultCode, model.FormatOperationTime(now)
	if err := tx.Commit(ctx); err != nil {
		return model.RegistryOperationStatus{}, errors.New("registry operation failure commit unknown")
	}
	return status, nil
}

func (s *Store) transitionRegistryOperation(ctx context.Context, ownerID string, command model.RegistryOperationCommand, target string) (model.RegistryOperationStatus, error) {
	if !model.ValidActor(ownerID) || model.ValidateRegistryOperationCommand(command) != nil || (target != "sent" && target != "unknown") {
		return model.RegistryOperationStatus{}, errors.New("invalid registry operation transition")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.RegistryOperationStatus{}, errors.New("registry operation transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	status, found, err := readRegistryOperationStatusForUpdate(ctx, tx, ownerID, command.OperationID)
	if err != nil {
		return model.RegistryOperationStatus{}, err
	}
	if !found {
		return model.RegistryOperationStatus{}, ErrOperationNotFound
	}
	if status.Receipt.RequestHash != command.RequestHash {
		return model.RegistryOperationStatus{}, ErrOperationConflict
	}
	if status.Phase == "succeeded" || status.Phase == "failed" || target == "sent" && (status.EffectState == "sent" || status.EffectState == "unknown") ||
		target == "unknown" && status.EffectState == "unknown" {
		return status, nil
	}
	if status.OperationVersion != command.OperationVersion || target == "sent" && status.EffectState != "not_sent" ||
		target == "unknown" && status.EffectState != "sent" || status.OperationVersion >= model.MaximumSafeInt {
		return model.RegistryOperationStatus{}, ErrOperationConflict
	}
	phase, effect := "applying", "sent"
	if target == "unknown" {
		phase, effect = "reconciling", "unknown"
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	if _, err := tx.Exec(ctx, `
		UPDATE agent_service.registry_operations
		SET phase=$3,effect_state=$4,operation_version=operation_version+1,updated_at=$5
		WHERE owner_id=$1 AND operation_id=$2`, ownerID, command.OperationID, phase, effect, now,
	); err != nil {
		return model.RegistryOperationStatus{}, errors.New("registry operation update unavailable")
	}
	status.Phase, status.EffectState, status.OperationVersion, status.UpdatedAt = phase, effect, status.OperationVersion+1, model.FormatOperationTime(now)
	if err := tx.Commit(ctx); err != nil {
		return model.RegistryOperationStatus{}, errors.New("registry operation update commit unknown")
	}
	return status, nil
}

func (s *Store) FinishRegistryOperation(
	ctx context.Context,
	ownerID string,
	finish model.RegistryOperationFinish,
	candidate registry.Verified,
) (model.RegistryOperationStatus, error) {
	if !model.ValidActor(ownerID) || model.ValidateRegistryOperationFinish(finish) != nil || candidate.Manifest.OwnerID != ownerID ||
		candidate.Manifest.RegistryVersion != finish.RegistryVersion || candidate.ManifestSHA256 != finish.RegistrySHA256 {
		return model.RegistryOperationStatus{}, errors.New("invalid registry operation result")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.RegistryOperationStatus{}, errors.New("registry operation transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "agent-service-import:"+ownerID); err != nil {
		return model.RegistryOperationStatus{}, errors.New("registry operation owner lock unavailable")
	}
	status, found, err := readRegistryOperationStatusForUpdate(ctx, tx, ownerID, finish.OperationID)
	if err != nil {
		return model.RegistryOperationStatus{}, err
	}
	if !found {
		return model.RegistryOperationStatus{}, ErrOperationNotFound
	}
	if status.Receipt.RequestHash != finish.RequestHash || status.Receipt.CandidateRegistryVersion != finish.RegistryVersion ||
		status.Receipt.CandidateRegistrySHA256 != finish.RegistrySHA256 {
		return model.RegistryOperationStatus{}, ErrOperationConflict
	}
	if status.Phase == "succeeded" {
		return status, nil
	}
	if status.OperationVersion != finish.OperationVersion || (status.EffectState != "sent" && status.EffectState != "unknown") ||
		status.EffectState == "unknown" && finish.EffectState != "reconciled" ||
		status.OperationVersion >= model.MaximumSafeInt {
		return model.RegistryOperationStatus{}, ErrOperationConflict
	}
	var intentText string
	if err := tx.QueryRow(ctx, `SELECT intent::text FROM agent_service.registry_operations WHERE owner_id=$1 AND operation_id=$2`, ownerID, finish.OperationID).Scan(&intentText); err != nil {
		return model.RegistryOperationStatus{}, errors.New("registry operation intent unavailable")
	}
	var intent model.RegistryOperationIntent
	if json.Unmarshal([]byte(intentText), &intent) != nil {
		return model.RegistryOperationStatus{}, errors.New("registry operation intent unavailable")
	}
	requestHash, hashErr := model.RegistryOperationRequestHash(intent)
	if hashErr != nil || requestHash != finish.RequestHash {
		return model.RegistryOperationStatus{}, errors.New("registry operation intent unavailable")
	}
	var currentVersion int64
	var currentHash string
	if err := tx.QueryRow(ctx, `
		SELECT registry_version,manifest_sha256 FROM agent_service.registry_state
		WHERE owner_id=$1 FOR UPDATE`, ownerID,
	).Scan(&currentVersion, &currentHash); err != nil ||
		currentVersion != status.Receipt.ExpectedRegistryVersion || currentHash != status.Receipt.ExpectedRegistrySHA256 {
		return model.RegistryOperationStatus{}, ErrOperationConflict
	}
	for _, node := range candidate.Manifest.Nodes {
		var hostID string
		var currentEpoch int64
		var currentBinding *string
		var currentProjected bool
		existing := true
		err := tx.QueryRow(ctx, `
			SELECT host_id::text,registration_epoch,registration_binding_sha256,registration_projected
			FROM agent_service.instances WHERE owner_id=$1 AND node_id=$2 FOR UPDATE`, ownerID, node.NodeID,
		).Scan(&hostID, &currentEpoch, &currentBinding, &currentProjected)
		if errors.Is(err, pgx.ErrNoRows) {
			if intent.NewNodeHostID == nil || !slices.Contains(status.Receipt.AffectedNodeIDs, node.NodeID) {
				return model.RegistryOperationStatus{}, ErrOperationConflict
			}
			existing = false
			hostID = *intent.NewNodeHostID
		} else if err != nil {
			return model.RegistryOperationStatus{}, errors.New("registry operation target unavailable")
		}
		binding, err := registrationBindingSHA(node, hostID, candidate.Manifest.Mode, node.Compatibility)
		if err != nil {
			return model.RegistryOperationStatus{}, errors.New("registry operation target unavailable")
		}
		command, err := tx.Exec(ctx, `
			INSERT INTO agent_service.instances(
				owner_id,node_id,host_id,name,engine,registry_mode,registration_mode,registry_version,manifest_sha256,
				registration_revision,registration_epoch,registration_binding_sha256,registration_projected
			) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,true)
			ON CONFLICT(owner_id,node_id) DO UPDATE SET
				name=EXCLUDED.name,engine=EXCLUDED.engine,registry_mode=EXCLUDED.registry_mode,
				registration_mode=EXCLUDED.registration_mode,registry_version=EXCLUDED.registry_version,
				manifest_sha256=EXCLUDED.manifest_sha256,registration_revision=EXCLUDED.registration_revision,
				registration_epoch=EXCLUDED.registration_epoch,registration_binding_sha256=EXCLUDED.registration_binding_sha256,
				registration_projected=true,updated_at=clock_timestamp()`,
			ownerID, node.NodeID, hostID, node.Name, node.Adapter, candidate.Manifest.Mode, node.Compatibility,
			candidate.Manifest.RegistryVersion, candidate.ManifestSHA256, node.RegistrationRevision, node.RegistrationEpoch, binding,
		)
		if err != nil || command.RowsAffected() != 1 {
			return model.RegistryOperationStatus{}, errors.New("registry operation target update unavailable")
		}
		identityChanged := existing && currentProjected &&
			(currentEpoch != node.RegistrationEpoch || currentBinding == nil || *currentBinding != binding)
		if identityChanged {
			reset, err := tx.Exec(ctx, `
				UPDATE agent_service.instances
				SET process_state='unknown',connection_state='unknown',readiness_state='unknown',occupancy_state='unknown',
					observed_at=NULL,observation_source=NULL,pending_count=NULL,updated_at=clock_timestamp()
				WHERE owner_id=$1 AND node_id=$2`, ownerID, node.NodeID,
			)
			if err != nil || reset.RowsAffected() != 1 {
				return model.RegistryOperationStatus{}, errors.New("registry operation observation reset unavailable")
			}
		}
	}
	stateUpdate, err := tx.Exec(ctx, `
		UPDATE agent_service.registry_state
		SET registry_version=$2,manifest_sha256=$3,node_count=$4,registry_schema_id='harness-router-registry-v1',
			registry_envelope=$5::json,imported_at=clock_timestamp()
		WHERE owner_id=$1`, ownerID, candidate.Manifest.RegistryVersion, candidate.ManifestSHA256, len(candidate.Manifest.Nodes), candidate.Envelope,
	)
	if err != nil || stateUpdate.RowsAffected() != 1 {
		return model.RegistryOperationStatus{}, errors.New("registry operation state update unavailable")
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	resultCode := "registry:" + candidate.ManifestSHA256
	if _, err := tx.Exec(ctx, `
		UPDATE agent_service.registry_operations
		SET phase='succeeded',effect_state=$3,operation_version=operation_version+1,result_code=$4,updated_at=$5
		WHERE owner_id=$1 AND operation_id=$2`, ownerID, finish.OperationID, finish.EffectState, resultCode, now,
	); err != nil {
		return model.RegistryOperationStatus{}, errors.New("registry operation result unavailable")
	}
	status.Phase, status.EffectState, status.OperationVersion = "succeeded", finish.EffectState, status.OperationVersion+1
	status.ResultCode, status.UpdatedAt = &resultCode, model.FormatOperationTime(now)
	if err := tx.Commit(ctx); err != nil {
		return model.RegistryOperationStatus{}, errors.New("registry operation result commit unknown")
	}
	return status, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func registryOperationStatus(
	operationID, requestHash string,
	expected model.RegistryExpected,
	candidate registry.Verified,
	affected []string,
	acceptedAt time.Time,
) model.RegistryOperationStatus {
	return model.RegistryOperationStatus{
		SchemaID: model.RegistryOperationStatusSchemaID,
		Receipt: model.RegistryOperationReceipt{
			SchemaID: model.RegistryOperationReceiptSchemaID, OperationID: operationID, RequestHash: requestHash,
			ExpectedRegistryVersion: expected.RegistryVersion, ExpectedRegistrySHA256: expected.RegistrySHA256,
			CandidateRegistryVersion: candidate.Manifest.RegistryVersion, CandidateRegistrySHA256: candidate.ManifestSHA256,
			AffectedNodeIDs: append([]string(nil), affected...), AcceptedAt: model.FormatOperationTime(acceptedAt),
		},
		Phase: "accepted", EffectState: "not_sent", OperationVersion: 1, UpdatedAt: model.FormatOperationTime(acceptedAt),
	}
}

type registryOperationQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readRegistryOperationStatus(ctx context.Context, query registryOperationQuerier, ownerID, operationID string) (model.RegistryOperationStatus, bool, error) {
	return scanRegistryOperationStatus(query.QueryRow(ctx, `
		SELECT request_hash,expected_registry_version,expected_registry_sha256,candidate_registry_version,candidate_registry_sha256,
			ARRAY(SELECT value::text FROM unnest(affected_node_ids) value ORDER BY value),accepted_at,
			phase,effect_state,operation_version,updated_at,result_code
		FROM agent_service.registry_operations WHERE owner_id=$1 AND operation_id=$2`, ownerID, operationID), operationID)
}

func readRegistryOperationStatusForUpdate(ctx context.Context, query registryOperationQuerier, ownerID, operationID string) (model.RegistryOperationStatus, bool, error) {
	return scanRegistryOperationStatus(query.QueryRow(ctx, `
		SELECT request_hash,expected_registry_version,expected_registry_sha256,candidate_registry_version,candidate_registry_sha256,
			ARRAY(SELECT value::text FROM unnest(affected_node_ids) value ORDER BY value),accepted_at,
			phase,effect_state,operation_version,updated_at,result_code
		FROM agent_service.registry_operations WHERE owner_id=$1 AND operation_id=$2 FOR UPDATE`, ownerID, operationID), operationID)
}

func scanRegistryOperationStatus(row pgx.Row, operationID string) (model.RegistryOperationStatus, bool, error) {
	var status model.RegistryOperationStatus
	status.SchemaID = model.RegistryOperationStatusSchemaID
	status.Receipt.SchemaID = model.RegistryOperationReceiptSchemaID
	status.Receipt.OperationID = operationID
	var acceptedAt, updatedAt time.Time
	err := row.Scan(
		&status.Receipt.RequestHash, &status.Receipt.ExpectedRegistryVersion, &status.Receipt.ExpectedRegistrySHA256,
		&status.Receipt.CandidateRegistryVersion, &status.Receipt.CandidateRegistrySHA256, &status.Receipt.AffectedNodeIDs,
		&acceptedAt, &status.Phase, &status.EffectState, &status.OperationVersion, &updatedAt, &status.ResultCode,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.RegistryOperationStatus{}, false, nil
	}
	if err != nil {
		return model.RegistryOperationStatus{}, false, errors.New("registry operation unavailable")
	}
	status.Receipt.AcceptedAt, status.UpdatedAt = model.FormatOperationTime(acceptedAt), model.FormatOperationTime(updatedAt)
	if model.ValidateRegistryOperationStatus(status) != nil {
		return model.RegistryOperationStatus{}, false, errors.New("invalid registry operation state")
	}
	return status, true, nil
}
