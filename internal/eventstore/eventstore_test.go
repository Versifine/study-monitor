package eventstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpenMigratesNewDatabaseAndReopensIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store := openTestStore(t, path)

	var schemaVersion int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&schemaVersion); err != nil {
		t.Fatal(err)
	}
	if schemaVersion != CurrentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", schemaVersion, CurrentSchemaVersion)
	}
	var journalMode string
	var foreignKeys, busyTimeout, migrationCount, mediaMigrationCount, m3MigrationCount int
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM media_schema_migrations").Scan(&mediaMigrationCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM m3_schema_migrations").Scan(&m3MigrationCount); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" || foreignKeys != 1 || busyTimeout != 5000 || migrationCount != 1 || mediaMigrationCount != 2 || m3MigrationCount != 1 {
		t.Fatalf("pragmas/migrations = journal:%s foreign:%d busy:%d core:%d media:%d m3:%d", journalMode, foreignKeys, busyTimeout, migrationCount, mediaMigrationCount, m3MigrationCount)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openTestStore(t, path)
	defer reopened.Close()
	if err := reopened.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != 1 {
		t.Fatalf("migration count after reopen = %d", migrationCount)
	}
	if err := reopened.db.QueryRow("SELECT COUNT(*) FROM media_schema_migrations").Scan(&mediaMigrationCount); err != nil {
		t.Fatal(err)
	}
	if mediaMigrationCount != 2 {
		t.Fatalf("media migration count after reopen = %d", mediaMigrationCount)
	}
	if err := reopened.db.QueryRow("SELECT COUNT(*) FROM m3_schema_migrations").Scan(&m3MigrationCount); err != nil {
		t.Fatal(err)
	}
	if m3MigrationCount != 1 {
		t.Fatalf("M3 migration count after reopen = %d", m3MigrationCount)
	}
}

func TestEmbeddedMigrationBytesHaveStableLineEndings(t *testing.T) {
	migrations, err := repositoryMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations {
		if strings.Contains(item.contents, "\r\n") {
			t.Fatalf("migration %s contains CRLF; checksum must not depend on Windows checkout conversion", item.name)
		}
	}
	mediaMigrations, err := repositoryMediaMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range mediaMigrations {
		if strings.Contains(item.contents, "\r\n") {
			t.Fatalf("media migration %s contains CRLF; checksum must not depend on Windows checkout conversion", item.name)
		}
	}
	m3Migrations, err := repositoryM3Migrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range m3Migrations {
		if strings.Contains(item.contents, "\r\n") {
			t.Fatalf("M3 migration %s contains CRLF; checksum must not depend on Windows checkout conversion", item.name)
		}
	}
}

func TestOpenRejectsUnsupportedAndModifiedMigrations(t *testing.T) {
	t.Run("newer schema", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "newer.db")
		db := openRawDatabase(t, path)
		if _, err := db.Exec("PRAGMA user_version = 2"); err != nil {
			t.Fatal(err)
		}
		db.Close()
		_, err := Open(context.Background(), path, testOptions())
		if ErrorCode(err) != CodeMigrationUnsupported {
			t.Fatalf("Open() error code = %q, want %q (err=%v)", ErrorCode(err), CodeMigrationUnsupported, err)
		}
	})

	t.Run("checksum mismatch", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "modified.db")
		store := openTestStore(t, path)
		if _, err := store.db.Exec("DROP TRIGGER schema_migrations_reject_update"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec("UPDATE schema_migrations SET checksum = ? WHERE version = 1", fmt.Sprintf("%064d", 0)); err != nil {
			t.Fatal(err)
		}
		store.Close()
		_, err := Open(context.Background(), path, testOptions())
		if ErrorCode(err) != CodeMigrationFailed {
			t.Fatalf("Open() error code = %q, want %q (err=%v)", ErrorCode(err), CodeMigrationFailed, err)
		}
	})

	t.Run("media checksum mismatch", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "modified-media.db")
		store := openTestStore(t, path)
		if _, err := store.db.Exec("DROP TRIGGER media_schema_migrations_reject_update"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec("UPDATE media_schema_migrations SET checksum = ? WHERE version = 1", fmt.Sprintf("%064d", 0)); err != nil {
			t.Fatal(err)
		}
		store.Close()
		_, err := Open(context.Background(), path, testOptions())
		if ErrorCode(err) != CodeMigrationFailed {
			t.Fatalf("Open() error code = %q, want %q (err=%v)", ErrorCode(err), CodeMigrationFailed, err)
		}
	})

	t.Run("M3 checksum mismatch", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "modified-m3.db")
		store := openTestStore(t, path)
		if _, err := store.db.Exec("DROP TRIGGER m3_schema_migrations_reject_update"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec("UPDATE m3_schema_migrations SET checksum = ? WHERE version = 1", fmt.Sprintf("%064d", 0)); err != nil {
			t.Fatal(err)
		}
		store.Close()
		_, err := Open(context.Background(), path, testOptions())
		if ErrorCode(err) != CodeMigrationFailed {
			t.Fatalf("Open() error code = %q, want %q (err=%v)", ErrorCode(err), CodeMigrationFailed, err)
		}
	})
}

func TestRawEventsAreAppendOnly(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	defer store.Close()
	result, err := store.AppendBatch(context.Background(), []Candidate{{Raw: testEvent("append-only", "study.activity", `{"window":"notes"}`)}})
	if err != nil || result[0].Status != StatusAccepted {
		t.Fatalf("AppendBatch() = %#v, %v", result, err)
	}
	if _, err := store.db.Exec("UPDATE raw_events SET event_type = 'changed' WHERE id = ?", result[0].EventID); err == nil {
		t.Fatal("UPDATE unexpectedly succeeded")
	}
	if _, err := store.db.Exec("DELETE FROM raw_events WHERE id = ?", result[0].EventID); err == nil {
		t.Fatal("DELETE unexpectedly succeeded")
	}
}

func TestAppendBatchIsIdempotentAndRejectsBadItemsIndependently(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	defer store.Close()

	original := testEvent("same-key", "study.activity", `{ "window": "notes", "seconds": 12 }`)
	candidates := []Candidate{
		{Raw: original},
		{Raw: json.RawMessage(`{"schema_version":1,"collector_id":"desktop","event_type":"study.activity","device_timestamp_raw":"2026-07-18T10:00:00","clock_offset_ms":0,"clock_error_ms":10,"idempotency_key":"bad-time","payload":{}}`)},
		{Raw: original},
		{Raw: testEvent("same-key", "study.activity", `{"window":"different"}`)},
	}
	results, err := store.AppendBatch(context.Background(), candidates)
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses := []string{StatusAccepted, StatusRejected, StatusDuplicate, StatusConflict}
	for index, want := range wantStatuses {
		if results[index].Index != index || results[index].Status != want {
			t.Fatalf("result[%d] = %#v, want status %q", index, results[index], want)
		}
	}
	if results[0].EventID == 0 || results[2].EventID != results[0].EventID || results[3].EventID != results[0].EventID {
		t.Fatalf("idempotent event ids are inconsistent: %#v", results)
	}
	if results[1].ErrorCode != CodeDeviceTimeInvalid || results[3].ErrorCode != CodeIdempotencyConflict {
		t.Fatalf("unexpected item error codes: %#v", results)
	}

	page, err := store.QueryPage(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("stored event count = %d, want 1", len(page.Events))
	}
	if string(page.Events[0].Payload) != `{ "window": "notes", "seconds": 12 }` {
		t.Fatalf("original payload was not preserved: %s", page.Events[0].Payload)
	}
	if page.Events[0].DeviceTimeUTC != "2026-07-18T02:00:00Z" || page.Events[0].ReceivedAtUTC != "2026-07-18T03:04:05Z" {
		t.Fatalf("unexpected normalized/server time: %#v", page.Events[0])
	}
}

func TestEventRejectsDuplicateKeysAtEveryObjectLevel(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	defer store.Close()

	base := string(testEvent("duplicate", "study.activity", `{}`))
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "event field",
			raw:  strings.Replace(base, `"idempotency_key":"duplicate"`, `"idempotency_key":"discarded","idempotency_key":"duplicate"`, 1),
		},
		{
			name: "payload field",
			raw:  strings.Replace(base, `"payload":{}`, `"payload":{"first":1},"payload":{"second":2}`, 1),
		},
		{
			name: "nested payload key",
			raw:  string(testEvent("nested-duplicate", "study.activity", `{"nested":{"key":1,"key":2}}`)),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: json.RawMessage(test.raw)}})
			if err != nil || len(results) != 1 || results[0].Status != StatusRejected || results[0].ErrorCode != CodeEventDecodeInvalid {
				t.Fatalf("AppendBatch() = %#v, %v", results, err)
			}
		})
	}
	assertEventCount(t, store.db, 0)
}

func TestEventRejectsCaseInsensitiveRootFieldAliases(t *testing.T) {
	fields := []string{
		"schema_version",
		"collector_id",
		"event_type",
		"device_timestamp_raw",
		"clock_offset_ms",
		"clock_error_ms",
		"idempotency_key",
		"payload",
	}
	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
			defer store.Close()
			raw := strings.Replace(
				string(testEvent("case-alias", "study.activity", `{}`)),
				`"`+field+`":`,
				`"`+strings.ToUpper(field)+`":`,
				1,
			)
			results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: json.RawMessage(raw)}})
			if err != nil || len(results) != 1 || results[0].Status != StatusRejected || results[0].ErrorCode != CodeEventDecodeInvalid {
				t.Fatalf("AppendBatch() = %#v, %v", results, err)
			}
			assertEventCount(t, store.db, 0)
		})
	}
}

func TestEventRejectsCaseVariantPayloadOverwrite(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	defer store.Close()

	raw := strings.Replace(
		string(testEvent("payload-case-overwrite", "study.activity", `{"discarded":true}`)),
		`"payload":{"discarded":true}`,
		`"payload":{"discarded":true},"Payload":{"kept":true}`,
		1,
	)
	results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: json.RawMessage(raw)}})
	if err != nil || len(results) != 1 || results[0].Status != StatusRejected || results[0].ErrorCode != CodeEventDecodeInvalid {
		t.Fatalf("AppendBatch() = %#v, %v", results, err)
	}
	assertEventCount(t, store.db, 0)
}

func TestEventPayloadKeepsArbitraryCaseSensitiveKeys(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	defer store.Close()

	results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: testEvent(
		"payload-case-keys",
		"study.activity",
		`{"payload":1,"Payload":2,"IDEMPOTENCY_KEY":"opaque"}`,
	)}})
	if err != nil || len(results) != 1 || results[0].Status != StatusAccepted {
		t.Fatalf("AppendBatch() = %#v, %v", results, err)
	}
	assertEventCount(t, store.db, 1)
}

func TestDeviceTimeRejectsUnknownNegativeZeroOffset(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	defer store.Close()

	tests := []struct {
		name   string
		key    string
		time   string
		status string
	}{
		{name: "Z", key: "utc-z", time: "2026-07-18T02:00:00Z", status: StatusAccepted},
		{name: "positive zero", key: "utc-plus-zero", time: "2026-07-18T02:00:00+00:00", status: StatusAccepted},
		{name: "unknown negative zero", key: "utc-unknown", time: "2026-07-18T02:00:00-00:00", status: StatusRejected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := strings.Replace(
				string(testEvent(test.key, "study.activity", `{}`)),
				"2026-07-18T10:00:00+08:00",
				test.time,
				1,
			)
			results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: json.RawMessage(raw)}})
			if err != nil || results[0].Status != test.status {
				t.Fatalf("AppendBatch() = %#v, %v", results, err)
			}
			if test.status == StatusRejected && results[0].ErrorCode != CodeDeviceTimeInvalid {
				t.Fatalf("error code = %q, want %q", results[0].ErrorCode, CodeDeviceTimeInvalid)
			}
		})
	}
	assertEventCount(t, store.db, 2)
}

func TestConcurrentSameIdempotencyKeyCreatesOneEvent(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	defer store.Close()

	const workers = 12
	start := make(chan struct{})
	results := make(chan WriteResult, workers)
	errorsFound := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			items, err := store.AppendBatch(context.Background(), []Candidate{{Raw: testEvent("concurrent-key", "study.activity", `{"ok":true}`)}})
			if err != nil {
				errorsFound <- err
				return
			}
			results <- items[0]
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatalf("concurrent AppendBatch() error = %v", err)
	}
	accepted, duplicate := 0, 0
	var eventID int64
	for result := range results {
		switch result.Status {
		case StatusAccepted:
			accepted++
			eventID = result.EventID
		case StatusDuplicate:
			duplicate++
		default:
			t.Fatalf("unexpected concurrent result: %#v", result)
		}
	}
	if accepted != 1 || duplicate != workers-1 || eventID == 0 {
		t.Fatalf("accepted=%d duplicate=%d eventID=%d", accepted, duplicate, eventID)
	}
}

func TestBatchEventAndPayloadLimitsRejectPredictably(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	options := testOptions()
	options.MaxBatchEvents = 1
	options.MaxEventBytes = 256
	options.MaxPayloadDepth = 2
	store, err := Open(context.Background(), path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, err = store.AppendBatch(context.Background(), []Candidate{
		{Raw: testEvent("one", "study.activity", `{}`)},
		{Raw: testEvent("two", "study.activity", `{}`)},
	})
	if ValidationCode(err) != CodeBatchTooLarge {
		t.Fatalf("batch limit code = %q, want %q (err=%v)", ValidationCode(err), CodeBatchTooLarge, err)
	}

	large := testEvent("large", "study.activity", fmt.Sprintf(`{"text":%q}`, strings.Repeat("x", 300)))
	results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: large}})
	if err != nil || results[0].Status != StatusRejected || results[0].ErrorCode != CodeEventTooLarge {
		t.Fatalf("event limit result = %#v, %v", results, err)
	}

	deep := testEvent("deep", "study.activity", `{"a":{"b":{}}}`)
	results, err = store.AppendBatch(context.Background(), []Candidate{{Raw: deep}})
	if err != nil || results[0].Status != StatusRejected || results[0].ErrorCode != CodePayloadTooDeep {
		t.Fatalf("payload depth result = %#v, %v", results, err)
	}
}

func TestBatchWriteFailureRollsBackAllItemsAndRetryIsSafe(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	defer store.Close()
	if _, err := store.db.Exec(`CREATE TRIGGER test_force_abort BEFORE INSERT ON raw_events
WHEN NEW.event_type = 'force.abort' BEGIN SELECT RAISE(ABORT, 'FORCED_ABORT'); END;`); err != nil {
		t.Fatal(err)
	}
	candidates := []Candidate{
		{Raw: testEvent("before-abort", "study.activity", `{"n":1}`)},
		{Raw: testEvent("abort", "force.abort", `{"n":2}`)},
	}
	if results, err := store.AppendBatch(context.Background(), candidates); err == nil || results != nil {
		t.Fatalf("failed batch = %#v, %v; want no acknowledgement", results, err)
	}
	assertEventCount(t, store.db, 0)
	if _, err := store.db.Exec("DROP TRIGGER test_force_abort"); err != nil {
		t.Fatal(err)
	}
	results, err := store.AppendBatch(context.Background(), candidates)
	if err != nil || results[0].Status != StatusAccepted || results[1].Status != StatusAccepted {
		t.Fatalf("retry = %#v, %v", results, err)
	}
}

func TestDatabaseBusyIsBoundedAndRetryable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	options := testOptions()
	options.BusyTimeout = 100 * time.Millisecond
	store, err := Open(context.Background(), path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	locker := openRawDatabase(t, path)
	defer locker.Close()
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = store.AppendBatch(context.Background(), []Candidate{{Raw: testEvent("busy-key", "study.activity", `{}`)}})
	if ErrorCode(err) != CodeBusy {
		t.Fatalf("busy error code = %q, want %q (err=%v)", ErrorCode(err), CodeBusy, err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("busy write exceeded bounded wait: %s", elapsed)
	}
	if _, err := locker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: testEvent("busy-key", "study.activity", `{}`)}})
	if err != nil || results[0].Status != StatusAccepted {
		t.Fatalf("retry after busy = %#v, %v", results, err)
	}
}

func TestQueryCursorPinsSnapshotAgainstNewWrites(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	defer store.Close()
	for index := 1; index <= 3; index++ {
		appendAccepted(t, store, fmt.Sprintf("key-%d", index))
	}
	first, err := store.QueryPage(context.Background(), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 2 || first.NextCursor == "" || first.SnapshotID != 3 {
		t.Fatalf("first page = %#v", first)
	}
	appendAccepted(t, store, "key-4")
	second, err := store.QueryPage(context.Background(), first.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 1 || second.Events[0].ID != 3 || second.SnapshotID != 3 || second.NextCursor != "" {
		t.Fatalf("second snapshot page = %#v", second)
	}
	fresh, err := store.QueryPage(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Events) != 4 || fresh.SnapshotID != 4 {
		t.Fatalf("fresh page = %#v", fresh)
	}
}

func TestConfirmedWriteSurvivesAbruptProcessExit(t *testing.T) {
	if os.Getenv("EXAM_MONITOR_ABRUPT_HELPER") == "1" {
		path := os.Getenv("EXAM_MONITOR_ABRUPT_DATABASE")
		store, err := Open(context.Background(), path, testOptions())
		if err != nil {
			os.Exit(41)
		}
		results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: testEvent("abrupt-key", "study.activity", `{"confirmed":true}`)}})
		if err != nil || results[0].Status != StatusAccepted {
			os.Exit(42)
		}
		os.Exit(0)
	}

	path := filepath.Join(t.TempDir(), "events.db")
	command := exec.Command(os.Args[0], "-test.run=^TestConfirmedWriteSurvivesAbruptProcessExit$")
	command.Env = append(os.Environ(), "EXAM_MONITOR_ABRUPT_HELPER=1", "EXAM_MONITOR_ABRUPT_DATABASE="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("abrupt helper failed: %v\n%s", err, output)
	}
	store := openTestStore(t, path)
	defer store.Close()
	page, err := store.QueryPage(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].IdempotencyKey != "abrupt-key" {
		t.Fatalf("confirmed event missing after abrupt exit: %#v", page)
	}
}

func TestInFlightBatchTerminationLeavesNoPartialFactsAndRetrySucceeds(t *testing.T) {
	if os.Getenv("EXAM_MONITOR_INFLIGHT_HELPER") == "1" {
		path := os.Getenv("EXAM_MONITOR_INFLIGHT_DATABASE")
		marker := os.Getenv("EXAM_MONITOR_INFLIGHT_MARKER")
		store, err := Open(context.Background(), path, testOptions())
		if err != nil {
			os.Exit(51)
		}
		ctx := context.WithValue(context.Background(), beforeCommitHookKey{}, func() {
			if err := os.WriteFile(marker, []byte("before-commit"), 0o600); err != nil {
				os.Exit(52)
			}
			time.Sleep(time.Hour)
		})
		_, _ = store.AppendBatch(ctx, inFlightCandidates())
		os.Exit(53)
	}

	directory := t.TempDir()
	path := filepath.Join(directory, "events.db")
	marker := filepath.Join(directory, "before-commit.marker")
	command := exec.Command(os.Args[0], "-test.run=^TestInFlightBatchTerminationLeavesNoPartialFactsAndRetrySucceeds$")
	command.Env = append(os.Environ(),
		"EXAM_MONITOR_INFLIGHT_HELPER=1",
		"EXAM_MONITOR_INFLIGHT_DATABASE="+path,
		"EXAM_MONITOR_INFLIGHT_MARKER="+marker,
	)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitResult := make(chan error, 1)
	go func() { waitResult <- command.Wait() }()
	stopped := false
	defer func() {
		if !stopped {
			_ = command.Process.Kill()
			<-waitResult
		}
	}()

	deadline := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	reachedBeforeCommit := false
	for !reachedBeforeCommit {
		select {
		case err := <-waitResult:
			stopped = true
			t.Fatalf("helper exited before commit barrier: %v\n%s", err, output.String())
		case <-deadline.C:
			t.Fatalf("helper did not reach commit barrier\n%s", output.String())
		case <-ticker.C:
			if _, err := os.Stat(marker); err == nil {
				reachedBeforeCommit = true
			}
		}
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-waitResult; err == nil {
		t.Fatal("killed helper unexpectedly exited successfully")
	}
	stopped = true

	store := openTestStore(t, path)
	defer store.Close()
	assertEventCount(t, store.db, 0)
	results, err := store.AppendBatch(context.Background(), inFlightCandidates())
	if err != nil || len(results) != 2 || results[0].Status != StatusAccepted || results[1].Status != StatusAccepted {
		t.Fatalf("retry after in-flight termination = %#v, %v", results, err)
	}
	assertEventCount(t, store.db, 2)
}

func TestReadinessDistinguishesWritableBusyAndReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	options := testOptions()
	options.MaxOpenConnections = 1
	options.BusyTimeout = 100 * time.Millisecond
	store, err := Open(context.Background(), path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if readiness := store.Readiness(context.Background()); readiness.Status != ReadinessWritable || readiness.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("writable readiness = %#v", readiness)
	}

	locker := openRawDatabase(t, path)
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if readiness := store.Readiness(context.Background()); readiness.Status != ReadinessBusy || readiness.ErrorCode != CodeBusy {
		t.Fatalf("busy readiness = %#v", readiness)
	}
	if _, err := locker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	locker.Close()

	connection, err := store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), "PRAGMA query_only = ON"); err != nil {
		t.Fatal(err)
	}
	connection.Close()
	if readiness := store.Readiness(context.Background()); readiness.Status != ReadinessReadOnly || readiness.ErrorCode != CodeReadOnly {
		t.Fatalf("read-only readiness = %#v", readiness)
	}
}

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(context.Background(), path, testOptions())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return store
}

func testOptions() Options {
	return Options{
		BusyTimeout:        5 * time.Second,
		MaxOpenConnections: 8,
		MaxBatchEvents:     100,
		MaxEventBytes:      64 << 10,
		MaxPayloadDepth:    16,
		MaxPageSize:        500,
		Now: func() time.Time {
			return time.Date(2026, 7, 18, 3, 4, 5, 0, time.UTC)
		},
	}
}

func openRawDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriverName, sqliteDSN(path, 100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	return db
}

func testEvent(key, eventType, payload string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"schema_version":1,"collector_id":"desktop","event_type":%q,"device_timestamp_raw":"2026-07-18T10:00:00+08:00","clock_offset_ms":250,"clock_error_ms":50,"idempotency_key":%q,"payload":%s}`, eventType, key, payload))
}

func inFlightCandidates() []Candidate {
	return []Candidate{
		{Raw: testEvent("inflight-one", "study.activity", `{"position":1}`)},
		{Raw: testEvent("inflight-two", "study.activity", `{"position":2}`)},
	}
}

func appendAccepted(t *testing.T, store *Store, key string) int64 {
	t.Helper()
	results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: testEvent(key, "study.activity", `{}`)}})
	if err != nil || len(results) != 1 || results[0].Status != StatusAccepted {
		t.Fatalf("AppendBatch(%q) = %#v, %v", key, results, err)
	}
	return results[0].EventID
}

func assertEventCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow("SELECT COUNT(*) FROM raw_events").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("event count = %d, want %d", got, want)
	}
}
