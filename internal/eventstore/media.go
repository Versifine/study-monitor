package eventstore

import (
	"context"
	"database/sql"
	"errors"
)

const (
	MediaWriteAccepted  = "accepted"
	MediaWriteDuplicate = "duplicate"
	MediaWriteConflict  = "conflict"
)

type MediaMetadata struct {
	CollectorID          string
	SourceIdempotencyKey string
	ManagedRelativePath  string
	DeviceStartRaw       string
	DeviceEndRaw         string
	DeviceStartUTC       string
	DeviceEndUTC         string
	ReceivedAtUTC        string
	ClockOffsetMS        int64
	ClockErrorMS         int64
	SizeBytes            int64
	DurationMS           int64
	CodecName            string
	FormatName           string
	MediaType            string
	SHA256               string
	MetadataHash         string
	SidecarSchemaVersion int
}

type MediaIngestEvent struct {
	EventKey              string
	IngestKey             string
	CollectorID           string
	SourceIdempotencyKey  string
	SourceName            string
	SourceFingerprint     string
	Status                string
	TemporaryRelativePath string
	MediaSegmentID        int64
	ErrorCode             string
	OccurredAtUTC         string
}

type MediaClaim struct {
	Status              string
	SegmentID           int64
	ManagedRelativePath string
	SHA256              string
	MetadataHash        string
}

type MediaIngestSummary struct {
	Backlog       int64  `json:"backlog"`
	Discovered    int64  `json:"discovered"`
	Pending       int64  `json:"pending"`
	Validated     int64  `json:"validated"`
	Quarantined   int64  `json:"quarantined"`
	Accepted      int64  `json:"accepted"`
	Failed        int64  `json:"failed"`
	TotalSegments int64  `json:"total_segments"`
	LastErrorCode string `json:"last_error_code,omitempty"`
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (store *Store) ResolveMediaClaim(ctx context.Context, collectorID, sourceKey, sha256Value, metadataHash string) (MediaClaim, error) {
	return resolveMediaClaim(ctx, store.db, collectorID, sourceKey, sha256Value, metadataHash)
}

func resolveMediaClaim(ctx context.Context, queryer rowQuerier, collectorID, sourceKey, sha256Value, metadataHash string) (MediaClaim, error) {
	claim, err := queryMediaClaim(ctx, queryer, `
SELECT ms.id, ms.managed_relative_path, ms.sha256, ms.metadata_hash
FROM media_segments AS ms
WHERE ms.collector_id = ? AND ms.source_idempotency_key = ?
UNION ALL
SELECT ms.id, ms.managed_relative_path, ms.sha256, ms.metadata_hash
FROM media_ingest_events AS mie
JOIN media_segments AS ms ON ms.id = mie.media_segment_id
WHERE mie.collector_id = ? AND mie.source_idempotency_key = ? AND mie.media_segment_id IS NOT NULL
ORDER BY 1
LIMIT 1`, collectorID, sourceKey, collectorID, sourceKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return MediaClaim{}, classifySQLiteError(CodeQueryFailed, "resolve media source claim", err)
	}
	if err == nil {
		if claim.SHA256 == sha256Value && claim.MetadataHash == metadataHash {
			claim.Status = MediaWriteDuplicate
		} else {
			claim.Status = MediaWriteConflict
		}
		return claim, nil
	}

	claim, err = queryMediaClaim(ctx, queryer, `
SELECT id, managed_relative_path, sha256, metadata_hash
FROM media_segments
WHERE sha256 = ?
ORDER BY id
LIMIT 1`, sha256Value)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaClaim{}, nil
	}
	if err != nil {
		return MediaClaim{}, classifySQLiteError(CodeQueryFailed, "resolve media content claim", err)
	}
	if claim.MetadataHash == metadataHash {
		claim.Status = MediaWriteDuplicate
	} else {
		claim.Status = MediaWriteConflict
	}
	return claim, nil
}

func queryMediaClaim(ctx context.Context, queryer rowQuerier, query string, arguments ...any) (MediaClaim, error) {
	var claim MediaClaim
	err := queryer.QueryRowContext(ctx, query, arguments...).Scan(
		&claim.SegmentID,
		&claim.ManagedRelativePath,
		&claim.SHA256,
		&claim.MetadataHash,
	)
	return claim, err
}

func (store *Store) AppendMediaIngestEvent(ctx context.Context, event MediaIngestEvent) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return classifySQLiteError(CodeWriteFailed, "begin media ingest event", err)
	}
	defer tx.Rollback()
	eventID, err := insertMediaIngestEvent(ctx, tx, event)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return classifySQLiteError(CodeWriteFailed, "commit media ingest event", err)
	}
	return store.applyMediaIngestProjection(ctx, eventID)
}

func (store *Store) AcceptMedia(ctx context.Context, metadata MediaMetadata, event MediaIngestEvent, stateEventKey string) (MediaClaim, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return MediaClaim{}, classifySQLiteError(CodeWriteFailed, "begin media acceptance", err)
	}
	defer tx.Rollback()

	claim, err := resolveMediaClaim(ctx, tx, metadata.CollectorID, metadata.SourceIdempotencyKey, metadata.SHA256, metadata.MetadataHash)
	if err != nil {
		return MediaClaim{}, err
	}
	if claim.Status == MediaWriteConflict {
		return claim, nil
	}

	stateEventID := int64(0)
	if claim.Status != MediaWriteDuplicate {
		result, err := tx.ExecContext(ctx, `
INSERT INTO media_segments (
    collector_id, source_idempotency_key, managed_relative_path,
    device_start_raw, device_end_raw, device_start_utc, device_end_utc, received_at_utc,
    clock_offset_ms, clock_error_ms, size_bytes, duration_ms, codec_name, format_name,
    media_type, sha256, metadata_hash, sidecar_schema_version, created_at_utc
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			metadata.CollectorID,
			metadata.SourceIdempotencyKey,
			metadata.ManagedRelativePath,
			metadata.DeviceStartRaw,
			metadata.DeviceEndRaw,
			metadata.DeviceStartUTC,
			metadata.DeviceEndUTC,
			metadata.ReceivedAtUTC,
			metadata.ClockOffsetMS,
			metadata.ClockErrorMS,
			metadata.SizeBytes,
			metadata.DurationMS,
			metadata.CodecName,
			metadata.FormatName,
			metadata.MediaType,
			metadata.SHA256,
			metadata.MetadataHash,
			metadata.SidecarSchemaVersion,
			metadata.ReceivedAtUTC,
		)
		if err != nil {
			return MediaClaim{}, classifySQLiteError(CodeWriteFailed, "insert accepted media", err)
		}
		segmentID, err := result.LastInsertId()
		if err != nil {
			return MediaClaim{}, wrap(CodeWriteFailed, "read accepted media id", err)
		}
		stateResult, err := tx.ExecContext(ctx, `
INSERT INTO media_segment_state_events (
    event_key, media_segment_id, status, reason_code, producer, occurred_at_utc
) VALUES (?, ?, 'accepted', NULL, 'recorder-core', ?)`, stateEventKey, segmentID, metadata.ReceivedAtUTC)
		if err != nil {
			return MediaClaim{}, classifySQLiteError(CodeWriteFailed, "append accepted media state", err)
		}
		stateEventID, err = stateResult.LastInsertId()
		if err != nil {
			return MediaClaim{}, wrap(CodeWriteFailed, "read accepted media state id", err)
		}
		claim = MediaClaim{
			Status: MediaWriteAccepted, SegmentID: segmentID,
			ManagedRelativePath: metadata.ManagedRelativePath,
			SHA256:              metadata.SHA256, MetadataHash: metadata.MetadataHash,
		}
	} else {
		if err := tx.QueryRowContext(ctx, `
SELECT MAX(id) FROM media_segment_state_events WHERE media_segment_id = ?`, claim.SegmentID).Scan(&stateEventID); err != nil {
			return MediaClaim{}, classifySQLiteError(CodeQueryFailed, "read latest media state event", err)
		}
	}

	event.Status = "accepted"
	event.MediaSegmentID = claim.SegmentID
	ingestEventID, err := insertMediaIngestEvent(ctx, tx, event)
	if err != nil {
		return MediaClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return MediaClaim{}, classifySQLiteError(CodeWriteFailed, "commit media acceptance", err)
	}
	if err := store.applyMediaIngestProjection(ctx, ingestEventID); err != nil {
		return claim, err
	}
	if err := store.applyMediaSegmentProjection(ctx, stateEventID); err != nil {
		return claim, err
	}
	return claim, nil
}

func insertMediaIngestEvent(ctx context.Context, tx *sql.Tx, event MediaIngestEvent) (int64, error) {
	_, err := tx.ExecContext(ctx, `
INSERT INTO media_ingest_events (
    event_key, ingest_key, collector_id, source_idempotency_key, source_name,
    source_fingerprint, status, temporary_relative_path, media_segment_id,
    error_code, occurred_at_utc
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(event_key) DO NOTHING`,
		event.EventKey,
		event.IngestKey,
		nullableString(event.CollectorID),
		nullableString(event.SourceIdempotencyKey),
		event.SourceName,
		event.SourceFingerprint,
		event.Status,
		nullableString(event.TemporaryRelativePath),
		nullableInt64(event.MediaSegmentID),
		nullableString(event.ErrorCode),
		event.OccurredAtUTC,
	)
	if err != nil {
		return 0, classifySQLiteError(CodeWriteFailed, "append media ingest event", err)
	}
	var eventID int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM media_ingest_events WHERE event_key = ?", event.EventKey).Scan(&eventID); err != nil {
		return 0, classifySQLiteError(CodeQueryFailed, "read media ingest event", err)
	}
	return eventID, nil
}

func (store *Store) applyMediaIngestProjection(ctx context.Context, eventID int64) error {
	_, err := store.db.ExecContext(ctx, `
INSERT INTO media_ingest_status (
    ingest_key, last_event_id, status, error_code, media_segment_id, updated_at_utc
)
SELECT ingest_key, id, status, error_code, media_segment_id, occurred_at_utc
FROM media_ingest_events WHERE id = ?
ON CONFLICT(ingest_key) DO UPDATE SET
    last_event_id = excluded.last_event_id,
    status = excluded.status,
    error_code = excluded.error_code,
    media_segment_id = excluded.media_segment_id,
    updated_at_utc = excluded.updated_at_utc
WHERE excluded.last_event_id > media_ingest_status.last_event_id`, eventID)
	if err != nil {
		return classifySQLiteError(CodeWriteFailed, "update media ingest projection", err)
	}
	return nil
}

func (store *Store) applyMediaSegmentProjection(ctx context.Context, stateEventID int64) error {
	_, err := store.db.ExecContext(ctx, `
INSERT INTO media_segment_status (
    media_segment_id, last_state_event_id, status, reason_code, updated_at_utc
)
SELECT media_segment_id, id, status, reason_code, occurred_at_utc
FROM media_segment_state_events WHERE id = ?
ON CONFLICT(media_segment_id) DO UPDATE SET
    last_state_event_id = excluded.last_state_event_id,
    status = excluded.status,
    reason_code = excluded.reason_code,
    updated_at_utc = excluded.updated_at_utc
WHERE excluded.last_state_event_id > media_segment_status.last_state_event_id`, stateEventID)
	if err != nil {
		return classifySQLiteError(CodeWriteFailed, "update media segment projection", err)
	}
	return nil
}

func (store *Store) RebuildMediaProjections(ctx context.Context) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return classifySQLiteError(CodeWriteFailed, "begin media projection rebuild", err)
	}
	defer tx.Rollback()
	statements := []string{
		"DELETE FROM media_ingest_status",
		`INSERT INTO media_ingest_status (ingest_key, last_event_id, status, error_code, media_segment_id, updated_at_utc)
SELECT event.ingest_key, event.id, event.status, event.error_code, event.media_segment_id, event.occurred_at_utc
FROM media_ingest_events AS event
JOIN (SELECT ingest_key, MAX(id) AS id FROM media_ingest_events GROUP BY ingest_key) AS latest
  ON latest.id = event.id`,
		"DELETE FROM media_segment_status",
		`INSERT INTO media_segment_status (media_segment_id, last_state_event_id, status, reason_code, updated_at_utc)
SELECT event.media_segment_id, event.id, event.status, event.reason_code, event.occurred_at_utc
FROM media_segment_state_events AS event
JOIN (SELECT media_segment_id, MAX(id) AS id FROM media_segment_state_events GROUP BY media_segment_id) AS latest
  ON latest.id = event.id`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return classifySQLiteError(CodeWriteFailed, "rebuild media projections", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return classifySQLiteError(CodeWriteFailed, "commit media projection rebuild", err)
	}
	return nil
}

func (store *Store) MediaIngestSummary(ctx context.Context) (MediaIngestSummary, error) {
	var summary MediaIngestSummary
	rows, err := store.db.QueryContext(ctx, "SELECT status, COUNT(*) FROM media_ingest_status GROUP BY status")
	if err != nil {
		return summary, classifySQLiteError(CodeQueryFailed, "query media ingest summary", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return summary, wrap(CodeQueryFailed, "scan media ingest summary", err)
		}
		switch status {
		case "discovered":
			summary.Discovered = count
		case "pending":
			summary.Pending = count
		case "validated":
			summary.Validated = count
		case "quarantined":
			summary.Quarantined = count
		case "accepted":
			summary.Accepted = count
		case "failed":
			summary.Failed = count
		}
	}
	if err := rows.Err(); err != nil {
		return summary, classifySQLiteError(CodeQueryFailed, "iterate media ingest summary", err)
	}
	summary.Backlog = summary.Discovered + summary.Pending + summary.Validated
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM media_segments").Scan(&summary.TotalSegments); err != nil {
		return summary, classifySQLiteError(CodeQueryFailed, "count media segments", err)
	}
	if err := store.db.QueryRowContext(ctx, `
SELECT error_code FROM media_ingest_status
WHERE error_code IS NOT NULL
ORDER BY last_event_id DESC
LIMIT 1`).Scan(&summary.LastErrorCode); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return summary, classifySQLiteError(CodeQueryFailed, "read latest media ingest error", err)
	}
	return summary, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
