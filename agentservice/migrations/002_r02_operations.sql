ALTER TABLE agent_service.instances
    ADD COLUMN registration_revision bigint NOT NULL DEFAULT 1
        CHECK (registration_revision BETWEEN 1 AND 9007199254740991),
    ADD COLUMN registration_epoch bigint NOT NULL DEFAULT 0
        CHECK (registration_epoch BETWEEN 0 AND 9007199254740991),
    ADD COLUMN registration_binding_sha256 text
        CHECK (registration_binding_sha256 IS NULL OR registration_binding_sha256 ~ '^[0-9a-f]{64}$'),
	ADD COLUMN registration_projected boolean NOT NULL DEFAULT false,
    ADD COLUMN operation_generation bigint NOT NULL DEFAULT 0
        CHECK (operation_generation BETWEEN 0 AND 9007199254740991);

ALTER TABLE agent_service.registry_state
	ADD COLUMN registry_schema_id text NOT NULL DEFAULT 'legacy'
		CHECK (registry_schema_id IN ('legacy','harness-router-registry-v1')),
	ADD COLUMN registry_envelope json
	CHECK (registry_envelope IS NULL OR json_typeof(registry_envelope) = 'object');

CREATE TABLE agent_service.operation_ids (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    operation_id text NOT NULL CHECK (operation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$'),
    operation_kind text NOT NULL CHECK (operation_kind IN ('adapter.fixture','registry.install')),
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (owner_id, operation_id)
);

CREATE TABLE agent_service.operations (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    operation_id text NOT NULL CHECK (operation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$'),
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
	intent jsonb NOT NULL CHECK (jsonb_typeof(intent) = 'object'),
    kind text NOT NULL CHECK (kind = 'adapter.fixture'),
    node_id uuid NOT NULL,
	host_id uuid NOT NULL,
    expected_registration_revision bigint NOT NULL
        CHECK (expected_registration_revision BETWEEN 1 AND 9007199254740991),
	expected_registration_epoch bigint NOT NULL
		CHECK (expected_registration_epoch BETWEEN 1 AND 9007199254740991),
	expected_registration_binding_sha256 text NOT NULL
		CHECK (expected_registration_binding_sha256 ~ '^[0-9a-f]{64}$'),
    expected_generation bigint NOT NULL
        CHECK (expected_generation BETWEEN 0 AND 9007199254740990),
    generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 9007199254740991),
    phase text NOT NULL DEFAULT 'accepted'
        CHECK (phase IN ('accepted','validating','waiting','building','preparing','applying','verifying','reconciling','succeeded','failed')),
    effect_state text NOT NULL DEFAULT 'not_sent'
        CHECK (effect_state IN ('not_sent','sent','acknowledged','reconciled','unknown','failed')),
    operation_version bigint NOT NULL DEFAULT 1
        CHECK (operation_version BETWEEN 1 AND 9007199254740991),
    result_code text CHECK (result_code IS NULL OR result_code ~ '^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$'),
    worker_id text CHECK (worker_id IS NULL OR worker_id ~ '^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$'),
    worker_token uuid,
    lease_expires_at timestamptz,
    accepted_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (owner_id, operation_id),
    FOREIGN KEY (owner_id, operation_id)
        REFERENCES agent_service.operation_ids(owner_id, operation_id) ON DELETE RESTRICT,
    UNIQUE (owner_id, node_id, generation),
    FOREIGN KEY (owner_id, node_id) REFERENCES agent_service.instances(owner_id, node_id),
    CHECK ((worker_id IS NULL) = (worker_token IS NULL)),
    CHECK ((worker_id IS NULL) = (lease_expires_at IS NULL)),
    CHECK (generation = expected_generation + 1),
    CHECK (effect_state <> 'unknown' OR phase = 'reconciling'),
    CHECK (phase <> 'succeeded' OR effect_state IN ('acknowledged','reconciled')),
    CHECK (phase <> 'failed' OR effect_state = 'failed')
);

CREATE UNIQUE INDEX operations_one_active_per_node_idx
    ON agent_service.operations(owner_id, node_id)
    WHERE phase NOT IN ('succeeded', 'failed');

CREATE INDEX operations_owner_updated_idx
    ON agent_service.operations(owner_id, updated_at, operation_id);

CREATE TABLE agent_service.operation_steps (
    owner_id text NOT NULL,
    operation_id text NOT NULL,
    step_id text NOT NULL CHECK (step_id ~ '^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$'),
    action text NOT NULL CHECK (action = 'adapter.fixture.apply'),
    generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 9007199254740991),
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    resource_ids jsonb NOT NULL CHECK (jsonb_typeof(resource_ids) = 'array'),
    state text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending','sent','acknowledged','reconciled','failed')),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (owner_id, operation_id, step_id),
    FOREIGN KEY (owner_id, operation_id)
        REFERENCES agent_service.operations(owner_id, operation_id) ON DELETE RESTRICT
);

CREATE TABLE agent_service.registry_operations (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    operation_id text NOT NULL CHECK (operation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$'),
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    intent json NOT NULL CHECK (json_typeof(intent) = 'object'),
    expected_registry_version bigint NOT NULL
        CHECK (expected_registry_version BETWEEN 1 AND 9007199254740990),
    expected_registry_sha256 text NOT NULL CHECK (expected_registry_sha256 ~ '^[0-9a-f]{64}$'),
    candidate_registry_version bigint NOT NULL
        CHECK (candidate_registry_version BETWEEN 2 AND 9007199254740991),
    candidate_registry_sha256 text NOT NULL CHECK (candidate_registry_sha256 ~ '^[0-9a-f]{64}$'),
    affected_node_ids uuid[] NOT NULL CHECK (cardinality(affected_node_ids) BETWEEN 1 AND 1000),
    new_node_host_id uuid,
    phase text NOT NULL DEFAULT 'accepted'
        CHECK (phase IN ('accepted','applying','reconciling','succeeded','failed')),
    effect_state text NOT NULL DEFAULT 'not_sent'
        CHECK (effect_state IN ('not_sent','sent','unknown','acknowledged','reconciled','failed')),
    operation_version bigint NOT NULL DEFAULT 1
        CHECK (operation_version BETWEEN 1 AND 9007199254740991),
    result_code text CHECK (result_code IS NULL OR result_code ~ '^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$'),
    accepted_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (owner_id, operation_id),
    FOREIGN KEY (owner_id, operation_id)
        REFERENCES agent_service.operation_ids(owner_id, operation_id) ON DELETE RESTRICT,
    FOREIGN KEY (owner_id, new_node_host_id) REFERENCES agent_service.hosts(owner_id, host_id),
    CHECK (candidate_registry_version = expected_registry_version + 1),
	CHECK (
		(phase='accepted' AND effect_state='not_sent') OR
		(phase='applying' AND effect_state='sent') OR
		(phase='reconciling' AND effect_state='unknown') OR
		(phase='succeeded' AND effect_state IN ('acknowledged','reconciled')) OR
		(phase='failed' AND effect_state='failed')
	)
);

CREATE UNIQUE INDEX registry_operations_one_active_per_owner_idx
    ON agent_service.registry_operations(owner_id)
    WHERE phase NOT IN ('succeeded','failed');

CREATE INDEX registry_operations_owner_updated_idx
    ON agent_service.registry_operations(owner_id,updated_at,operation_id);
