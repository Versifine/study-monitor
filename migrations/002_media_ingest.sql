CREATE TABLE media_segments (
    id INTEGER PRIMARY KEY,
    collector_id TEXT NOT NULL CHECK (length(collector_id) BETWEEN 1 AND 128),
    source_idempotency_key TEXT NOT NULL CHECK (length(source_idempotency_key) BETWEEN 1 AND 256),
    managed_relative_path TEXT NOT NULL CHECK (length(managed_relative_path) BETWEEN 1 AND 512),
    device_start_raw TEXT NOT NULL CHECK (length(device_start_raw) BETWEEN 1 AND 128),
    device_end_raw TEXT NOT NULL CHECK (length(device_end_raw) BETWEEN 1 AND 128),
    device_start_utc TEXT NOT NULL CHECK (length(device_start_utc) BETWEEN 20 AND 35),
    device_end_utc TEXT NOT NULL CHECK (length(device_end_utc) BETWEEN 20 AND 35),
    received_at_utc TEXT NOT NULL CHECK (length(received_at_utc) BETWEEN 20 AND 35),
    clock_offset_ms INTEGER NOT NULL,
    clock_error_ms INTEGER NOT NULL CHECK (clock_error_ms >= 0),
    size_bytes INTEGER NOT NULL CHECK (size_bytes > 0),
    duration_ms INTEGER NOT NULL CHECK (duration_ms > 0),
    codec_name TEXT NOT NULL CHECK (length(codec_name) BETWEEN 1 AND 128),
    format_name TEXT NOT NULL CHECK (length(format_name) BETWEEN 1 AND 128),
    media_type TEXT NOT NULL CHECK (media_type = 'video'),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64),
    metadata_hash TEXT NOT NULL CHECK (length(metadata_hash) = 64),
    sidecar_schema_version INTEGER NOT NULL CHECK (sidecar_schema_version > 0),
    created_at_utc TEXT NOT NULL,
    UNIQUE (collector_id, source_idempotency_key)
) STRICT;

CREATE INDEX media_segments_sha256 ON media_segments (sha256);

CREATE TABLE media_ingest_events (
    id INTEGER PRIMARY KEY,
    event_key TEXT NOT NULL UNIQUE CHECK (length(event_key) = 64),
    ingest_key TEXT NOT NULL CHECK (length(ingest_key) = 64),
    collector_id TEXT CHECK (collector_id IS NULL OR length(collector_id) BETWEEN 1 AND 128),
    source_idempotency_key TEXT CHECK (source_idempotency_key IS NULL OR length(source_idempotency_key) BETWEEN 1 AND 256),
    source_name TEXT NOT NULL CHECK (length(source_name) BETWEEN 1 AND 512),
    source_fingerprint TEXT NOT NULL CHECK (length(source_fingerprint) = 64),
    status TEXT NOT NULL CHECK (status IN ('discovered', 'pending', 'validated', 'quarantined', 'accepted', 'failed')),
    temporary_relative_path TEXT CHECK (temporary_relative_path IS NULL OR length(temporary_relative_path) BETWEEN 1 AND 512),
    media_segment_id INTEGER REFERENCES media_segments(id),
    error_code TEXT CHECK (error_code IS NULL OR length(error_code) BETWEEN 1 AND 128),
    occurred_at_utc TEXT NOT NULL
) STRICT;

CREATE INDEX media_ingest_events_ingest_id ON media_ingest_events (ingest_key, id);
CREATE INDEX media_ingest_events_status_id ON media_ingest_events (status, id);

CREATE TABLE media_segment_state_events (
    id INTEGER PRIMARY KEY,
    event_key TEXT NOT NULL UNIQUE CHECK (length(event_key) = 64),
    media_segment_id INTEGER NOT NULL REFERENCES media_segments(id),
    status TEXT NOT NULL CHECK (status IN ('accepted', 'missing', 'restored', 'retention_deleted')),
    reason_code TEXT CHECK (reason_code IS NULL OR length(reason_code) BETWEEN 1 AND 128),
    producer TEXT NOT NULL CHECK (length(producer) BETWEEN 1 AND 128),
    occurred_at_utc TEXT NOT NULL
) STRICT;

CREATE INDEX media_segment_state_events_segment_id ON media_segment_state_events (media_segment_id, id);

CREATE TABLE media_ingest_status (
    ingest_key TEXT PRIMARY KEY CHECK (length(ingest_key) = 64),
    last_event_id INTEGER NOT NULL REFERENCES media_ingest_events(id),
    status TEXT NOT NULL,
    error_code TEXT,
    media_segment_id INTEGER REFERENCES media_segments(id),
    updated_at_utc TEXT NOT NULL
) STRICT;

CREATE TABLE media_segment_status (
    media_segment_id INTEGER PRIMARY KEY REFERENCES media_segments(id),
    last_state_event_id INTEGER NOT NULL REFERENCES media_segment_state_events(id),
    status TEXT NOT NULL,
    reason_code TEXT,
    updated_at_utc TEXT NOT NULL
) STRICT;

CREATE TRIGGER media_segments_reject_update
BEFORE UPDATE ON media_segments
BEGIN
    SELECT RAISE(ABORT, 'MEDIA_SEGMENTS_APPEND_ONLY');
END;

CREATE TRIGGER media_segments_reject_delete
BEFORE DELETE ON media_segments
BEGIN
    SELECT RAISE(ABORT, 'MEDIA_SEGMENTS_APPEND_ONLY');
END;

CREATE TRIGGER media_ingest_events_reject_update
BEFORE UPDATE ON media_ingest_events
BEGIN
    SELECT RAISE(ABORT, 'MEDIA_INGEST_EVENTS_APPEND_ONLY');
END;

CREATE TRIGGER media_ingest_events_reject_delete
BEFORE DELETE ON media_ingest_events
BEGIN
    SELECT RAISE(ABORT, 'MEDIA_INGEST_EVENTS_APPEND_ONLY');
END;

CREATE TRIGGER media_segment_state_events_reject_update
BEFORE UPDATE ON media_segment_state_events
BEGIN
    SELECT RAISE(ABORT, 'MEDIA_SEGMENT_STATE_EVENTS_APPEND_ONLY');
END;

CREATE TRIGGER media_segment_state_events_reject_delete
BEFORE DELETE ON media_segment_state_events
BEGIN
    SELECT RAISE(ABORT, 'MEDIA_SEGMENT_STATE_EVENTS_APPEND_ONLY');
END;
