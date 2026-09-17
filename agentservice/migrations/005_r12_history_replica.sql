CREATE TABLE agent_service.history_replica_streams (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    stream_id uuid NOT NULL,
    logical_dialog_id uuid NOT NULL,
    node_id uuid NOT NULL,
    node_dialog_id uuid NOT NULL,
    binding_generation bigint NOT NULL CHECK (binding_generation BETWEEN 1 AND 9007199254740991),
    imported_through bigint NOT NULL DEFAULT 0 CHECK (imported_through BETWEEN 0 AND 9007199254740991),
    imported_chain_hash text NOT NULL
        CHECK (imported_chain_hash ~ '^[0-9a-f]{64}$'),
    imported_checkpoint jsonb NOT NULL CHECK (jsonb_typeof(imported_checkpoint) = 'object'),
    source_through bigint NOT NULL CHECK (source_through BETWEEN 0 AND 9007199254740991),
    source_chain_hash text NOT NULL CHECK (source_chain_hash ~ '^[0-9a-f]{64}$'),
    source_checkpoint jsonb NOT NULL CHECK (jsonb_typeof(source_checkpoint) = 'object'),
    source_captured_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    complete boolean NOT NULL DEFAULT false,
    PRIMARY KEY (owner_id, stream_id),
    UNIQUE (owner_id, logical_dialog_id, node_id, node_dialog_id, binding_generation),
    FOREIGN KEY (owner_id, logical_dialog_id)
        REFERENCES agent_service.logical_dialogs(owner_id, logical_dialog_id) ON DELETE RESTRICT,
    FOREIGN KEY (owner_id, node_id)
        REFERENCES agent_service.instances(owner_id, node_id) ON DELETE RESTRICT,
    CHECK (imported_through <= source_through),
    CHECK ((imported_checkpoint->>'throughSeq')::bigint = imported_through),
    CHECK (imported_checkpoint->>'chainHash' = imported_chain_hash),
    CHECK ((source_checkpoint->>'throughSeq')::bigint = source_through),
    CHECK (source_checkpoint->>'chainHash' = source_chain_hash),
    CHECK (complete = (imported_through = source_through AND (source_checkpoint->>'ready')::boolean
        AND (imported_checkpoint->>'ready')::boolean))
);

CREATE INDEX history_replica_streams_dialog_idx
    ON agent_service.history_replica_streams(owner_id, logical_dialog_id, observed_at DESC, stream_id);

CREATE TABLE agent_service.history_replica_records (
    owner_id text NOT NULL,
    stream_id uuid NOT NULL,
    stream_seq bigint NOT NULL CHECK (stream_seq BETWEEN 1 AND 9007199254740991),
    record_id uuid NOT NULL,
    record_type text NOT NULL
        CHECK (record_type IN ('entry','execution_fact','receipt_revision','text_manifest','text_chunk','asset_manifest')),
    entity_id uuid NOT NULL,
    revision bigint NOT NULL CHECK (revision BETWEEN 1 AND 9007199254740991),
    record_hash text NOT NULL CHECK (record_hash ~ '^[0-9a-f]{64}$'),
    prev_hash text NOT NULL CHECK (prev_hash ~ '^[0-9a-f]{64}$'),
    chain_hash text NOT NULL CHECK (chain_hash ~ '^[0-9a-f]{64}$'),
    record_json jsonb NOT NULL CHECK (jsonb_typeof(record_json) = 'object'),
    imported_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, stream_id, stream_seq),
    UNIQUE (owner_id, stream_id, record_id),
    FOREIGN KEY (owner_id, stream_id)
        REFERENCES agent_service.history_replica_streams(owner_id, stream_id) ON DELETE RESTRICT
);

CREATE INDEX history_replica_records_entity_idx
    ON agent_service.history_replica_records(owner_id, entity_id, record_type, revision);

CREATE TABLE agent_service.history_entries (
    owner_id text NOT NULL,
    logical_dialog_id uuid NOT NULL,
    source_stream_id uuid NOT NULL,
    source_stream_seq bigint NOT NULL CHECK (source_stream_seq BETWEEN 1 AND 9007199254740991),
    entry_id uuid NOT NULL,
    entry_hash text NOT NULL CHECK (entry_hash ~ '^[0-9a-f]{64}$'),
    origin_node_id uuid NOT NULL,
    origin_node_dialog_id uuid NOT NULL,
    message_id uuid NOT NULL,
    request_id uuid,
    attempt_id uuid,
    role text NOT NULL CHECK (role IN ('user','assistant')),
    created_at timestamptz NOT NULL,
    execution_ordinal bigint NOT NULL CHECK (execution_ordinal BETWEEN 1 AND 9007199254740991),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    imported_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, logical_dialog_id, entry_id),
    FOREIGN KEY (owner_id, logical_dialog_id)
        REFERENCES agent_service.logical_dialogs(owner_id, logical_dialog_id) ON DELETE RESTRICT,
    FOREIGN KEY (owner_id, source_stream_id, source_stream_seq)
        REFERENCES agent_service.history_replica_records(owner_id, stream_id, stream_seq) ON DELETE RESTRICT
);

CREATE INDEX history_entries_read_idx
    ON agent_service.history_entries(owner_id, logical_dialog_id, created_at, entry_id);

CREATE TABLE agent_service.history_execution_facts (
    owner_id text NOT NULL,
    logical_dialog_id uuid NOT NULL,
    source_stream_id uuid NOT NULL,
    source_stream_seq bigint NOT NULL CHECK (source_stream_seq BETWEEN 1 AND 9007199254740991),
    fact_id uuid NOT NULL,
    fact_hash text NOT NULL CHECK (fact_hash ~ '^[0-9a-f]{64}$'),
    origin_node_id uuid NOT NULL,
    origin_node_dialog_id uuid NOT NULL,
    request_id uuid,
    attempt_id uuid,
    event_seq bigint NOT NULL CHECK (event_seq BETWEEN 1 AND 9007199254740991),
    kind text NOT NULL CHECK (octet_length(kind) BETWEEN 1 AND 120),
    occurred_at timestamptz NOT NULL,
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    imported_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, logical_dialog_id, fact_id),
    FOREIGN KEY (owner_id, logical_dialog_id)
        REFERENCES agent_service.logical_dialogs(owner_id, logical_dialog_id) ON DELETE RESTRICT,
    FOREIGN KEY (owner_id, source_stream_id, source_stream_seq)
        REFERENCES agent_service.history_replica_records(owner_id, stream_id, stream_seq) ON DELETE RESTRICT
);

CREATE INDEX history_execution_facts_attempt_idx
    ON agent_service.history_execution_facts(owner_id, logical_dialog_id, attempt_id, event_seq);

CREATE TABLE agent_service.history_receipt_revisions (
    owner_id text NOT NULL,
    logical_dialog_id uuid NOT NULL,
    source_stream_id uuid NOT NULL,
    source_stream_seq bigint NOT NULL CHECK (source_stream_seq BETWEEN 1 AND 9007199254740991),
    receipt_revision_id uuid NOT NULL,
    receipt_hash text NOT NULL CHECK (receipt_hash ~ '^[0-9a-f]{64}$'),
    origin_node_id uuid NOT NULL,
    origin_node_dialog_id uuid NOT NULL,
    command_id uuid NOT NULL,
    command_kind text NOT NULL CHECK (octet_length(command_kind) BETWEEN 1 AND 120),
    fingerprint text NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    request_id uuid,
    attempt_id uuid,
    revision bigint NOT NULL CHECK (revision BETWEEN 1 AND 9007199254740991),
    predecessor_hash text NOT NULL CHECK (predecessor_hash ~ '^[0-9a-f]{64}$'),
    accepted_at timestamptz NOT NULL,
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    imported_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, logical_dialog_id, receipt_revision_id),
    UNIQUE (owner_id, origin_node_id, command_id, revision),
    FOREIGN KEY (owner_id, logical_dialog_id)
        REFERENCES agent_service.logical_dialogs(owner_id, logical_dialog_id) ON DELETE RESTRICT,
    FOREIGN KEY (owner_id, source_stream_id, source_stream_seq)
        REFERENCES agent_service.history_replica_records(owner_id, stream_id, stream_seq) ON DELETE RESTRICT
);

CREATE INDEX history_receipts_origin_lookup_idx
    ON agent_service.history_receipt_revisions(owner_id, origin_node_id, command_id, revision DESC);

CREATE TABLE agent_service.history_text_manifests (
    owner_id text NOT NULL,
    logical_dialog_id uuid NOT NULL,
    source_stream_id uuid NOT NULL,
    source_stream_seq bigint NOT NULL CHECK (source_stream_seq BETWEEN 1 AND 9007199254740991),
    text_id uuid NOT NULL,
    manifest_hash text NOT NULL CHECK (manifest_hash ~ '^[0-9a-f]{64}$'),
    attempt_id uuid NOT NULL,
    complete boolean NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 0 AND 9007199254740991),
    text_sha256 text NOT NULL CHECK (text_sha256 ~ '^[0-9a-f]{64}$'),
    chunk_count bigint NOT NULL CHECK (chunk_count BETWEEN 0 AND 9007199254740991),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    imported_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, logical_dialog_id, text_id),
    FOREIGN KEY (owner_id, logical_dialog_id)
        REFERENCES agent_service.logical_dialogs(owner_id, logical_dialog_id) ON DELETE RESTRICT,
    FOREIGN KEY (owner_id, source_stream_id, source_stream_seq)
        REFERENCES agent_service.history_replica_records(owner_id, stream_id, stream_seq) ON DELETE RESTRICT
);

CREATE TABLE agent_service.history_text_chunks (
    owner_id text NOT NULL,
    logical_dialog_id uuid NOT NULL,
    source_stream_id uuid NOT NULL,
    source_stream_seq bigint NOT NULL CHECK (source_stream_seq BETWEEN 1 AND 9007199254740991),
    text_id uuid NOT NULL,
    chunk_index bigint NOT NULL CHECK (chunk_index BETWEEN 0 AND 9007199254740991),
    offset_bytes bigint NOT NULL CHECK (offset_bytes BETWEEN 0 AND 9007199254740991),
    size_bytes integer NOT NULL CHECK (size_bytes BETWEEN 0 AND 1048576),
    chunk_sha256 text NOT NULL CHECK (chunk_sha256 ~ '^[0-9a-f]{64}$'),
    content bytea NOT NULL CHECK (octet_length(content) = size_bytes),
    imported_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, logical_dialog_id, text_id, chunk_index),
    FOREIGN KEY (owner_id, logical_dialog_id, text_id)
        REFERENCES agent_service.history_text_manifests(owner_id, logical_dialog_id, text_id) ON DELETE RESTRICT,
    FOREIGN KEY (owner_id, source_stream_id, source_stream_seq)
        REFERENCES agent_service.history_replica_records(owner_id, stream_id, stream_seq) ON DELETE RESTRICT
);

CREATE TABLE agent_service.history_asset_manifests (
    owner_id text NOT NULL,
    logical_dialog_id uuid NOT NULL,
    source_stream_id uuid NOT NULL,
    source_stream_seq bigint NOT NULL CHECK (source_stream_seq BETWEEN 1 AND 9007199254740991),
    manifest_entity_id uuid NOT NULL,
    revision bigint NOT NULL CHECK (revision BETWEEN 1 AND 9007199254740991),
    manifest_hash text NOT NULL CHECK (manifest_hash ~ '^[0-9a-f]{64}$'),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    imported_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, logical_dialog_id, manifest_entity_id, revision),
    FOREIGN KEY (owner_id, logical_dialog_id)
        REFERENCES agent_service.logical_dialogs(owner_id, logical_dialog_id) ON DELETE RESTRICT,
    FOREIGN KEY (owner_id, source_stream_id, source_stream_seq)
        REFERENCES agent_service.history_replica_records(owner_id, stream_id, stream_seq) ON DELETE RESTRICT
);
