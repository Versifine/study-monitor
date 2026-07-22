package eventstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestM4FactsAreAppendOnlyAndSchemaIsIndependentlyVersioned(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "m4.db"))
	defer store.Close()
	ctx := context.Background()
	var count int
	now := time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if err := store.AppendFaultEvent(ctx, FaultEvent{Module: "storage", Severity: "P1", Status: "active", ErrorCode: "LOW_DISK", Detail: "injected", OccurredAtUTC: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendModuleStateEvent(ctx, ModuleStateEvent{Module: "retention", Status: "disabled", ReasonCode: "RETENTION_DISABLED", OccurredAtUTC: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendModeTransition(ctx, "record-only", "current-user", "startup_config", "CONFIG_MODE", now); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendModeTransition(ctx, "record-only", "current-user", "startup_config", "CONFIG_MODE", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE fault_events SET detail='changed'"); err == nil {
		t.Fatal("fault UPDATE unexpectedly succeeded")
	}
	if _, err := store.db.Exec("DELETE FROM module_state_events"); err == nil {
		t.Fatal("module state DELETE unexpectedly succeeded")
	}
	if _, err := store.db.Exec("UPDATE mode_transition_events SET operator='changed'"); err == nil {
		t.Fatal("mode transition UPDATE unexpectedly succeeded")
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM mode_transition_events").Scan(&count); err != nil || count != 1 {
		t.Fatalf("mode transition count=%d err=%v", count, err)
	}
	info, err := store.SchemaInfo(ctx)
	if err != nil || info != (SchemaInfo{Core: 1, Media: 2, M3: 1, M4: 1}) {
		t.Fatalf("schema=%#v err=%v", info, err)
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM m4_schema_migrations").Scan(&count); err != nil || count != 1 {
		t.Fatalf("M4 ledger count=%d err=%v", count, err)
	}
}

func TestRetentionDeletionFactsAndProjectionCommitAtomically(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "retention-atomic.db"))
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 7, 22, 2, 30, 0, 0, time.UTC).Format(time.RFC3339Nano)
	metadata := testMediaMetadata("camera", "retention-atomic", repeatedHex('a'), repeatedHex('b'))
	claim, err := store.AcceptMedia(ctx, metadata, testMediaIngestEvent("retention-accepted", "retention-atomic", "accepted"), repeatedHex('c'))
	if err != nil || claim.SegmentID == 0 {
		t.Fatalf("accept media claim=%#v err=%v", claim, err)
	}
	planned := RetentionEvent{MediaSegmentID: claim.SegmentID, Status: "planned", ReasonCode: "RETENTION_POLICY", ManagedRelativePath: metadata.ManagedRelativePath, OccurredAtUTC: now}
	if err := store.AppendRetentionEvent(ctx, planned); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER inject_retention_projection_failure BEFORE UPDATE ON media_segment_status BEGIN SELECT RAISE(ABORT, 'INJECTED'); END`); err != nil {
		t.Fatal(err)
	}
	deleted := planned
	deleted.Status = "deleted"
	if err := store.CommitRetentionDeletion(ctx, deleted); err == nil {
		t.Fatal("injected projection failure did not fail retention transaction")
	}
	var deletedFacts, deletedStates int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM retention_events WHERE status='deleted'").Scan(&deletedFacts); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM media_segment_state_events WHERE status='retention_deleted'").Scan(&deletedStates); err != nil {
		t.Fatal(err)
	}
	if deletedFacts != 0 || deletedStates != 0 {
		t.Fatalf("failed transaction partially committed facts=%d states=%d", deletedFacts, deletedStates)
	}
	if plannedStill, err := store.RetentionDeletionPlanned(ctx, claim.SegmentID, metadata.ManagedRelativePath); err != nil || !plannedStill {
		t.Fatalf("failed transaction lost retryable plan=%v err=%v", plannedStill, err)
	}
	if _, err := store.db.Exec("DROP TRIGGER inject_retention_projection_failure"); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitRetentionDeletion(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitRetentionDeletion(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM retention_events WHERE status='deleted'").Scan(&deletedFacts); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM media_segment_state_events WHERE status='retention_deleted'").Scan(&deletedStates); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := store.db.QueryRow("SELECT status FROM media_segment_status WHERE media_segment_id=?", claim.SegmentID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if deletedFacts != 1 || deletedStates != 1 || status != "retention_deleted" {
		t.Fatalf("idempotent commit facts=%d states=%d status=%q", deletedFacts, deletedStates, status)
	}
}

func TestConsistentDatabaseBackupIsVerifiedAndDoesNotReplaceExistingTarget(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "live.db"))
	defer store.Close()
	ctx := context.Background()
	result, err := store.AppendBatch(ctx, []Candidate{{Raw: testEvent("backup-1", "study.activity", `{"value":1}`)}})
	if err != nil || result[0].Status != StatusAccepted {
		t.Fatalf("append=%#v err=%v", result, err)
	}
	target := filepath.Join(t.TempDir(), "snapshot.db")
	if err := store.BackupTo(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDatabase(ctx, target); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSchemaInfo(ctx, target); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("read-only schema inspection modified the database file")
	}
	media, err := ReadMediaManifest(ctx, target)
	if err != nil || len(media) != 0 {
		t.Fatalf("snapshot media manifest=%#v err=%v", media, err)
	}
	db, err := sql.Open(sqliteDriverName, sqliteDSN(target, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM raw_events").Scan(&count); err != nil || count != 1 {
		t.Fatalf("snapshot event count=%d err=%v", count, err)
	}
	if err := store.BackupTo(ctx, target); ErrorCode(err) != CodePathInvalid {
		t.Fatalf("existing target error code=%s err=%v", ErrorCode(err), err)
	}
}

func TestOpenRejectsModifiedM4MigrationLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modified-m4.db")
	store := openTestStore(t, path)
	if _, err := store.db.Exec("DROP TRIGGER m4_schema_migrations_reject_update"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE m4_schema_migrations SET checksum = ? WHERE version = 1", fmt.Sprintf("%064d", 0)); err != nil {
		t.Fatal(err)
	}
	store.Close()
	_, err := Open(context.Background(), path, testOptions())
	if ErrorCode(err) != CodeMigrationFailed {
		t.Fatalf("code=%q err=%v", ErrorCode(err), err)
	}
}

func TestReadSchemaInfoTreatsPreM4DatabaseAsM4VersionZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-m4.db")
	store := openTestStore(t, path)
	if _, err := store.db.Exec("DROP TRIGGER m4_schema_migrations_reject_update; DROP TRIGGER m4_schema_migrations_reject_delete; DROP TABLE m4_schema_migrations"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()
	info, err := ReadSchemaInfo(context.Background(), path)
	if err != nil || info != (SchemaInfo{Core: 1, Media: 2, M3: 1, M4: 0}) {
		t.Fatalf("pre-M4 schema=%#v err=%v", info, err)
	}
}

func TestVerifyDatabaseRejectsModifiedMigrationLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modified-ledger.db")
	store := openTestStore(t, path)
	if _, err := store.db.Exec("DROP TRIGGER media_schema_migrations_reject_update"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE media_schema_migrations SET checksum=? WHERE version=2", repeatedHex('0')); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()
	if err := VerifyDatabase(context.Background(), path); ErrorCode(err) != CodeMigrationFailed {
		t.Fatalf("modified ledger verification code=%q err=%v", ErrorCode(err), err)
	}
}
