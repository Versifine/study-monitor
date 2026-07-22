package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"time"
)

// CertificationDatabaseSnapshot is a read-only, cumulative view of a verified
// database snapshot. Daily M6 reports calculate deltas between these snapshots;
// this type is not persisted in the Recorder Core database.
type CertificationDatabaseSnapshot struct {
	SchemaVersion              int                        `json:"schema_version"`
	Integrity                  string                     `json:"integrity"`
	DatabaseSchema             SchemaInfo                 `json:"database_schema"`
	Counts                     CertificationCounts        `json:"counts"`
	RawEventsByCollector       []CertificationGroupCount  `json:"raw_events_by_collector"`
	HeartbeatsByCollectorState []CertificationGroupCount  `json:"heartbeats_by_collector_state"`
	MediaByCollector           []CertificationGroupCount  `json:"media_by_collector"`
	MediaIngestByStatus        []CertificationGroupCount  `json:"media_ingest_by_status"`
	MediaCurrentByStatus       []CertificationGroupCount  `json:"media_current_by_status"`
	FaultsBySeverityStatus     []CertificationGroupCount  `json:"faults_by_severity_status"`
	ModulesByState             []CertificationGroupCount  `json:"modules_by_state"`
	ModesByState               []CertificationGroupCount  `json:"modes_by_state"`
	RetentionByStatus          []CertificationGroupCount  `json:"retention_by_status"`
	CoverageProjections        []CertificationProjection  `json:"coverage_projections"`
	PotentialDuplicates        CertificationDuplicateScan `json:"potential_duplicates"`
	MaximumMediaDurationMS     int64                      `json:"maximum_media_duration_ms"`
}

type CertificationCounts struct {
	RawEvents                int64 `json:"raw_events"`
	CollectorHeartbeats      int64 `json:"collector_heartbeats"`
	MediaSegments            int64 `json:"media_segments"`
	MediaIngestEvents        int64 `json:"media_ingest_events"`
	MediaStateEvents         int64 `json:"media_state_events"`
	FaultEvents              int64 `json:"fault_events"`
	ModuleStateEvents        int64 `json:"module_state_events"`
	ModeTransitionEvents     int64 `json:"mode_transition_events"`
	RetentionEvents          int64 `json:"retention_events"`
	TimelineEntries          int64 `json:"timeline_entries"`
	CoverageIntervals        int64 `json:"coverage_intervals"`
	ActivityWatchCheckpoints int64 `json:"activitywatch_checkpoints"`
}

type CertificationGroupCount struct {
	Key1  string `json:"key_1"`
	Key2  string `json:"key_2,omitempty"`
	Key3  string `json:"key_3,omitempty"`
	Count int64  `json:"count"`
}

type CertificationProjection struct {
	CollectorID   string `json:"collector_id"`
	Status        string `json:"status"`
	Generation    int64  `json:"generation"`
	FactWatermark int64  `json:"fact_watermark"`
	BuiltAtUTC    string `json:"built_at_utc"`
	ErrorCode     string `json:"error_code,omitempty"`
}

type CertificationDuplicateScan struct {
	RawEventIdentityGroups  int64 `json:"raw_event_identity_groups"`
	HeartbeatIdentityGroups int64 `json:"heartbeat_identity_groups"`
	MediaSHA256Groups       int64 `json:"media_sha256_groups"`
}

// ReadCertificationDatabaseSnapshot verifies a consistent snapshot and then
// reads only aggregate facts needed by M6. It never opens the live database in
// write mode and never changes migrations or projections.
func ReadCertificationDatabaseSnapshot(ctx context.Context, path string) (CertificationDatabaseSnapshot, error) {
	if !filepath.IsAbs(path) {
		return CertificationDatabaseSnapshot{}, &Error{Code: CodePathInvalid, Err: errors.New("certification database path must be absolute")}
	}
	if err := VerifyDatabase(ctx, path); err != nil {
		return CertificationDatabaseSnapshot{}, err
	}
	db, err := sql.Open(sqliteDriverName, inspectionDSN(path, 5*time.Second))
	if err != nil {
		return CertificationDatabaseSnapshot{}, wrap(CodeOpenFailed, "open certification database snapshot", err)
	}
	defer db.Close()

	schema, err := readSchemaInfo(ctx, db)
	if err != nil {
		return CertificationDatabaseSnapshot{}, err
	}
	result := CertificationDatabaseSnapshot{SchemaVersion: 1, Integrity: "ok", DatabaseSchema: schema}
	if err := readCertificationCounts(ctx, db, &result.Counts); err != nil {
		return CertificationDatabaseSnapshot{}, err
	}
	groups := []struct {
		query  string
		target *[]CertificationGroupCount
	}{
		{`SELECT collector_id, '', '', COUNT(*) FROM raw_events GROUP BY collector_id ORDER BY collector_id`, &result.RawEventsByCollector},
		{`SELECT collector_id, state, '', COUNT(*) FROM collector_heartbeats GROUP BY collector_id, state ORDER BY collector_id, state`, &result.HeartbeatsByCollectorState},
		{`SELECT collector_id, '', '', COUNT(*) FROM media_segments GROUP BY collector_id ORDER BY collector_id`, &result.MediaByCollector},
		{`SELECT status, COALESCE(error_code, ''), '', COUNT(*) FROM media_ingest_events GROUP BY status, COALESCE(error_code, '') ORDER BY status, COALESCE(error_code, '')`, &result.MediaIngestByStatus},
		{`SELECT status, '', '', COUNT(*) FROM media_segment_status GROUP BY status ORDER BY status`, &result.MediaCurrentByStatus},
		{`SELECT severity, status, error_code, COUNT(*) FROM fault_events GROUP BY severity, status, error_code ORDER BY severity, status, error_code`, &result.FaultsBySeverityStatus},
		{`SELECT module, status, reason_code, COUNT(*) FROM module_state_events GROUP BY module, status, reason_code ORDER BY module, status, reason_code`, &result.ModulesByState},
		{`SELECT old_mode, new_mode, reason_code, COUNT(*) FROM mode_transition_events GROUP BY old_mode, new_mode, reason_code ORDER BY old_mode, new_mode, reason_code`, &result.ModesByState},
		{`SELECT status, reason_code, '', COUNT(*) FROM retention_events GROUP BY status, reason_code ORDER BY status, reason_code`, &result.RetentionByStatus},
	}
	for _, group := range groups {
		values, queryErr := queryCertificationGroups(ctx, db, group.query)
		if queryErr != nil {
			return CertificationDatabaseSnapshot{}, queryErr
		}
		*group.target = values
	}
	projections, err := queryCertificationProjections(ctx, db)
	if err != nil {
		return CertificationDatabaseSnapshot{}, err
	}
	result.CoverageProjections = projections
	if err := readPotentialDuplicates(ctx, db, &result.PotentialDuplicates); err != nil {
		return CertificationDatabaseSnapshot{}, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(duration_ms), 0) FROM media_segments`).Scan(&result.MaximumMediaDurationMS); err != nil {
		return CertificationDatabaseSnapshot{}, classifySQLiteError(CodeQueryFailed, "scan maximum media duration", err)
	}
	return result, nil
}

func readCertificationCounts(ctx context.Context, db *sql.DB, target *CertificationCounts) error {
	queries := []struct {
		name string
		out  *int64
	}{
		{"raw_events", &target.RawEvents}, {"collector_heartbeats", &target.CollectorHeartbeats},
		{"media_segments", &target.MediaSegments}, {"media_ingest_events", &target.MediaIngestEvents},
		{"media_segment_state_events", &target.MediaStateEvents}, {"fault_events", &target.FaultEvents},
		{"module_state_events", &target.ModuleStateEvents}, {"mode_transition_events", &target.ModeTransitionEvents},
		{"retention_events", &target.RetentionEvents}, {"timeline_entries", &target.TimelineEntries},
		{"coverage_intervals", &target.CoverageIntervals}, {"activitywatch_checkpoints", &target.ActivityWatchCheckpoints},
	}
	for _, item := range queries {
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+item.name).Scan(item.out); err != nil {
			return classifySQLiteError(CodeQueryFailed, "count certification table "+item.name, err)
		}
	}
	return nil
}

func queryCertificationGroups(ctx context.Context, db *sql.DB, query string) ([]CertificationGroupCount, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, classifySQLiteError(CodeQueryFailed, "query certification groups", err)
	}
	defer rows.Close()
	result := []CertificationGroupCount{}
	for rows.Next() {
		var item CertificationGroupCount
		if err := rows.Scan(&item.Key1, &item.Key2, &item.Key3, &item.Count); err != nil {
			return nil, classifySQLiteError(CodeQueryFailed, "scan certification groups", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, classifySQLiteError(CodeQueryFailed, "iterate certification groups", err)
	}
	return result, nil
}

func queryCertificationProjections(ctx context.Context, db *sql.DB) ([]CertificationProjection, error) {
	rows, err := db.QueryContext(ctx, `SELECT collector_id, status, generation, fact_watermark, built_at_utc, COALESCE(error_code, '') FROM coverage_projection_state ORDER BY collector_id`)
	if err != nil {
		return nil, classifySQLiteError(CodeQueryFailed, "query certification coverage projections", err)
	}
	defer rows.Close()
	result := []CertificationProjection{}
	for rows.Next() {
		var item CertificationProjection
		if err := rows.Scan(&item.CollectorID, &item.Status, &item.Generation, &item.FactWatermark, &item.BuiltAtUTC, &item.ErrorCode); err != nil {
			return nil, classifySQLiteError(CodeQueryFailed, "scan certification coverage projections", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, classifySQLiteError(CodeQueryFailed, "iterate certification coverage projections", err)
	}
	return result, nil
}

func readPotentialDuplicates(ctx context.Context, db *sql.DB, target *CertificationDuplicateScan) error {
	queries := []struct {
		query string
		out   *int64
	}{
		{`SELECT COUNT(*) FROM (SELECT 1 FROM raw_events GROUP BY collector_id, event_type, device_timestamp_raw, clock_offset_ms, clock_error_ms, payload_hash HAVING COUNT(*) > 1)`, &target.RawEventIdentityGroups},
		{`SELECT COUNT(*) FROM (SELECT 1 FROM collector_heartbeats GROUP BY collector_id, state, device_start_raw, device_end_raw, clock_offset_ms, clock_error_ms, quality_flags_json HAVING COUNT(*) > 1)`, &target.HeartbeatIdentityGroups},
		{`SELECT COUNT(*) FROM (SELECT 1 FROM media_segments GROUP BY sha256 HAVING COUNT(*) > 1)`, &target.MediaSHA256Groups},
	}
	for _, item := range queries {
		if err := db.QueryRowContext(ctx, item.query).Scan(item.out); err != nil {
			return classifySQLiteError(CodeQueryFailed, "scan potential duplicate evidence", err)
		}
	}
	return nil
}
