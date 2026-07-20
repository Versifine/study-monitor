package eventstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Versifine/study-monitor/internal/config"
	coveragecalc "github.com/Versifine/study-monitor/internal/coverage"
)

const (
	CodeCoverageRangeInvalid = "COVERAGE_RANGE_INVALID"
	CodeCoverageBuildFailed  = "COVERAGE_BUILD_FAILED"
)

type CoverageInterval struct {
	ID           int64    `json:"projection_id"`
	CollectorID  string   `json:"collector_id"`
	StartUTC     string   `json:"start_utc"`
	EndUTC       string   `json:"end_utc"`
	Availability string   `json:"availability"`
	QualityFlags []string `json:"quality_flags"`
	ReasonCode   string   `json:"reason_code"`
	Generation   int64    `json:"generation"`
	BuiltAtUTC   string   `json:"built_at_utc"`
}

type CoverageProjectionStatus struct {
	CollectorID   string `json:"collector_id"`
	RangeStartUTC string `json:"range_start_utc"`
	RangeEndUTC   string `json:"range_end_utc"`
	Generation    int64  `json:"generation"`
	Status        string `json:"status"`
	ErrorCode     string `json:"error_code,omitempty"`
	FactWatermark int64  `json:"fact_watermark"`
	BuiltAtUTC    string `json:"built_at_utc"`
}

type CoverageResult struct {
	RangeStartUTC string                     `json:"range_start_utc"`
	RangeEndUTC   string                     `json:"range_end_utc"`
	Projections   []CoverageProjectionStatus `json:"projections"`
	Intervals     []CoverageInterval         `json:"intervals"`
}

func (store *Store) RebuildCoverage(ctx context.Context, collectors []config.CollectorConfig, start, end, now time.Time, uncertainAfter time.Duration, maxFacts int) (CoverageResult, error) {
	start, end, now = start.UTC(), end.UTC(), now.UTC()
	if !start.Before(end) || maxFacts < 1 {
		return CoverageResult{}, &Error{Code: CodeCoverageRangeInvalid, Err: errors.New("coverage range or limits are invalid")}
	}
	result := CoverageResult{RangeStartUTC: fixedUTC(start), RangeEndUTC: fixedUTC(end), Projections: []CoverageProjectionStatus{}, Intervals: []CoverageInterval{}}
	for _, collector := range collectors {
		if !collector.Enabled {
			continue
		}
		if err := store.syncTimelineProjection(ctx, start.Add(-collector.OfflineAfterDuration()), end, uncertainAfter, maxFacts, collector.ID, true); err != nil {
			if code := ErrorCode(err); code == CodeTimelineSyncBusy || code == CodeCanceled {
				return result, err
			}
			result.Projections = append(result.Projections, store.markCoverageStale(ctx, collector.ID, start, end, err))
			continue
		}
		status, intervals, err := store.rebuildCollectorCoverage(ctx, collector, start, end, now, maxFacts)
		if err != nil {
			if code := ErrorCode(err); code == CodeCanceled || ctx.Err() != nil {
				return result, err
			}
			status = store.markCoverageStale(ctx, collector.ID, start, end, err)
			result.Projections = append(result.Projections, status)
			continue
		}
		result.Projections = append(result.Projections, status)
		result.Intervals = append(result.Intervals, intervals...)
	}
	return result, nil
}

func (store *Store) rebuildCollectorCoverage(ctx context.Context, collector config.CollectorConfig, start, end, now time.Time, maxFacts int) (CoverageProjectionStatus, []CoverageInterval, error) {
	rows, err := store.db.QueryContext(ctx, `
SELECT id, stable_id, source_type, event_type, corrected_start_utc, COALESCE(corrected_end_utc, ''),
       quality_flags_json, payload_json
FROM timeline_entries
WHERE collector_id = ? AND corrected_start_utc < ?
  AND COALESCE(corrected_end_utc, corrected_start_utc) > ?
  AND corrected_end_utc IS NOT NULL
  AND corrected_end_utc > corrected_start_utc
  AND (source_type IN ('heartbeat', 'media_segment')
       OR (source_type = 'raw_event' AND event_type = 'activitywatch.event'))
ORDER BY corrected_start_utc, id
`, collector.ID, fixedUTC(end), fixedUTC(start.Add(-collector.OfflineAfterDuration())))
	if err != nil {
		return CoverageProjectionStatus{}, nil, classifySQLiteError(CodeCoverageBuildFailed, "read coverage facts", err)
	}
	facts := make([]coveragecalc.Fact, 0)
	var watermark int64
	for rows.Next() {
		var id int64
		var stableID, sourceType, eventType, startText, endText, flagsJSON, payloadJSON string
		if err := rows.Scan(&id, &stableID, &sourceType, &eventType, &startText, &endText, &flagsJSON, &payloadJSON); err != nil {
			rows.Close()
			return CoverageProjectionStatus{}, nil, wrap(CodeCoverageBuildFailed, "scan coverage fact", err)
		}
		fact, usable, err := timelineCoverageFact(stableID, sourceType, eventType, startText, endText, flagsJSON, payloadJSON)
		if err != nil {
			rows.Close()
			return CoverageProjectionStatus{}, nil, err
		}
		if usable {
			if len(facts) >= maxFacts {
				rows.Close()
				return CoverageProjectionStatus{}, nil, &Error{Code: CodeTimelineFactLimit, Err: errors.New("collector coverage exceeds the configured fact limit")}
			}
			if id > watermark {
				watermark = id
			}
			facts = append(facts, fact)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CoverageProjectionStatus{}, nil, classifySQLiteError(CodeCoverageBuildFailed, "iterate coverage facts", err)
	}
	rows.Close()
	intervals, err := coveragecalc.Build(collector, start, end, now, facts)
	if err != nil {
		return CoverageProjectionStatus{}, nil, &Error{Code: CodeCoverageBuildFailed, Err: err}
	}
	builtAt := fixedUTC(store.now())
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return CoverageProjectionStatus{}, nil, classifySQLiteError(CodeCoverageBuildFailed, "begin coverage rebuild", err)
	}
	defer tx.Rollback()
	var generation int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(generation, 0) FROM coverage_projection_state WHERE collector_id = ?", collector.ID).Scan(&generation); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return CoverageProjectionStatus{}, nil, classifySQLiteError(CodeCoverageBuildFailed, "read coverage generation", err)
	}
	generation++
	if _, err := tx.ExecContext(ctx, "DELETE FROM coverage_intervals WHERE collector_id = ?", collector.ID); err != nil {
		return CoverageProjectionStatus{}, nil, classifySQLiteError(CodeCoverageBuildFailed, "replace coverage intervals", err)
	}
	stored := make([]CoverageInterval, 0, len(intervals))
	for _, interval := range intervals {
		flags := qualityJSON(interval.QualityFlags, false)
		insert, err := tx.ExecContext(ctx, `
INSERT INTO coverage_intervals (
    collector_id, start_utc, end_utc, availability, quality_flags_json,
    reason_code, generation, built_at_utc
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, collector.ID, fixedUTC(interval.Start), fixedUTC(interval.End),
			interval.Availability, flags, interval.ReasonCode, generation, builtAt)
		if err != nil {
			return CoverageProjectionStatus{}, nil, classifySQLiteError(CodeCoverageBuildFailed, "insert coverage interval", err)
		}
		id, err := insert.LastInsertId()
		if err != nil {
			return CoverageProjectionStatus{}, nil, wrap(CodeCoverageBuildFailed, "read coverage interval id", err)
		}
		stored = append(stored, CoverageInterval{ID: id, CollectorID: collector.ID, StartUTC: fixedUTC(interval.Start), EndUTC: fixedUTC(interval.End), Availability: interval.Availability, QualityFlags: interval.QualityFlags, ReasonCode: interval.ReasonCode, Generation: generation, BuiltAtUTC: builtAt})
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO coverage_projection_state (
    collector_id, range_start_utc, range_end_utc, generation, status,
    error_code, fact_watermark, built_at_utc
) VALUES (?, ?, ?, ?, 'fresh', NULL, ?, ?)
ON CONFLICT(collector_id) DO UPDATE SET
    range_start_utc = excluded.range_start_utc,
    range_end_utc = excluded.range_end_utc,
    generation = excluded.generation,
    status = excluded.status,
    error_code = NULL,
    fact_watermark = excluded.fact_watermark,
    built_at_utc = excluded.built_at_utc`, collector.ID, fixedUTC(start), fixedUTC(end), generation, watermark, builtAt); err != nil {
		return CoverageProjectionStatus{}, nil, classifySQLiteError(CodeCoverageBuildFailed, "save coverage projection state", err)
	}
	if err := tx.Commit(); err != nil {
		return CoverageProjectionStatus{}, nil, classifySQLiteError(CodeCoverageBuildFailed, "commit coverage rebuild", err)
	}
	status := CoverageProjectionStatus{CollectorID: collector.ID, RangeStartUTC: fixedUTC(start), RangeEndUTC: fixedUTC(end), Generation: generation, Status: "fresh", FactWatermark: watermark, BuiltAtUTC: builtAt}
	return status, stored, nil
}

func timelineCoverageFact(stableID, sourceType, eventType, startText, endText, flagsJSON, payloadJSON string) (coveragecalc.Fact, bool, error) {
	if endText == "" {
		return coveragecalc.Fact{}, false, nil
	}
	start, startErr := time.Parse(fixedUTCLayout, startText)
	end, endErr := time.Parse(fixedUTCLayout, endText)
	if startErr != nil || endErr != nil || end.Before(start) {
		return coveragecalc.Fact{}, false, &Error{Code: CodeCoverageBuildFailed, Err: errors.New("timeline coverage fact has invalid bounds")}
	}
	if start.Equal(end) {
		// Zero-duration ActivityWatch events are valid point facts on the
		// timeline, but they do not establish interval coverage.
		return coveragecalc.Fact{}, false, nil
	}
	var flags []string
	if err := json.Unmarshal([]byte(flagsJSON), &flags); err != nil {
		return coveragecalc.Fact{}, false, &Error{Code: CodeCoverageBuildFailed, Err: errors.New("timeline coverage quality flags are invalid")}
	}
	fact := coveragecalc.Fact{Start: start, End: end, Availability: coveragecalc.Covered, QualityFlags: flags, StableID: stableID, Priority: 20, ReasonCode: "EVIDENCE_INTERVAL"}
	switch sourceType {
	case "heartbeat":
		var payload struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			return coveragecalc.Fact{}, false, &Error{Code: CodeCoverageBuildFailed, Err: errors.New("heartbeat projection payload is invalid")}
		}
		fact.Priority = 30
		fact.ReasonCode = "HEARTBEAT_ACTIVE"
		if payload.State == HeartbeatStateIdle {
			fact.Availability = coveragecalc.ConfirmedIdle
			fact.ReasonCode = "HEARTBEAT_IDLE"
		}
	case "raw_event":
		if eventType != "activitywatch.event" {
			return coveragecalc.Fact{}, false, nil
		}
		var payload struct {
			BucketType string                     `json:"bucket_type"`
			Data       map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			return coveragecalc.Fact{}, false, nil
		}
		fact.ReasonCode = "ACTIVITYWATCH_EVENT"
		var status string
		_ = json.Unmarshal(payload.Data["status"], &status)
		if status == "afk" && (payload.BucketType == "afkstatus" || payload.BucketType == "currentafkstatus") {
			fact.Availability = coveragecalc.ConfirmedIdle
			fact.Priority = 40
			fact.ReasonCode = "ACTIVITYWATCH_AFK"
		}
	case "media_segment":
		fact.Priority = 30
		fact.ReasonCode = "MEDIA_ACCEPTED"
	default:
		return coveragecalc.Fact{}, false, &Error{Code: CodeCoverageBuildFailed, Err: fmt.Errorf("unsupported timeline coverage source %q", sourceType)}
	}
	return fact, true, nil
}

func (store *Store) markCoverageStale(ctx context.Context, collectorID string, start, end time.Time, buildErr error) CoverageProjectionStatus {
	builtAt := fixedUTC(store.now())
	code := ErrorCode(buildErr)
	if code == "" {
		code = CodeCoverageBuildFailed
	}
	var generation int64
	_ = store.db.QueryRowContext(ctx, "SELECT COALESCE(generation, 0) FROM coverage_projection_state WHERE collector_id = ?", collectorID).Scan(&generation)
	if generation < 1 {
		generation = 1
	}
	_, _ = store.db.ExecContext(ctx, `
INSERT INTO coverage_projection_state (
    collector_id, range_start_utc, range_end_utc, generation, status,
    error_code, fact_watermark, built_at_utc
) VALUES (?, ?, ?, ?, 'stale', ?, 0, ?)
ON CONFLICT(collector_id) DO UPDATE SET
    range_start_utc = excluded.range_start_utc,
    range_end_utc = excluded.range_end_utc,
    status = 'stale',
    error_code = excluded.error_code,
    built_at_utc = excluded.built_at_utc`, collectorID, fixedUTC(start), fixedUTC(end), generation, code, builtAt)
	return CoverageProjectionStatus{CollectorID: collectorID, RangeStartUTC: fixedUTC(start), RangeEndUTC: fixedUTC(end), Generation: generation, Status: "stale", ErrorCode: code, BuiltAtUTC: builtAt}
}
