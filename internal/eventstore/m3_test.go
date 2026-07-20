package eventstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Versifine/study-monitor/internal/config"
)

func TestHeartbeatFactsAreAppendOnlyIdempotentAndIndependentlyRejected(t *testing.T) {
	store := openM3TestStore(t, func() time.Time { return time.Date(2026, 7, 20, 0, 2, 0, 0, time.UTC) })
	defer store.Close()
	valid := heartbeatJSON("generic.one", "hb-1", HeartbeatStateIdle, "2026-07-20T08:00:00+08:00", "2026-07-20T08:01:00+08:00", 0, 25, []string{"incomplete"})
	unknown := heartbeatJSON("missing", "hb-x", HeartbeatStateActive, "2026-07-20T00:00:00Z", "2026-07-20T00:01:00Z", 0, 0, nil)
	results, err := store.AppendHeartbeatBatch(context.Background(), []HeartbeatCandidate{{Raw: valid}, {Raw: unknown}, {Raw: valid}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || results[0].Status != StatusAccepted || results[1].Status != StatusRejected || results[1].ErrorCode != CodeHeartbeatCollector || results[2].Status != StatusDuplicate || results[2].HeartbeatID != results[0].HeartbeatID {
		t.Fatalf("unexpected heartbeat results: %#v", results)
	}
	conflict := heartbeatJSON("generic.one", "hb-1", HeartbeatStateActive, "2026-07-20T08:00:00+08:00", "2026-07-20T08:01:00+08:00", 0, 25, []string{"incomplete"})
	conflictResult, err := store.AppendHeartbeatBatch(context.Background(), []HeartbeatCandidate{{Raw: conflict}})
	if err != nil || conflictResult[0].Status != StatusConflict || conflictResult[0].ErrorCode != CodeHeartbeatConflict {
		t.Fatalf("conflict = %#v, %v", conflictResult, err)
	}
	if _, err := store.db.Exec("UPDATE collector_heartbeats SET state = 'active' WHERE id = ?", results[0].HeartbeatID); err == nil {
		t.Fatal("heartbeat UPDATE unexpectedly succeeded")
	}
	if _, err := store.db.Exec("DELETE FROM collector_heartbeats WHERE id = ?", results[0].HeartbeatID); err == nil {
		t.Fatal("heartbeat DELETE unexpectedly succeeded")
	}
	missingFlags := json.RawMessage(`{"schema_version":1,"collector_id":"generic.one","state":"active","device_start_raw":"2026-07-20T00:00:00Z","device_end_raw":"2026-07-20T00:01:00Z","clock_offset_ms":0,"clock_error_ms":0,"idempotency_key":"missing-flags"}`)
	missingResult, err := store.AppendHeartbeatBatch(context.Background(), []HeartbeatCandidate{{Raw: missingFlags}})
	if err != nil || missingResult[0].Status != StatusRejected || missingResult[0].ErrorCode != CodeHeartbeatQuality {
		t.Fatalf("missing quality_flags=%#v err=%v", missingResult, err)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM collector_heartbeats").Scan(&count); err != nil || count != 1 {
		t.Fatalf("heartbeat count=%d err=%v", count, err)
	}
}

func TestActivityWatchCheckpointOnlyMovesForwardAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m3.db")
	store := openM3TestStoreAt(t, path, func() time.Time { return time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC) })
	ctx := context.Background()
	first := ActivityWatchCheckpoint{CollectorID: "aw.afk", BucketID: "bucket", SourceTimeUTC: "2026-07-20T00:00:00.000000000Z", SourceEventID: 10}
	if err := store.SaveActivityWatchCheckpoint(ctx, first); err != nil {
		t.Fatal(err)
	}
	older := first
	older.SourceEventID = 9
	if err := store.SaveActivityWatchCheckpoint(ctx, older); err != nil {
		t.Fatal(err)
	}
	loaded, exists, err := store.LoadActivityWatchCheckpoint(ctx, "aw.afk", "bucket")
	if err != nil || !exists || loaded.SourceEventID != 10 {
		t.Fatalf("checkpoint=%#v exists=%v err=%v", loaded, exists, err)
	}
	store.Close()
	store = openM3TestStoreAt(t, path, func() time.Time { return time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC) })
	defer store.Close()
	loaded, exists, err = store.LoadActivityWatchCheckpoint(ctx, "aw.afk", "bucket")
	if err != nil || !exists || loaded.SourceEventID != 10 {
		t.Fatalf("restart checkpoint=%#v exists=%v err=%v", loaded, exists, err)
	}
	if _, _, err := store.LoadActivityWatchCheckpoint(ctx, "aw.afk", "different"); ErrorCode(err) != CodeCheckpointConflict {
		t.Fatalf("bucket mismatch code=%q err=%v", ErrorCode(err), err)
	}
}

func TestTimelinePreservesClockEvidenceDSTAndStableSnapshotPagination(t *testing.T) {
	now := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)
	store := openM3TestStore(t, func() time.Time { return now })
	defer store.Close()
	first := m3Event("generic.one", "dst-edt", "2026-11-01T01:30:00-04:00", 0, 5000, `{ "side": "EDT" }`)
	second := m3Event("generic.one", "dst-est", "2026-11-01T01:30:00-05:00", -1000, 25, `{"side":"EST"}`)
	results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: first}, {Raw: second}})
	if err != nil || results[0].Status != StatusAccepted || results[1].Status != StatusAccepted {
		t.Fatalf("append=%#v err=%v", results, err)
	}
	start := time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)
	page, err := store.QueryTimeline(context.Background(), start, end, "", 1, time.Second, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.NextCursor == "" || page.Entries[0].DeviceStartRaw != "2026-11-01T01:30:00-04:00" || page.Entries[0].DeviceStartUTC != "2026-11-01T05:30:00Z" || page.Entries[0].CorrectedStartUTC != "2026-11-01T05:30:00.000000000Z" || !page.Entries[0].ClockUncertain || len(page.Entries[0].QualityFlags) != 1 || page.Entries[0].QualityFlags[0] != "clock_uncertain" {
		t.Fatalf("first timeline page=%#v", page)
	}
	now = now.Add(time.Minute)
	late := m3Event("generic.one", "new-after-snapshot", "2026-11-01T01:45:00-04:00", 0, 0, `{}`)
	if result, err := store.AppendBatch(context.Background(), []Candidate{{Raw: late}}); err != nil || result[0].Status != StatusAccepted {
		t.Fatalf("late append=%#v err=%v", result, err)
	}
	secondPage, err := store.QueryTimeline(context.Background(), start, end, page.NextCursor, 2, time.Second, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Entries) != 1 || secondPage.Entries[0].DeviceStartRaw != "2026-11-01T01:30:00-05:00" || secondPage.Entries[0].CorrectedStartUTC != "2026-11-01T06:29:59.000000000Z" {
		t.Fatalf("snapshot leaked new fact or clock correction is wrong: %#v", secondPage)
	}
}

func TestTimelineOrdersForwardAndBackwardClockCorrectionsDeterministically(t *testing.T) {
	store := openM3TestStore(t, func() time.Time { return time.Date(2026, 7, 20, 12, 5, 0, 0, time.UTC) })
	defer store.Close()
	device := "2026-07-20T12:00:00Z"
	forward := m3Event("generic.one", "clock-forward", device, 60000, 10, `{"jump":"forward"}`)
	backward := m3Event("generic.one", "clock-backward", device, -60000, 10, `{"jump":"backward"}`)
	if results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: forward}, {Raw: backward}}); err != nil || results[0].Status != StatusAccepted || results[1].Status != StatusAccepted {
		t.Fatalf("append clock jumps=%#v err=%v", results, err)
	}
	page, err := store.QueryTimeline(context.Background(), time.Date(2026, 7, 20, 11, 58, 0, 0, time.UTC), time.Date(2026, 7, 20, 12, 2, 0, 0, time.UTC), "", 10, time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 2 || page.Entries[0].CorrectedStartUTC != "2026-07-20T11:59:00.000000000Z" || page.Entries[1].CorrectedStartUTC != "2026-07-20T12:01:00.000000000Z" || page.Entries[0].ClockOffsetMS != -60000 || page.Entries[1].ClockOffsetMS != 60000 {
		t.Fatalf("clock correction ordering=%#v", page.Entries)
	}
}

func TestTimelineProjectionByteBudgetFailsBoundedlyAndLeavesWritesAvailable(t *testing.T) {
	store := openM3TestStore(t, func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) })
	defer store.Close()
	store.maxTimelineProjectionBytes = 1024
	large := m3Event("generic.one", "large-projection", "2026-07-20T00:05:00Z", 0, 0, `{"blob":"`+strings.Repeat("x", 2048)+`"}`)
	if results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: large}}); err != nil || results[0].Status != StatusAccepted {
		t.Fatalf("append large fact=%#v err=%v", results, err)
	}
	_, err := store.QueryTimeline(context.Background(), time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC), "", 10, time.Second, 100)
	if ErrorCode(err) != CodeTimelineByteLimit {
		t.Fatalf("projection byte limit error=%v code=%q", err, ErrorCode(err))
	}
	after := m3Event("generic.one", "write-after-byte-limit", "2026-07-20T00:06:00Z", 0, 0, `{}`)
	if results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: after}}); err != nil || results[0].Status != StatusAccepted {
		t.Fatalf("projection byte limit blocked evidence writes=%#v err=%v", results, err)
	}
}

func TestProjectionBoundaryPrefilterDoesNotConsumeFactLimitOrHideExactMatches(t *testing.T) {
	start := time.Date(2026, 7, 20, 0, 5, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	store := openM3TestStore(t, func() time.Time { return end })
	defer store.Close()
	collector := m3Collector("generic.one", config.CollectorGenericJSON)
	candidates := make([]Candidate, 0, 102)
	outside := start.Add(-collector.OfflineAfterDuration()).Add(-500 * time.Millisecond).Format(time.RFC3339Nano)
	for index := 0; index < 101; index++ {
		candidates = append(candidates, Candidate{Raw: m3Event("generic.one", fmt.Sprintf("outside-boundary-%03d", index), outside, 0, 0, `{}`)})
	}
	candidates = append(candidates, Candidate{Raw: m3Event("generic.one", "inside-after-boundary-burst", start.Add(time.Second).Format(time.RFC3339Nano), 0, 0, `{}`)})
	for offset := 0; offset < len(candidates); offset += 100 {
		batchEnd := offset + 100
		if batchEnd > len(candidates) {
			batchEnd = len(candidates)
		}
		results, err := store.AppendBatch(context.Background(), candidates[offset:batchEnd])
		if err != nil {
			t.Fatal(err)
		}
		for _, result := range results {
			if result.Status != StatusAccepted {
				t.Fatalf("boundary fixture append result=%#v", result)
			}
		}
	}
	page, err := store.QueryTimeline(context.Background(), start, end, "", 10, time.Second, 100)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].StableID == "" || !bytes.Contains(page.Entries[0].Payload, []byte("{}")) {
		t.Fatalf("boundary false positives consumed limit or hid exact match: page=%#v err=%v code=%q", page, err, ErrorCode(err))
	}
	projection, err := store.RebuildCoverage(context.Background(), []config.CollectorConfig{collector}, start, end, end, time.Second, 100)
	if err != nil || len(projection.Projections) != 1 || projection.Projections[0].Status != "fresh" {
		t.Fatalf("boundary false positives staled coverage: projection=%#v err=%v code=%q", projection, err, ErrorCode(err))
	}
}

func TestCoverageBudgetIgnoresGenericPointNoiseAndKeepsHeartbeatEvidence(t *testing.T) {
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	store := openM3TestStore(t, func() time.Time { return end })
	defer store.Close()
	candidates := make([]Candidate, 0, 101)
	for index := 0; index < 101; index++ {
		at := start.Add(time.Duration(index+1) * time.Second).Format(time.RFC3339Nano)
		candidates = append(candidates, Candidate{Raw: m3Event("generic.one", fmt.Sprintf("coverage-point-noise-%03d", index), at, 0, 0, `{}`)})
	}
	for offset := 0; offset < len(candidates); offset += 100 {
		batchEnd := offset + 100
		if batchEnd > len(candidates) {
			batchEnd = len(candidates)
		}
		results, err := store.AppendBatch(context.Background(), candidates[offset:batchEnd])
		if err != nil {
			t.Fatal(err)
		}
		for _, result := range results {
			if result.Status != StatusAccepted {
				t.Fatalf("generic point fixture result=%#v", result)
			}
		}
	}
	heartbeat := heartbeatJSON("generic.one", "coverage-signal", HeartbeatStateActive, "2026-07-20T00:05:00Z", "2026-07-20T00:06:00Z", 0, 0, nil)
	if results, err := store.AppendHeartbeatBatch(context.Background(), []HeartbeatCandidate{{Raw: heartbeat}}); err != nil || results[0].Status != StatusAccepted {
		t.Fatalf("heartbeat fixture=%#v err=%v", results, err)
	}
	projection, err := store.RebuildCoverage(context.Background(), []config.CollectorConfig{m3Collector("generic.one", config.CollectorGenericJSON)}, start, end, end, time.Second, 100)
	if err != nil || len(projection.Projections) != 1 || projection.Projections[0].Status != "fresh" {
		t.Fatalf("generic point noise exhausted coverage budget: projection=%#v err=%v code=%q", projection, err, ErrorCode(err))
	}
	covered := false
	for _, interval := range projection.Intervals {
		if interval.Availability == "covered" && interval.StartUTC == "2026-07-20T00:05:00.000000000Z" && interval.EndUTC == "2026-07-20T00:06:00.000000000Z" {
			covered = true
		}
	}
	if !covered {
		t.Fatalf("heartbeat evidence was hidden by generic point noise: %#v", projection.Intervals)
	}
}

func TestProjectionSourceQueriesUseM3RangeIndexes(t *testing.T) {
	store := openM3TestStore(t, func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) })
	defer store.Close()
	start := "2026-07-20T00:00:00.000000000Z"
	end := "2026-07-20T00:10:00.000000000Z"
	queries := make([]projectionSourceQuery, 0, 9)
	queries = append(queries, rawEventProjectionQueries("", false, start, end)...)
	queries = append(queries, rawEventProjectionQueries("generic.one", true, start, end)...)
	queries = append(queries, heartbeatProjectionQueries("", false, start, end)...)
	queries = append(queries, heartbeatProjectionQueries("generic.one", true, start, end)...)
	queries = append(queries, mediaProjectionQueries("", false, start, end)...)
	queries = append(queries, mediaProjectionQueries("media.screen", true, start, end)...)
	if len(queries) != 9 {
		t.Fatalf("projection source query count=%d, want 9", len(queries))
	}
	for _, query := range queries {
		marker := "INDEXED BY "
		indexOffset := strings.Index(query.sql, marker)
		if indexOffset < 0 {
			t.Fatalf("projection query has no forced range index: %s", query.sql)
		}
		expectedIndex := strings.Fields(query.sql[indexOffset+len(marker):])[0]
		rows, err := store.db.Query("EXPLAIN QUERY PLAN "+query.sql, query.args...)
		if err != nil {
			t.Fatalf("explain projection query using %s: %v\n%s", expectedIndex, err, query.sql)
		}
		details := make([]string, 0)
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		plan := strings.Join(details, " | ")
		if !strings.Contains(plan, "USING INDEX "+expectedIndex) || strings.Contains(plan, "SCAN raw_events") || strings.Contains(plan, "SCAN collector_heartbeats") || strings.Contains(plan, "SCAN media_segments") {
			t.Fatalf("projection source query did not use bounded range index %s: %s", expectedIndex, plan)
		}
	}
}

func TestTimelineResyncDoesNotUpdateUnchangedProjectionRows(t *testing.T) {
	store := openM3TestStore(t, func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) })
	defer store.Close()
	raw := m3Event("generic.one", "unchanged-projection", "2026-07-20T00:05:00Z", 0, 0, `{}`)
	if results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: raw}}); err != nil || results[0].Status != StatusAccepted {
		t.Fatalf("append unchanged fact=%#v err=%v", results, err)
	}
	start, end := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC)
	if _, err := store.QueryTimeline(context.Background(), start, end, "", 10, time.Second, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_unchanged_timeline_update BEFORE UPDATE ON timeline_entries BEGIN SELECT RAISE(ABORT, 'unchanged projection updated'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueryTimeline(context.Background(), start, end, "", 10, time.Second, 100); err != nil {
		t.Fatalf("unchanged projection attempted an UPDATE: %v", err)
	}
}

func TestProjectionGateContentionDoesNotPersistCollectorStale(t *testing.T) {
	store := openM3TestStore(t, func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) })
	defer store.Close()
	store.timelineSync <- struct{}{}
	defer func() { <-store.timelineSync }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := store.RebuildCoverage(ctx, []config.CollectorConfig{m3Collector("generic.one", config.CollectorGenericJSON)}, time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC), time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC), time.Second, 100)
	if ErrorCode(err) != CodeCanceled {
		t.Fatalf("projection contention error=%v code=%q", err, ErrorCode(err))
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM coverage_projection_state WHERE collector_id = 'generic.one'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("resource contention persisted stale state: count=%d err=%v", count, err)
	}
}

func TestZeroDurationActivityWatchPointDoesNotStaleCoverage(t *testing.T) {
	now := time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC)
	store := openM3TestStore(t, func() time.Time { return now })
	defer store.Close()
	raw := json.RawMessage(`{"schema_version":1,"collector_id":"aw.afk","event_type":"activitywatch.event","device_timestamp_raw":"2026-07-20T00:05:00Z","clock_offset_ms":0,"clock_error_ms":100,"idempotency_key":"aw-zero","payload":{"bucket_id":"aw-watcher-afk_host","bucket_type":"afkstatus","source_event_id":7,"duration_seconds":0,"data":{"status":"not-afk"}}}`)
	results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: raw}})
	if err != nil || len(results) != 1 || results[0].Status != StatusAccepted {
		t.Fatalf("append zero-duration ActivityWatch event=%#v err=%v", results, err)
	}
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	timeline, err := store.QueryTimeline(context.Background(), start, now, "", 10, time.Second, 100)
	if err != nil || len(timeline.Entries) != 1 || timeline.Entries[0].CorrectedStartUTC != timeline.Entries[0].CorrectedEndUTC {
		t.Fatalf("zero-duration point timeline=%#v err=%v", timeline, err)
	}
	projection, err := store.RebuildCoverage(context.Background(), []config.CollectorConfig{m3Collector("aw.afk", config.CollectorActivityWatch)}, start, now, now, time.Second, 100)
	if err != nil || len(projection.Projections) != 1 || projection.Projections[0].Status != "fresh" {
		t.Fatalf("zero-duration point staled coverage=%#v err=%v", projection, err)
	}
}

func TestZeroDurationActivityWatchBurstDoesNotExhaustCoverageBudget(t *testing.T) {
	now := time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC)
	store := openM3TestStore(t, func() time.Time { return now })
	defer store.Close()
	candidates := make([]Candidate, 0, 101)
	for index := 0; index < 101; index++ {
		raw := json.RawMessage(fmt.Sprintf(`{"schema_version":1,"collector_id":"aw.afk","event_type":"activitywatch.event","device_timestamp_raw":"2026-07-20T00:05:00Z","clock_offset_ms":0,"clock_error_ms":100,"idempotency_key":"aw-zero-burst-%03d","payload":{"bucket_id":"aw-watcher-afk_host","bucket_type":"afkstatus","source_event_id":%d,"duration_seconds":0,"data":{"status":"not-afk"}}}`, index, index+1000))
		candidates = append(candidates, Candidate{Raw: raw})
	}
	for offset := 0; offset < len(candidates); offset += 100 {
		batchEnd := offset + 100
		if batchEnd > len(candidates) {
			batchEnd = len(candidates)
		}
		results, err := store.AppendBatch(context.Background(), candidates[offset:batchEnd])
		if err != nil {
			t.Fatal(err)
		}
		for _, result := range results {
			if result.Status != StatusAccepted {
				t.Fatalf("zero-duration burst fixture result=%#v", result)
			}
		}
	}
	heartbeat := heartbeatJSON("aw.afk", "coverage-heartbeat-after-zero-burst", HeartbeatStateActive, "2026-07-20T00:06:00Z", "2026-07-20T00:07:00Z", 0, 0, nil)
	if results, err := store.AppendHeartbeatBatch(context.Background(), []HeartbeatCandidate{{Raw: heartbeat}}); err != nil || results[0].Status != StatusAccepted {
		t.Fatalf("heartbeat fixture=%#v err=%v", results, err)
	}
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	projection, err := store.RebuildCoverage(context.Background(), []config.CollectorConfig{m3Collector("aw.afk", config.CollectorActivityWatch)}, start, now, now, time.Second, 100)
	if err != nil || len(projection.Projections) != 1 || projection.Projections[0].Status != "fresh" {
		t.Fatalf("zero-duration burst exhausted coverage budget: projection=%#v err=%v code=%q", projection, err, ErrorCode(err))
	}
	if availabilityAt(projection.Intervals, time.Date(2026, 7, 20, 0, 6, 30, 0, time.UTC)) != "covered" {
		t.Fatalf("heartbeat interval was hidden by zero-duration burst: %#v", projection.Intervals)
	}
}

func TestMalformedGenericActivityWatchNameDegradesWithoutPoisoningTimeline(t *testing.T) {
	now := time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC)
	store := openM3TestStore(t, func() time.Time { return now })
	defer store.Close()
	malformed := json.RawMessage(`{"schema_version":1,"collector_id":"aw.afk","event_type":"activitywatch.event","device_timestamp_raw":"2026-07-20T00:05:00Z","clock_offset_ms":0,"clock_error_ms":100,"idempotency_key":"aw-malformed","payload":{"duration_seconds":"not-a-number"}}`)
	good := m3Event("generic.one", "good-after-malformed", "2026-07-20T00:06:00Z", 0, 0, `{}`)
	results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: malformed}, {Raw: good}})
	if err != nil || results[0].Status != StatusAccepted || results[1].Status != StatusAccepted {
		t.Fatalf("append mixed generic facts=%#v err=%v", results, err)
	}
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	timeline, err := store.QueryTimeline(context.Background(), start, now, "", 10, time.Second, 100)
	if err != nil || len(timeline.Entries) != 2 {
		t.Fatalf("malformed generic ActivityWatch name poisoned timeline=%#v err=%v", timeline, err)
	}
	if timeline.Entries[0].CorrectedEndUTC != "" || len(timeline.Entries[0].QualityFlags) != 1 || timeline.Entries[0].QualityFlags[0] != "incomplete" {
		t.Fatalf("malformed event was not preserved as an incomplete point: %#v", timeline.Entries[0])
	}
	coverage, err := store.RebuildCoverage(context.Background(), []config.CollectorConfig{
		m3Collector("aw.afk", config.CollectorActivityWatch),
		m3Collector("generic.one", config.CollectorGenericJSON),
	}, start, now, now, time.Second, 100)
	if err != nil || len(coverage.Projections) != 2 || coverage.Projections[0].Status != "fresh" || coverage.Projections[1].Status != "fresh" {
		t.Fatalf("malformed source poisoned coverage=%#v err=%v", coverage, err)
	}
}

func TestCoverageProjectionRebuildIsNonOverlappingAndLateFactsRepairGaps(t *testing.T) {
	now := time.Date(2026, 7, 20, 0, 12, 0, 0, time.UTC)
	store := openM3TestStore(t, func() time.Time { return now })
	defer store.Close()
	collector := m3Collector("generic.one", config.CollectorGenericJSON)
	idle := heartbeatJSON("generic.one", "idle", HeartbeatStateIdle, "2026-07-20T00:00:00Z", "2026-07-20T00:01:00Z", 0, 0, nil)
	active := heartbeatJSON("generic.one", "active", HeartbeatStateActive, "2026-07-20T00:10:00Z", "2026-07-20T00:11:00Z", 0, 0, nil)
	if result, err := store.AppendHeartbeatBatch(context.Background(), []HeartbeatCandidate{{Raw: idle}, {Raw: active}}); err != nil || result[0].Status != StatusAccepted || result[1].Status != StatusAccepted {
		t.Fatalf("append heartbeats=%#v err=%v", result, err)
	}
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(12 * time.Minute)
	first, err := store.RebuildCoverage(context.Background(), []config.CollectorConfig{collector}, start, end, now, time.Second, 100)
	if err != nil || len(first.Projections) != 1 || first.Projections[0].Status != "fresh" {
		t.Fatalf("first rebuild=%#v err=%v", first, err)
	}
	assertStoredCoverage(t, first.Intervals)
	if len(first.Intervals) > 0 {
		interval := first.Intervals[0]
		if _, err := store.db.Exec(`INSERT INTO coverage_intervals (collector_id, start_utc, end_utc, availability, quality_flags_json, reason_code, generation, built_at_utc) VALUES (?, ?, ?, 'unknown', '[]', 'OVERLAP_TEST', ?, ?)`, interval.CollectorID, interval.StartUTC, interval.EndUTC, interval.Generation, interval.BuiltAtUTC); err == nil {
			t.Fatal("database accepted an overlapping coverage interval")
		}
	}
	if availabilityAt(first.Intervals, start.Add(30*time.Second)) != "confirmed_idle" || availabilityAt(first.Intervals, start.Add(7*time.Minute)) != "offline" {
		t.Fatalf("idle/offline projection is wrong: %#v", first.Intervals)
	}
	var factCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM collector_heartbeats").Scan(&factCount); err != nil {
		t.Fatal(err)
	}
	second, err := store.RebuildCoverage(context.Background(), []config.CollectorConfig{collector}, start, end, now, time.Second, 100)
	if err != nil || second.Projections[0].Generation != first.Projections[0].Generation+1 {
		t.Fatalf("second rebuild=%#v err=%v", second, err)
	}
	var afterCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM collector_heartbeats").Scan(&afterCount); err != nil || afterCount != factCount {
		t.Fatalf("projection rebuild changed facts: before=%d after=%d err=%v", factCount, afterCount, err)
	}
	late := heartbeatJSON("generic.one", "late", HeartbeatStateActive, "2026-07-20T00:06:00Z", "2026-07-20T00:07:00Z", 0, 0, nil)
	if result, err := store.AppendHeartbeatBatch(context.Background(), []HeartbeatCandidate{{Raw: late}}); err != nil || result[0].Status != StatusAccepted {
		t.Fatalf("late heartbeat=%#v err=%v", result, err)
	}
	repaired, err := store.RebuildCoverage(context.Background(), []config.CollectorConfig{collector}, start, end, now, time.Second, 100)
	if err != nil || availabilityAt(repaired.Intervals, start.Add(6*time.Minute+30*time.Second)) != "covered" {
		t.Fatalf("late fact did not repair projection: %#v err=%v", repaired, err)
	}
}

func TestCoverageProjectionFailureIsIsolatedPerCollector(t *testing.T) {
	now := time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC)
	store := openM3TestStore(t, func() time.Time { return now })
	defer store.Close()
	good := m3Collector("generic.one", config.CollectorGenericJSON)
	bad := m3Collector("broken.projection", config.CollectorGenericJSON)
	bad.PlannedSchedule.Timezone = "Missing/Timezone"
	result, err := store.RebuildCoverage(context.Background(), []config.CollectorConfig{good, bad}, now.Add(-10*time.Minute), now, now, time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projections) != 2 || result.Projections[0].CollectorID != good.ID || result.Projections[0].Status != "fresh" || result.Projections[1].CollectorID != bad.ID || result.Projections[1].Status != "stale" || result.Projections[1].ErrorCode != CodeCoverageBuildFailed {
		t.Fatalf("projection isolation=%#v", result.Projections)
	}
	if readiness := store.Readiness(context.Background()); readiness.Status != ReadinessWritable {
		t.Fatalf("projection failure changed core readiness: %#v", readiness)
	}
}

func TestCoverageSyncLimitIsIsolatedPerCollector(t *testing.T) {
	now := time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC)
	store := openM3TestStore(t, func() time.Time { return now })
	defer store.Close()
	noisy := []Candidate{
		{Raw: m3Event("noisy.one", "noisy-1", "2026-07-20T00:01:00Z", 0, 0, `{}`)},
		{Raw: m3Event("noisy.one", "noisy-2", "2026-07-20T00:02:00Z", 0, 0, `{}`)},
		{Raw: m3Event("noisy.one", "noisy-3", "2026-07-20T00:03:00Z", 0, 0, `{}`)},
	}
	if results, err := store.AppendBatch(context.Background(), noisy); err != nil || results[0].Status != StatusAccepted || results[1].Status != StatusAccepted || results[2].Status != StatusAccepted {
		t.Fatalf("append noisy source=%#v err=%v", results, err)
	}
	goodHeartbeat := heartbeatJSON("generic.one", "good-isolated", HeartbeatStateActive, "2026-07-20T00:05:00Z", "2026-07-20T00:06:00Z", 0, 0, nil)
	if results, err := store.AppendHeartbeatBatch(context.Background(), []HeartbeatCandidate{{Raw: goodHeartbeat}}); err != nil || results[0].Status != StatusAccepted {
		t.Fatalf("append good heartbeat=%#v err=%v", results, err)
	}
	result, err := store.RebuildCoverage(context.Background(), []config.CollectorConfig{m3Collector("generic.one", config.CollectorGenericJSON)}, now.Add(-10*time.Minute), now, now, time.Second, 2)
	if err != nil || len(result.Projections) != 1 || result.Projections[0].Status != "fresh" {
		t.Fatalf("noisy collector blocked good collector=%#v err=%v", result, err)
	}
}

func TestCoverageSyncIncludesIntervalCrossingLookbackBoundary(t *testing.T) {
	now := time.Date(2026, 7, 20, 0, 22, 0, 0, time.UTC)
	store := openM3TestStore(t, func() time.Time { return now })
	defer store.Close()
	prior := heartbeatJSON("generic.one", "cross-left-boundary", HeartbeatStateActive, "2026-07-20T00:14:30Z", "2026-07-20T00:15:30Z", 0, 0, nil)
	if results, err := store.AppendHeartbeatBatch(context.Background(), []HeartbeatCandidate{{Raw: prior}}); err != nil || results[0].Status != StatusAccepted {
		t.Fatalf("append boundary heartbeat=%#v err=%v", results, err)
	}
	start := time.Date(2026, 7, 20, 0, 20, 0, 0, time.UTC)
	result, err := store.RebuildCoverage(context.Background(), []config.CollectorConfig{m3Collector("generic.one", config.CollectorGenericJSON)}, start, now, now, time.Second, 100)
	if err != nil || len(result.Projections) != 1 || result.Projections[0].Status != "fresh" {
		t.Fatalf("boundary interval rebuild=%#v err=%v", result, err)
	}
	if availabilityAt(result.Intervals, start.Add(15*time.Second)) != "delayed" || availabilityAt(result.Intervals, start.Add(45*time.Second)) != "offline" {
		t.Fatalf("cross-boundary interval did not set exact deadlines: %#v", result.Intervals)
	}
}

func TestM3MigrationKeepsPreviousM2SchemaPathCompatible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m3-compatible.db")
	store := openM3TestStoreAt(t, path, func() time.Time { return time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC) })
	if results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: m3Event("generic.one", "compatibility", "2026-07-20T00:00:00Z", 0, 0, `{}`)}}); err != nil || results[0].Status != StatusAccepted {
		t.Fatalf("append compatibility event=%#v err=%v", results, err)
	}
	store.Close()
	db := openRawDatabase(t, path)
	defer db.Close()
	coreMigrations, err := repositoryMigrations()
	if err != nil {
		t.Fatal(err)
	}
	mediaMigrations, err := repositoryMediaMigrations()
	if err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC) }
	if err := applyMigrations(context.Background(), db, coreMigrations, now); err != nil {
		t.Fatalf("previous core migrator rejected M3 database: %v", err)
	}
	if err := applyMediaMigrations(context.Background(), db, mediaMigrations, now); err != nil {
		t.Fatalf("previous M2 media migrator rejected M3 database: %v", err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM raw_events").Scan(&count); err != nil || count != 1 {
		t.Fatalf("previous M2 path could not read core facts: count=%d err=%v", count, err)
	}
}

func openM3TestStore(t *testing.T, now func() time.Time) *Store {
	t.Helper()
	return openM3TestStoreAt(t, filepath.Join(t.TempDir(), "m3.db"), now)
}

func openM3TestStoreAt(t *testing.T, path string, now func() time.Time) *Store {
	t.Helper()
	options := testOptions()
	options.Now = now
	options.CollectorPolicies = map[string]CollectorPolicy{
		"generic.one": {Kind: config.CollectorGenericJSON, HeartbeatPeriod: time.Minute},
		"aw.afk":      {Kind: config.CollectorActivityWatch, HeartbeatPeriod: time.Minute},
		"desk.media":  {Kind: config.CollectorMedia, HeartbeatPeriod: 5 * time.Minute},
	}
	store, err := Open(context.Background(), path, options)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func heartbeatJSON(collector, key, state, start, end string, offsetMS, errorMS int64, flags []string) json.RawMessage {
	if flags == nil {
		flags = []string{}
	}
	raw, _ := json.Marshal(map[string]any{"schema_version": 1, "collector_id": collector, "state": state, "device_start_raw": start, "device_end_raw": end, "clock_offset_ms": offsetMS, "clock_error_ms": errorMS, "idempotency_key": key, "quality_flags": flags})
	return raw
}

func m3Event(collector, key, timestamp string, offsetMS, errorMS int64, payload string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"schema_version":1,"collector_id":%q,"event_type":"generic.evidence","device_timestamp_raw":%q,"clock_offset_ms":%d,"clock_error_ms":%d,"idempotency_key":%q,"payload":%s}`, collector, timestamp, offsetMS, errorMS, key, payload))
}

func m3Collector(id, kind string) config.CollectorConfig {
	return config.CollectorConfig{ID: id, Kind: kind, Enabled: true, HeartbeatPeriod: "1m", AllowedLateness: "1m", OfflineAfter: "5m", PlannedSchedule: config.PlannedScheduleConfig{Timezone: "UTC", Windows: []config.ScheduleWindowConfig{{Days: []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}, StartLocal: "00:00", EndLocal: "24:00"}}}}
}

func availabilityAt(intervals []CoverageInterval, at time.Time) string {
	text := fixedUTC(at)
	for _, interval := range intervals {
		if interval.StartUTC <= text && text < interval.EndUTC {
			return interval.Availability
		}
	}
	return ""
}

func assertStoredCoverage(t *testing.T, intervals []CoverageInterval) {
	t.Helper()
	for index, interval := range intervals {
		if interval.StartUTC >= interval.EndUTC {
			t.Fatalf("invalid interval: %#v", interval)
		}
		if index > 0 && interval.CollectorID == intervals[index-1].CollectorID && interval.StartUTC < intervals[index-1].EndUTC {
			t.Fatalf("overlapping intervals: %#v %#v", intervals[index-1], interval)
		}
	}
}
