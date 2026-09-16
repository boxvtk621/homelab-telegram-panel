CREATE TABLE agent_service.host_descriptors (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    host_id uuid NOT NULL,
    host_version bigint NOT NULL CHECK (host_version BETWEEN 1 AND 9007199254740991),
    probe_revision bigint NOT NULL DEFAULT 0 CHECK (probe_revision BETWEEN 0 AND 9007199254740991),
    schema_id text NOT NULL DEFAULT 'agent-host-v1' CHECK (schema_id = 'agent-host-v1'),
    display_name text NOT NULL CHECK (octet_length(display_name) BETWEEN 1 AND 120),
    transport text NOT NULL CHECK (transport IN ('local','ssh')),
    target_ref text NOT NULL CHECK (target_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$'),
    credential_ref text CHECK (credential_ref IS NULL OR credential_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$'),
    registry_credential_ref text CHECK (registry_credential_ref IS NULL OR registry_credential_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$'),
    expected_host_key text CHECK (expected_host_key IS NULL OR expected_host_key ~ '^SHA256:[A-Za-z0-9+/]{20,64}$'),
    docker_context_ref text NOT NULL CHECK (docker_context_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$'),
    expected_identity_sha256 text CHECK (expected_identity_sha256 IS NULL OR expected_identity_sha256 ~ '^[0-9a-f]{64}$'),
    host_platform text NOT NULL CHECK (host_platform IN ('linux','darwin','windows')),
    host_architecture text NOT NULL CHECK (host_architecture IN ('amd64','arm64')),
    observed_at timestamptz,
    availability text NOT NULL DEFAULT 'unverified' CHECK (availability IN ('unverified','ready','unavailable')),
    failure_stage text CHECK (failure_stage IS NULL OR failure_stage ~ '^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$'),
    failure_code text CHECK (failure_code IS NULL OR failure_code ~ '^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$'),
    next_action text CHECK (next_action IS NULL OR octet_length(next_action) BETWEEN 1 AND 300),
    host_key_sha256 text CHECK (host_key_sha256 IS NULL OR octet_length(host_key_sha256) BETWEEN 24 AND 80),
    daemon_id text CHECK (daemon_id IS NULL OR octet_length(daemon_id) BETWEEN 1 AND 128),
    context_endpoint text CHECK (context_endpoint IS NULL OR octet_length(context_endpoint) BETWEEN 1 AND 80),
    engine_os text CHECK (engine_os IS NULL OR octet_length(engine_os) BETWEEN 1 AND 160),
    architecture text CHECK (architecture IS NULL OR octet_length(architecture) BETWEEN 1 AND 160),
    api_version text CHECK (api_version IS NULL OR octet_length(api_version) BETWEEN 1 AND 32),
    engine_version text CHECK (engine_version IS NULL OR octet_length(engine_version) BETWEEN 1 AND 64),
    capabilities jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(capabilities) = 'array'),
    identity_sha256 text CHECK (identity_sha256 IS NULL OR identity_sha256 ~ '^[0-9a-f]{64}$'),
    registry_availability text NOT NULL DEFAULT 'not_configured'
        CHECK (registry_availability IN ('not_configured','not_checked','ready','unavailable')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, host_id),
    FOREIGN KEY (owner_id, host_id) REFERENCES agent_service.hosts(owner_id, host_id) ON DELETE RESTRICT,
    CHECK ((transport = 'local' AND credential_ref IS NULL AND expected_host_key IS NULL) OR
           (transport = 'ssh' AND credential_ref IS NOT NULL)),
    CHECK ((availability = 'unverified' AND observed_at IS NULL AND failure_code IS NULL AND identity_sha256 IS NULL) OR
           (availability = 'ready' AND observed_at IS NOT NULL AND failure_code IS NULL AND identity_sha256 IS NOT NULL AND engine_os = 'linux' AND architecture IN ('amd64','arm64')) OR
           (availability = 'unavailable' AND observed_at IS NOT NULL AND failure_stage IS NOT NULL AND failure_code IS NOT NULL AND next_action IS NOT NULL AND identity_sha256 IS NULL))
);

CREATE INDEX host_descriptors_owner_page_idx
    ON agent_service.host_descriptors(owner_id, host_id);
