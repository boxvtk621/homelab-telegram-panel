// Package store owns the R01 PostgreSQL schema and durable identity mapping.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/registry"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/migrations"
)

const migrationLockID = int64(0x484c323638523031)

type Store struct {
	pool    *pgxpool.Pool
	now     func() time.Time
	newUUID func() (string, error)
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	if databaseURL == "" {
		return nil, errors.New("database URL is required")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	config.MaxConns = 8
	config.MinConns = 0
	config.MaxConnIdleTime = time.Minute
	config.MaxConnLifetime = 30 * time.Minute
	config.HealthCheckPeriod = 30 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("database connection failed")
	}
	ping, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(ping); err != nil {
		pool.Close()
		return nil, errors.New("database unavailable")
	}
	return &Store{pool: pool, now: time.Now, newUUID: randomUUID}, nil
}

func NewForPool(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: time.Now, newUUID: randomUUID}
}

func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

func (s *Store) Migrate(ctx context.Context) error {
	all, err := migrations.All()
	if err != nil {
		return err
	}
	connection, err := s.pool.Acquire(ctx)
	if err != nil {
		return errors.New("migration connection unavailable")
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return errors.New("migration lock unavailable")
	}
	defer func() {
		_, _ = connection.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID)
	}()
	if _, err := connection.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS agent_service"); err != nil {
		return errors.New("migration schema unavailable")
	}
	if _, err := connection.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS agent_service.schema_migrations (
			version bigint PRIMARY KEY,
			name text NOT NULL,
			sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
			applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
		)`); err != nil {
		return errors.New("migration metadata unavailable")
	}
	for _, migration := range all {
		digest := sha256.Sum256([]byte(migration.SQL))
		checksum := hex.EncodeToString(digest[:])
		var name, storedChecksum string
		err := connection.QueryRow(ctx,
			"SELECT name, sha256 FROM agent_service.schema_migrations WHERE version=$1",
			migration.Version,
		).Scan(&name, &storedChecksum)
		if err == nil {
			if name != migration.Name || storedChecksum != checksum {
				return fmt.Errorf("migration %d checksum mismatch", migration.Version)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return errors.New("migration metadata read failed")
		}
		tx, err := connection.Begin(ctx)
		if err != nil {
			return errors.New("migration transaction failed")
		}
		if _, err = tx.Exec(ctx, migration.SQL); err == nil {
			_, err = tx.Exec(ctx,
				"INSERT INTO agent_service.schema_migrations(version,name,sha256) VALUES($1,$2,$3)",
				migration.Version, migration.Name, checksum,
			)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %d failed", migration.Version)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("migration %d commit failed", migration.Version)
		}
	}
	return nil
}

func (s *Store) CheckSchema(ctx context.Context) error {
	all, err := migrations.All()
	if err != nil || len(all) == 0 {
		return errors.New("migration catalog unavailable")
	}
	rows, err := s.pool.Query(ctx, "SELECT version,name,sha256 FROM agent_service.schema_migrations ORDER BY version")
	if err != nil {
		return errors.New("database schema is not current")
	}
	defer rows.Close()
	position := 0
	for rows.Next() {
		if position >= len(all) {
			return errors.New("database schema is not current")
		}
		var version int64
		var name, checksum string
		if rows.Scan(&version, &name, &checksum) != nil {
			return errors.New("database schema is not current")
		}
		digest := sha256.Sum256([]byte(all[position].SQL))
		if version != all[position].Version || name != all[position].Name || checksum != hex.EncodeToString(digest[:]) {
			return errors.New("database schema is not current")
		}
		position++
	}
	if rows.Err() != nil || position != len(all) {
		return errors.New("database schema is not current")
	}
	return nil
}

type ImportResult struct {
	OwnerID         string
	NodesSeen       int
	DialogsSeen     int
	MappingsCreated int
	ManifestSHA256  string
	RegistryVersion int64
}

func (s *Store) Import(ctx context.Context, verified registry.Verified, snapshot model.ImportSnapshot) (ImportResult, error) {
	if err := model.ValidateSnapshot(verified.Manifest, snapshot); err != nil {
		return ImportResult{}, err
	}
	if err := s.CheckSchema(ctx); err != nil {
		return ImportResult{}, err
	}
	manifestNodes := make(map[string]model.RegistryNode, len(verified.Manifest.Nodes))
	for _, node := range verified.Manifest.Nodes {
		manifestNodes[node.NodeID] = node
	}
	incomingSchema := verified.Manifest.SchemaID
	if incomingSchema == "" {
		incomingSchema = "legacy"
	}
	// The owner advisory lock is the serialization point. Read Committed is
	// required so a transaction that waited for that lock observes the winner's
	// committed registry/operation state instead of an earlier MVCC snapshot.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ImportResult{}, errors.New("import transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "agent-service-import:"+verified.Manifest.OwnerID); err != nil {
		return ImportResult{}, errors.New("import owner lock unavailable")
	}
	var registryOperationActive bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM agent_service.registry_operations
			WHERE owner_id=$1 AND phase NOT IN ('succeeded','failed')
		)`, verified.Manifest.OwnerID).Scan(&registryOperationActive); err != nil || registryOperationActive {
		return ImportResult{}, errors.New("registry import is fenced by an active operation")
	}
	registryExists := false
	var currentVersion int64
	var currentHash, currentSchema string
	err = tx.QueryRow(ctx, `
		SELECT registry_version,manifest_sha256,registry_schema_id
		FROM agent_service.registry_state WHERE owner_id=$1 FOR UPDATE`,
		verified.Manifest.OwnerID,
	).Scan(&currentVersion, &currentHash, &currentSchema)
	if err == nil {
		registryExists = true
		if incomingSchema != currentSchema || verified.Manifest.RegistryVersion != currentVersion ||
			verified.ManifestSHA256 != currentHash {
			return ImportResult{}, errors.New("registry changes require a coordinated operation")
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ImportResult{}, errors.New("registry state unavailable")
	}
	for _, host := range snapshot.Hosts {
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_service.hosts(owner_id,host_id,name)
			VALUES($1,$2,$3)
			ON CONFLICT(owner_id,host_id) DO UPDATE
			SET name=EXCLUDED.name, updated_at=clock_timestamp()`,
			verified.Manifest.OwnerID, host.HostID, host.Name,
		); err != nil {
			return ImportResult{}, errors.New("host import failed")
		}
	}
	result := ImportResult{
		OwnerID: verified.Manifest.OwnerID, NodesSeen: len(snapshot.Nodes),
		ManifestSHA256: verified.ManifestSHA256, RegistryVersion: verified.Manifest.RegistryVersion,
	}
	for _, input := range snapshot.Nodes {
		node := manifestNodes[input.NodeID]
		bindingSHA256, marshalErr := registrationBindingSHA(node, input.HostID, verified.Manifest.Mode, input.RegistrationMode)
		if marshalErr != nil {
			return ImportResult{}, errors.New("instance registration unavailable")
		}
		nextRevision, nextEpoch := int64(1), int64(1)
		dynamic := verified.Manifest.SchemaID != ""
		if dynamic {
			nextRevision, nextEpoch = node.RegistrationRevision, node.RegistrationEpoch
		}
		var currentRevision, currentEpoch int64
		var currentHostID, currentRegistrationMode string
		var currentProjected bool
		var currentBinding *string
		err = tx.QueryRow(ctx, `
			SELECT registration_revision,registration_epoch,registration_binding_sha256,registration_projected,
				host_id::text,registration_mode
			FROM agent_service.instances WHERE owner_id=$1 AND node_id=$2 FOR UPDATE`,
			verified.Manifest.OwnerID, node.NodeID,
		).Scan(&currentRevision, &currentEpoch, &currentBinding, &currentProjected, &currentHostID, &currentRegistrationMode)
		if err == nil {
			if registryExists && (currentHostID != input.HostID || currentRegistrationMode != input.RegistrationMode) {
				return ImportResult{}, errors.New("registration changes require a coordinated operation")
			}
			nextRevision, nextEpoch = currentRevision, currentEpoch
			if nextEpoch == 0 {
				nextEpoch = 1
			}
			changed := currentBinding != nil && *currentBinding != bindingSHA256
			adoptingProjection := dynamic && !currentProjected
			if changed {
				var active bool
				if err := tx.QueryRow(ctx, `
					SELECT EXISTS (
						SELECT 1 FROM agent_service.operations
						WHERE owner_id=$1 AND node_id=$2 AND phase NOT IN ('succeeded','failed')
					)`, verified.Manifest.OwnerID, node.NodeID).Scan(&active); err != nil {
					return ImportResult{}, errors.New("instance registration unavailable")
				}
				if active || nextRevision >= model.MaximumSafeInt || nextEpoch >= model.MaximumSafeInt {
					return ImportResult{}, errors.New("instance registration is fenced by an active operation")
				}
				if adoptingProjection {
					nextRevision, nextEpoch = node.RegistrationRevision, node.RegistrationEpoch
				} else if dynamic {
					if node.RegistrationRevision != currentRevision+1 || node.RegistrationEpoch != currentEpoch+1 {
						return ImportResult{}, errors.New("signed node registration version is not the next revision")
					}
					nextRevision, nextEpoch = node.RegistrationRevision, node.RegistrationEpoch
				} else {
					nextRevision++
					nextEpoch++
				}
			} else if dynamic {
				if currentBinding == nil || adoptingProjection {
					nextRevision, nextEpoch = node.RegistrationRevision, node.RegistrationEpoch
				} else if node.RegistrationRevision != currentRevision || node.RegistrationEpoch != currentEpoch {
					return ImportResult{}, errors.New("signed node registration version changed without a binding change")
				}
			}
		} else if registryExists && errors.Is(err, pgx.ErrNoRows) {
			return ImportResult{}, errors.New("registry read model is incomplete")
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return ImportResult{}, errors.New("instance registration unavailable")
		}
		observation := model.StoredObservation{Process: "unknown", Connection: "unknown", Readiness: "unknown", Occupancy: "unknown"}
		if input.Observation != nil {
			observedAt, _ := time.Parse(time.RFC3339Nano, input.Observation.ObservedAt)
			source := input.Observation.Source
			observation = model.StoredObservation{
				Process: input.Observation.Process, Connection: input.Observation.Connection,
				Readiness: input.Observation.Readiness, Occupancy: input.Observation.Occupancy,
				ObservedAt: &observedAt, Source: &source, PendingCount: input.Observation.PendingCount,
			}
		}
		command, err := tx.Exec(ctx, `
			INSERT INTO agent_service.instances(
				owner_id,node_id,host_id,name,engine,registry_mode,registration_mode,registry_version,manifest_sha256,
				process_state,connection_state,readiness_state,occupancy_state,observed_at,observation_source,pending_count,
				registration_revision,registration_epoch,registration_binding_sha256,registration_projected
			) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
			ON CONFLICT(owner_id,node_id) DO UPDATE SET
				host_id=EXCLUDED.host_id, name=EXCLUDED.name, engine=EXCLUDED.engine, registry_mode=EXCLUDED.registry_mode,
				registration_mode=EXCLUDED.registration_mode, registry_version=EXCLUDED.registry_version,
				manifest_sha256=EXCLUDED.manifest_sha256, process_state=EXCLUDED.process_state,
				connection_state=EXCLUDED.connection_state, readiness_state=EXCLUDED.readiness_state,
				occupancy_state=EXCLUDED.occupancy_state, observed_at=EXCLUDED.observed_at,
				observation_source=EXCLUDED.observation_source, pending_count=EXCLUDED.pending_count,
				registration_revision=EXCLUDED.registration_revision,
				registration_epoch=EXCLUDED.registration_epoch,
				registration_binding_sha256=EXCLUDED.registration_binding_sha256,
				registration_projected=EXCLUDED.registration_projected,
				updated_at=clock_timestamp()`,
			verified.Manifest.OwnerID, node.NodeID, input.HostID, node.Name, node.Adapter, verified.Manifest.Mode,
			input.RegistrationMode, verified.Manifest.RegistryVersion, verified.ManifestSHA256,
			observation.Process, observation.Connection, observation.Readiness, observation.Occupancy,
			observation.ObservedAt, observation.Source, observation.PendingCount,
			nextRevision, nextEpoch, bindingSHA256, dynamic,
		)
		if err != nil || command.RowsAffected() != 1 {
			return ImportResult{}, errors.New("instance import failed")
		}
		for _, dialog := range input.Dialogs {
			result.DialogsSeen++
			var logicalID string
			var bindingVersion int64
			err := tx.QueryRow(ctx, `
				SELECT logical_dialog_id::text,binding_version
				FROM agent_service.dialog_bindings
				WHERE owner_id=$1 AND node_id=$2 AND node_dialog_id=$3`,
				verified.Manifest.OwnerID, node.NodeID, dialog.NodeDialogID,
			).Scan(&logicalID, &bindingVersion)
			if err == nil {
				continue
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return ImportResult{}, errors.New("dialog mapping read failed")
			}
			logicalID, err = s.newUUID()
			if err != nil {
				return ImportResult{}, errors.New("logical dialog identity unavailable")
			}
			if _, err := tx.Exec(ctx,
				"INSERT INTO agent_service.logical_dialogs(owner_id,logical_dialog_id) VALUES($1,$2)",
				verified.Manifest.OwnerID, logicalID,
			); err != nil {
				return ImportResult{}, errors.New("logical dialog import failed")
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO agent_service.dialog_bindings(
					owner_id,logical_dialog_id,binding_version,node_id,node_dialog_id,state
				) VALUES($1,$2,1,$3,$4,'active')`,
				verified.Manifest.OwnerID, logicalID, node.NodeID, dialog.NodeDialogID,
			); err != nil {
				return ImportResult{}, errors.New("dialog mapping import failed")
			}
			result.MappingsCreated++
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_service.registry_state(owner_id,registry_version,manifest_sha256,host_count,node_count,registry_schema_id,registry_envelope)
		VALUES($1,$2,$3,$4,$5,$6,$7::json)
		ON CONFLICT(owner_id) DO UPDATE SET
			host_count=EXCLUDED.host_count, node_count=EXCLUDED.node_count,
			registry_envelope=CASE WHEN agent_service.registry_state.registry_envelope IS NULL
				THEN EXCLUDED.registry_envelope ELSE agent_service.registry_state.registry_envelope END,
			imported_at=clock_timestamp()`,
		verified.Manifest.OwnerID, verified.Manifest.RegistryVersion, verified.ManifestSHA256,
		len(snapshot.Hosts), len(snapshot.Nodes), incomingSchema, verified.Envelope,
	); err != nil {
		return ImportResult{}, errors.New("registry state import failed")
	}
	if err := tx.Commit(ctx); err != nil {
		return ImportResult{}, errors.New("import commit failed")
	}
	return result, nil
}

func registrationBindingSHA(node model.RegistryNode, hostID, registryMode, registrationMode string) (string, error) {
	canonical, err := json.Marshal(struct {
		NodeID                string `json:"nodeId"`
		Name                  string `json:"name"`
		Adapter               string `json:"adapter"`
		URL                   string `json:"url"`
		CertificateSHA256     string `json:"certificateSHA256"`
		EndpointBindingSHA256 string `json:"endpointBindingSHA256,omitempty"`
		Compatibility         string `json:"compatibility"`
		HostID                string `json:"hostId"`
		RegistryMode          string `json:"registryMode"`
		RegistrationMode      string `json:"registrationMode"`
	}{
		NodeID: node.NodeID, Name: node.Name, Adapter: node.Adapter, URL: node.URL,
		CertificateSHA256: node.CertificateSHA256, EndpointBindingSHA256: node.EndpointBindingSHA256,
		Compatibility: node.Compatibility,
		HostID:        hostID, RegistryMode: registryMode, RegistrationMode: registrationMode,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

type ListResult struct {
	Items   []model.InventoryItem
	HasMore bool
	After   string
}

func (s *Store) ListInventory(ctx context.Context, ownerID, after string, limit int) (ListResult, error) {
	if !model.ValidActor(ownerID) || limit < 1 || limit > 100 || (after != "" && !model.ValidUUID(after)) {
		return ListResult{}, errors.New("invalid inventory query")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT i.node_id::text,i.name,i.engine,i.registry_mode,i.registration_mode,h.host_id::text,h.name,
			i.process_state,i.connection_state,i.readiness_state,i.occupancy_state,
			i.observed_at,i.observation_source,i.pending_count,
			(SELECT count(*) FROM agent_service.dialog_bindings b
			 WHERE b.owner_id=i.owner_id AND b.node_id=i.node_id)
		FROM agent_service.instances i
		JOIN agent_service.hosts h ON h.owner_id=i.owner_id AND h.host_id=i.host_id
		WHERE i.owner_id=$1 AND ($2='' OR i.node_id > $2::uuid)
		ORDER BY i.node_id
		LIMIT $3`, ownerID, after, limit+1)
	if err != nil {
		return ListResult{}, errors.New("inventory unavailable")
	}
	defer rows.Close()
	items := make([]model.InventoryItem, 0, limit+1)
	for rows.Next() {
		var item model.InventoryItem
		var observation model.StoredObservation
		if err := rows.Scan(
			&item.NodeID, &item.Name, &item.Engine, &item.SourceMode, &item.RegistrationMode,
			&item.Host.HostID, &item.Host.Name, &observation.Process, &observation.Connection,
			&observation.Readiness, &observation.Occupancy, &observation.ObservedAt,
			&observation.Source, &observation.PendingCount, &item.DialogCount,
		); err != nil {
			return ListResult{}, errors.New("inventory unavailable")
		}
		if item.DialogCount < 0 || item.DialogCount > model.MaximumSafeInt {
			return ListResult{}, errors.New("inventory unavailable")
		}
		item.State = model.StateAxes{Process: observation.Process, Connection: observation.Connection, Readiness: observation.Readiness, Occupancy: observation.Occupancy}
		item.Status = model.DeriveStatus(s.now(), item.RegistrationMode, observation)
		item.Actions = model.ActionsFor(item.Status)
		if observation.ObservedAt != nil && observation.Source != nil {
			formatted := observation.ObservedAt.UTC().Format(time.RFC3339Nano)
			item.ObservedAt = &formatted
			item.Source = observation.Source
			if observation.PendingCount != nil {
				item.PendingCount = &model.MetricInt64{Value: *observation.PendingCount, ObservedAt: formatted, Source: *observation.Source}
			}
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		return ListResult{}, errors.New("inventory unavailable")
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if len(items) == 0 {
		return ListResult{Items: items}, nil
	}
	return ListResult{Items: items, HasMore: hasMore, After: items[len(items)-1].NodeID}, nil
}

type BindingListResult struct {
	Items   []model.DialogMapping
	HasMore bool
	After   string
}

func (s *Store) ListDialogBindings(ctx context.Context, ownerID, nodeID, after string, limit int) (BindingListResult, error) {
	if !model.ValidActor(ownerID) || !model.ValidUUID(nodeID) || limit < 1 || limit > 100 ||
		(after != "" && !model.ValidUUID(after)) {
		return BindingListResult{}, errors.New("invalid dialog binding query")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT node_dialog_id::text,logical_dialog_id::text,binding_version
		FROM agent_service.dialog_bindings
		WHERE owner_id=$1 AND node_id=$2 AND ($3='' OR node_dialog_id > $3::uuid)
		ORDER BY node_dialog_id
		LIMIT $4`, ownerID, nodeID, after, limit+1)
	if err != nil {
		return BindingListResult{}, errors.New("dialog bindings unavailable")
	}
	defer rows.Close()
	items := make([]model.DialogMapping, 0, limit+1)
	for rows.Next() {
		var mapping model.DialogMapping
		if err := rows.Scan(&mapping.NodeDialogID, &mapping.LogicalDialogID, &mapping.BindingVersion); err != nil ||
			mapping.BindingVersion < 1 || mapping.BindingVersion > model.MaximumSafeInt {
			return BindingListResult{}, errors.New("dialog bindings unavailable")
		}
		items = append(items, mapping)
	}
	if rows.Err() != nil {
		return BindingListResult{}, errors.New("dialog bindings unavailable")
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if len(items) == 0 {
		return BindingListResult{Items: items}, nil
	}
	return BindingListResult{Items: items, HasMore: hasMore, After: items[len(items)-1].NodeDialogID}, nil
}

func randomUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	h := hex.EncodeToString(value[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
