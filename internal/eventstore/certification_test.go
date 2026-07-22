package eventstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCertificationSnapshotVerifiesAndCountsCumulativeFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.db")
	store := openTestStore(t, path)
	for _, key := range []string{"first-key", "second-key"} {
		results, err := store.AppendBatch(context.Background(), []Candidate{{Raw: testEvent(key, "study.activity", `{"position":1}`)}})
		if err != nil || len(results) != 1 || results[0].Status != StatusAccepted {
			t.Fatalf("AppendBatch(%q) = %#v, %v", key, results, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	snapshot, err := ReadCertificationDatabaseSnapshot(context.Background(), path)
	if err != nil {
		t.Fatalf("ReadCertificationDatabaseSnapshot() error = %v", err)
	}
	if snapshot.SchemaVersion != 1 || snapshot.Integrity != "ok" || snapshot.DatabaseSchema != (SchemaInfo{Core: 1, Media: 2, M3: 1, M4: 1}) {
		t.Fatalf("snapshot header = %#v", snapshot)
	}
	if snapshot.Counts.RawEvents != 2 || snapshot.Counts.CollectorHeartbeats != 0 || snapshot.Counts.MediaSegments != 0 {
		t.Fatalf("snapshot counts = %#v", snapshot.Counts)
	}
	if len(snapshot.RawEventsByCollector) != 1 || snapshot.RawEventsByCollector[0].Key1 != "desktop" || snapshot.RawEventsByCollector[0].Count != 2 {
		t.Fatalf("raw event groups = %#v", snapshot.RawEventsByCollector)
	}
	if snapshot.PotentialDuplicates.RawEventIdentityGroups != 1 || snapshot.PotentialDuplicates.HeartbeatIdentityGroups != 0 || snapshot.PotentialDuplicates.MediaSHA256Groups != 0 {
		t.Fatalf("potential duplicate scan = %#v", snapshot.PotentialDuplicates)
	}
	if snapshot.MaximumMediaDurationMS != 0 {
		t.Fatalf("maximum media duration = %d", snapshot.MaximumMediaDurationMS)
	}
	if snapshot.HeartbeatsByCollectorState == nil || snapshot.MediaByCollector == nil || snapshot.CoverageProjections == nil {
		t.Fatalf("empty collections must encode as JSON arrays: %#v", snapshot)
	}
}

func TestCertificationSnapshotRequiresAbsoluteVerifiedDatabase(t *testing.T) {
	if _, err := ReadCertificationDatabaseSnapshot(context.Background(), "relative.db"); ErrorCode(err) != CodePathInvalid {
		t.Fatalf("relative path error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "not-a-database.db")
	if err := writeTestFile(path, []byte("not sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCertificationDatabaseSnapshot(context.Background(), path); err == nil {
		t.Fatal("corrupt database was accepted")
	}
}

func writeTestFile(path string, contents []byte) error {
	return os.WriteFile(path, contents, 0o600)
}
