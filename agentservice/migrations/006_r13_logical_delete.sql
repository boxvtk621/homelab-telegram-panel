ALTER TABLE agent_service.logical_dialogs
    ADD COLUMN hold_scope_revision bigint NOT NULL DEFAULT 0
        CHECK (hold_scope_revision BETWEEN 0 AND 9007199254740991);

ALTER TABLE agent_service.dialog_bindings
    DROP CONSTRAINT dialog_bindings_state_check;

ALTER TABLE agent_service.dialog_bindings
    ADD CONSTRAINT dialog_bindings_state_check
        CHECK (state IN ('active','deleting','tombstoned'));

DROP INDEX agent_service.dialog_bindings_one_active_idx;

CREATE UNIQUE INDEX dialog_bindings_one_active_idx
    ON agent_service.dialog_bindings(owner_id, logical_dialog_id)
    WHERE state IN ('active','deleting');

CREATE TABLE agent_service.dialog_operation_reservations (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    operation_id uuid NOT NULL,
    logical_dialog_id uuid NOT NULL,
    operation_kind text NOT NULL CHECK (operation_kind IN ('delete','transfer','retirement')),
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','succeeded','failed')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, operation_id),
    FOREIGN KEY (owner_id, logical_dialog_id)
        REFERENCES agent_service.logical_dialogs(owner_id, logical_dialog_id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX dialog_operation_one_active_idx
    ON agent_service.dialog_operation_reservations(owner_id, logical_dialog_id)
    WHERE state = 'active';

CREATE TABLE agent_service.dialog_closure_descriptors (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    logical_dialog_id uuid NOT NULL,
    binding_version bigint NOT NULL CHECK (binding_version BETWEEN 1 AND 9007199254740991),
    node_id uuid NOT NULL,
    node_dialog_id uuid NOT NULL,
    dialog_version bigint NOT NULL CHECK (dialog_version BETWEEN 1 AND 9007199254740991),
    descriptor_version bigint NOT NULL CHECK (descriptor_version BETWEEN 1 AND 9007199254740991),
    state text NOT NULL CHECK (state = 'verified'),
    zero_pending boolean NOT NULL CHECK (zero_pending),
    final_checkpoint jsonb NOT NULL CHECK (jsonb_typeof(final_checkpoint) = 'object'),
    final_checkpoint_sha256 text NOT NULL CHECK (final_checkpoint_sha256 ~ '^[0-9a-f]{64}$'),
    verified_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, logical_dialog_id, descriptor_version),
    UNIQUE (owner_id, logical_dialog_id, binding_version),
    FOREIGN KEY (owner_id, logical_dialog_id, binding_version)
        REFERENCES agent_service.dialog_bindings(owner_id, logical_dialog_id, binding_version) ON DELETE RESTRICT
);

CREATE FUNCTION agent_service.reject_dialog_closure_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'dialog closure descriptors are immutable';
END;
$$;

CREATE TRIGGER dialog_closure_no_update
    BEFORE UPDATE OR DELETE ON agent_service.dialog_closure_descriptors
    FOR EACH ROW EXECUTE FUNCTION agent_service.reject_dialog_closure_mutation();

CREATE TABLE agent_service.logical_dialog_delete_operations (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    operation_id uuid NOT NULL,
    request_hash text NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    command_id uuid NOT NULL,
    logical_dialog_id uuid NOT NULL,
    expected_binding_version bigint NOT NULL CHECK (expected_binding_version BETWEEN 1 AND 9007199254740991),
    expected_dialog_version bigint NOT NULL CHECK (expected_dialog_version BETWEEN 1 AND 9007199254740991),
    node_id uuid NOT NULL,
    node_dialog_id uuid NOT NULL,
    registry_version bigint NOT NULL CHECK (registry_version BETWEEN 1 AND 9007199254740991),
    identity_epoch bigint NOT NULL CHECK (identity_epoch BETWEEN 1 AND 9007199254740991),
    hold_scope_revision bigint NOT NULL CHECK (hold_scope_revision BETWEEN 0 AND 9007199254740991),
    hold_version bigint CHECK (hold_version BETWEEN 1 AND 9007199254740991),
    phase text NOT NULL DEFAULT 'accepted'
        CHECK (phase IN ('accepted','holding','reconciling','succeeded','failed')),
    effect_state text NOT NULL DEFAULT 'not_sent'
        CHECK (effect_state IN ('not_sent','sent','unknown','reconciled','failed')),
    operation_version bigint NOT NULL DEFAULT 1 CHECK (operation_version BETWEEN 1 AND 9007199254740991),
    archived boolean NOT NULL DEFAULT false,
    result_code text CHECK (result_code IS NULL OR result_code ~ '^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$'),
    node_receipt jsonb CHECK (node_receipt IS NULL OR jsonb_typeof(node_receipt) = 'object'),
    accepted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, operation_id),
    FOREIGN KEY (owner_id, operation_id)
        REFERENCES agent_service.dialog_operation_reservations(owner_id, operation_id) ON DELETE RESTRICT,
    FOREIGN KEY (owner_id, logical_dialog_id, expected_binding_version)
        REFERENCES agent_service.dialog_bindings(owner_id, logical_dialog_id, binding_version) ON DELETE RESTRICT,
    FOREIGN KEY (owner_id, node_id)
        REFERENCES agent_service.instances(owner_id, node_id) ON DELETE RESTRICT,
    CHECK (effect_state <> 'unknown' OR phase = 'reconciling'),
    CHECK (phase <> 'succeeded' OR effect_state = 'reconciled'),
    CHECK (phase <> 'failed' OR effect_state = 'failed')
);

CREATE TABLE agent_service.logical_dialog_tombstones (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    logical_dialog_id uuid NOT NULL,
    operation_id uuid NOT NULL,
    binding_version bigint NOT NULL CHECK (binding_version BETWEEN 1 AND 9007199254740991),
    node_id uuid NOT NULL,
    node_dialog_id uuid NOT NULL,
    dialog_version bigint NOT NULL CHECK (dialog_version BETWEEN 1 AND 9007199254740991),
    source text NOT NULL CHECK (source IN ('active_node_receipt','archive_closure')),
    event_version bigint NOT NULL DEFAULT 1 CHECK (event_version = 1),
    event_sha256 text NOT NULL CHECK (event_sha256 ~ '^[0-9a-f]{64}$'),
    receipt jsonb CHECK (receipt IS NULL OR jsonb_typeof(receipt) = 'object'),
    deleted_at timestamptz NOT NULL,
    PRIMARY KEY (owner_id, logical_dialog_id),
    UNIQUE (owner_id, operation_id),
    FOREIGN KEY (owner_id, operation_id)
        REFERENCES agent_service.logical_dialog_delete_operations(owner_id, operation_id) ON DELETE RESTRICT,
    FOREIGN KEY (owner_id, logical_dialog_id)
        REFERENCES agent_service.logical_dialogs(owner_id, logical_dialog_id) ON DELETE RESTRICT
);

CREATE FUNCTION agent_service.reject_logical_tombstone_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'logical dialog tombstones are immutable';
END;
$$;

CREATE TRIGGER logical_tombstone_no_update
    BEFORE UPDATE OR DELETE ON agent_service.logical_dialog_tombstones
    FOR EACH ROW EXECUTE FUNCTION agent_service.reject_logical_tombstone_mutation();
