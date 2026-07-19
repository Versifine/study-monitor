package eventstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"
)

func TestMediaAcceptanceIsIdempotentAndConflictsAreVisible(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	defer store.Close()
	metadata := testMediaMetadata("camera", "segment-1", repeatedHex('a'), repeatedHex('b'))
	event := testMediaIngestEvent("accepted-one", "ingest-one", "accepted")

	accepted, err := store.AcceptMedia(context.Background(), metadata, event, repeatedHex('c'))
	if err != nil || accepted.Status != MediaWriteAccepted || accepted.SegmentID == 0 {
		t.Fatalf("first AcceptMedia() = %#v, %v", accepted, err)
	}
	duplicate, err := store.AcceptMedia(context.Background(), metadata, event, repeatedHex('c'))
	if err != nil || duplicate.Status != MediaWriteDuplicate || duplicate.SegmentID != accepted.SegmentID {
		t.Fatalf("duplicate AcceptMedia() = %#v, %v", duplicate, err)
	}

	changedContent := metadata
	changedContent.SHA256 = repeatedHex('d')
	changedContent.MetadataHash = repeatedHex('e')
	conflict, err := store.AcceptMedia(context.Background(), changedContent, testMediaIngestEvent("accepted-conflict", "ingest-one", "accepted"), repeatedHex('f'))
	if err != nil || conflict.Status != MediaWriteConflict || conflict.SegmentID != accepted.SegmentID {
		t.Fatalf("source conflict = %#v, %v", conflict, err)
	}

	changedMetadata := testMediaMetadata("other-camera", "segment-2", metadata.SHA256, repeatedHex('1'))
	conflict, err = store.AcceptMedia(context.Background(), changedMetadata, testMediaIngestEvent("accepted-metadata-conflict", "ingest-two", "accepted"), repeatedHex('2'))
	if err != nil || conflict.Status != MediaWriteConflict || conflict.SegmentID != accepted.SegmentID {
		t.Fatalf("metadata conflict = %#v, %v", conflict, err)
	}
	assertTableCount(t, store.db, "media_segments", 1)
	assertTableCount(t, store.db, "media_ingest_events", 1)
	assertTableCount(t, store.db, "media_segment_state_events", 1)
}

func TestMediaFactsAreAppendOnlyAndProjectionsRebuild(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	defer store.Close()
	metadata := testMediaMetadata("camera", "segment-append-only", repeatedHex('3'), repeatedHex('4'))
	event := testMediaIngestEvent("accepted-append-only", "ingest-append-only", "accepted")
	claim, err := store.AcceptMedia(context.Background(), metadata, event, repeatedHex('5'))
	if err != nil || claim.Status != MediaWriteAccepted {
		t.Fatalf("AcceptMedia() = %#v, %v", claim, err)
	}
	for _, statement := range []string{
		"UPDATE media_segments SET codec_name = 'changed'",
		"DELETE FROM media_segments",
		"UPDATE media_ingest_events SET status = 'failed'",
		"DELETE FROM media_ingest_events",
		"UPDATE media_segment_state_events SET status = 'missing'",
		"DELETE FROM media_segment_state_events",
	} {
		if _, err := store.db.Exec(statement); err == nil {
			t.Fatalf("append-only statement unexpectedly succeeded: %s", statement)
		}
	}
	if _, err := store.db.Exec("DELETE FROM media_ingest_status; DELETE FROM media_segment_status"); err != nil {
		t.Fatal(err)
	}
	if err := store.RebuildMediaProjections(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertTableCount(t, store.db, "media_ingest_status", 1)
	assertTableCount(t, store.db, "media_segment_status", 1)
	summary, err := store.MediaIngestSummary(context.Background())
	if err != nil || summary.Accepted != 1 || summary.TotalSegments != 1 || summary.Backlog != 0 {
		t.Fatalf("MediaIngestSummary() = %#v, %v", summary, err)
	}
}

func TestMediaProjectionFailurePreservesAuthoritativeFactAndRetryRepairs(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	defer store.Close()
	if _, err := store.db.Exec(`CREATE TRIGGER test_media_projection_failure
BEFORE INSERT ON media_ingest_status
BEGIN SELECT RAISE(ABORT, 'FORCED_PROJECTION_FAILURE'); END;`); err != nil {
		t.Fatal(err)
	}
	event := testMediaIngestEvent("pending-projection", "ingest-projection", "pending")
	if err := store.AppendMediaIngestEvent(context.Background(), event); err == nil {
		t.Fatal("projection failure unexpectedly succeeded")
	}
	assertTableCount(t, store.db, "media_ingest_events", 1)
	assertTableCount(t, store.db, "media_ingest_status", 0)
	if _, err := store.db.Exec("DROP TRIGGER test_media_projection_failure"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMediaIngestEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	assertTableCount(t, store.db, "media_ingest_events", 1)
	assertTableCount(t, store.db, "media_ingest_status", 1)
}

func TestMediaStateProjectionFailureIsRepairedByDuplicateRetry(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	defer store.Close()
	if _, err := store.db.Exec(`CREATE TRIGGER test_media_state_projection_failure
BEFORE INSERT ON media_segment_status
BEGIN SELECT RAISE(ABORT, 'FORCED_STATE_PROJECTION_FAILURE'); END;`); err != nil {
		t.Fatal(err)
	}
	metadata := testMediaMetadata("camera", "state-projection", repeatedHex('9'), repeatedHex('a'))
	event := testMediaIngestEvent("accepted-state-projection", "ingest-state-projection", "accepted")
	stateEventKey := repeatedHex('b')
	claim, err := store.AcceptMedia(context.Background(), metadata, event, stateEventKey)
	if err == nil || claim.Status != MediaWriteAccepted {
		t.Fatalf("projection failure AcceptMedia() = %#v, %v", claim, err)
	}
	assertTableCount(t, store.db, "media_segments", 1)
	assertTableCount(t, store.db, "media_segment_state_events", 1)
	assertTableCount(t, store.db, "media_segment_status", 0)
	if _, err := store.db.Exec("DROP TRIGGER test_media_state_projection_failure"); err != nil {
		t.Fatal(err)
	}
	claim, err = store.AcceptMedia(context.Background(), metadata, event, stateEventKey)
	if err != nil || claim.Status != MediaWriteDuplicate {
		t.Fatalf("duplicate repair AcceptMedia() = %#v, %v", claim, err)
	}
	assertTableCount(t, store.db, "media_segments", 1)
	assertTableCount(t, store.db, "media_segment_state_events", 1)
	assertTableCount(t, store.db, "media_segment_status", 1)
}

func TestVersionOneDatabaseMigratesForwardWithoutChangingRawEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	db := openRawDatabase(t, path)
	migrations, err := repositoryMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if err := applyMigrationsThrough(context.Background(), db, migrations, func() time.Time {
		return time.Date(2026, 7, 18, 1, 0, 0, 0, time.UTC)
	}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO raw_events (
collector_id, event_type, device_timestamp_raw, device_time_utc, received_at_utc,
clock_offset_ms, clock_error_ms, idempotency_key, payload_json, payload_hash, content_hash, schema_version
) VALUES ('legacy', 'study.activity', '2026-07-18T10:00:00+08:00', '2026-07-18T02:00:00Z',
'2026-07-18T02:00:01Z', 0, 0, 'legacy-key', '{}', ?, ?, 1)`, repeatedHex('6'), repeatedHex('7')); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, path)
	defer store.Close()
	assertTableCount(t, store.db, "raw_events", 1)
	assertTableCount(t, store.db, "schema_migrations", 2)
	assertTableCount(t, store.db, "media_segments", 0)
}

func testMediaMetadata(collector, sourceKey, sha, metadataHash string) MediaMetadata {
	return MediaMetadata{
		CollectorID:          collector,
		SourceIdempotencyKey: sourceKey,
		ManagedRelativePath:  "accepted/" + sha + ".media",
		DeviceStartRaw:       "2026-07-18T10:00:00+08:00",
		DeviceEndRaw:         "2026-07-18T10:00:01+08:00",
		DeviceStartUTC:       "2026-07-18T02:00:00Z",
		DeviceEndUTC:         "2026-07-18T02:00:01Z",
		ReceivedAtUTC:        "2026-07-18T02:00:02Z",
		ClockOffsetMS:        0,
		ClockErrorMS:         10,
		SizeBytes:            1024,
		DurationMS:           1000,
		CodecName:            "h264",
		FormatName:           "mov,mp4,m4a,3gp,3g2,mj2",
		MediaType:            "video",
		SHA256:               sha,
		MetadataHash:         metadataHash,
		SidecarSchemaVersion: 1,
	}
}

func testMediaIngestEvent(eventKey, ingestKey, status string) MediaIngestEvent {
	return MediaIngestEvent{
		EventKey:             hashTestValue(eventKey),
		IngestKey:            hashTestValue(ingestKey),
		CollectorID:          "camera",
		SourceIdempotencyKey: "segment-1",
		SourceName:           "segment.mp4",
		SourceFingerprint:    repeatedHex('8'),
		Status:               status,
		OccurredAtUTC:        "2026-07-18T02:00:02Z",
	}
}

func hashTestValue(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func repeatedHex(value byte) string {
	return string(makeRepeated(value, 64))
}

func makeRepeated(value byte, count int) []byte {
	result := make([]byte, count)
	for index := range result {
		result[index] = value
	}
	return result
}

func assertTableCount(t *testing.T, db interface{ QueryRow(string, ...any) *sql.Row }, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}
