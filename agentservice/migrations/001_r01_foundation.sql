CREATE TABLE agent_service.hosts (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    host_id uuid NOT NULL,
    name text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 200),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, host_id)
);

CREATE TABLE agent_service.registry_state (
    owner_id text PRIMARY KEY CHECK (length(owner_id) BETWEEN 1 AND 128),
    registry_version bigint NOT NULL CHECK (registry_version > 0),
    manifest_sha256 text NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    host_count integer NOT NULL CHECK (host_count >= 1),
    node_count integer NOT NULL CHECK (node_count >= 0),
    imported_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE agent_service.instances (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    node_id uuid NOT NULL,
    host_id uuid NOT NULL,
    name text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 200),
    engine text NOT NULL CHECK (engine IN ('cursor', 'codex')),
    registry_mode text NOT NULL CHECK (registry_mode IN ('live', 'fixture')),
    registration_mode text NOT NULL CHECK (registration_mode IN ('legacy_readonly', 'compatible')),
    registry_version bigint NOT NULL CHECK (registry_version > 0),
    manifest_sha256 text NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    process_state text NOT NULL DEFAULT 'unknown' CHECK (process_state IN ('running', 'stopped', 'unknown')),
    connection_state text NOT NULL DEFAULT 'unknown' CHECK (connection_state IN ('online', 'offline', 'unknown')),
    readiness_state text NOT NULL DEFAULT 'unknown' CHECK (readiness_state IN ('ready', 'unready', 'unknown')),
    occupancy_state text NOT NULL DEFAULT 'unknown' CHECK (occupancy_state IN ('idle', 'busy', 'unknown')),
    observed_at timestamptz,
    observation_source text CHECK (observation_source IS NULL OR octet_length(observation_source) BETWEEN 1 AND 100),
    pending_count bigint CHECK (pending_count IS NULL OR pending_count BETWEEN 0 AND 9007199254740991),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, node_id),
    FOREIGN KEY (owner_id, host_id) REFERENCES agent_service.hosts(owner_id, host_id),
    CHECK ((observed_at IS NULL) = (observation_source IS NULL)),
    CHECK (pending_count IS NULL OR (observed_at IS NOT NULL AND observation_source IS NOT NULL))
);

CREATE INDEX instances_owner_host_idx
    ON agent_service.instances(owner_id, host_id, node_id);

CREATE TABLE agent_service.logical_dialogs (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    logical_dialog_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    deleted_at timestamptz,
    PRIMARY KEY (owner_id, logical_dialog_id)
);

CREATE TABLE agent_service.dialog_bindings (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    logical_dialog_id uuid NOT NULL,
    binding_version bigint NOT NULL CHECK (binding_version > 0),
    node_id uuid NOT NULL,
    node_dialog_id uuid NOT NULL,
    state text NOT NULL DEFAULT 'active' CHECK (state = 'active'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, logical_dialog_id, binding_version),
    UNIQUE (owner_id, node_id, node_dialog_id),
    FOREIGN KEY (owner_id, logical_dialog_id)
        REFERENCES agent_service.logical_dialogs(owner_id, logical_dialog_id),
    FOREIGN KEY (owner_id, node_id)
        REFERENCES agent_service.instances(owner_id, node_id)
);

CREATE UNIQUE INDEX dialog_bindings_one_active_idx
    ON agent_service.dialog_bindings(owner_id, logical_dialog_id)
    WHERE state = 'active';

CREATE INDEX dialog_bindings_node_idx
    ON agent_service.dialog_bindings(owner_id, node_id, node_dialog_id);

CREATE TABLE agent_service.retirement_descriptors (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    node_id uuid NOT NULL,
    descriptor_version bigint NOT NULL CHECK (descriptor_version > 0),
    schema_id text NOT NULL DEFAULT 'agent-retirement-v1'
        CHECK (schema_id = 'agent-retirement-v1'),
    kind text NOT NULL CHECK (kind IN ('managed_stopped', 'external_detached')),
    state text NOT NULL CHECK (state IN ('pending', 'verified')),
    observed_at timestamptz NOT NULL,
    process_exit_observed boolean,
    ownership_release_proof text
        CHECK (ownership_release_proof IS NULL OR octet_length(ownership_release_proof) BETWEEN 1 AND 500),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, node_id, descriptor_version),
    FOREIGN KEY (owner_id, node_id)
        REFERENCES agent_service.instances(owner_id, node_id),
    CHECK (
        state <> 'verified'
        OR (kind = 'managed_stopped' AND process_exit_observed IS TRUE AND ownership_release_proof IS NULL)
        OR (kind = 'external_detached' AND process_exit_observed IS NULL AND ownership_release_proof IS NOT NULL)
    )
);
