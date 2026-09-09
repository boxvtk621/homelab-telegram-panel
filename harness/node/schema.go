package node

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

func currentSchemaFingerprint() string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("harness-schema-v%d\x00%s", SchemaVersion, strings.Join(schemaStatements, "\x00"))))
	return hex.EncodeToString(digest[:])
}

func (node *Node) verifySchemaFingerprint(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) error {
	var fingerprint string
	if err := query.QueryRowContext(ctx, "SELECT fingerprint FROM schema_meta WHERE singleton=1").Scan(&fingerprint); err != nil {
		return fmt.Errorf("schema fingerprint: %w", err)
	}
	if fingerprint != currentSchemaFingerprint() {
		return errors.New("database schema fingerprint does not match binary")
	}
	node.runtime.SchemaFingerprint = fingerprint
	return nil
}

func verifySchemaDDL(ctx context.Context, db *sql.DB) error {
	expected := make(map[string]string, len(schemaStatements))
	for _, statement := range schemaStatements {
		fields := strings.Fields(statement)
		nameIndex := 2
		if len(fields) > 3 && fields[1] == "UNIQUE" {
			nameIndex = 3
		}
		if len(fields) <= nameIndex {
			return errors.New("invalid compiled schema statement")
		}
		name := strings.Trim(fields[nameIndex], "`\"")
		if opening := strings.IndexByte(name, '('); opening >= 0 {
			name = name[:opening]
		}
		expected[name] = strings.Join(fields, " ")
	}
	rows, err := db.QueryContext(ctx, `SELECT name,sql FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	actual := make(map[string]string)
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			return err
		}
		actual[name] = strings.Join(strings.Fields(ddl), " ")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return errors.New("sqlite_schema object count does not match binary")
	}
	for name, ddl := range expected {
		if actual[name] != ddl {
			return fmt.Errorf("sqlite_schema object %s does not match binary", name)
		}
	}
	return nil
}

func (node *Node) migrate(ctx context.Context, newVolume bool) error {
	var version int
	if err := node.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > SchemaVersion {
		return fmt.Errorf("database schema %d is newer than binary schema %d", version, SchemaVersion)
	}
	if version == 0 && !newVolume {
		return errors.New("pre-existing unversioned database is not a Harness volume")
	}
	if version < 0 || (version != 0 && version != SchemaVersion) {
		return fmt.Errorf("unsupported database schema %d", version)
	}
	if version == SchemaVersion {
		node.runtime.SchemaVersion = version
		return node.verifySchemaFingerprint(ctx, node.db)
	}
	if node.config.StartupFault != nil {
		if err := node.config.StartupFault(StartupBeforeMigration); err != nil {
			return err
		}
	}
	tx, err := node.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range schemaStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("schema migration: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_meta(singleton,fingerprint) VALUES(1,?)", currentSchemaFingerprint()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO node_state(
		singleton,node_id,owner_id,registry_version,epoch,state_version,last_event_seq,queue_version,queue_paused,
		transport_availability,engine_readiness,occupancy,active_attempt_id,pending_count,blocked_reasons,next_queue_sequence,next_message_sequence
	) VALUES(1,?,?,?,1,0,0,0,0,'online','blocked','idle',NULL,0,'["policy_unavailable"]',1,1)`, node.config.NodeID, node.config.OwnerID, node.config.RegistryVersion); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if node.config.StartupFault != nil {
		if err := node.config.StartupFault(StartupAfterMigration); err != nil {
			return err
		}
	}
	node.runtime.SchemaVersion = SchemaVersion
	node.runtime.SchemaFingerprint = currentSchemaFingerprint()
	return nil
}

var schemaStatements = []string{
	`CREATE TABLE schema_meta(singleton INTEGER PRIMARY KEY CHECK(singleton=1), fingerprint TEXT NOT NULL) STRICT`,
	`CREATE TABLE node_state(
		singleton INTEGER PRIMARY KEY CHECK(singleton=1), node_id TEXT NOT NULL UNIQUE, owner_id TEXT NOT NULL,
		registry_version INTEGER NOT NULL, epoch INTEGER NOT NULL, state_version INTEGER NOT NULL, last_event_seq INTEGER NOT NULL,
		queue_version INTEGER NOT NULL, queue_paused INTEGER NOT NULL CHECK(queue_paused IN(0,1)),
		transport_availability TEXT NOT NULL, engine_readiness TEXT NOT NULL, occupancy TEXT NOT NULL,
		active_attempt_id TEXT, pending_count INTEGER NOT NULL, blocked_reasons TEXT NOT NULL,
		next_queue_sequence INTEGER NOT NULL, next_message_sequence INTEGER NOT NULL
	) STRICT`,
	`CREATE TABLE dialogs(
		dialog_id TEXT PRIMARY KEY, node_id TEXT NOT NULL, owner_id TEXT NOT NULL, version INTEGER NOT NULL,
		title TEXT, created_at TEXT NOT NULL, engine_session_ref BLOB, policy_revision TEXT,
		UNIQUE(node_id,dialog_id)
	) STRICT`,
	`CREATE TABLE messages(
		message_id TEXT PRIMARY KEY, dialog_id TEXT NOT NULL REFERENCES dialogs(dialog_id), sequence INTEGER NOT NULL,
		version INTEGER NOT NULL, role TEXT NOT NULL CHECK(role IN('user','assistant')), text TEXT, content_json BLOB,
		disposition TEXT, command_id TEXT, request_id TEXT, attempt_id TEXT, finish_reason TEXT, created_at TEXT NOT NULL,
		UNIQUE(dialog_id,sequence)
	) STRICT`,
	`CREATE TABLE requests(
		request_id TEXT PRIMARY KEY, dialog_id TEXT NOT NULL REFERENCES dialogs(dialog_id), input_message_id TEXT NOT NULL REFERENCES messages(message_id),
		queue_sequence INTEGER NOT NULL UNIQUE, version INTEGER NOT NULL, status TEXT NOT NULL,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL
	) STRICT`,
	`CREATE TABLE attempts(
		attempt_id TEXT PRIMARY KEY, node_id TEXT NOT NULL, dialog_id TEXT NOT NULL REFERENCES dialogs(dialog_id),
		request_id TEXT NOT NULL REFERENCES requests(request_id), generation INTEGER NOT NULL, version INTEGER NOT NULL,
		state TEXT NOT NULL, effect_status TEXT NOT NULL, context_boundary_message_id TEXT, policy_hash TEXT,
		started_at TEXT, finished_at TEXT, engine_ref BLOB,
		output_bytes INTEGER NOT NULL DEFAULT 0 CHECK(output_bytes>=0), UNIQUE(request_id,generation)
	) STRICT`,
	`CREATE UNIQUE INDEX one_active_attempt ON attempts(node_id) WHERE state IN('dispatching','running','waiting_input','stopping','unknown')`,
	`CREATE TABLE commands(
		command_id TEXT PRIMARY KEY, actor_id TEXT NOT NULL, node_id TEXT NOT NULL, kind TEXT NOT NULL,
		canonical_json BLOB NOT NULL, canonical_payload_hash TEXT NOT NULL, receipt_json BLOB NOT NULL,
		accepted_at TEXT NOT NULL, event_seq INTEGER NOT NULL
	) STRICT`,
	`CREATE TABLE control_actions(
		command_id TEXT PRIMARY KEY, kind TEXT NOT NULL, attempt_id TEXT NOT NULL, message_id TEXT, actor_id TEXT,
		payload BLOB NOT NULL, status TEXT NOT NULL CHECK(status IN('pending','inflight','acknowledged','rejected','unknown'))
	) STRICT`,
	`CREATE TABLE events(
		node_id TEXT NOT NULL, seq INTEGER NOT NULL, epoch INTEGER NOT NULL, event_json BLOB NOT NULL,
		attempt_id TEXT, dialog_id TEXT, projection_key TEXT, projection_hash TEXT,
		late INTEGER NOT NULL DEFAULT 0 CHECK(late IN(0,1)), created_at TEXT NOT NULL,
		PRIMARY KEY(node_id,seq)
	) STRICT`,
	`CREATE UNIQUE INDEX one_event_projection ON events(attempt_id,projection_key) WHERE projection_key IS NOT NULL`,
	`CREATE TABLE tool_calls(
		call_id TEXT PRIMARY KEY, attempt_id TEXT NOT NULL REFERENCES attempts(attempt_id), action_hash TEXT NOT NULL,
		version INTEGER NOT NULL, status TEXT NOT NULL, safe_input_json BLOB NOT NULL, safe_result_json BLOB, effect_ref TEXT
	) STRICT`,
	`CREATE TABLE approvals(
		approval_id TEXT PRIMARY KEY, attempt_id TEXT NOT NULL REFERENCES attempts(attempt_id), call_id TEXT NOT NULL,
		action_hash TEXT NOT NULL, version INTEGER NOT NULL, status TEXT NOT NULL, decision TEXT, actor_id TEXT
	) STRICT`,
	`CREATE TABLE input_requests(
		input_request_id TEXT PRIMARY KEY, attempt_id TEXT NOT NULL REFERENCES attempts(attempt_id), version INTEGER NOT NULL,
		status TEXT NOT NULL, prompt_json BLOB NOT NULL, response_message_id TEXT
	) STRICT`,
	`CREATE TABLE artifacts(
		artifact_id TEXT PRIMARY KEY, dialog_id TEXT NOT NULL REFERENCES dialogs(dialog_id), attempt_id TEXT NOT NULL REFERENCES attempts(attempt_id),
		call_id TEXT, name TEXT NOT NULL, media_type TEXT NOT NULL, size_bytes INTEGER NOT NULL, sha256 TEXT NOT NULL,
		redaction TEXT NOT NULL, truncated INTEGER NOT NULL CHECK(truncated IN(0,1)), disposition TEXT NOT NULL, relative_path TEXT NOT NULL UNIQUE
	) STRICT`,
	`CREATE TABLE late_observations(
		observation_id INTEGER PRIMARY KEY, attempt_id TEXT NOT NULL, generation INTEGER NOT NULL, kind TEXT NOT NULL,
		observation_json BLOB NOT NULL, event_seq INTEGER NOT NULL, observed_at TEXT NOT NULL,
		projection_key TEXT, projection_hash TEXT, output_bytes INTEGER NOT NULL DEFAULT 0 CHECK(output_bytes>=0)
	) STRICT`,
	`CREATE UNIQUE INDEX one_late_projection ON late_observations(attempt_id,projection_key) WHERE projection_key IS NOT NULL`,
	`CREATE TABLE policy_snapshots(
		effective_hash TEXT PRIMARY KEY, revision TEXT NOT NULL, content BLOB NOT NULL, content_hash TEXT NOT NULL,
		tool_manifest BLOB NOT NULL, tool_manifest_hash TEXT NOT NULL, approval_mode TEXT NOT NULL
	) STRICT`,
}
