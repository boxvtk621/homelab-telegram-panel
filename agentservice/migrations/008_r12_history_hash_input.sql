ALTER TABLE agent_service.history_replica_records
    ADD COLUMN record_hash_input bytea;

ALTER TABLE agent_service.history_replica_records
    ADD CONSTRAINT history_replica_records_hash_input_size
    CHECK (record_hash_input IS NULL OR octet_length(record_hash_input) BETWEEN 1 AND 8388608);

COMMENT ON COLUMN agent_service.history_replica_records.record_hash_input IS
    'Exact accepted record-core JSON bytes bound by record_hash; NULL means exact input was not captured and may be repaired only by exact replay.';
