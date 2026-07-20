package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const CodeCheckpointConflict = "ACTIVITYWATCH_CHECKPOINT_CONFLICT"

type ActivityWatchCheckpoint struct {
	CollectorID   string `json:"collector_id"`
	BucketID      string `json:"bucket_id"`
	SourceTimeUTC string `json:"source_time_utc"`
	SourceEventID int64  `json:"source_event_id"`
	UpdatedAtUTC  string `json:"updated_at_utc"`
}

func (store *Store) LoadActivityWatchCheckpoint(ctx context.Context, collectorID, bucketID string) (ActivityWatchCheckpoint, bool, error) {
	var checkpoint ActivityWatchCheckpoint
	err := store.db.QueryRowContext(ctx, `
SELECT collector_id, bucket_id, source_time_utc, source_event_id, updated_at_utc
FROM activitywatch_checkpoints
WHERE collector_id = ?`, collectorID).Scan(
		&checkpoint.CollectorID, &checkpoint.BucketID, &checkpoint.SourceTimeUTC,
		&checkpoint.SourceEventID, &checkpoint.UpdatedAtUTC)
	if errors.Is(err, sql.ErrNoRows) {
		return ActivityWatchCheckpoint{}, false, nil
	}
	if err != nil {
		return ActivityWatchCheckpoint{}, false, classifySQLiteError(CodeQueryFailed, "read ActivityWatch checkpoint", err)
	}
	if checkpoint.BucketID != bucketID {
		return ActivityWatchCheckpoint{}, false, &Error{Code: CodeCheckpointConflict, Err: errors.New("ActivityWatch collector checkpoint belongs to a different bucket")}
	}
	return checkpoint, true, nil
}

func (store *Store) SaveActivityWatchCheckpoint(ctx context.Context, checkpoint ActivityWatchCheckpoint) error {
	sourceTime, err := time.Parse("2006-01-02T15:04:05.000000000Z", checkpoint.SourceTimeUTC)
	if err != nil || checkpoint.SourceEventID < 0 || !validIdentifier(checkpoint.CollectorID, 128) || !validIdentifier(checkpoint.BucketID, 256) {
		return &Error{Code: CodeCheckpointConflict, Err: errors.New("ActivityWatch checkpoint is invalid")}
	}
	checkpoint.SourceTimeUTC = fixedUTC(sourceTime)
	checkpoint.UpdatedAtUTC = fixedUTC(store.now())
	result, err := store.db.ExecContext(ctx, `
INSERT INTO activitywatch_checkpoints (
    collector_id, bucket_id, source_time_utc, source_event_id, updated_at_utc
) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(collector_id) DO UPDATE SET
    source_time_utc = excluded.source_time_utc,
    source_event_id = excluded.source_event_id,
    updated_at_utc = excluded.updated_at_utc
WHERE activitywatch_checkpoints.bucket_id = excluded.bucket_id
  AND (activitywatch_checkpoints.source_time_utc < excluded.source_time_utc
       OR (activitywatch_checkpoints.source_time_utc = excluded.source_time_utc
           AND activitywatch_checkpoints.source_event_id < excluded.source_event_id))`,
		checkpoint.CollectorID, checkpoint.BucketID, checkpoint.SourceTimeUTC,
		checkpoint.SourceEventID, checkpoint.UpdatedAtUTC)
	if err != nil {
		return classifySQLiteError(CodeWriteFailed, "save ActivityWatch checkpoint", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return wrap(CodeWriteFailed, "read ActivityWatch checkpoint result", err)
	}
	if rows == 0 {
		existing, exists, err := store.LoadActivityWatchCheckpoint(ctx, checkpoint.CollectorID, checkpoint.BucketID)
		if err != nil {
			return err
		}
		if !exists {
			return &Error{Code: CodeCheckpointConflict, Err: errors.New("ActivityWatch checkpoint was not persisted")}
		}
		if existing.SourceTimeUTC > checkpoint.SourceTimeUTC || (existing.SourceTimeUTC == checkpoint.SourceTimeUTC && existing.SourceEventID >= checkpoint.SourceEventID) {
			return nil
		}
		return &Error{Code: CodeCheckpointConflict, Err: errors.New("ActivityWatch checkpoint update conflicted with stored state")}
	}
	return nil
}
