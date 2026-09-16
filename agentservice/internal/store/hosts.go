package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

var (
	ErrHostConflict = errors.New("host version conflict")
	ErrHostNotFound = errors.New("host not found")
)

type HostListResult struct {
	Items   []model.HostRecord
	After   string
	HasMore bool
}

const hostColumns = `
	host_id::text,host_version,display_name,transport,target_ref,credential_ref,registry_credential_ref,
	expected_host_key,docker_context_ref,expected_identity_sha256,host_platform,host_architecture,
	observed_at,availability,failure_stage,failure_code,next_action,host_key_sha256,daemon_id,
	context_endpoint,engine_os,architecture,api_version,engine_version,capabilities,identity_sha256,
	registry_availability`

func (s *Store) UpsertHost(ctx context.Context, owner string, input model.HostUpsert) (model.HostRecord, error) {
	if !model.ValidActor(owner) || model.ValidateHostUpsert(input) != nil {
		return model.HostRecord{}, errors.New("invalid host descriptor")
	}
	if err := s.CheckSchema(ctx); err != nil {
		return model.HostRecord{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.HostRecord{}, errors.New("host transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "agent-host-owner:"+owner); err != nil {
		return model.HostRecord{}, errors.New("host owner lock unavailable")
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "agent-host:"+owner+":"+input.HostID); err != nil {
		return model.HostRecord{}, errors.New("host lock unavailable")
	}
	var currentVersion int64
	err = tx.QueryRow(ctx, `SELECT host_version FROM agent_service.host_descriptors WHERE owner_id=$1 AND host_id=$2 FOR UPDATE`, owner, input.HostID).Scan(&currentVersion)
	if input.ExpectedHostVersion == 0 {
		if err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return model.HostRecord{}, ErrHostConflict
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_service.hosts(owner_id,host_id,name) VALUES($1,$2,$3)
			ON CONFLICT(owner_id,host_id) DO UPDATE SET name=EXCLUDED.name,updated_at=clock_timestamp()`, owner, input.HostID, input.DisplayName); err != nil {
			return model.HostRecord{}, errors.New("host metadata unavailable")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_service.host_descriptors(
				owner_id,host_id,host_version,display_name,transport,target_ref,credential_ref,registry_credential_ref,
				expected_host_key,docker_context_ref,expected_identity_sha256,host_platform,host_architecture
			) VALUES($1,$2,1,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			owner, input.HostID, input.DisplayName, input.Transport, input.TargetRef, nullable(input.CredentialRef),
			nullable(input.RegistryCredentialRef), nullable(input.ExpectedHostKey), input.DockerContextRef,
			nullable(input.ExpectedIdentitySHA256), input.HostPlatform, input.HostArchitecture,
		); err != nil {
			return model.HostRecord{}, errors.New("host descriptor unavailable")
		}
	} else {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.HostRecord{}, ErrHostNotFound
		}
		if err != nil {
			return model.HostRecord{}, errors.New("host descriptor unavailable")
		}
		if currentVersion != input.ExpectedHostVersion || currentVersion >= model.MaximumSafeInt {
			return model.HostRecord{}, ErrHostConflict
		}
		var active bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM agent_service.operations o
				JOIN agent_service.instances i ON i.owner_id=o.owner_id AND i.node_id=o.node_id
				WHERE o.owner_id=$1 AND i.host_id=$2 AND o.phase NOT IN ('succeeded','failed')
			)`, owner, input.HostID).Scan(&active); err != nil {
			return model.HostRecord{}, errors.New("host operation state unavailable")
		}
		if active {
			return model.HostRecord{}, ErrHostConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_service.hosts SET name=$3,updated_at=clock_timestamp() WHERE owner_id=$1 AND host_id=$2`, owner, input.HostID, input.DisplayName); err != nil {
			return model.HostRecord{}, errors.New("host metadata unavailable")
		}
		command, err := tx.Exec(ctx, `
			UPDATE agent_service.host_descriptors SET
				host_version=host_version+1,display_name=$4,transport=$5,target_ref=$6,credential_ref=$7,
				registry_credential_ref=$8,expected_host_key=$9,docker_context_ref=$10,expected_identity_sha256=$11,
				host_platform=$12,host_architecture=$13,observed_at=NULL,availability='unverified',failure_stage=NULL,
				failure_code=NULL,next_action=NULL,host_key_sha256=NULL,daemon_id=NULL,context_endpoint=NULL,
				engine_os=NULL,architecture=NULL,api_version=NULL,engine_version=NULL,capabilities='[]'::jsonb,
				identity_sha256=NULL,registry_availability='not_configured',updated_at=clock_timestamp()
			WHERE owner_id=$1 AND host_id=$2 AND host_version=$3`,
			owner, input.HostID, input.ExpectedHostVersion, input.DisplayName, input.Transport, input.TargetRef,
			nullable(input.CredentialRef), nullable(input.RegistryCredentialRef), nullable(input.ExpectedHostKey),
			input.DockerContextRef, nullable(input.ExpectedIdentitySHA256), input.HostPlatform, input.HostArchitecture,
		)
		if err != nil || command.RowsAffected() != 1 {
			return model.HostRecord{}, ErrHostConflict
		}
	}
	result, err := getHostRow(ctx, tx, owner, input.HostID)
	if err != nil {
		return model.HostRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.HostRecord{}, errors.New("host commit unavailable")
	}
	return result, nil
}

func (s *Store) GetHost(ctx context.Context, owner, hostID string) (model.HostRecord, error) {
	if !model.ValidActor(owner) || !model.ValidUUID(hostID) {
		return model.HostRecord{}, ErrHostNotFound
	}
	return getHostRow(ctx, s.pool, owner, hostID)
}

func (s *Store) ListHosts(ctx context.Context, owner, after string, limit int) (HostListResult, error) {
	if !model.ValidActor(owner) || after != "" && !model.ValidUUID(after) || limit < 1 || limit > 100 {
		return HostListResult{}, errors.New("invalid host page")
	}
	rows, err := s.pool.Query(ctx, `SELECT `+hostColumns+`
		FROM agent_service.host_descriptors
		WHERE owner_id=$1 AND ($2='' OR host_id::text>$2)
		ORDER BY host_id LIMIT $3`, owner, after, limit+1)
	if err != nil {
		return HostListResult{}, errors.New("host page unavailable")
	}
	defer rows.Close()
	result := HostListResult{Items: make([]model.HostRecord, 0, limit)}
	for rows.Next() {
		record, err := scanHost(rows)
		if err != nil {
			return HostListResult{}, errors.New("host page unavailable")
		}
		if len(result.Items) == limit {
			result.HasMore = true
			break
		}
		result.Items = append(result.Items, record)
	}
	if rows.Err() != nil {
		return HostListResult{}, errors.New("host page unavailable")
	}
	if len(result.Items) > 0 {
		result.After = result.Items[len(result.Items)-1].HostID
	}
	return result, nil
}

// BeginHostProbe assigns a database-ordered revision before any remote I/O and
// clears the last ready identity. A later probe therefore fences every earlier
// in-flight result even when the earlier network request finishes last.
func (s *Store) BeginHostProbe(ctx context.Context, owner, hostID string, expectedHostVersion int64) (model.HostRecord, int64, error) {
	if !model.ValidActor(owner) || !model.ValidUUID(hostID) || expectedHostVersion < 1 || expectedHostVersion > model.MaximumSafeInt {
		return model.HostRecord{}, 0, ErrHostConflict
	}
	if err := s.CheckSchema(ctx); err != nil {
		return model.HostRecord{}, 0, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.HostRecord{}, 0, errors.New("host probe transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var probeRevision int64
	err = tx.QueryRow(ctx, `
		UPDATE agent_service.host_descriptors SET
			probe_revision=probe_revision+1,observed_at=NULL,availability='unverified',failure_stage=NULL,
			failure_code=NULL,next_action=NULL,host_key_sha256=NULL,daemon_id=NULL,context_endpoint=NULL,
			engine_os=NULL,architecture=NULL,api_version=NULL,engine_version=NULL,capabilities='[]'::jsonb,
			identity_sha256=NULL,registry_availability=CASE WHEN registry_credential_ref IS NULL THEN 'not_configured' ELSE 'not_checked' END,
			updated_at=clock_timestamp()
		WHERE owner_id=$1 AND host_id=$2 AND host_version=$3 AND probe_revision<$4
		RETURNING probe_revision`, owner, hostID, expectedHostVersion, model.MaximumSafeInt).Scan(&probeRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := getHostRow(ctx, tx, owner, hostID); errors.Is(getErr, ErrHostNotFound) {
			return model.HostRecord{}, 0, ErrHostNotFound
		}
		return model.HostRecord{}, 0, ErrHostConflict
	}
	if err != nil {
		return model.HostRecord{}, 0, errors.New("host probe reservation unavailable")
	}
	record, err := getHostRow(ctx, tx, owner, hostID)
	if err != nil {
		return model.HostRecord{}, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.HostRecord{}, 0, errors.New("host probe reservation unavailable")
	}
	return record, probeRevision, nil
}

func (s *Store) RecordHostObservation(ctx context.Context, owner string, observation model.HostObservation, probeRevision int64) (model.HostRecord, error) {
	if !model.ValidActor(owner) || model.ValidateHostObservation(observation) != nil || probeRevision < 1 || probeRevision > model.MaximumSafeInt {
		return model.HostRecord{}, errors.New("invalid host observation")
	}
	capabilities, err := json.Marshal(observation.Capabilities)
	if err != nil {
		return model.HostRecord{}, errors.New("invalid host observation")
	}
	observedAt, _ := time.Parse(time.RFC3339Nano, observation.ObservedAt)
	command, err := s.pool.Exec(ctx, `
		UPDATE agent_service.host_descriptors SET
			observed_at=$4,availability=$5,failure_stage=$6,failure_code=$7,next_action=$8,
			host_key_sha256=$9,daemon_id=$10,context_endpoint=$11,engine_os=$12,architecture=$13,
			api_version=$14,engine_version=$15,capabilities=$16::jsonb,identity_sha256=$17,
			registry_availability=$18,updated_at=clock_timestamp()
		WHERE owner_id=$1 AND host_id=$2 AND host_version=$3 AND docker_context_ref=$19 AND probe_revision=$20
			AND ($5<>'ready' OR expected_host_key IS NULL OR expected_host_key=$9)
			AND ($5<>'ready' OR expected_identity_sha256 IS NULL OR expected_identity_sha256=$17)`,
		owner, observation.HostID, observation.HostVersion, observedAt, observation.Availability,
		nullable(observation.FailureStage), nullable(observation.FailureCode), nullable(observation.NextAction),
		nullable(observation.HostKeySHA256), nullable(observation.DaemonID), nullable(observation.ContextEndpoint),
		nullable(observation.EngineOS), nullable(observation.Architecture), nullable(observation.APIVersion),
		nullable(observation.EngineVersion), capabilities, nullable(observation.IdentitySHA256),
		observation.RegistryAvailability, observation.DockerContextRef, probeRevision,
	)
	if err != nil {
		return model.HostRecord{}, errors.New("host observation unavailable")
	}
	if command.RowsAffected() != 1 {
		if _, getErr := s.GetHost(ctx, owner, observation.HostID); errors.Is(getErr, ErrHostNotFound) {
			return model.HostRecord{}, ErrHostNotFound
		}
		return model.HostRecord{}, ErrHostConflict
	}
	return s.GetHost(ctx, owner, observation.HostID)
}

type hostRow interface {
	Scan(...any) error
}

type hostQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getHostRow(ctx context.Context, query hostQuery, owner, hostID string) (model.HostRecord, error) {
	record, err := scanHost(query.QueryRow(ctx, `SELECT `+hostColumns+` FROM agent_service.host_descriptors WHERE owner_id=$1 AND host_id=$2`, owner, hostID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.HostRecord{}, ErrHostNotFound
	}
	if err != nil {
		return model.HostRecord{}, errors.New("host unavailable")
	}
	return record, nil
}

func scanHost(row hostRow) (model.HostRecord, error) {
	var record model.HostRecord
	var credentialRef, registryCredentialRef, expectedHostKey, expectedIdentity *string
	var observedAt *time.Time
	var failureStage, failureCode, nextAction, hostKey, daemonID, contextEndpoint *string
	var engineOS, architecture, apiVersion, engineVersion, identity *string
	var capabilities []byte
	err := row.Scan(
		&record.HostID, &record.HostVersion, &record.DisplayName, &record.Transport, &record.TargetRef,
		&credentialRef, &registryCredentialRef, &expectedHostKey, &record.DockerContextRef, &expectedIdentity,
		&record.HostPlatform, &record.HostArchitecture, &observedAt, &record.Availability, &failureStage,
		&failureCode, &nextAction, &hostKey, &daemonID, &contextEndpoint, &engineOS, &architecture,
		&apiVersion, &engineVersion, &capabilities, &identity, &record.RegistryAvailability,
	)
	if err != nil {
		return model.HostRecord{}, err
	}
	record.SchemaID = model.HostSchemaID
	record.CredentialRef = dereference(credentialRef)
	record.RegistryCredentialRef = dereference(registryCredentialRef)
	record.ExpectedHostKey = dereference(expectedHostKey)
	record.ExpectedIdentitySHA256 = dereference(expectedIdentity)
	record.FailureStage = dereference(failureStage)
	record.FailureCode = dereference(failureCode)
	record.NextAction = dereference(nextAction)
	record.HostKeySHA256 = dereference(hostKey)
	record.DaemonID = dereference(daemonID)
	record.ContextEndpoint = dereference(contextEndpoint)
	record.EngineOS = dereference(engineOS)
	record.Architecture = dereference(architecture)
	record.APIVersion = dereference(apiVersion)
	record.EngineVersion = dereference(engineVersion)
	record.IdentitySHA256 = dereference(identity)
	if observedAt != nil {
		value := observedAt.UTC().Format(time.RFC3339Nano)
		record.ObservedAt = &value
	}
	if json.Unmarshal(capabilities, &record.Capabilities) != nil || record.Capabilities == nil {
		return model.HostRecord{}, errors.New("invalid stored capabilities")
	}
	return record, nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func dereference(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
