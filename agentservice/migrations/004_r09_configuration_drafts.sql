CREATE TABLE agent_service.configuration_drafts (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    node_id uuid NOT NULL,
    draft_version bigint NOT NULL CHECK (draft_version BETWEEN 1 AND 9007199254740991),
    schema_id text NOT NULL DEFAULT 'agent-configuration-draft-v2' CHECK (schema_id = 'agent-configuration-draft-v2'),
    raw_json_text text NOT NULL CHECK (octet_length(raw_json_text) <= 262144),
    raw_dockerfile_text text NOT NULL CHECK (octet_length(raw_dockerfile_text) <= 131072),
    build_context_manifest jsonb CHECK (build_context_manifest IS NULL OR jsonb_typeof(build_context_manifest) = 'object'),
    validation jsonb NOT NULL CHECK (jsonb_typeof(validation) = 'object'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, node_id),
    FOREIGN KEY (owner_id, node_id) REFERENCES agent_service.instances(owner_id, node_id) ON DELETE RESTRICT
);

CREATE INDEX configuration_drafts_owner_idx ON agent_service.configuration_drafts(owner_id, node_id);
