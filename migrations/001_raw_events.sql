CREATE TABLE raw_events (
    id INTEGER PRIMARY KEY,
    collector_id TEXT NOT NULL CHECK (length(collector_id) BETWEEN 1 AND 128),
    event_type TEXT NOT NULL CHECK (length(event_type) BETWEEN 1 AND 128),
    device_timestamp_raw TEXT NOT NULL CHECK (length(device_timestamp_raw) BETWEEN 1 AND 128),
    device_time_utc TEXT NOT NULL CHECK (length(device_time_utc) BETWEEN 20 AND 35),
    received_at_utc TEXT NOT NULL CHECK (length(received_at_utc) BETWEEN 20 AND 35),
    clock_offset_ms INTEGER NOT NULL,
    clock_error_ms INTEGER NOT NULL CHECK (clock_error_ms >= 0),
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 256),
    payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
    payload_hash TEXT NOT NULL CHECK (length(payload_hash) = 64),
    content_hash TEXT NOT NULL CHECK (length(content_hash) = 64),
    schema_version INTEGER NOT NULL CHECK (schema_version > 0)
) STRICT;

CREATE UNIQUE INDEX raw_events_collector_idempotency
    ON raw_events (collector_id, idempotency_key);

CREATE INDEX raw_events_received_id
    ON raw_events (received_at_utc, id);

CREATE TRIGGER raw_events_reject_update
BEFORE UPDATE ON raw_events
BEGIN
    SELECT RAISE(ABORT, 'RAW_EVENTS_APPEND_ONLY');
END;

CREATE TRIGGER raw_events_reject_delete
BEFORE DELETE ON raw_events
BEGIN
    SELECT RAISE(ABORT, 'RAW_EVENTS_APPEND_ONLY');
END;
