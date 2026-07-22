package eventstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type FaultEvent struct {
	Module, Severity, Status, ErrorCode, Detail, OccurredAtUTC string
}

type ModuleStateEvent struct {
	Module, Status, ReasonCode, OccurredAtUTC string
}

type ModeTransitionEvent struct {
	OldMode, NewMode, Operator, Trigger, ReasonCode, OccurredAtUTC string
}

type RetentionEvent struct {
	MediaSegmentID                                         int64
	Status, ReasonCode, ManagedRelativePath, OccurredAtUTC string
}

type RetentionCandidate struct {
	MediaSegmentID                            int64
	ManagedRelativePath, SHA256, CreatedAtUTC string
	SizeBytes                                 int64
}

type SchemaInfo struct {
	Core  int `json:"core"`
	Media int `json:"media"`
	M3    int `json:"m3"`
	M4    int `json:"m4"`
}

type MediaManifestEntry struct {
	MediaSegmentID      int64  `json:"media_segment_id"`
	ManagedRelativePath string `json:"managed_relative_path"`
	SHA256              string `json:"sha256"`
	SizeBytes           int64  `json:"size_bytes"`
	Status              string `json:"status"`
}

func eventKey(parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:])
}

func (store *Store) AppendFaultEvent(ctx context.Context, event FaultEvent) error {
	_, err := store.db.ExecContext(ctx, `
INSERT INTO fault_events(event_key, module, severity, status, error_code, detail, occurred_at_utc)
VALUES(?, ?, ?, ?, ?, ?, ?) ON CONFLICT(event_key) DO NOTHING`,
		eventKey(event.Module, event.Severity, event.Status, event.ErrorCode, event.Detail, event.OccurredAtUTC),
		event.Module, event.Severity, event.Status, event.ErrorCode, event.Detail, event.OccurredAtUTC)
	if err != nil {
		return classifySQLiteError(CodeWriteFailed, "append fault event", err)
	}
	return nil
}

func (store *Store) AppendModuleStateEvent(ctx context.Context, event ModuleStateEvent) error {
	_, err := store.db.ExecContext(ctx, `
INSERT INTO module_state_events(event_key, module, status, reason_code, occurred_at_utc)
VALUES(?, ?, ?, ?, ?) ON CONFLICT(event_key) DO NOTHING`,
		eventKey(event.Module, event.Status, event.ReasonCode, event.OccurredAtUTC),
		event.Module, event.Status, event.ReasonCode, event.OccurredAtUTC)
	if err != nil {
		return classifySQLiteError(CodeWriteFailed, "append module state event", err)
	}
	return nil
}

func (store *Store) AppendModeTransition(ctx context.Context, newMode, operator, trigger, reason, occurredAtUTC string) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return classifySQLiteError(CodeWriteFailed, "begin mode transition", err)
	}
	defer tx.Rollback()
	oldMode := "unknown"
	if err := tx.QueryRowContext(ctx, "SELECT new_mode FROM mode_transition_events ORDER BY id DESC LIMIT 1").Scan(&oldMode); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return classifySQLiteError(CodeQueryFailed, "read current runtime mode", err)
	}
	if oldMode == newMode {
		return nil
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO mode_transition_events(event_key, old_mode, new_mode, operator, trigger_name, reason_code, occurred_at_utc)
VALUES(?, ?, ?, ?, ?, ?, ?) ON CONFLICT(event_key) DO NOTHING`,
		eventKey(oldMode, newMode, operator, trigger, reason, occurredAtUTC), oldMode, newMode, operator, trigger, reason, occurredAtUTC)
	if err != nil {
		return classifySQLiteError(CodeWriteFailed, "append mode transition", err)
	}
	if err := tx.Commit(); err != nil {
		return classifySQLiteError(CodeWriteFailed, "commit mode transition", err)
	}
	return nil
}

func (store *Store) RetentionCandidates(ctx context.Context, cutoffUTC string, limit int) ([]RetentionCandidate, error) {
	rows, err := store.db.QueryContext(ctx, `
SELECT ms.id, ms.managed_relative_path, ms.sha256, ms.size_bytes, ms.created_at_utc
FROM media_segments ms JOIN media_segment_status status ON status.media_segment_id = ms.id
WHERE status.status IN ('accepted', 'restored') AND ms.created_at_utc < ?
ORDER BY ms.created_at_utc, ms.id LIMIT ?`, cutoffUTC, limit)
	if err != nil {
		return nil, classifySQLiteError(CodeQueryFailed, "query retention candidates", err)
	}
	defer rows.Close()
	var candidates []RetentionCandidate
	for rows.Next() {
		var candidate RetentionCandidate
		if err := rows.Scan(&candidate.MediaSegmentID, &candidate.ManagedRelativePath, &candidate.SHA256, &candidate.SizeBytes, &candidate.CreatedAtUTC); err != nil {
			return nil, classifySQLiteError(CodeQueryFailed, "scan retention candidate", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, classifySQLiteError(CodeQueryFailed, "iterate retention candidates", err)
	}
	return candidates, nil
}

func (store *Store) TemporaryPathReferenced(ctx context.Context, relativePath string) (bool, error) {
	var count int
	err := store.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM media_ingest_events event
JOIN (SELECT ingest_key, MAX(id) id FROM media_ingest_events GROUP BY ingest_key) latest ON latest.id = event.id
WHERE event.temporary_relative_path = ? AND event.status IN ('discovered', 'pending', 'validated', 'failed')`, relativePath).Scan(&count)
	if err != nil {
		return false, classifySQLiteError(CodeQueryFailed, "check temporary media reference", err)
	}
	return count > 0, nil
}

func (store *Store) AppendRetentionEvent(ctx context.Context, event RetentionEvent) error {
	_, err := store.db.ExecContext(ctx, `
INSERT INTO retention_events(event_key, media_segment_id, status, reason_code, managed_relative_path, occurred_at_utc)
VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(event_key) DO NOTHING`,
		eventKey(fmt.Sprint(event.MediaSegmentID), event.Status, event.ReasonCode, event.ManagedRelativePath, event.OccurredAtUTC),
		event.MediaSegmentID, event.Status, event.ReasonCode, event.ManagedRelativePath, event.OccurredAtUTC)
	if err != nil {
		return classifySQLiteError(CodeWriteFailed, "append retention event", err)
	}
	return nil
}

func (store *Store) RetentionDeletionPlanned(ctx context.Context, segmentID int64, relativePath string) (bool, error) {
	var status string
	err := store.db.QueryRowContext(ctx, `SELECT status FROM retention_events
WHERE media_segment_id = ? AND managed_relative_path = ? ORDER BY id DESC LIMIT 1`, segmentID, relativePath).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, classifySQLiteError(CodeQueryFailed, "read retention plan", err)
	}
	return status == "planned", nil
}

func (store *Store) AppendMediaState(ctx context.Context, segmentID int64, status, reason, producer, occurredAtUTC string) error {
	key := eventKey(fmt.Sprint(segmentID), status, reason, producer, occurredAtUTC)
	_, err := store.db.ExecContext(ctx, `
INSERT INTO media_segment_state_events(event_key, media_segment_id, status, reason_code, producer, occurred_at_utc)
VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(event_key) DO NOTHING`, key, segmentID, status, nullableString(reason), producer, occurredAtUTC)
	if err != nil {
		return classifySQLiteError(CodeWriteFailed, "append media state", err)
	}
	var id int64
	if err := store.db.QueryRowContext(ctx, "SELECT id FROM media_segment_state_events WHERE event_key = ?", key).Scan(&id); err != nil {
		return classifySQLiteError(CodeQueryFailed, "read media state", err)
	}
	return store.applyMediaSegmentProjection(ctx, id)
}

// CommitRetentionDeletion atomically records the irreversible filesystem outcome,
// the corresponding media state fact, and its read projection. A retry is safe
// even if the caller did not observe the first commit result.
func (store *Store) CommitRetentionDeletion(ctx context.Context, event RetentionEvent) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return classifySQLiteError(CodeWriteFailed, "begin retention deletion commit", err)
	}
	defer tx.Rollback()
	retentionKey := eventKey(fmt.Sprint(event.MediaSegmentID), "deleted", event.ReasonCode, event.ManagedRelativePath)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO retention_events(event_key, media_segment_id, status, reason_code, managed_relative_path, occurred_at_utc)
VALUES(?, ?, 'deleted', ?, ?, ?) ON CONFLICT(event_key) DO NOTHING`, retentionKey, event.MediaSegmentID, event.ReasonCode, event.ManagedRelativePath, event.OccurredAtUTC); err != nil {
		return classifySQLiteError(CodeWriteFailed, "append retention deletion", err)
	}
	stateKey := eventKey(fmt.Sprint(event.MediaSegmentID), "retention_deleted", event.ReasonCode, "recorder-core-retention")
	if _, err := tx.ExecContext(ctx, `
INSERT INTO media_segment_state_events(event_key, media_segment_id, status, reason_code, producer, occurred_at_utc)
VALUES(?, ?, 'retention_deleted', ?, 'recorder-core-retention', ?) ON CONFLICT(event_key) DO NOTHING`, stateKey, event.MediaSegmentID, nullableString(event.ReasonCode), event.OccurredAtUTC); err != nil {
		return classifySQLiteError(CodeWriteFailed, "append retention media state", err)
	}
	var stateEventID int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM media_segment_state_events WHERE event_key = ?", stateKey).Scan(&stateEventID); err != nil {
		return classifySQLiteError(CodeQueryFailed, "read retention media state", err)
	}
	if _, err := tx.ExecContext(ctx, `
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
WHERE excluded.last_state_event_id > media_segment_status.last_state_event_id`, stateEventID); err != nil {
		return classifySQLiteError(CodeWriteFailed, "update retention media projection", err)
	}
	if err := tx.Commit(); err != nil {
		return classifySQLiteError(CodeWriteFailed, "commit retention deletion", err)
	}
	return nil
}

func (store *Store) Checkpoint(ctx context.Context, truncate bool) error {
	mode := "PASSIVE"
	if truncate {
		mode = "TRUNCATE"
	}
	if _, err := store.db.ExecContext(ctx, "PRAGMA wal_checkpoint("+mode+")"); err != nil {
		return classifySQLiteError(CodeWriteFailed, "checkpoint WAL", err)
	}
	return nil
}

func (store *Store) SchemaInfo(ctx context.Context) (SchemaInfo, error) {
	return readSchemaInfo(ctx, store.db)
}

func ReadSchemaInfo(ctx context.Context, path string) (SchemaInfo, error) {
	if !filepath.IsAbs(path) {
		return SchemaInfo{}, &Error{Code: CodePathInvalid, Err: errors.New("schema inspection path must be absolute")}
	}
	db, err := sql.Open(sqliteDriverName, inspectionDSN(path, 5*time.Second))
	if err != nil {
		return SchemaInfo{}, wrap(CodeOpenFailed, "open database for schema inspection", err)
	}
	defer db.Close()
	return readSchemaInfo(ctx, db)
}

func readSchemaInfo(ctx context.Context, db *sql.DB) (SchemaInfo, error) {
	var result SchemaInfo
	for _, item := range []struct {
		name     string
		target   *int
		optional bool
	}{
		{"schema_migrations", &result.Core, false}, {"media_schema_migrations", &result.Media, false},
		{"m3_schema_migrations", &result.M3, false}, {"m4_schema_migrations", &result.M4, true},
	} {
		if item.optional {
			var exists int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name=?", item.name).Scan(&exists); err != nil {
				return SchemaInfo{}, classifySQLiteError(CodeQueryFailed, "inspect schema compatibility ledger", err)
			}
			if exists == 0 {
				*item.target = 0
				continue
			}
		}
		if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM "+item.name).Scan(item.target); err != nil {
			return SchemaInfo{}, classifySQLiteError(CodeQueryFailed, "read schema compatibility", err)
		}
	}
	return result, nil
}

func (store *Store) BackupTo(ctx context.Context, target string) error {
	if !filepath.IsAbs(target) {
		return &Error{Code: CodePathInvalid, Err: errors.New("backup database path must be absolute")}
	}
	if _, err := os.Stat(target); err == nil {
		return &Error{Code: CodePathInvalid, Err: errors.New("backup database target already exists")}
	} else if !errors.Is(err, os.ErrNotExist) {
		return wrap(CodeOpenFailed, "inspect backup target", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return wrap(CodeOpenFailed, "create backup directory", err)
	}
	quoted := strings.ReplaceAll(filepath.Clean(target), "'", "''")
	if _, err := store.db.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
		return classifySQLiteError(CodeWriteFailed, "create consistent database snapshot", err)
	}
	if err := VerifyDatabase(ctx, target); err != nil {
		_ = os.Remove(target)
		return err
	}
	return nil
}

func VerifyDatabase(ctx context.Context, path string) error {
	if !filepath.IsAbs(path) {
		return &Error{Code: CodePathInvalid, Err: errors.New("database verification path must be absolute")}
	}
	dsn := inspectionDSN(path, 5*time.Second)
	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		return wrap(CodeOpenFailed, "open database for verification", err)
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return classifySQLiteError(CodeQueryFailed, "verify database integrity", err)
	}
	if integrity != "ok" {
		return &Error{Code: CodeQueryFailed, Err: fmt.Errorf("database integrity check returned %q", integrity)}
	}
	if _, err = readSchemaInfo(ctx, db); err != nil {
		return err
	}
	return verifyCurrentMigrationLedgers(ctx, db)
}

func verifyCurrentMigrationLedgers(ctx context.Context, db *sql.DB) error {
	var userVersion int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		return classifySQLiteError(CodeQueryFailed, "read verified database user version", err)
	}
	if userVersion != CurrentSchemaVersion {
		return &Error{Code: CodeMigrationFailed, Err: fmt.Errorf("core schema version %d does not match required version %d", userVersion, CurrentSchemaVersion)}
	}
	sets := []struct {
		table string
		load  func() ([]migration, error)
	}{
		{"schema_migrations", repositoryMigrations},
		{"media_schema_migrations", repositoryMediaMigrations},
		{"m3_schema_migrations", repositoryM3Migrations},
		{"m4_schema_migrations", repositoryM4Migrations},
	}
	for _, set := range sets {
		expected, err := set.load()
		if err != nil {
			return wrap(CodeMigrationFailed, "load migrations for verification", err)
		}
		rows, err := db.QueryContext(ctx, "SELECT version, name, checksum FROM "+set.table+" ORDER BY version")
		if err != nil {
			return classifySQLiteError(CodeMigrationFailed, "read verified migration ledger", err)
		}
		index := 0
		for rows.Next() {
			var version int
			var name, checksum string
			if err := rows.Scan(&version, &name, &checksum); err != nil {
				rows.Close()
				return classifySQLiteError(CodeMigrationFailed, "scan verified migration ledger", err)
			}
			if index >= len(expected) || version != expected[index].version || name != expected[index].name || checksum != expected[index].checksum {
				rows.Close()
				return &Error{Code: CodeMigrationFailed, Err: fmt.Errorf("%s contains an unknown or modified migration at version %d", set.table, version)}
			}
			index++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return classifySQLiteError(CodeMigrationFailed, "iterate verified migration ledger", err)
		}
		if err := rows.Close(); err != nil {
			return wrap(CodeMigrationFailed, "close verified migration ledger", err)
		}
		if index != len(expected) {
			return &Error{Code: CodeMigrationFailed, Err: fmt.Errorf("%s contains %d migrations, expected %d", set.table, index, len(expected))}
		}
	}
	return nil
}

func ReadMediaManifest(ctx context.Context, path string) ([]MediaManifestEntry, error) {
	if err := VerifyDatabase(ctx, path); err != nil {
		return nil, err
	}
	db, err := sql.Open(sqliteDriverName, inspectionDSN(path, 5*time.Second))
	if err != nil {
		return nil, wrap(CodeOpenFailed, "open media manifest database", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `
SELECT ms.id, ms.managed_relative_path, ms.sha256, ms.size_bytes, status.status
FROM media_segments ms JOIN media_segment_status status ON status.media_segment_id = ms.id
WHERE status.status IN ('accepted', 'restored') ORDER BY ms.id`)
	if err != nil {
		return nil, classifySQLiteError(CodeQueryFailed, "query backup media manifest", err)
	}
	defer rows.Close()
	var result []MediaManifestEntry
	for rows.Next() {
		var entry MediaManifestEntry
		if err := rows.Scan(&entry.MediaSegmentID, &entry.ManagedRelativePath, &entry.SHA256, &entry.SizeBytes, &entry.Status); err != nil {
			return nil, classifySQLiteError(CodeQueryFailed, "scan backup media manifest", err)
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, classifySQLiteError(CodeQueryFailed, "iterate backup media manifest", err)
	}
	return result, nil
}

func inspectionDSN(path string, busyTimeout time.Duration) string {
	uriPath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	databaseURL := &url.URL{Scheme: "file", Path: uriPath}
	query := databaseURL.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	databaseURL.RawQuery = query.Encode()
	return databaseURL.String()
}
