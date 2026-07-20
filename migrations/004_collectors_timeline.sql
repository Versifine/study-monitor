CREATE TABLE collector_heartbeats (
    id INTEGER PRIMARY KEY,
    collector_id TEXT NOT NULL CHECK (length(collector_id) BETWEEN 1 AND 128),
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 256),
    state TEXT NOT NULL CHECK (state IN ('active', 'idle')),
    device_start_raw TEXT NOT NULL CHECK (length(device_start_raw) BETWEEN 1 AND 128),
    device_end_raw TEXT NOT NULL CHECK (length(device_end_raw) BETWEEN 1 AND 128),
    device_start_utc TEXT NOT NULL CHECK (length(device_start_utc) BETWEEN 20 AND 35),
    device_end_utc TEXT NOT NULL CHECK (length(device_end_utc) BETWEEN 20 AND 35),
    received_at_utc TEXT NOT NULL CHECK (length(received_at_utc) BETWEEN 20 AND 35),
    corrected_start_utc TEXT NOT NULL CHECK (length(corrected_start_utc) = 30),
    corrected_end_utc TEXT NOT NULL CHECK (length(corrected_end_utc) = 30),
    clock_offset_ms INTEGER NOT NULL,
    clock_error_ms INTEGER NOT NULL CHECK (clock_error_ms >= 0),
    quality_flags_json TEXT NOT NULL CHECK (json_valid(quality_flags_json) AND json_type(quality_flags_json) = 'array'),
    content_hash TEXT NOT NULL CHECK (length(content_hash) = 64),
    schema_version INTEGER NOT NULL CHECK (schema_version > 0),
    UNIQUE (collector_id, idempotency_key)
) STRICT;

CREATE INDEX collector_heartbeats_collector_time
    ON collector_heartbeats (collector_id, corrected_start_utc, id);

CREATE INDEX collector_heartbeats_time
    ON collector_heartbeats (corrected_start_utc, id);

CREATE INDEX collector_heartbeats_collector_end
    ON collector_heartbeats (collector_id, corrected_end_utc, id);

CREATE INDEX collector_heartbeats_end
    ON collector_heartbeats (corrected_end_utc, id);

CREATE TRIGGER collector_heartbeats_reject_update
BEFORE UPDATE ON collector_heartbeats
BEGIN
    SELECT RAISE(ABORT, 'COLLECTOR_HEARTBEATS_APPEND_ONLY');
END;

CREATE TRIGGER collector_heartbeats_reject_delete
BEFORE DELETE ON collector_heartbeats
BEGIN
    SELECT RAISE(ABORT, 'COLLECTOR_HEARTBEATS_APPEND_ONLY');
END;

CREATE TABLE activitywatch_checkpoints (
    collector_id TEXT PRIMARY KEY CHECK (length(collector_id) BETWEEN 1 AND 128),
    bucket_id TEXT NOT NULL CHECK (length(bucket_id) BETWEEN 1 AND 256),
    source_time_utc TEXT NOT NULL CHECK (length(source_time_utc) = 30),
    source_event_id INTEGER NOT NULL CHECK (source_event_id >= 0),
    updated_at_utc TEXT NOT NULL CHECK (length(updated_at_utc) = 30)
) STRICT;

CREATE INDEX raw_events_corrected_start
    ON raw_events ((julianday(device_time_utc) + (clock_offset_ms / 86400000.0)), id);

CREATE INDEX raw_events_collector_corrected_start
    ON raw_events (collector_id, (julianday(device_time_utc) + (clock_offset_ms / 86400000.0)), id);

CREATE INDEX raw_events_collector_activitywatch_corrected_end
    ON raw_events (
        collector_id,
        (julianday(device_time_utc) + (clock_offset_ms / 86400000.0)
            + (CAST(json_extract(payload_json, '$.duration_seconds') AS REAL) / 86400.0)),
        id
    )
    WHERE event_type = 'activitywatch.event'
      AND json_type(payload_json, '$.bucket_id') = 'text'
      AND json_type(payload_json, '$.bucket_type') = 'text'
      AND json_type(payload_json, '$.source_event_id') = 'integer'
      AND CAST(json_extract(payload_json, '$.source_event_id') AS INTEGER) >= 0
      AND json_type(payload_json, '$.duration_seconds') IN ('integer', 'real')
      AND CAST(json_extract(payload_json, '$.duration_seconds') AS REAL) > 0
      AND CAST(json_extract(payload_json, '$.duration_seconds') AS REAL) <= 31622400
      AND json_type(payload_json, '$.data') = 'object';

CREATE INDEX raw_events_collector_activitywatch_corrected_start
    ON raw_events (
        collector_id,
        (julianday(device_time_utc) + (clock_offset_ms / 86400000.0)),
        id
    )
    WHERE event_type = 'activitywatch.event'
      AND json_type(payload_json, '$.bucket_id') = 'text'
      AND json_type(payload_json, '$.bucket_type') = 'text'
      AND json_type(payload_json, '$.source_event_id') = 'integer'
      AND CAST(json_extract(payload_json, '$.source_event_id') AS INTEGER) >= 0
      AND json_type(payload_json, '$.duration_seconds') IN ('integer', 'real')
      AND CAST(json_extract(payload_json, '$.duration_seconds') AS REAL) > 0
      AND CAST(json_extract(payload_json, '$.duration_seconds') AS REAL) <= 31622400
      AND json_type(payload_json, '$.data') = 'object';

CREATE INDEX raw_events_activitywatch_corrected_end
    ON raw_events (
        (julianday(device_time_utc) + (clock_offset_ms / 86400000.0)
            + (CAST(json_extract(payload_json, '$.duration_seconds') AS REAL) / 86400.0)),
        id
    )
    WHERE event_type = 'activitywatch.event'
      AND json_type(payload_json, '$.bucket_id') = 'text'
      AND json_type(payload_json, '$.bucket_type') = 'text'
      AND json_type(payload_json, '$.source_event_id') = 'integer'
      AND CAST(json_extract(payload_json, '$.source_event_id') AS INTEGER) >= 0
      AND json_type(payload_json, '$.duration_seconds') IN ('integer', 'real')
      AND CAST(json_extract(payload_json, '$.duration_seconds') AS REAL) > 0
      AND CAST(json_extract(payload_json, '$.duration_seconds') AS REAL) <= 31622400
      AND json_type(payload_json, '$.data') = 'object';

CREATE INDEX media_segments_corrected_start
    ON media_segments ((julianday(device_start_utc) + (clock_offset_ms / 86400000.0)), id);

CREATE INDEX media_segments_collector_corrected_start
    ON media_segments (collector_id, (julianday(device_start_utc) + (clock_offset_ms / 86400000.0)), id);

CREATE INDEX media_segments_collector_corrected_end
    ON media_segments (collector_id, (julianday(device_end_utc) + (clock_offset_ms / 86400000.0)), id);

CREATE INDEX media_segments_corrected_end
    ON media_segments ((julianday(device_end_utc) + (clock_offset_ms / 86400000.0)), id);

CREATE TABLE timeline_entries (
    id INTEGER PRIMARY KEY,
    stable_id TEXT NOT NULL UNIQUE CHECK (length(stable_id) BETWEEN 3 AND 160),
    source_type TEXT NOT NULL CHECK (source_type IN ('raw_event', 'heartbeat', 'media_segment')),
    source_id INTEGER NOT NULL CHECK (source_id > 0),
    collector_id TEXT NOT NULL CHECK (length(collector_id) BETWEEN 1 AND 128),
    event_type TEXT NOT NULL CHECK (length(event_type) BETWEEN 1 AND 128),
    device_start_raw TEXT NOT NULL CHECK (length(device_start_raw) BETWEEN 1 AND 128),
    device_end_raw TEXT CHECK (device_end_raw IS NULL OR length(device_end_raw) BETWEEN 1 AND 128),
    device_start_utc TEXT NOT NULL CHECK (length(device_start_utc) BETWEEN 20 AND 35),
    device_end_utc TEXT CHECK (device_end_utc IS NULL OR length(device_end_utc) BETWEEN 20 AND 35),
    received_at_utc TEXT NOT NULL CHECK (length(received_at_utc) BETWEEN 20 AND 35),
    corrected_start_utc TEXT NOT NULL CHECK (length(corrected_start_utc) = 30),
    corrected_end_utc TEXT CHECK (corrected_end_utc IS NULL OR length(corrected_end_utc) = 30),
    clock_offset_ms INTEGER NOT NULL,
    clock_error_ms INTEGER NOT NULL CHECK (clock_error_ms >= 0),
    clock_uncertain INTEGER NOT NULL CHECK (clock_uncertain IN (0, 1)),
    quality_flags_json TEXT NOT NULL CHECK (json_valid(quality_flags_json) AND json_type(quality_flags_json) = 'array'),
    payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
    source_schema_version INTEGER NOT NULL CHECK (source_schema_version > 0),
    UNIQUE (source_type, source_id)
) STRICT;

CREATE INDEX timeline_entries_order
    ON timeline_entries (corrected_start_utc, received_at_utc, source_type, source_id, id);

CREATE INDEX timeline_entries_collector_order
    ON timeline_entries (collector_id, corrected_start_utc, id);

CREATE TABLE coverage_intervals (
    id INTEGER PRIMARY KEY,
    collector_id TEXT NOT NULL CHECK (length(collector_id) BETWEEN 1 AND 128),
    start_utc TEXT NOT NULL CHECK (length(start_utc) = 30),
    end_utc TEXT NOT NULL CHECK (length(end_utc) = 30),
    availability TEXT NOT NULL CHECK (availability IN ('covered', 'confirmed_idle', 'pending', 'delayed', 'offline', 'unknown')),
    quality_flags_json TEXT NOT NULL CHECK (json_valid(quality_flags_json) AND json_type(quality_flags_json) = 'array'),
    reason_code TEXT NOT NULL CHECK (length(reason_code) BETWEEN 1 AND 128),
    generation INTEGER NOT NULL CHECK (generation > 0),
    built_at_utc TEXT NOT NULL CHECK (length(built_at_utc) = 30),
    CHECK (start_utc < end_utc),
    UNIQUE (collector_id, start_utc, end_utc)
) STRICT;

CREATE INDEX coverage_intervals_collector_range
    ON coverage_intervals (collector_id, start_utc, end_utc);

CREATE TRIGGER coverage_intervals_reject_insert_overlap
BEFORE INSERT ON coverage_intervals
WHEN EXISTS (
    SELECT 1 FROM coverage_intervals existing
    WHERE existing.collector_id = NEW.collector_id
      AND existing.start_utc < NEW.end_utc
      AND existing.end_utc > NEW.start_utc
)
BEGIN
    SELECT RAISE(ABORT, 'COVERAGE_INTERVAL_OVERLAP');
END;

CREATE TRIGGER coverage_intervals_reject_update_overlap
BEFORE UPDATE ON coverage_intervals
WHEN EXISTS (
    SELECT 1 FROM coverage_intervals existing
    WHERE existing.collector_id = NEW.collector_id
      AND existing.id <> OLD.id
      AND existing.start_utc < NEW.end_utc
      AND existing.end_utc > NEW.start_utc
)
BEGIN
    SELECT RAISE(ABORT, 'COVERAGE_INTERVAL_OVERLAP');
END;

CREATE TABLE coverage_projection_state (
    collector_id TEXT PRIMARY KEY CHECK (length(collector_id) BETWEEN 1 AND 128),
    range_start_utc TEXT NOT NULL CHECK (length(range_start_utc) = 30),
    range_end_utc TEXT NOT NULL CHECK (length(range_end_utc) = 30),
    generation INTEGER NOT NULL CHECK (generation > 0),
    status TEXT NOT NULL CHECK (status IN ('fresh', 'stale')),
    error_code TEXT,
    fact_watermark INTEGER NOT NULL CHECK (fact_watermark >= 0),
    built_at_utc TEXT NOT NULL CHECK (length(built_at_utc) = 30),
    CHECK (range_start_utc < range_end_utc)
) STRICT;
