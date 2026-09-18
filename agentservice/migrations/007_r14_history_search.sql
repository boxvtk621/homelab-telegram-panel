CREATE TABLE agent_service.history_dialog_metadata (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    logical_dialog_id uuid NOT NULL,
    node_id uuid NOT NULL,
    node_dialog_id uuid NOT NULL,
    binding_generation bigint NOT NULL CHECK (binding_generation BETWEEN 1 AND 9007199254740991),
    title text NOT NULL CHECK (octet_length(title) BETWEEN 0 AND 1000),
    archived boolean NOT NULL,
    source_updated_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, logical_dialog_id),
    FOREIGN KEY (owner_id, logical_dialog_id)
        REFERENCES agent_service.logical_dialogs(owner_id, logical_dialog_id) ON DELETE RESTRICT,
    FOREIGN KEY (owner_id, logical_dialog_id, binding_generation)
        REFERENCES agent_service.dialog_bindings(owner_id, logical_dialog_id, binding_version) ON DELETE RESTRICT,
    FOREIGN KEY (owner_id, node_id)
        REFERENCES agent_service.instances(owner_id, node_id) ON DELETE RESTRICT
);

CREATE INDEX history_dialog_metadata_filter_idx
    ON agent_service.history_dialog_metadata(owner_id, node_id, archived, logical_dialog_id);

CREATE TABLE agent_service.history_search_documents (
    owner_id text NOT NULL,
    logical_dialog_id uuid NOT NULL,
    entry_id uuid NOT NULL,
    node_id uuid NOT NULL,
    node_dialog_id uuid NOT NULL,
    role text NOT NULL CHECK (role IN ('user','assistant')),
    kind text NOT NULL CHECK (octet_length(kind) BETWEEN 1 AND 120),
    created_at timestamptz NOT NULL,
    title_text text NOT NULL DEFAULT '',
    message_text text NOT NULL DEFAULT '',
    normalized_text text GENERATED ALWAYS AS (
        lower(regexp_replace(title_text || ' ' || message_text, '\s+', ' ', 'g'))
    ) STORED,
    search_vector tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', title_text), 'A') ||
        setweight(to_tsvector('simple', message_text), 'B')
    ) STORED,
    PRIMARY KEY (owner_id, logical_dialog_id, entry_id),
    FOREIGN KEY (owner_id, logical_dialog_id, entry_id)
        REFERENCES agent_service.history_entries(owner_id, logical_dialog_id, entry_id) ON DELETE RESTRICT
);

CREATE INDEX history_search_documents_gin_idx
    ON agent_service.history_search_documents USING gin(search_vector);
CREATE INDEX history_search_documents_order_idx
    ON agent_service.history_search_documents(owner_id, created_at DESC, entry_id DESC);
CREATE INDEX history_search_documents_dialog_idx
    ON agent_service.history_search_documents(owner_id, logical_dialog_id, created_at DESC, entry_id DESC);

-- R14 resolves safe tool text to the newest admitted transcript entry for an
-- attempt.  Keep both sides of that lookup indexed so long-lived dialogs do
-- not turn a bounded replica tail into a manifests-by-entries scan.
CREATE INDEX history_entries_search_attempt_idx
    ON agent_service.history_entries(owner_id, logical_dialog_id, attempt_id, created_at DESC, entry_id DESC)
    WHERE attempt_id IS NOT NULL;
CREATE INDEX history_text_manifests_search_attempt_idx
    ON agent_service.history_text_manifests(owner_id, logical_dialog_id, attempt_id, text_id)
    WHERE complete;

CREATE TABLE agent_service.history_search_tool_sources (
    owner_id text NOT NULL,
    logical_dialog_id uuid NOT NULL,
    text_id uuid NOT NULL,
    entry_id uuid NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 0 AND 9007199254740991),
    text_sha256 text NOT NULL CHECK (text_sha256 ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (owner_id, logical_dialog_id, text_id),
    FOREIGN KEY (owner_id, logical_dialog_id, entry_id)
        REFERENCES agent_service.history_entries(owner_id, logical_dialog_id, entry_id) ON DELETE RESTRICT,
    FOREIGN KEY (owner_id, logical_dialog_id, text_id)
        REFERENCES agent_service.history_text_manifests(owner_id, logical_dialog_id, text_id) ON DELETE RESTRICT
);

CREATE TABLE agent_service.history_search_tool_segments (
    owner_id text NOT NULL,
    logical_dialog_id uuid NOT NULL,
    text_id uuid NOT NULL,
    entry_id uuid NOT NULL,
    segment_index integer NOT NULL CHECK (segment_index >= 0),
    safe_text text NOT NULL CHECK (octet_length(safe_text) <= 262144),
    normalized_text text GENERATED ALWAYS AS (lower(regexp_replace(safe_text, '\s+', ' ', 'g'))) STORED,
    search_vector tsvector GENERATED ALWAYS AS (setweight(to_tsvector('simple', safe_text), 'C')) STORED,
    PRIMARY KEY (owner_id, logical_dialog_id, text_id, segment_index),
    FOREIGN KEY (owner_id, logical_dialog_id, text_id)
        REFERENCES agent_service.history_search_tool_sources(owner_id, logical_dialog_id, text_id) ON DELETE RESTRICT
);

CREATE INDEX history_search_tool_segments_gin_idx
    ON agent_service.history_search_tool_segments USING gin(search_vector);
CREATE INDEX history_search_tool_segments_entry_idx
    ON agent_service.history_search_tool_segments(owner_id, logical_dialog_id, entry_id);

-- Backfill only entries whose content object is guaranteed to pass the exact
-- R14 deep-link contract. R12 opaque content remains stored but is not exposed
-- as a search hit.
INSERT INTO agent_service.history_search_documents(
    owner_id,logical_dialog_id,entry_id,node_id,node_dialog_id,role,kind,created_at,message_text)
SELECT e.owner_id,e.logical_dialog_id,e.entry_id,e.origin_node_id,e.origin_node_dialog_id,e.role,
       e.payload->>'kind',e.created_at,
       CASE WHEN e.payload->'content'->>'kind'='inline'
                 AND jsonb_typeof(e.payload->'content')='object'
                 AND jsonb_typeof(e.payload->'content'->'content')='string'
                 AND octet_length(e.payload->'content'->>'content')<=65536
                 AND ((e.role='user' AND (e.payload->'content')=jsonb_build_object(
                          'kind','inline','content',(e.payload->'content'->>'content')))
                      OR (e.role='assistant' AND (e.payload->'content')=jsonb_build_object(
                              'kind','inline','content',(e.payload->'content'->>'content'),
                              'redaction',(e.payload->'content'->>'redaction'),'truncated',(e.payload->'content'->'truncated'))
                          AND e.payload->'content'->>'redaction' IN ('none','applied')
                          AND jsonb_typeof(e.payload->'content'->'truncated')='boolean'))
            THEN e.payload->'content'->>'content' ELSE '' END
FROM agent_service.history_entries e
WHERE jsonb_typeof(e.payload->'content')='object' AND (
    (e.role='user' AND e.payload->'content'=jsonb_build_object(
        'kind','inline','content',(e.payload->'content'->>'content'))
        AND jsonb_typeof(e.payload->'content'->'content')='string'
        AND octet_length(e.payload->'content'->>'content')<=65536)
    OR
    (e.role='assistant' AND (
        (e.payload->'content'=jsonb_build_object(
            'kind','inline','content',(e.payload->'content'->>'content'),
            'redaction',(e.payload->'content'->>'redaction'),'truncated',(e.payload->'content'->'truncated'))
            AND jsonb_typeof(e.payload->'content'->'content')='string'
            AND octet_length(e.payload->'content'->>'content')<=65536
            AND e.payload->'content'->>'redaction' IN ('none','applied')
            AND jsonb_typeof(e.payload->'content'->'truncated')='boolean')
        OR
        (e.payload->'content'=jsonb_build_object(
            'kind','artifact','artifactId',(e.payload->'content'->>'artifactId'),
            'sizeBytes',(e.payload->'content'->'sizeBytes'),'sha256',(e.payload->'content'->>'sha256'),
            'redaction',(e.payload->'content'->>'redaction'),'truncated',(e.payload->'content'->'truncated'))
            AND e.payload->'content'->>'artifactId' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
            AND jsonb_typeof(e.payload->'content'->'sizeBytes')='number'
            AND e.payload->'content'->>'sizeBytes' ~ '^(0|[1-9][0-9]{0,15})$'
            AND (e.payload->'content'->>'sizeBytes')::numeric<=9007199254740991
            AND e.payload->'content'->>'sha256' ~ '^[0-9a-f]{64}$'
            AND e.payload->'content'->>'redaction' IN ('none','applied')
            AND jsonb_typeof(e.payload->'content'->'truncated')='boolean')
        OR
        (e.payload->'content'=jsonb_build_object(
            'kind','unavailable','reason',(e.payload->'content'->>'reason'),
            'redaction',(e.payload->'content'->>'redaction'),'truncated',(e.payload->'content'->'truncated'))
            AND e.payload->'content'->>'reason' IN ('not_observed','provider_redacted','output_limit','unmapped')
            AND e.payload->'content'->>'redaction' IN ('none','applied','unknown')
            AND jsonb_typeof(e.payload->'content'->'truncated')='boolean')
    ))
);

-- A completed replica stream is the durable R12 proof that chunk indexes,
-- offsets, sizes, per-chunk hashes and the aggregate manifest digest were
-- verified transactionally. Backfill only those streams and only safe entry
-- targets. Segment conversion is bounded and invalid binary output is skipped
-- instead of aborting the migration.
CREATE FUNCTION agent_service.history_search_try_utf8(value bytea,allow_left_trim boolean,allow_right_trim boolean) RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT AS $$
DECLARE
    left_trim integer;
    right_trim integer;
    value_length integer := octet_length(value);
    maximum_left_trim integer := CASE WHEN allow_left_trim THEN 3 ELSE 0 END;
    maximum_right_trim integer := CASE WHEN allow_right_trim THEN 3 ELSE 0 END;
BEGIN
    IF value_length=0 THEN
        RETURN '';
    END IF;
    FOR left_trim IN 0..maximum_left_trim LOOP
        FOR right_trim IN 0..maximum_right_trim LOOP
            IF value_length-left_trim-right_trim>0 THEN
                BEGIN
                    RETURN convert_from(substring(value FROM left_trim+1 FOR value_length-left_trim-right_trim),'UTF8');
                EXCEPTION WHEN data_exception THEN
                    NULL;
                END;
            END IF;
        END LOOP;
    END LOOP;
    RETURN NULL;
END;
$$;

CREATE TEMP TABLE r14_history_search_backfill_segments ON COMMIT DROP AS
WITH candidates AS (
    SELECT m.owner_id,m.logical_dialog_id,m.text_id,target.entry_id,m.size_bytes,m.text_sha256,
           part.start_byte,(part.start_byte/261136)::bigint AS segment_index
    FROM agent_service.history_text_manifests m
    JOIN agent_service.history_replica_streams r
      ON r.owner_id=m.owner_id AND r.stream_id=m.source_stream_id AND r.complete
    JOIN LATERAL (
        SELECT e.entry_id
        FROM agent_service.history_entries e
        JOIN agent_service.history_search_documents d ON d.owner_id=e.owner_id
          AND d.logical_dialog_id=e.logical_dialog_id AND d.entry_id=e.entry_id
        WHERE e.owner_id=m.owner_id AND e.logical_dialog_id=m.logical_dialog_id AND e.attempt_id=m.attempt_id
        ORDER BY e.created_at DESC,e.entry_id DESC LIMIT 1
    ) target ON true
    CROSS JOIN LATERAL generate_series(0,m.size_bytes-1,261136) part(start_byte)
    WHERE m.complete AND m.size_bytes>0
)
SELECT candidate.owner_id,candidate.logical_dialog_id,candidate.text_id,candidate.entry_id,
       candidate.size_bytes,candidate.text_sha256,candidate.segment_index,
       agent_service.history_search_try_utf8(string_agg(
           substring(chunk.content
             FROM (GREATEST(candidate.start_byte,chunk.offset_bytes)-chunk.offset_bytes+1)::integer
             FOR (LEAST(candidate.start_byte+262144,chunk.offset_bytes+chunk.size_bytes)
                  -GREATEST(candidate.start_byte,chunk.offset_bytes))::integer),
           ''::bytea ORDER BY chunk.chunk_index),candidate.start_byte>0,
           candidate.start_byte+262144<candidate.size_bytes) AS safe_text
FROM candidates candidate
JOIN agent_service.history_text_chunks chunk
  ON chunk.owner_id=candidate.owner_id AND chunk.logical_dialog_id=candidate.logical_dialog_id
 AND chunk.text_id=candidate.text_id
 AND chunk.offset_bytes<candidate.start_byte+262144
 AND chunk.offset_bytes+chunk.size_bytes>candidate.start_byte
GROUP BY candidate.owner_id,candidate.logical_dialog_id,candidate.text_id,candidate.entry_id,
         candidate.size_bytes,candidate.text_sha256,candidate.start_byte,candidate.segment_index;

INSERT INTO agent_service.history_search_tool_sources(
    owner_id,logical_dialog_id,text_id,entry_id,size_bytes,text_sha256)
SELECT m.owner_id,m.logical_dialog_id,m.text_id,target.entry_id,m.size_bytes,m.text_sha256
FROM agent_service.history_text_manifests m
JOIN agent_service.history_replica_streams r
  ON r.owner_id=m.owner_id AND r.stream_id=m.source_stream_id AND r.complete
JOIN LATERAL (
    SELECT e.entry_id
    FROM agent_service.history_entries e
    JOIN agent_service.history_search_documents d ON d.owner_id=e.owner_id
      AND d.logical_dialog_id=e.logical_dialog_id AND d.entry_id=e.entry_id
    WHERE e.owner_id=m.owner_id AND e.logical_dialog_id=m.logical_dialog_id AND e.attempt_id=m.attempt_id
    ORDER BY e.created_at DESC,e.entry_id DESC LIMIT 1
) target ON true
WHERE m.complete AND (
    m.size_bytes=0 OR (
        (SELECT count(*) FROM r14_history_search_backfill_segments segment
         WHERE segment.owner_id=m.owner_id AND segment.logical_dialog_id=m.logical_dialog_id AND segment.text_id=m.text_id)
          = ((m.size_bytes-1)/261136)+1
        AND NOT EXISTS (
            SELECT 1 FROM r14_history_search_backfill_segments segment
            WHERE segment.owner_id=m.owner_id AND segment.logical_dialog_id=m.logical_dialog_id
              AND segment.text_id=m.text_id AND segment.safe_text IS NULL
        )
    )
);

INSERT INTO agent_service.history_search_tool_segments(
    owner_id,logical_dialog_id,text_id,entry_id,segment_index,safe_text)
SELECT segment.owner_id,segment.logical_dialog_id,segment.text_id,segment.entry_id,
       segment.segment_index,segment.safe_text
FROM r14_history_search_backfill_segments segment
JOIN agent_service.history_search_tool_sources source
  ON source.owner_id=segment.owner_id AND source.logical_dialog_id=segment.logical_dialog_id
 AND source.text_id=segment.text_id
WHERE segment.safe_text IS NOT NULL;

DROP TABLE r14_history_search_backfill_segments;
DROP FUNCTION agent_service.history_search_try_utf8(bytea,boolean,boolean);

-- Search snapshots are deliberately server-side: cursor bytes never contain result
-- rows and cannot be used to cross owner or query boundaries.
CREATE TABLE agent_service.history_search_snapshots (
    owner_id text NOT NULL CHECK (length(owner_id) BETWEEN 1 AND 128),
    snapshot_id uuid NOT NULL,
    query_hash text NOT NULL CHECK (query_hash ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    total_count bigint NOT NULL CHECK (total_count BETWEEN 0 AND 9007199254740991),
    observed_at timestamptz NOT NULL,
    lag_millis bigint NOT NULL CHECK (lag_millis BETWEEN 0 AND 9007199254740991),
    incomplete boolean NOT NULL,
    PRIMARY KEY (owner_id, snapshot_id),
    CHECK (expires_at = created_at + interval '5 minutes')
);

CREATE INDEX history_search_snapshots_expiry_idx
    ON agent_service.history_search_snapshots(expires_at);

-- Snapshot rows are a five-minute ordering cache, not a second copy of every
-- message/tool segment.  Bound UTF-8 by bytes before persistence; the DTO-side
-- truncation remains defence in depth only.
CREATE FUNCTION agent_service.history_search_snippet(value text, maximum_bytes integer) RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT AS $$
DECLARE
    low integer := 0;
    high integer;
    middle integer;
BEGIN
    IF maximum_bytes<=0 THEN
        RETURN '';
    END IF;
    IF octet_length(value)<=maximum_bytes THEN
        RETURN value;
    END IF;
    high := LEAST(char_length(value),maximum_bytes);
    WHILE low<high LOOP
        middle := (low+high+1)/2;
        IF octet_length(left(value,middle))<=maximum_bytes THEN
            low := middle;
        ELSE
            high := middle-1;
        END IF;
    END LOOP;
    RETURN left(value,low);
END;
$$;

CREATE TABLE agent_service.history_search_snapshot_items (
    owner_id text NOT NULL,
    snapshot_id uuid NOT NULL,
    position bigint NOT NULL CHECK (position BETWEEN 0 AND 9007199254740991),
    logical_dialog_id uuid NOT NULL,
    entry_id uuid NOT NULL,
    rank real NOT NULL,
    snippet text NOT NULL CHECK (octet_length(snippet) <= 512),
    PRIMARY KEY (owner_id, snapshot_id, position),
    UNIQUE (owner_id, snapshot_id, logical_dialog_id, entry_id),
    FOREIGN KEY (owner_id, snapshot_id)
        REFERENCES agent_service.history_search_snapshots(owner_id, snapshot_id) ON DELETE CASCADE
);

CREATE INDEX history_search_snapshot_items_entry_idx
    ON agent_service.history_search_snapshot_items(owner_id, logical_dialog_id, entry_id);
