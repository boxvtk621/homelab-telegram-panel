package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

var (
	ErrOperationConflict = errors.New("operation conflict")
	ErrOperationNotFound = errors.New("operation not found")
	ErrStaleWorker       = errors.New("stale operation worker")
)

type AcceptResult struct {
	Receipt  model.OperationReceipt
	Existing bool
}

type OperationClaim struct {
	OperationID string
	RequestHash string
	NodeID      string
	WorkerID    string
	Token       string
	Generation  int64
	Version     int64
	ExpiresAt   time.Time
}

type ClaimedOperation struct {
	Intent      model.OperationIntent
	Claim       OperationClaim
	EffectState string
}

type OperationUpdate struct {
	Phase       string
	EffectState string
	ResultCode  *string
}

func (s *Store) AcceptOperation(ctx context.Context, ownerID string, intent model.OperationIntent) (AcceptResult, error) {
	if !model.ValidActor(ownerID) {
		return AcceptResult{}, errors.New("invalid operation owner")
	}
	requestHash, err := model.OperationRequestHash(intent)
	if err != nil {
		return AcceptResult{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return AcceptResult{}, errors.New("operation transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "agent-service-import:"+ownerID); err != nil {
		return AcceptResult{}, errors.New("operation owner lock unavailable")
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "agent-operation:"+ownerID+":"+intent.OperationID); err != nil {
		return AcceptResult{}, errors.New("operation lock unavailable")
	}
	var storedKind, storedHash string
	err = tx.QueryRow(ctx, `
		SELECT operation_kind,request_hash FROM agent_service.operation_ids
		WHERE owner_id=$1 AND operation_id=$2`, ownerID, intent.OperationID,
	).Scan(&storedKind, &storedHash)
	if err == nil {
		if storedKind != intent.Kind || storedHash != requestHash {
			return AcceptResult{}, ErrOperationConflict
		}
		receipt, operationHash, found, readErr := readOperationReceipt(ctx, tx, ownerID, intent.OperationID)
		if readErr != nil || !found || operationHash != requestHash {
			return AcceptResult{}, errors.New("operation identity unavailable")
		}
		return AcceptResult{Receipt: receipt, Existing: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AcceptResult{}, errors.New("operation identity unavailable")
	}

	var registrationRevision, registrationEpoch, generation int64
	var hostID, registryMode, registrationMode string
	var registrationBindingSHA256 *string
	err = tx.QueryRow(ctx, `
		SELECT host_id::text,registration_revision,registration_epoch,operation_generation,
			registry_mode,registration_mode,registration_binding_sha256
		FROM agent_service.instances
		WHERE owner_id=$1 AND node_id=$2
		FOR UPDATE`, ownerID, intent.Target.NodeID).Scan(
		&hostID, &registrationRevision, &registrationEpoch, &generation,
		&registryMode, &registrationMode, &registrationBindingSHA256,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AcceptResult{}, ErrOperationNotFound
	}
	if err != nil {
		return AcceptResult{}, errors.New("operation target unavailable")
	}
	var registryOperationActive bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM agent_service.registry_operations
			WHERE owner_id=$1 AND $2::uuid=ANY(affected_node_ids) AND phase NOT IN ('succeeded','failed')
		)`, ownerID, intent.Target.NodeID).Scan(&registryOperationActive); err != nil {
		return AcceptResult{}, errors.New("operation target unavailable")
	}
	if registrationBindingSHA256 == nil || hostID != intent.Target.HostID ||
		registrationRevision != intent.Target.RegistrationRevision || registrationEpoch != intent.Target.RegistrationEpoch ||
		generation != intent.Target.Generation || generation >= model.MaximumSafeInt ||
		registryOperationActive || (intent.Kind == "adapter.fixture" && (registryMode != "fixture" || registrationMode != "compatible")) {
		return AcceptResult{}, ErrOperationConflict
	}
	var activeID string
	err = tx.QueryRow(ctx, `
		SELECT operation_id FROM agent_service.operations
		WHERE owner_id=$1 AND node_id=$2 AND phase NOT IN ('succeeded','failed')
		LIMIT 1`, ownerID, intent.Target.NodeID).Scan(&activeID)
	if err == nil {
		return AcceptResult{}, ErrOperationConflict
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AcceptResult{}, errors.New("operation target unavailable")
	}

	acceptedAt := s.now().UTC().Truncate(time.Microsecond)
	acceptedGeneration := generation + 1
	resources, err := json.Marshal(intent.Step.ResourceIDs)
	if err != nil {
		return AcceptResult{}, errors.New("operation resources unavailable")
	}
	intentJSON, err := json.Marshal(intent)
	if err != nil {
		return AcceptResult{}, errors.New("operation intent unavailable")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_service.operation_ids(owner_id,operation_id,operation_kind,request_hash)
		VALUES($1,$2,$3,$4)`, ownerID, intent.OperationID, intent.Kind, requestHash,
	); err != nil {
		if uniqueViolation(err) {
			return AcceptResult{}, ErrOperationConflict
		}
		return AcceptResult{}, errors.New("operation identity unavailable")
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO agent_service.operations(
			owner_id,operation_id,request_hash,intent,kind,node_id,host_id,expected_registration_revision,
			expected_registration_epoch,expected_registration_binding_sha256,
			expected_generation,generation,accepted_at,updated_at
		) VALUES($1,$2,$3,$4::jsonb,$5,$6,$7,$8,$9,$10,$11,$12,$13,$13)`,
		ownerID, intent.OperationID, requestHash, intentJSON, intent.Kind, intent.Target.NodeID, intent.Target.HostID,
		intent.Target.RegistrationRevision, intent.Target.RegistrationEpoch, *registrationBindingSHA256,
		intent.Target.Generation, acceptedGeneration, acceptedAt,
	)
	if err != nil {
		if uniqueViolation(err) {
			return AcceptResult{}, ErrOperationConflict
		}
		return AcceptResult{}, errors.New("operation intent unavailable")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_service.operation_steps(
			owner_id,operation_id,step_id,action,generation,request_hash,resource_ids,state,updated_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,'pending',$8)`,
		ownerID, intent.OperationID, intent.Step.StepID, intent.Step.Action,
		acceptedGeneration, requestHash, resources, acceptedAt,
	); err != nil {
		return AcceptResult{}, errors.New("operation step unavailable")
	}
	command, err := tx.Exec(ctx, `
		UPDATE agent_service.instances SET operation_generation=$3,updated_at=$4
		WHERE owner_id=$1 AND node_id=$2 AND operation_generation=$5 AND host_id=$6
			AND registration_revision=$7 AND registration_epoch=$8 AND registration_binding_sha256=$9`,
		ownerID, intent.Target.NodeID, acceptedGeneration, acceptedAt,
		intent.Target.Generation, intent.Target.HostID, intent.Target.RegistrationRevision,
		intent.Target.RegistrationEpoch, *registrationBindingSHA256,
	)
	if err != nil || command.RowsAffected() != 1 {
		return AcceptResult{}, ErrOperationConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return AcceptResult{}, errors.New("operation commit unknown")
	}
	return AcceptResult{Receipt: operationReceipt(intent.OperationID, requestHash, intent.Target, acceptedGeneration, acceptedAt)}, nil
}

func (s *Store) GetOperation(ctx context.Context, ownerID, operationID string) (model.OperationStatus, error) {
	if !model.ValidActor(ownerID) || !model.ValidActor(operationID) {
		return model.OperationStatus{}, errors.New("invalid operation query")
	}
	return readOperationStatus(ctx, s.pool, ownerID, operationID)
}

func (s *Store) GetOperationTarget(ctx context.Context, ownerID, nodeID string) (model.OperationTargetStatus, error) {
	if !model.ValidActor(ownerID) || !model.ValidUUID(nodeID) {
		return model.OperationTargetStatus{}, errors.New("invalid operation target query")
	}
	var value model.OperationTargetStatus
	value.SchemaID = model.OperationTargetSchemaID
	value.Target.NodeID = nodeID
	var binding *string
	err := s.pool.QueryRow(ctx, `
		SELECT host_id::text,registration_revision,registration_epoch,operation_generation,
			registry_mode,registration_mode,registration_binding_sha256
		FROM agent_service.instances WHERE owner_id=$1 AND node_id=$2`, ownerID, nodeID,
	).Scan(&value.Target.HostID, &value.Target.RegistrationRevision, &value.Target.RegistrationEpoch,
		&value.Target.Generation, &value.RegistryMode, &value.RegistrationMode, &binding)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.OperationTargetStatus{}, ErrOperationNotFound
	}
	if err != nil || binding == nil || model.ValidateOperationTargetStatus(value) != nil {
		return model.OperationTargetStatus{}, errors.New("operation target unavailable")
	}
	return value, nil
}

func (s *Store) ClaimOperation(ctx context.Context, ownerID, operationID, workerID string, lease time.Duration) (OperationClaim, error) {
	if !model.ValidActor(ownerID) || !model.ValidActor(operationID) || !model.ValidActor(workerID) || lease < time.Second || lease > 5*time.Minute {
		return OperationClaim{}, errors.New("invalid operation claim")
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return OperationClaim{}, errors.New("operation transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var phase, nodeID, requestHash string
	var generation, version, expectedRevision, currentRevision, expectedEpoch, currentEpoch int64
	var expectedHostID, currentHostID, expectedBinding, currentBinding string
	var currentWorker, currentToken *string
	var currentExpiry *time.Time
	err = tx.QueryRow(ctx, `
		SELECT o.phase,o.request_hash,o.node_id::text,o.generation,o.operation_version,o.worker_id,o.worker_token::text,o.lease_expires_at,
			o.host_id::text,i.host_id::text,o.expected_registration_revision,i.registration_revision,
			o.expected_registration_epoch,i.registration_epoch,
			o.expected_registration_binding_sha256,i.registration_binding_sha256
		FROM agent_service.operations o
		JOIN agent_service.instances i ON i.owner_id=o.owner_id AND i.node_id=o.node_id
		WHERE o.owner_id=$1 AND o.operation_id=$2 FOR UPDATE OF o,i`, ownerID, operationID,
	).Scan(&phase, &requestHash, &nodeID, &generation, &version, &currentWorker, &currentToken, &currentExpiry,
		&expectedHostID, &currentHostID, &expectedRevision, &currentRevision,
		&expectedEpoch, &currentEpoch, &expectedBinding, &currentBinding)
	if errors.Is(err, pgx.ErrNoRows) {
		return OperationClaim{}, ErrOperationNotFound
	}
	if err != nil {
		return OperationClaim{}, errors.New("operation unavailable")
	}
	if model.TerminalOperationPhase(phase) || expectedHostID != currentHostID || currentRevision != expectedRevision ||
		currentEpoch != expectedEpoch || currentBinding != expectedBinding {
		return OperationClaim{}, ErrOperationConflict
	}
	if currentWorker != nil && currentToken != nil && currentExpiry != nil && currentExpiry.After(now) {
		if *currentWorker != workerID {
			return OperationClaim{}, ErrOperationConflict
		}
		return OperationClaim{OperationID: operationID, RequestHash: requestHash, NodeID: nodeID, WorkerID: workerID, Token: *currentToken, Generation: generation, Version: version, ExpiresAt: *currentExpiry}, nil
	}
	if version >= model.MaximumSafeInt {
		return OperationClaim{}, ErrOperationConflict
	}
	token, err := s.newUUID()
	if err != nil {
		return OperationClaim{}, errors.New("operation worker identity unavailable")
	}
	expires := now.Add(lease)
	nextPhase := phase
	if phase == "accepted" {
		nextPhase = "validating"
	}
	version++
	if _, err := tx.Exec(ctx, `
		UPDATE agent_service.operations
		SET phase=$3,worker_id=$4,worker_token=$5::uuid,lease_expires_at=$6,
			operation_version=$7,updated_at=$8
		WHERE owner_id=$1 AND operation_id=$2`,
		ownerID, operationID, nextPhase, workerID, token, expires, version, now,
	); err != nil {
		return OperationClaim{}, errors.New("operation claim unavailable")
	}
	if err := tx.Commit(ctx); err != nil {
		return OperationClaim{}, errors.New("operation claim commit unknown")
	}
	return OperationClaim{OperationID: operationID, RequestHash: requestHash, NodeID: nodeID, WorkerID: workerID, Token: token, Generation: generation, Version: version, ExpiresAt: expires}, nil
}

// ClaimNextOperation reconstructs work from the durable intent rather than
// from an in-memory queue. The partial unique index guarantees at most one
// unfinished operation for a node; ClaimOperation arbitrates concurrent
// workers with the exact DB lease and operation version.
func (s *Store) ClaimNextOperation(ctx context.Context, ownerID, nodeID, workerID string, lease time.Duration) (ClaimedOperation, error) {
	if !model.ValidActor(ownerID) || !model.ValidUUID(nodeID) || !model.ValidActor(workerID) {
		return ClaimedOperation{}, errors.New("invalid operation claim")
	}
	var operationID string
	err := s.pool.QueryRow(ctx, `
		SELECT operation_id FROM agent_service.operations
		WHERE owner_id=$1 AND node_id=$2 AND phase NOT IN ('succeeded','failed')
		ORDER BY accepted_at,operation_id LIMIT 1`, ownerID, nodeID,
	).Scan(&operationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ClaimedOperation{}, ErrOperationNotFound
	}
	if err != nil {
		return ClaimedOperation{}, errors.New("operation unavailable")
	}
	claim, err := s.ClaimOperation(ctx, ownerID, operationID, workerID, lease)
	if err != nil {
		return ClaimedOperation{}, err
	}
	var raw []byte
	var storedHash, effectState string
	if err := s.pool.QueryRow(ctx, `
		SELECT intent::text,request_hash,effect_state FROM agent_service.operations
		WHERE owner_id=$1 AND operation_id=$2`, ownerID, operationID,
	).Scan(&raw, &storedHash, &effectState); err != nil {
		return ClaimedOperation{}, errors.New("operation intent unavailable")
	}
	var intent model.OperationIntent
	if json.Unmarshal(raw, &intent) != nil {
		return ClaimedOperation{}, errors.New("operation intent unavailable")
	}
	hash, hashErr := model.OperationRequestHash(intent)
	if hashErr != nil || hash != storedHash || hash != claim.RequestHash || !model.ValidEffectState(effectState) ||
		intent.OperationID != claim.OperationID ||
		intent.Target.NodeID != claim.NodeID || intent.Target.Generation+1 != claim.Generation {
		return ClaimedOperation{}, errors.New("operation intent unavailable")
	}
	return ClaimedOperation{Intent: intent, Claim: claim, EffectState: effectState}, nil
}

// CheckOperationAuthority is the DB-side half of the adapter effect fence. A
// later transport stage can expose this check to the adapter without giving it
// database access; R02 uses it to prove that an expired or superseded worker
// cannot retain authority to begin an effect.
func (s *Store) CheckOperationAuthority(ctx context.Context, ownerID, nodeID string, claim OperationClaim) error {
	if !model.ValidActor(ownerID) || !model.ValidUUID(nodeID) || !model.ValidActor(claim.OperationID) ||
		claim.NodeID != nodeID || !model.ValidActor(claim.WorkerID) || !model.ValidUUID(claim.Token) || claim.Generation < 1 || claim.Version < 1 {
		return ErrStaleWorker
	}
	var currentNodeID, phase, requestHash string
	var generation, operationGeneration, version, expectedRevision, currentRevision, expectedEpoch, currentEpoch int64
	var expectedHostID, currentHostID, expectedBinding, currentBinding string
	var worker, token *string
	var expires *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT o.node_id::text,o.phase,o.request_hash,o.generation,o.operation_version,o.worker_id,o.worker_token::text,o.lease_expires_at,
			o.host_id::text,i.host_id::text,o.expected_registration_revision,i.registration_revision,
			o.expected_registration_epoch,i.registration_epoch,
			o.expected_registration_binding_sha256,i.registration_binding_sha256,i.operation_generation
		FROM agent_service.operations o
		JOIN agent_service.instances i ON i.owner_id=o.owner_id AND i.node_id=o.node_id
		WHERE o.owner_id=$1 AND o.operation_id=$2`, ownerID, claim.OperationID,
	).Scan(&currentNodeID, &phase, &requestHash, &generation, &version, &worker, &token, &expires,
		&expectedHostID, &currentHostID, &expectedRevision, &currentRevision,
		&expectedEpoch, &currentEpoch, &expectedBinding, &currentBinding, &operationGeneration)
	if err != nil || worker == nil || token == nil || expires == nil || currentNodeID != nodeID ||
		model.TerminalOperationPhase(phase) || requestHash != claim.RequestHash || generation != claim.Generation || operationGeneration != generation ||
		version != claim.Version || *worker != claim.WorkerID || *token != claim.Token ||
		!expires.After(s.now().UTC()) || expectedHostID != currentHostID || expectedRevision != currentRevision ||
		expectedEpoch != currentEpoch || expectedBinding != currentBinding {
		return ErrStaleWorker
	}
	return nil
}

// MarkOperationSent is the DB-side durable handoff immediately after the
// adapter fsyncs its sent journal entry. On recovery an already-unknown call
// remains unknown; it is never regressed merely to make the transition fit.
func (s *Store) MarkOperationSent(ctx context.Context, ownerID string, claim OperationClaim) (OperationClaim, error) {
	status, err := s.GetOperation(ctx, ownerID, claim.OperationID)
	if err != nil {
		return OperationClaim{}, err
	}
	if status.EffectState == "unknown" {
		if err := s.CheckOperationAuthority(ctx, ownerID, claim.NodeID, claim); err != nil {
			return OperationClaim{}, err
		}
		return claim, nil
	}
	if status.EffectState != "not_sent" && status.EffectState != "sent" {
		return OperationClaim{}, ErrOperationConflict
	}
	return s.AdvanceOperation(ctx, ownerID, claim, OperationUpdate{Phase: "applying", EffectState: "sent"})
}

func (s *Store) AdvanceOperation(ctx context.Context, ownerID string, claim OperationClaim, update OperationUpdate) (OperationClaim, error) {
	if !model.ValidActor(ownerID) || !model.ValidActor(claim.OperationID) || !model.ValidActor(claim.WorkerID) ||
		!model.ValidUUID(claim.NodeID) || !model.ValidUUID(claim.Token) || claim.Generation < 1 || claim.Version < 1 ||
		(update.ResultCode != nil && !model.ValidActor(*update.ResultCode)) {
		return OperationClaim{}, errors.New("invalid operation update")
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return OperationClaim{}, errors.New("operation transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var phase, effectState, token, worker, nodeID, requestHash string
	var generation, version, expectedRevision, currentRevision, expectedEpoch, currentEpoch int64
	var expectedHostID, currentHostID, expectedBinding, currentBinding string
	var expires time.Time
	err = tx.QueryRow(ctx, `
		SELECT o.phase,o.effect_state,o.request_hash,o.node_id::text,o.generation,o.operation_version,o.worker_id,o.worker_token::text,o.lease_expires_at,
			o.host_id::text,i.host_id::text,o.expected_registration_revision,i.registration_revision,
			o.expected_registration_epoch,i.registration_epoch,
			o.expected_registration_binding_sha256,i.registration_binding_sha256
		FROM agent_service.operations o
		JOIN agent_service.instances i ON i.owner_id=o.owner_id AND i.node_id=o.node_id
		WHERE o.owner_id=$1 AND o.operation_id=$2 FOR UPDATE OF o,i`, ownerID, claim.OperationID,
	).Scan(&phase, &effectState, &requestHash, &nodeID, &generation, &version, &worker, &token, &expires,
		&expectedHostID, &currentHostID, &expectedRevision, &currentRevision,
		&expectedEpoch, &currentEpoch, &expectedBinding, &currentBinding)
	if errors.Is(err, pgx.ErrNoRows) {
		return OperationClaim{}, ErrOperationNotFound
	}
	if err != nil {
		return OperationClaim{}, errors.New("operation unavailable")
	}
	if requestHash != claim.RequestHash || nodeID != claim.NodeID || generation != claim.Generation || version != claim.Version || worker != claim.WorkerID || token != claim.Token ||
		!expires.After(now) || expectedHostID != currentHostID || currentRevision != expectedRevision ||
		currentEpoch != expectedEpoch || currentBinding != expectedBinding {
		return OperationClaim{}, ErrStaleWorker
	}
	if !model.ValidOperationTransition(phase, effectState, update.Phase, update.EffectState) || version >= model.MaximumSafeInt {
		return OperationClaim{}, ErrOperationConflict
	}
	version++
	release := model.TerminalOperationPhase(update.Phase) || update.EffectState == "unknown"
	var nextWorker, nextToken any = worker, token
	var nextExpiry any = expires
	if release {
		nextWorker, nextToken, nextExpiry = nil, nil, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE agent_service.operations
		SET phase=$3,effect_state=$4,result_code=$5,operation_version=$6,updated_at=$7,
			worker_id=$8,worker_token=$9::uuid,lease_expires_at=$10
		WHERE owner_id=$1 AND operation_id=$2`, ownerID, claim.OperationID,
		update.Phase, update.EffectState, update.ResultCode, version, now,
		nextWorker, nextToken, nextExpiry,
	); err != nil {
		return OperationClaim{}, errors.New("operation update unavailable")
	}
	stepState := map[string]string{
		"not_sent": "pending", "sent": "sent", "unknown": "sent", "acknowledged": "acknowledged",
		"reconciled": "reconciled", "failed": "failed",
	}[update.EffectState]
	if _, err := tx.Exec(ctx, `
		UPDATE agent_service.operation_steps SET state=$3,updated_at=$4
		WHERE owner_id=$1 AND operation_id=$2`, ownerID, claim.OperationID, stepState, now,
	); err != nil {
		return OperationClaim{}, errors.New("operation step update unavailable")
	}
	if err := tx.Commit(ctx); err != nil {
		return OperationClaim{}, errors.New("operation update commit unknown")
	}
	if release {
		return OperationClaim{}, nil
	}
	claim.Version = version
	return claim, nil
}

func operationReceipt(operationID, requestHash string, target model.OperationTarget, generation int64, acceptedAt time.Time) model.OperationReceipt {
	return model.OperationReceipt{
		SchemaID: model.OperationReceiptSchemaID, OperationID: operationID, RequestHash: requestHash,
		Target: target, AcceptedGeneration: generation, AcceptedAt: model.FormatOperationTime(acceptedAt),
	}
}

type operationQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readOperationReceipt(ctx context.Context, query operationQuerier, ownerID, operationID string) (model.OperationReceipt, string, bool, error) {
	var requestHash, nodeID, hostID string
	var registrationRevision, registrationEpoch, expectedGeneration, generation int64
	var acceptedAt time.Time
	err := query.QueryRow(ctx, `
		SELECT request_hash,node_id::text,host_id::text,expected_registration_revision,
			expected_registration_epoch,expected_generation,generation,accepted_at
		FROM agent_service.operations WHERE owner_id=$1 AND operation_id=$2`, ownerID, operationID,
	).Scan(&requestHash, &nodeID, &hostID, &registrationRevision, &registrationEpoch, &expectedGeneration, &generation, &acceptedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.OperationReceipt{}, "", false, nil
	}
	if err != nil {
		return model.OperationReceipt{}, "", false, errors.New("operation receipt unavailable")
	}
	target := model.OperationTarget{
		NodeID: nodeID, HostID: hostID, RegistrationRevision: registrationRevision,
		RegistrationEpoch: registrationEpoch, Generation: expectedGeneration,
	}
	return operationReceipt(operationID, requestHash, target, generation, acceptedAt), requestHash, true, nil
}

func readOperationStatus(ctx context.Context, query operationQuerier, ownerID, operationID string) (model.OperationStatus, error) {
	receipt, _, found, err := readOperationReceipt(ctx, query, ownerID, operationID)
	if err != nil {
		return model.OperationStatus{}, err
	}
	if !found {
		return model.OperationStatus{}, ErrOperationNotFound
	}
	var phase, effectState string
	var version int64
	var updatedAt time.Time
	var resultCode *string
	if err := query.QueryRow(ctx, `
		SELECT phase,effect_state,operation_version,updated_at,result_code
		FROM agent_service.operations WHERE owner_id=$1 AND operation_id=$2`, ownerID, operationID,
	).Scan(&phase, &effectState, &version, &updatedAt, &resultCode); err != nil {
		return model.OperationStatus{}, errors.New("operation status unavailable")
	}
	return model.OperationStatus{
		SchemaID: model.OperationStatusSchemaID, Receipt: receipt, Phase: phase, EffectState: effectState,
		OperationVersion: version, UpdatedAt: model.FormatOperationTime(updatedAt), ResultCode: resultCode,
	}, nil
}

func uniqueViolation(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23505"
}
