CREATE TABLE fault_events (
    id INTEGER PRIMARY KEY,
    event_key TEXT NOT NULL UNIQUE CHECK (length(event_key) = 64),
    module TEXT NOT NULL CHECK (length(module) BETWEEN 1 AND 128),
    severity TEXT NOT NULL CHECK (severity IN ('P0', 'P1', 'P2', 'P3')),
    status TEXT NOT NULL CHECK (status IN ('active', 'recovered', 'degraded', 'disabled')),
    error_code TEXT NOT NULL CHECK (length(error_code) BETWEEN 1 AND 128),
    detail TEXT NOT NULL CHECK (length(detail) BETWEEN 1 AND 1024),
    occurred_at_utc TEXT NOT NULL CHECK (length(occurred_at_utc) BETWEEN 20 AND 35)
) STRICT;

CREATE INDEX fault_events_module_id ON fault_events (module, id);

CREATE TABLE module_state_events (
    id INTEGER PRIMARY KEY,
    event_key TEXT NOT NULL UNIQUE CHECK (length(event_key) = 64),
    module TEXT NOT NULL CHECK (length(module) BETWEEN 1 AND 128),
    status TEXT NOT NULL CHECK (status IN ('healthy', 'degraded', 'disabled', 'unavailable')),
    reason_code TEXT NOT NULL CHECK (length(reason_code) BETWEEN 1 AND 128),
    occurred_at_utc TEXT NOT NULL CHECK (length(occurred_at_utc) BETWEEN 20 AND 35)
) STRICT;

CREATE INDEX module_state_events_module_id ON module_state_events (module, id);

CREATE TABLE mode_transition_events (
    id INTEGER PRIMARY KEY,
    event_key TEXT NOT NULL UNIQUE CHECK (length(event_key) = 64),
    old_mode TEXT NOT NULL CHECK (old_mode IN ('unknown', 'record-only', 'minimum')),
    new_mode TEXT NOT NULL CHECK (new_mode IN ('record-only', 'minimum')),
    operator TEXT NOT NULL CHECK (length(operator) BETWEEN 1 AND 128),
    trigger_name TEXT NOT NULL CHECK (length(trigger_name) BETWEEN 1 AND 128),
    reason_code TEXT NOT NULL CHECK (length(reason_code) BETWEEN 1 AND 128),
    occurred_at_utc TEXT NOT NULL CHECK (length(occurred_at_utc) BETWEEN 20 AND 35)
) STRICT;

CREATE INDEX mode_transition_events_id ON mode_transition_events (id);

CREATE TABLE retention_events (
    id INTEGER PRIMARY KEY,
    event_key TEXT NOT NULL UNIQUE CHECK (length(event_key) = 64),
    media_segment_id INTEGER NOT NULL REFERENCES media_segments(id),
    status TEXT NOT NULL CHECK (status IN ('planned', 'deleted', 'failed')),
    reason_code TEXT NOT NULL CHECK (length(reason_code) BETWEEN 1 AND 128),
    managed_relative_path TEXT NOT NULL CHECK (length(managed_relative_path) BETWEEN 1 AND 512),
    occurred_at_utc TEXT NOT NULL CHECK (length(occurred_at_utc) BETWEEN 20 AND 35)
) STRICT;

CREATE INDEX retention_events_segment_id ON retention_events (media_segment_id, id);

CREATE TRIGGER fault_events_reject_update BEFORE UPDATE ON fault_events
BEGIN SELECT RAISE(ABORT, 'FAULT_EVENTS_APPEND_ONLY'); END;
CREATE TRIGGER fault_events_reject_delete BEFORE DELETE ON fault_events
BEGIN SELECT RAISE(ABORT, 'FAULT_EVENTS_APPEND_ONLY'); END;
CREATE TRIGGER module_state_events_reject_update BEFORE UPDATE ON module_state_events
BEGIN SELECT RAISE(ABORT, 'MODULE_STATE_EVENTS_APPEND_ONLY'); END;
CREATE TRIGGER module_state_events_reject_delete BEFORE DELETE ON module_state_events
BEGIN SELECT RAISE(ABORT, 'MODULE_STATE_EVENTS_APPEND_ONLY'); END;
CREATE TRIGGER mode_transition_events_reject_update BEFORE UPDATE ON mode_transition_events
BEGIN SELECT RAISE(ABORT, 'MODE_TRANSITION_EVENTS_APPEND_ONLY'); END;
CREATE TRIGGER mode_transition_events_reject_delete BEFORE DELETE ON mode_transition_events
BEGIN SELECT RAISE(ABORT, 'MODE_TRANSITION_EVENTS_APPEND_ONLY'); END;
CREATE TRIGGER retention_events_reject_update BEFORE UPDATE ON retention_events
BEGIN SELECT RAISE(ABORT, 'RETENTION_EVENTS_APPEND_ONLY'); END;
CREATE TRIGGER retention_events_reject_delete BEFORE DELETE ON retention_events
BEGIN SELECT RAISE(ABORT, 'RETENTION_EVENTS_APPEND_ONLY'); END;
