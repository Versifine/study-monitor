CREATE TABLE media_file_checks (
    ingest_key TEXT PRIMARY KEY CHECK (length(ingest_key) = 64),
    check_kind TEXT NOT NULL CHECK (check_kind IN ('confirmed', 'quarantined')),
    signature TEXT NOT NULL CHECK (length(signature) = 64),
    checked_at_utc TEXT NOT NULL
) STRICT;
