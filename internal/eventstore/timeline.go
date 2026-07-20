package eventstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	CodeTimelineRangeInvalid = "TIMELINE_RANGE_INVALID"
	CodeTimelineFactLimit    = "TIMELINE_FACT_LIMIT"
	CodeTimelineByteLimit    = "TIMELINE_BYTE_LIMIT"
	CodeTimelineSyncBusy     = "TIMELINE_SYNC_BUSY"
	CodeTimelineCursor       = "TIMELINE_CURSOR_INVALID"

	timelineProjectionChunkSize = 128
	timelineProjectionWait      = 2 * time.Second
)

type TimelineEntry struct {
	ID                  int64           `json:"projection_id"`
	StableID            string          `json:"stable_id"`
	SourceType          string          `json:"source_type"`
	SourceID            int64           `json:"source_id"`
	CollectorID         string          `json:"collector_id"`
	EventType           string          `json:"event_type"`
	DeviceStartRaw      string          `json:"device_start_raw"`
	DeviceEndRaw        string          `json:"device_end_raw,omitempty"`
	DeviceStartUTC      string          `json:"device_start_utc"`
	DeviceEndUTC        string          `json:"device_end_utc,omitempty"`
	ReceivedAtUTC       string          `json:"received_at_utc"`
	CorrectedStartUTC   string          `json:"corrected_start_utc"`
	CorrectedEndUTC     string          `json:"corrected_end_utc,omitempty"`
	ClockOffsetMS       int64           `json:"clock_offset_ms"`
	ClockErrorMS        int64           `json:"clock_error_ms"`
	ClockUncertain      bool            `json:"clock_uncertain"`
	QualityFlags        []string        `json:"quality_flags"`
	Payload             json.RawMessage `json:"payload"`
	SourceSchemaVersion int             `json:"source_schema_version"`
}

type TimelinePage struct {
	RangeStartUTC string          `json:"range_start_utc"`
	RangeEndUTC   string          `json:"range_end_utc"`
	SnapshotID    int64           `json:"snapshot_id"`
	Entries       []TimelineEntry `json:"entries"`
	NextCursor    string          `json:"next_cursor,omitempty"`
}

type timelineCursor struct {
	Version       int    `json:"v"`
	RangeStartUTC string `json:"range_start_utc"`
	RangeEndUTC   string `json:"range_end_utc"`
	SnapshotID    int64  `json:"snapshot_id"`
	CorrectedUTC  string `json:"corrected_utc"`
	ReceivedUTC   string `json:"received_utc"`
	SourceType    string `json:"source_type"`
	SourceID      int64  `json:"source_id"`
	ProjectionID  int64  `json:"projection_id"`
}

type timelineProjection struct {
	StableID            string
	SourceType          string
	SourceID            int64
	CollectorID         string
	EventType           string
	DeviceStartRaw      string
	DeviceEndRaw        string
	DeviceStartUTC      string
	DeviceEndUTC        string
	ReceivedAtUTC       string
	CorrectedStartUTC   string
	CorrectedEndUTC     string
	ClockOffsetMS       int64
	ClockErrorMS        int64
	ClockUncertain      bool
	QualityFlagsJSON    string
	PayloadJSON         string
	SourceSchemaVersion int
}

func (store *Store) QueryTimeline(ctx context.Context, start, end time.Time, cursorText string, limit int, uncertainAfter time.Duration, maxFacts int) (TimelinePage, error) {
	start = start.UTC()
	end = end.UTC()
	if !start.Before(end) || limit < 1 || limit > store.maxPageSize || uncertainAfter < 0 || maxFacts < 1 {
		return TimelinePage{}, &Error{Code: CodeTimelineRangeInvalid, Err: errors.New("timeline range or limits are invalid")}
	}
	startText, endText := fixedUTC(start), fixedUTC(end)
	cursor := timelineCursor{Version: 1, RangeStartUTC: startText, RangeEndUTC: endText}
	if cursorText == "" {
		if err := store.syncTimelineProjection(ctx, start, end, uncertainAfter, maxFacts, "", false); err != nil {
			return TimelinePage{}, err
		}
		if err := store.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM timeline_entries").Scan(&cursor.SnapshotID); err != nil {
			return TimelinePage{}, classifySQLiteError(CodeQueryFailed, "create timeline snapshot", err)
		}
	} else {
		decoded, err := decodeTimelineCursor(cursorText)
		if err != nil {
			return TimelinePage{}, err
		}
		if decoded.RangeStartUTC != startText || decoded.RangeEndUTC != endText {
			return TimelinePage{}, &Error{Code: CodeTimelineCursor, Err: errors.New("timeline cursor range does not match the query")}
		}
		cursor = decoded
	}

	rows, err := store.db.QueryContext(ctx, `
SELECT id, stable_id, source_type, source_id, collector_id, event_type,
       device_start_raw, COALESCE(device_end_raw, ''), device_start_utc, COALESCE(device_end_utc, ''),
       received_at_utc, corrected_start_utc, COALESCE(corrected_end_utc, ''),
       clock_offset_ms, clock_error_ms, clock_uncertain, quality_flags_json,
       payload_json, source_schema_version
FROM timeline_entries
WHERE id <= ? AND corrected_start_utc >= ? AND corrected_start_utc < ?
  AND (? = '' OR corrected_start_utc > ?
       OR (corrected_start_utc = ? AND received_at_utc > ?)
       OR (corrected_start_utc = ? AND received_at_utc = ? AND source_type > ?)
       OR (corrected_start_utc = ? AND received_at_utc = ? AND source_type = ? AND source_id > ?)
       OR (corrected_start_utc = ? AND received_at_utc = ? AND source_type = ? AND source_id = ? AND id > ?))
ORDER BY corrected_start_utc, received_at_utc, source_type, source_id, id
LIMIT ?`, cursor.SnapshotID, startText, endText,
		cursor.CorrectedUTC, cursor.CorrectedUTC,
		cursor.CorrectedUTC, cursor.ReceivedUTC,
		cursor.CorrectedUTC, cursor.ReceivedUTC, cursor.SourceType,
		cursor.CorrectedUTC, cursor.ReceivedUTC, cursor.SourceType, cursor.SourceID,
		cursor.CorrectedUTC, cursor.ReceivedUTC, cursor.SourceType, cursor.SourceID, cursor.ProjectionID,
		limit+1)
	if err != nil {
		return TimelinePage{}, classifySQLiteError(CodeQueryFailed, "query timeline", err)
	}
	entries := make([]TimelineEntry, 0, limit+1)
	for rows.Next() {
		var entry TimelineEntry
		var uncertain int
		var flagsJSON, payloadJSON string
		if err := rows.Scan(&entry.ID, &entry.StableID, &entry.SourceType, &entry.SourceID, &entry.CollectorID, &entry.EventType,
			&entry.DeviceStartRaw, &entry.DeviceEndRaw, &entry.DeviceStartUTC, &entry.DeviceEndUTC,
			&entry.ReceivedAtUTC, &entry.CorrectedStartUTC, &entry.CorrectedEndUTC,
			&entry.ClockOffsetMS, &entry.ClockErrorMS, &uncertain, &flagsJSON, &payloadJSON, &entry.SourceSchemaVersion); err != nil {
			rows.Close()
			return TimelinePage{}, wrap(CodeQueryFailed, "scan timeline entry", err)
		}
		entry.ClockUncertain = uncertain == 1
		if err := json.Unmarshal([]byte(flagsJSON), &entry.QualityFlags); err != nil {
			rows.Close()
			return TimelinePage{}, wrap(CodeQueryFailed, "decode timeline quality flags", err)
		}
		entry.Payload = json.RawMessage(payloadJSON)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return TimelinePage{}, classifySQLiteError(CodeQueryFailed, "iterate timeline", err)
	}
	if err := rows.Close(); err != nil {
		return TimelinePage{}, wrap(CodeQueryFailed, "close timeline", err)
	}
	next := ""
	if len(entries) > limit {
		entries = entries[:limit]
		last := entries[len(entries)-1]
		next, err = encodeTimelineCursor(timelineCursor{Version: 1, RangeStartUTC: startText, RangeEndUTC: endText,
			SnapshotID: cursor.SnapshotID, CorrectedUTC: last.CorrectedStartUTC, ReceivedUTC: last.ReceivedAtUTC,
			SourceType: last.SourceType, SourceID: last.SourceID, ProjectionID: last.ID})
		if err != nil {
			return TimelinePage{}, wrap(CodeQueryFailed, "encode timeline cursor", err)
		}
	}
	return TimelinePage{RangeStartUTC: startText, RangeEndUTC: endText, SnapshotID: cursor.SnapshotID, Entries: entries, NextCursor: next}, nil
}

func (store *Store) syncTimelineProjection(ctx context.Context, start, end time.Time, uncertainAfter time.Duration, maxFacts int, collectorID string, includeOverlapping bool) error {
	wait := time.NewTimer(timelineProjectionWait)
	defer wait.Stop()
	select {
	case <-ctx.Done():
		return classifySQLiteError(CodeCanceled, "wait for timeline projection sync", ctx.Err())
	case store.timelineSync <- struct{}{}:
		defer func() { <-store.timelineSync }()
	case <-wait.C:
		return &Error{Code: CodeTimelineSyncBusy, Err: errors.New("another timeline projection sync is already running")}
	}
	projections := make([]timelineProjection, 0)
	projectionBytes := 0
	if err := store.collectRawEventProjections(ctx, start, end, uncertainAfter, maxFacts, collectorID, includeOverlapping, &projections, &projectionBytes); err != nil {
		return err
	}
	if err := store.collectHeartbeatProjections(ctx, start, end, uncertainAfter, maxFacts, collectorID, includeOverlapping, &projections, &projectionBytes); err != nil {
		return err
	}
	if err := store.collectMediaProjections(ctx, start, end, uncertainAfter, maxFacts, collectorID, includeOverlapping, &projections, &projectionBytes); err != nil {
		return err
	}
	if len(projections) == 0 {
		return nil
	}
	for offset := 0; offset < len(projections); offset += timelineProjectionChunkSize {
		end := offset + timelineProjectionChunkSize
		if end > len(projections) {
			end = len(projections)
		}
		if err := store.upsertTimelineProjectionChunk(ctx, projections[offset:end]); err != nil {
			return err
		}
	}
	return nil
}

type projectionSourceQuery struct {
	sql  string
	args []any
}

const rawEventProjectionColumns = `
SELECT id, collector_id, event_type, device_timestamp_raw, device_time_utc, received_at_utc,
       clock_offset_ms, clock_error_ms, payload_json, schema_version
FROM raw_events`

const rawEventCorrectedStartExpression = `(julianday(device_time_utc) + (clock_offset_ms / 86400000.0))`

const rawEventCorrectedEndExpression = `(julianday(device_time_utc) + (clock_offset_ms / 86400000.0) + (CAST(json_extract(payload_json, '$.duration_seconds') AS REAL) / 86400.0))`

const activityWatchIntervalSQL = `event_type = 'activitywatch.event'
  AND json_type(payload_json, '$.bucket_id') = 'text'
  AND json_type(payload_json, '$.bucket_type') = 'text'
  AND json_type(payload_json, '$.source_event_id') = 'integer'
  AND CAST(json_extract(payload_json, '$.source_event_id') AS INTEGER) >= 0
  AND json_type(payload_json, '$.duration_seconds') IN ('integer', 'real')
  AND CAST(json_extract(payload_json, '$.duration_seconds') AS REAL) > 0
  AND CAST(json_extract(payload_json, '$.duration_seconds') AS REAL) <= 31622400
  AND json_type(payload_json, '$.data') = 'object'`

func (store *Store) collectRawEventProjections(ctx context.Context, start, end time.Time, uncertainAfter time.Duration, maxFacts int, collectorID string, includeOverlapping bool, target *[]timelineProjection, projectionBytes *int) error {
	// SQLite julianday values have coarser precision than the stored timestamps.
	// The indexed SQL range is therefore a bounded candidate prefilter. Exact
	// nanosecond bounds are applied before resource accounting and persistence.
	candidateStart := start.UTC().Add(-time.Second).Format(time.RFC3339Nano)
	candidateEnd := end.UTC().Add(time.Second).Format(time.RFC3339Nano)
	queries := rawEventProjectionQueries(collectorID, includeOverlapping, candidateStart, candidateEnd)
	for _, query := range queries {
		rows, err := store.db.QueryContext(ctx, query.sql, query.args...)
		if err != nil {
			return classifySQLiteError(CodeQueryFailed, "read raw events for timeline projection", err)
		}
		for rows.Next() {
			var id, offsetMS, errorMS int64
			var sourceCollectorID, eventType, rawStart, deviceUTC, receivedUTC, payload string
			var schemaVersion int
			if err := rows.Scan(&id, &sourceCollectorID, &eventType, &rawStart, &deviceUTC, &receivedUTC, &offsetMS, &errorMS, &payload, &schemaVersion); err != nil {
				rows.Close()
				return wrap(CodeQueryFailed, "scan raw event timeline source", err)
			}
			projection, err := rawEventProjection(id, sourceCollectorID, eventType, rawStart, deviceUTC, receivedUTC, offsetMS, errorMS, payload, schemaVersion, uncertainAfter)
			if err != nil {
				rows.Close()
				return err
			}
			matches, err := projectionMatchesRange(projection, start, end, includeOverlapping)
			if err != nil {
				rows.Close()
				return err
			}
			if !matches {
				continue
			}
			if includeOverlapping && !projectionHasPositiveInterval(projection) {
				continue
			}
			if err := appendTimelineProjection(target, projection, maxFacts, projectionBytes, store.maxTimelineProjectionBytes); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return classifySQLiteError(CodeQueryFailed, "iterate raw event timeline sources", err)
		}
		if err := rows.Close(); err != nil {
			return wrap(CodeQueryFailed, "close raw event timeline sources", err)
		}
	}
	return nil
}

func projectionHasPositiveInterval(item timelineProjection) bool {
	if item.CorrectedEndUTC == "" {
		return false
	}
	start, startErr := time.Parse(time.RFC3339Nano, item.CorrectedStartUTC)
	end, endErr := time.Parse(time.RFC3339Nano, item.CorrectedEndUTC)
	return startErr == nil && endErr == nil && start.Before(end)
}

func rawEventProjectionQueries(collectorID string, includeOverlapping bool, candidateStart, candidateEnd string) []projectionSourceQuery {
	queries := make([]projectionSourceQuery, 0, 2)
	if collectorID == "" {
		queries = append(queries, projectionSourceQuery{sql: rawEventProjectionColumns + ` INDEXED BY raw_events_corrected_start
WHERE ` + rawEventCorrectedStartExpression + ` >= julianday(?)
  AND ` + rawEventCorrectedStartExpression + ` < julianday(?)`, args: []any{candidateStart, candidateEnd}})
		if includeOverlapping {
			queries = append(queries, projectionSourceQuery{sql: rawEventProjectionColumns + ` INDEXED BY raw_events_activitywatch_corrected_end
WHERE ` + activityWatchIntervalSQL + `
  AND ` + rawEventCorrectedStartExpression + ` < julianday(?)
  AND ` + rawEventCorrectedEndExpression + ` > julianday(?)`, args: []any{candidateStart, candidateStart}})
		}
	} else {
		if includeOverlapping {
			// Coverage consumes interval evidence only. Generic point events remain
			// immutable raw evidence and are projected when the timeline is queried,
			// but cannot exhaust or obscure the coverage-specific fact budget.
			queries = append(queries, projectionSourceQuery{sql: rawEventProjectionColumns + ` INDEXED BY raw_events_collector_activitywatch_corrected_start
WHERE collector_id = ?
  AND ` + activityWatchIntervalSQL + `
  AND ` + rawEventCorrectedStartExpression + ` >= julianday(?)
  AND ` + rawEventCorrectedStartExpression + ` < julianday(?)`, args: []any{collectorID, candidateStart, candidateEnd}})
			queries = append(queries, projectionSourceQuery{sql: rawEventProjectionColumns + ` INDEXED BY raw_events_collector_activitywatch_corrected_end
WHERE collector_id = ?
  AND ` + activityWatchIntervalSQL + `
  AND ` + rawEventCorrectedStartExpression + ` < julianday(?)
  AND ` + rawEventCorrectedEndExpression + ` > julianday(?)`, args: []any{collectorID, candidateStart, candidateStart}})
		} else {
			queries = append(queries, projectionSourceQuery{sql: rawEventProjectionColumns + ` INDEXED BY raw_events_collector_corrected_start
WHERE collector_id = ?
  AND ` + rawEventCorrectedStartExpression + ` >= julianday(?)
  AND ` + rawEventCorrectedStartExpression + ` < julianday(?)`, args: []any{collectorID, candidateStart, candidateEnd}})
		}
	}
	return queries
}

func (store *Store) upsertTimelineProjectionChunk(ctx context.Context, projections []timelineProjection) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return classifySQLiteError(CodeWriteFailed, "begin timeline projection chunk", err)
	}
	defer tx.Rollback()
	for _, item := range projections {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO timeline_entries (
    stable_id, source_type, source_id, collector_id, event_type,
    device_start_raw, device_end_raw, device_start_utc, device_end_utc,
    received_at_utc, corrected_start_utc, corrected_end_utc,
    clock_offset_ms, clock_error_ms, clock_uncertain, quality_flags_json,
    payload_json, source_schema_version
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source_type, source_id) DO UPDATE SET
    stable_id = excluded.stable_id,
    collector_id = excluded.collector_id,
    event_type = excluded.event_type,
    device_start_raw = excluded.device_start_raw,
    device_end_raw = excluded.device_end_raw,
    device_start_utc = excluded.device_start_utc,
    device_end_utc = excluded.device_end_utc,
    received_at_utc = excluded.received_at_utc,
    corrected_start_utc = excluded.corrected_start_utc,
    corrected_end_utc = excluded.corrected_end_utc,
    clock_offset_ms = excluded.clock_offset_ms,
    clock_error_ms = excluded.clock_error_ms,
    clock_uncertain = excluded.clock_uncertain,
    quality_flags_json = excluded.quality_flags_json,
    payload_json = excluded.payload_json,
    source_schema_version = excluded.source_schema_version
WHERE timeline_entries.stable_id IS NOT excluded.stable_id
   OR timeline_entries.collector_id IS NOT excluded.collector_id
   OR timeline_entries.event_type IS NOT excluded.event_type
   OR timeline_entries.device_start_raw IS NOT excluded.device_start_raw
   OR timeline_entries.device_end_raw IS NOT excluded.device_end_raw
   OR timeline_entries.device_start_utc IS NOT excluded.device_start_utc
   OR timeline_entries.device_end_utc IS NOT excluded.device_end_utc
   OR timeline_entries.received_at_utc IS NOT excluded.received_at_utc
   OR timeline_entries.corrected_start_utc IS NOT excluded.corrected_start_utc
   OR timeline_entries.corrected_end_utc IS NOT excluded.corrected_end_utc
   OR timeline_entries.clock_offset_ms IS NOT excluded.clock_offset_ms
   OR timeline_entries.clock_error_ms IS NOT excluded.clock_error_ms
   OR timeline_entries.clock_uncertain IS NOT excluded.clock_uncertain
   OR timeline_entries.quality_flags_json IS NOT excluded.quality_flags_json
   OR timeline_entries.payload_json IS NOT excluded.payload_json
   OR timeline_entries.source_schema_version IS NOT excluded.source_schema_version`,
			item.StableID, item.SourceType, item.SourceID, item.CollectorID, item.EventType,
			item.DeviceStartRaw, nullableString(item.DeviceEndRaw), item.DeviceStartUTC, nullableString(item.DeviceEndUTC),
			item.ReceivedAtUTC, item.CorrectedStartUTC, nullableString(item.CorrectedEndUTC),
			item.ClockOffsetMS, item.ClockErrorMS, boolInt(item.ClockUncertain), item.QualityFlagsJSON,
			item.PayloadJSON, item.SourceSchemaVersion); err != nil {
			return classifySQLiteError(CodeWriteFailed, "insert timeline projection", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return classifySQLiteError(CodeWriteFailed, "commit timeline projection chunk", err)
	}
	return nil
}

func appendTimelineProjection(target *[]timelineProjection, item timelineProjection, maxFacts int, usedBytes *int, maxBytes int) error {
	if len(*target) >= maxFacts {
		return &Error{Code: CodeTimelineFactLimit, Err: errors.New("timeline range exceeds the configured projection fact limit")}
	}
	size := timelineProjectionSize(item)
	if size > maxBytes-*usedBytes {
		return &Error{Code: CodeTimelineByteLimit, Err: errors.New("timeline range exceeds the fixed projection memory budget")}
	}
	*target = append(*target, item)
	*usedBytes += size
	return nil
}

func timelineProjectionSize(item timelineProjection) int {
	return 512 + len(item.StableID) + len(item.SourceType) + len(item.CollectorID) + len(item.EventType) +
		len(item.DeviceStartRaw) + len(item.DeviceEndRaw) + len(item.DeviceStartUTC) + len(item.DeviceEndUTC) +
		len(item.ReceivedAtUTC) + len(item.CorrectedStartUTC) + len(item.CorrectedEndUTC) +
		len(item.QualityFlagsJSON) + len(item.PayloadJSON)
}

func rawEventProjection(id int64, collectorID, eventType, rawStart, deviceUTC, receivedUTC string, offsetMS, errorMS int64, payload string, schemaVersion int, uncertainAfter time.Duration) (timelineProjection, error) {
	deviceTime, err := time.Parse(time.RFC3339Nano, deviceUTC)
	if err != nil {
		return timelineProjection{}, wrap(CodeQueryFailed, "parse raw event device time", err)
	}
	received, err := time.Parse(time.RFC3339Nano, receivedUTC)
	if err != nil {
		return timelineProjection{}, wrap(CodeQueryFailed, "parse raw event received time", err)
	}
	offset := time.Duration(offsetMS) * time.Millisecond
	projection := timelineProjection{
		StableID: fmt.Sprintf("event:%d", id), SourceType: "raw_event", SourceID: id,
		CollectorID: collectorID, EventType: eventType, DeviceStartRaw: rawStart,
		DeviceStartUTC: deviceTime.UTC().Format(time.RFC3339Nano), ReceivedAtUTC: fixedUTC(received),
		CorrectedStartUTC: fixedUTC(deviceTime.Add(offset)), ClockOffsetMS: offsetMS, ClockErrorMS: errorMS,
		ClockUncertain:   time.Duration(errorMS)*time.Millisecond > uncertainAfter,
		QualityFlagsJSON: qualityJSON(nil, time.Duration(errorMS)*time.Millisecond > uncertainAfter),
		PayloadJSON:      payload, SourceSchemaVersion: schemaVersion,
	}
	if eventType == "activitywatch.event" {
		durationSeconds, valid := activityWatchProjectionDuration(payload)
		if !valid {
			// M1 intentionally accepts generic event types. A generic producer may
			// therefore use the ActivityWatch event name with an incompatible
			// payload. Preserve that immutable evidence as an incomplete point fact
			// instead of allowing one bad source to poison the whole projection.
			projection.QualityFlagsJSON = qualityJSON([]string{"incomplete"}, projection.ClockUncertain)
			return projection, nil
		}
		end := deviceTime.Add(time.Duration(durationSeconds * float64(time.Second)))
		projection.DeviceEndUTC = end.UTC().Format(time.RFC3339Nano)
		projection.DeviceEndRaw = end.Format(time.RFC3339Nano)
		projection.CorrectedEndUTC = fixedUTC(end.Add(offset))
	}
	return projection, nil
}

func activityWatchProjectionDuration(payload string) (float64, bool) {
	var aw struct {
		BucketID        string          `json:"bucket_id"`
		BucketType      string          `json:"bucket_type"`
		SourceEventID   *int64          `json:"source_event_id"`
		DurationSeconds *float64        `json:"duration_seconds"`
		Data            json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(payload), &aw); err != nil || !validIdentifier(aw.BucketID, 256) || !validIdentifier(aw.BucketType, 128) || aw.SourceEventID == nil || *aw.SourceEventID < 0 || aw.DurationSeconds == nil || math.IsNaN(*aw.DurationSeconds) || math.IsInf(*aw.DurationSeconds, 0) || *aw.DurationSeconds < 0 || *aw.DurationSeconds > 366*24*60*60 {
		return 0, false
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(aw.Data, &data); err != nil || data == nil {
		return 0, false
	}
	return *aw.DurationSeconds, true
}

const heartbeatProjectionColumns = `
SELECT id, collector_id, state, device_start_raw, device_end_raw, device_start_utc, device_end_utc,
       received_at_utc, corrected_start_utc, corrected_end_utc, clock_offset_ms, clock_error_ms,
       quality_flags_json, schema_version
FROM collector_heartbeats`

func (store *Store) collectHeartbeatProjections(ctx context.Context, start, end time.Time, uncertainAfter time.Duration, maxFacts int, collectorID string, includeOverlapping bool, target *[]timelineProjection, projectionBytes *int) error {
	startText, endText := fixedUTC(start), fixedUTC(end)
	queries := heartbeatProjectionQueries(collectorID, includeOverlapping, startText, endText)
	for _, query := range queries {
		rows, err := store.db.QueryContext(ctx, query.sql, query.args...)
		if err != nil {
			return classifySQLiteError(CodeQueryFailed, "read heartbeat timeline sources", err)
		}
		for rows.Next() {
			var item timelineProjection
			var state, flags string
			if err := rows.Scan(&item.SourceID, &item.CollectorID, &state, &item.DeviceStartRaw, &item.DeviceEndRaw,
				&item.DeviceStartUTC, &item.DeviceEndUTC, &item.ReceivedAtUTC, &item.CorrectedStartUTC, &item.CorrectedEndUTC,
				&item.ClockOffsetMS, &item.ClockErrorMS, &flags, &item.SourceSchemaVersion); err != nil {
				rows.Close()
				return wrap(CodeQueryFailed, "scan heartbeat timeline source", err)
			}
			item.StableID = fmt.Sprintf("heartbeat:%d", item.SourceID)
			item.SourceType = "heartbeat"
			item.EventType = "collector.heartbeat"
			item.ClockUncertain = time.Duration(item.ClockErrorMS)*time.Millisecond > uncertainAfter
			var quality []string
			if err := json.Unmarshal([]byte(flags), &quality); err != nil {
				rows.Close()
				return wrap(CodeQueryFailed, "decode heartbeat quality flags", err)
			}
			item.QualityFlagsJSON = qualityJSON(quality, item.ClockUncertain)
			payload, _ := json.Marshal(map[string]string{"state": state})
			item.PayloadJSON = string(payload)
			if err := appendTimelineProjection(target, item, maxFacts, projectionBytes, store.maxTimelineProjectionBytes); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return classifySQLiteError(CodeQueryFailed, "iterate heartbeat timeline sources", err)
		}
		if err := rows.Close(); err != nil {
			return wrap(CodeQueryFailed, "close heartbeat timeline sources", err)
		}
	}
	return nil
}

func heartbeatProjectionQueries(collectorID string, includeOverlapping bool, startText, endText string) []projectionSourceQuery {
	queries := make([]projectionSourceQuery, 0, 2)
	if collectorID == "" {
		queries = append(queries, projectionSourceQuery{sql: heartbeatProjectionColumns + ` INDEXED BY collector_heartbeats_time
WHERE corrected_start_utc >= ? AND corrected_start_utc < ?`, args: []any{startText, endText}})
		if includeOverlapping {
			queries = append(queries, projectionSourceQuery{sql: heartbeatProjectionColumns + ` INDEXED BY collector_heartbeats_end
WHERE corrected_start_utc < ? AND corrected_end_utc > ?`, args: []any{startText, startText}})
		}
	} else {
		queries = append(queries, projectionSourceQuery{sql: heartbeatProjectionColumns + ` INDEXED BY collector_heartbeats_collector_time
WHERE collector_id = ? AND corrected_start_utc >= ? AND corrected_start_utc < ?`, args: []any{collectorID, startText, endText}})
		if includeOverlapping {
			queries = append(queries, projectionSourceQuery{sql: heartbeatProjectionColumns + ` INDEXED BY collector_heartbeats_collector_end
WHERE collector_id = ? AND corrected_start_utc < ? AND corrected_end_utc > ?`, args: []any{collectorID, startText, startText}})
		}
	}
	return queries
}

const mediaProjectionColumns = `
SELECT id, collector_id, managed_relative_path, device_start_raw, device_end_raw, device_start_utc, device_end_utc,
       received_at_utc, clock_offset_ms, clock_error_ms, size_bytes, duration_ms, codec_name, format_name,
       media_type, sha256, sidecar_schema_version
FROM media_segments`

const mediaCorrectedStartExpression = `(julianday(device_start_utc) + (clock_offset_ms / 86400000.0))`

const mediaCorrectedEndExpression = `(julianday(device_end_utc) + (clock_offset_ms / 86400000.0))`

func (store *Store) collectMediaProjections(ctx context.Context, start, end time.Time, uncertainAfter time.Duration, maxFacts int, collectorID string, includeOverlapping bool, target *[]timelineProjection, projectionBytes *int) error {
	candidateStart := start.UTC().Add(-time.Second).Format(time.RFC3339Nano)
	candidateEnd := end.UTC().Add(time.Second).Format(time.RFC3339Nano)
	queries := mediaProjectionQueries(collectorID, includeOverlapping, candidateStart, candidateEnd)
	for _, query := range queries {
		rows, err := store.db.QueryContext(ctx, query.sql, query.args...)
		if err != nil {
			return classifySQLiteError(CodeQueryFailed, "read media timeline sources", err)
		}
		for rows.Next() {
			var item timelineProjection
			var managedPath, codec, format, mediaType, sha string
			var sizeBytes, durationMS int64
			if err := rows.Scan(&item.SourceID, &item.CollectorID, &managedPath, &item.DeviceStartRaw, &item.DeviceEndRaw,
				&item.DeviceStartUTC, &item.DeviceEndUTC, &item.ReceivedAtUTC, &item.ClockOffsetMS, &item.ClockErrorMS,
				&sizeBytes, &durationMS, &codec, &format, &mediaType, &sha, &item.SourceSchemaVersion); err != nil {
				rows.Close()
				return wrap(CodeQueryFailed, "scan media timeline source", err)
			}
			startTime, startErr := time.Parse(time.RFC3339Nano, item.DeviceStartUTC)
			endTime, endErr := time.Parse(time.RFC3339Nano, item.DeviceEndUTC)
			received, receivedErr := time.Parse(time.RFC3339Nano, item.ReceivedAtUTC)
			if startErr != nil || endErr != nil || receivedErr != nil {
				rows.Close()
				return &Error{Code: CodeQueryFailed, Err: errors.New("stored media time is invalid")}
			}
			offset := time.Duration(item.ClockOffsetMS) * time.Millisecond
			item.StableID = fmt.Sprintf("media:%d", item.SourceID)
			item.SourceType = "media_segment"
			item.EventType = "media.segment"
			item.ReceivedAtUTC = fixedUTC(received)
			item.CorrectedStartUTC = fixedUTC(startTime.Add(offset))
			item.CorrectedEndUTC = fixedUTC(endTime.Add(offset))
			item.ClockUncertain = time.Duration(item.ClockErrorMS)*time.Millisecond > uncertainAfter
			item.QualityFlagsJSON = qualityJSON(nil, item.ClockUncertain)
			payload, _ := json.Marshal(map[string]any{"managed_relative_path": managedPath, "size_bytes": sizeBytes, "duration_ms": durationMS, "codec_name": codec, "format_name": format, "media_type": mediaType, "sha256": sha})
			item.PayloadJSON = string(payload)
			matches, err := projectionMatchesRange(item, start, end, includeOverlapping)
			if err != nil {
				rows.Close()
				return err
			}
			if !matches {
				continue
			}
			if err := appendTimelineProjection(target, item, maxFacts, projectionBytes, store.maxTimelineProjectionBytes); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return classifySQLiteError(CodeQueryFailed, "iterate media timeline sources", err)
		}
		if err := rows.Close(); err != nil {
			return wrap(CodeQueryFailed, "close media timeline sources", err)
		}
	}
	return nil
}

func mediaProjectionQueries(collectorID string, includeOverlapping bool, candidateStart, candidateEnd string) []projectionSourceQuery {
	queries := make([]projectionSourceQuery, 0, 2)
	if collectorID == "" {
		queries = append(queries, projectionSourceQuery{sql: mediaProjectionColumns + ` INDEXED BY media_segments_corrected_start
WHERE ` + mediaCorrectedStartExpression + ` >= julianday(?)
  AND ` + mediaCorrectedStartExpression + ` < julianday(?)`, args: []any{candidateStart, candidateEnd}})
		if includeOverlapping {
			queries = append(queries, projectionSourceQuery{sql: mediaProjectionColumns + ` INDEXED BY media_segments_corrected_end
WHERE ` + mediaCorrectedStartExpression + ` < julianday(?)
  AND ` + mediaCorrectedEndExpression + ` > julianday(?)`, args: []any{candidateStart, candidateStart}})
		}
	} else {
		queries = append(queries, projectionSourceQuery{sql: mediaProjectionColumns + ` INDEXED BY media_segments_collector_corrected_start
WHERE collector_id = ?
  AND ` + mediaCorrectedStartExpression + ` >= julianday(?)
  AND ` + mediaCorrectedStartExpression + ` < julianday(?)`, args: []any{collectorID, candidateStart, candidateEnd}})
		if includeOverlapping {
			queries = append(queries, projectionSourceQuery{sql: mediaProjectionColumns + ` INDEXED BY media_segments_collector_corrected_end
WHERE collector_id = ?
  AND ` + mediaCorrectedStartExpression + ` < julianday(?)
  AND ` + mediaCorrectedEndExpression + ` > julianday(?)`, args: []any{collectorID, candidateStart, candidateStart}})
		}
	}
	return queries
}

func projectionMatchesRange(item timelineProjection, start, end time.Time, includeOverlapping bool) (bool, error) {
	correctedStart, err := time.Parse(time.RFC3339Nano, item.CorrectedStartUTC)
	if err != nil {
		return false, wrap(CodeQueryFailed, "parse corrected projection start", err)
	}
	if !correctedStart.Before(end) {
		return false, nil
	}
	if !correctedStart.Before(start) {
		return true, nil
	}
	if !includeOverlapping || item.CorrectedEndUTC == "" {
		return false, nil
	}
	correctedEnd, err := time.Parse(time.RFC3339Nano, item.CorrectedEndUTC)
	if err != nil {
		return false, wrap(CodeQueryFailed, "parse corrected projection end", err)
	}
	return correctedEnd.After(start), nil
}

func qualityJSON(flags []string, clockUncertain bool) string {
	seen := make(map[string]bool, len(flags)+1)
	for _, flag := range flags {
		seen[flag] = true
	}
	if clockUncertain {
		seen["clock_uncertain"] = true
	}
	result := make([]string, 0, len(seen))
	for flag := range seen {
		result = append(result, flag)
	}
	sort.Strings(result)
	raw, _ := json.Marshal(result)
	return string(raw)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func encodeTimelineCursor(cursor timelineCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeTimelineCursor(value string) (timelineCursor, error) {
	if len(value) > 2048 {
		return timelineCursor{}, &Error{Code: CodeTimelineCursor, Err: errors.New("timeline cursor is too large")}
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return timelineCursor{}, &Error{Code: CodeTimelineCursor, Err: errors.New("timeline cursor is not valid base64url")}
	}
	var cursor timelineCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || ensureJSONEOF(decoder) != nil || cursor.Version != 1 || cursor.SnapshotID < 0 || cursor.SourceID < 0 || cursor.ProjectionID < 0 || cursor.ProjectionID > cursor.SnapshotID {
		return timelineCursor{}, &Error{Code: CodeTimelineCursor, Err: errors.New("timeline cursor is invalid")}
	}
	start, err := time.Parse(fixedUTCLayout, cursor.RangeStartUTC)
	if err != nil {
		return timelineCursor{}, &Error{Code: CodeTimelineCursor, Err: errors.New("timeline cursor start is invalid")}
	}
	end, err := time.Parse(fixedUTCLayout, cursor.RangeEndUTC)
	if err != nil || !start.Before(end) {
		return timelineCursor{}, &Error{Code: CodeTimelineCursor, Err: errors.New("timeline cursor end is invalid")}
	}
	if cursor.ProjectionID > 0 {
		if cursor.SourceID <= 0 || (cursor.SourceType != "raw_event" && cursor.SourceType != "heartbeat" && cursor.SourceType != "media_segment") {
			return timelineCursor{}, &Error{Code: CodeTimelineCursor, Err: errors.New("timeline cursor source identity is invalid")}
		}
		if _, err := time.Parse(fixedUTCLayout, cursor.CorrectedUTC); err != nil {
			return timelineCursor{}, &Error{Code: CodeTimelineCursor, Err: errors.New("timeline cursor corrected time is invalid")}
		}
		if _, err := time.Parse(fixedUTCLayout, cursor.ReceivedUTC); err != nil {
			return timelineCursor{}, &Error{Code: CodeTimelineCursor, Err: errors.New("timeline cursor received time is invalid")}
		}
	}
	return cursor, nil
}
