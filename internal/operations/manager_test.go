package operations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Versifine/study-monitor/internal/config"
	"github.com/Versifine/study-monitor/internal/eventstore"
)

type fakeRepository struct {
	faults      []eventstore.FaultEvent
	modules     []eventstore.ModuleStateEvent
	candidates  []eventstore.RetentionCandidate
	retention   []eventstore.RetentionEvent
	states      []string
	planned     bool
	referenced  map[string]bool
	checkpoints []bool
	commitErr   error
	modes       []string
}

type sequenceProbe struct {
	mu     sync.Mutex
	values []uint64
	index  int
}

func (probe *sequenceProbe) FreeBytes(string) (uint64, error) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	index := probe.index
	if index >= len(probe.values) {
		index = len(probe.values) - 1
	} else {
		probe.index++
	}
	return probe.values[index], nil
}

func (repository *fakeRepository) AppendFaultEvent(_ context.Context, event eventstore.FaultEvent) error {
	repository.faults = append(repository.faults, event)
	return nil
}
func (repository *fakeRepository) AppendModuleStateEvent(_ context.Context, event eventstore.ModuleStateEvent) error {
	repository.modules = append(repository.modules, event)
	return nil
}
func (repository *fakeRepository) AppendModeTransition(_ context.Context, mode, _, _, _, _ string) error {
	repository.modes = append(repository.modes, mode)
	return nil
}
func (repository *fakeRepository) RetentionCandidates(context.Context, string, int) ([]eventstore.RetentionCandidate, error) {
	return repository.candidates, nil
}
func (repository *fakeRepository) AppendRetentionEvent(_ context.Context, event eventstore.RetentionEvent) error {
	repository.retention = append(repository.retention, event)
	repository.planned = event.Status == "planned"
	return nil
}
func (repository *fakeRepository) RetentionDeletionPlanned(context.Context, int64, string) (bool, error) {
	return repository.planned, nil
}
func (repository *fakeRepository) CommitRetentionDeletion(_ context.Context, event eventstore.RetentionEvent) error {
	if repository.commitErr != nil {
		return repository.commitErr
	}
	repository.retention = append(repository.retention, event)
	repository.states = append(repository.states, "retention_deleted")
	repository.planned = false
	return nil
}
func (repository *fakeRepository) TemporaryPathReferenced(_ context.Context, path string) (bool, error) {
	return repository.referenced[path], nil
}
func (repository *fakeRepository) Checkpoint(_ context.Context, truncate bool) error {
	repository.checkpoints = append(repository.checkpoints, truncate)
	return nil
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	local := t.TempDir()
	cfg, err := config.Load("", func(key string) (string, bool) {
		if key == "LOCALAPPDATA" {
			return local, true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.Paths.DataDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestInjectedDiskLevelsProtectMediaBeforeCoreWrites(t *testing.T) {
	cfg := testConfig(t)
	repository := &fakeRepository{}
	now := time.Date(2026, 7, 22, 1, 2, 3, 0, time.UTC)
	tests := []struct {
		free        uint64
		level       string
		media, core bool
	}{
		{uint64(cfg.Storage.WarningFreeBytes + 1), DiskNormal, true, true},
		{uint64(cfg.Storage.WarningFreeBytes), DiskWarning, false, true},
		{uint64(cfg.Storage.CriticalFreeBytes), DiskCritical, false, true},
		{uint64(cfg.Storage.DatabaseReserveBytes), DiskReserve, false, false},
	}
	for _, test := range tests {
		manager := NewWithProbe(cfg, repository, nil, fixedProbe{bytes: test.free}, func() time.Time { return now })
		manager.ScanDiskOnce(context.Background())
		if got := manager.Status(context.Background()).DiskLevel; got != test.level {
			t.Fatalf("free=%d level=%s want=%s", test.free, got, test.level)
		}
		media, _ := manager.MediaAllowed()
		core, _ := manager.CoreWritesAllowed()
		if media != test.media || core != test.core {
			t.Fatalf("free=%d media=%v core=%v", test.free, media, core)
		}
	}
}

func TestStatusIncludesBoundedRuntimeCertificationCounters(t *testing.T) {
	status := WithRuntimeResources(Status{SchemaVersion: 1, DiskLevel: DiskNormal, Retention: "disabled"})
	if status.Runtime.Goroutines < 1 || status.Runtime.HeapAllocBytes == 0 || status.Runtime.HeapInUseBytes == 0 || status.Runtime.StackInUseBytes == 0 || status.Runtime.RuntimeSystemBytes == 0 {
		t.Fatalf("runtime resources = %#v", status.Runtime)
	}
}

func TestCoreGateRefreshesRealProbeInsteadOfReusingPeriodicCache(t *testing.T) {
	cfg := testConfig(t)
	repository := &fakeRepository{}
	probe := &sequenceProbe{values: []uint64{uint64(cfg.Storage.WarningFreeBytes + 1), uint64(cfg.Storage.DatabaseReserveBytes), uint64(cfg.Storage.DatabaseReserveBytes)}}
	manager := NewWithProbe(cfg, repository, nil, probe, time.Now)
	manager.ScanDiskOnce(context.Background())
	if manager.Status(context.Background()).DiskLevel != DiskNormal {
		t.Fatal("initial periodic scan was not normal")
	}
	if allowed, code := manager.CoreWritesAllowed(); allowed || code != CodeDiskReserve {
		t.Fatalf("live core gate allowed newly threatened reserve: allowed=%v code=%q", allowed, code)
	}
	if manager.Status(context.Background()).DiskLevel != DiskReserve {
		t.Fatal("live gate did not refresh visible disk status")
	}
	manager.ScanDiskOnce(context.Background())
	if len(repository.faults) != 1 || repository.faults[0].ErrorCode != CodeDiskReserve {
		t.Fatalf("periodic scan did not persist live transition=%#v", repository.faults)
	}
}

func TestRetentionDefaultsOffAndRequiresVerifiedFullBackup(t *testing.T) {
	cfg := testConfig(t)
	contents := []byte("immutable evidence")
	digest := sha256.Sum256(contents)
	hash := hex.EncodeToString(digest[:])
	path := filepath.Join(cfg.MediaStorageDirectory(), "accepted", hash+".media")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	repository := &fakeRepository{candidates: []eventstore.RetentionCandidate{{MediaSegmentID: 1, ManagedRelativePath: "accepted/" + hash + ".media", SHA256: hash, SizeBytes: int64(len(contents)), CreatedAtUTC: "2026-01-01T00:00:00Z"}}}
	manager := NewWithProbe(cfg, repository, nil, fixedProbe{bytes: uint64(cfg.Storage.CriticalFreeBytes)}, time.Now)
	manager.ScanDiskOnce(context.Background())
	manager.RetentionOnce(context.Background())
	if _, err := os.Stat(path); err != nil {
		t.Fatal("default-off retention removed Evidence")
	}

	cfg.Retention.Enabled = true
	manager = NewWithProbe(cfg, repository, nil, fixedProbe{bytes: uint64(cfg.Storage.CriticalFreeBytes)}, time.Now)
	manager.ScanDiskOnce(context.Background())
	manager.RetentionOnce(context.Background())
	if _, err := os.Stat(path); err != nil {
		t.Fatal("retention without a verified full backup removed Evidence")
	}

	backupRoot := t.TempDir()
	backupRelative := "media/accepted/" + hash + ".media"
	backupBody := filepath.Join(backupRoot, filepath.FromSlash(backupRelative))
	if err := os.MkdirAll(filepath.Dir(backupBody), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupBody, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	unrelatedContents := []byte("unrelated backed up evidence")
	unrelatedDigest := sha256.Sum256(unrelatedContents)
	unrelatedHash := hex.EncodeToString(unrelatedDigest[:])
	unrelatedRelative := "media/accepted/" + unrelatedHash + ".media"
	unrelatedBody := filepath.Join(backupRoot, filepath.FromSlash(unrelatedRelative))
	if err := os.WriteFile(unrelatedBody, unrelatedContents, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{"schema_version": 1, "type": "full", "files": []map[string]any{
		{"relative_path": "accepted/" + hash + ".media", "backup_path": backupRelative, "sha256": hash, "kind": "media", "size_bytes": len(contents), "included": true},
		{"relative_path": "accepted/" + unrelatedHash + ".media", "backup_path": unrelatedRelative, "sha256": unrelatedHash, "kind": "media", "size_bytes": len(unrelatedContents), "included": true},
	}}
	manifestRaw, _ := json.Marshal(manifest)
	manifestPath := filepath.Join(backupRoot, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifestRaw)
	marker := map[string]any{"schema_version": 1, "manifest_path": manifestPath, "manifest_sha256": hex.EncodeToString(manifestDigest[:])}
	markerRaw, _ := json.Marshal(marker)
	markerPath := filepath.Join(cfg.Paths.DataDirectory, "backup", "latest-full.json")
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, markerRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	var hashedPaths []string
	manager.hashFile = func(path string) (string, error) {
		hashedPaths = append(hashedPaths, path)
		return fileSHA256(path)
	}
	if err := os.WriteFile(backupBody, []byte("corrupt backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.RetentionOnce(context.Background())
	if _, err := os.Stat(path); err != nil {
		t.Fatal("retention trusted a missing or corrupt backup body")
	}
	if len(hashedPaths) != 0 {
		t.Fatalf("size-mismatched candidate backup should not be hashed: %#v", hashedPaths)
	}
	if err := os.WriteFile(backupBody, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	hashedPaths = nil
	manager.RetentionOnce(context.Background())
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("eligible managed Evidence was not deleted: %v", err)
	}
	if len(repository.retention) != 2 || repository.retention[0].Status != "planned" || repository.retention[1].Status != "deleted" || len(repository.states) != 1 || repository.states[0] != "retention_deleted" {
		t.Fatalf("retention facts=%#v states=%#v", repository.retention, repository.states)
	}
	if len(hashedPaths) != 2 || hashedPaths[0] != backupBody || hashedPaths[1] != path {
		t.Fatalf("retention hashing was not bounded to candidate backup and source: %#v", hashedPaths)
	}
}

func TestRetentionRecoversDeleteBeforeDatabaseCommit(t *testing.T) {
	cfg := testConfig(t)
	cfg.Retention.Enabled = true
	cfg.Retention.RequireFullBackup = false
	repository := &fakeRepository{planned: true, candidates: []eventstore.RetentionCandidate{{MediaSegmentID: 9, ManagedRelativePath: "accepted/missing.media", SHA256: string(make([]byte, 64)), SizeBytes: 1}}}
	manager := NewWithProbe(cfg, repository, nil, fixedProbe{bytes: uint64(cfg.Storage.CriticalFreeBytes)}, time.Now)
	manager.ScanDiskOnce(context.Background())
	manager.RetentionOnce(context.Background())
	if len(repository.retention) != 1 || repository.retention[0].Status != "deleted" || len(repository.states) != 1 {
		t.Fatalf("delete-before-commit did not converge: %#v %#v", repository.retention, repository.states)
	}
}

func TestRetentionConfirmationFailureLeavesPlanRetryable(t *testing.T) {
	cfg := testConfig(t)
	cfg.Retention.Enabled = true
	cfg.Retention.RequireFullBackup = false
	repository := &fakeRepository{planned: true, commitErr: errors.New("injected transaction failure"), candidates: []eventstore.RetentionCandidate{{MediaSegmentID: 10, ManagedRelativePath: "accepted/missing.media", SHA256: string(make([]byte, 64)), SizeBytes: 1}}}
	manager := NewWithProbe(cfg, repository, nil, fixedProbe{bytes: uint64(cfg.Storage.CriticalFreeBytes)}, time.Now)
	manager.ScanDiskOnce(context.Background())
	manager.RetentionOnce(context.Background())
	if !repository.planned || len(repository.retention) != 0 || len(repository.states) != 0 {
		t.Fatalf("failed confirmation partially committed: planned=%v retention=%#v states=%#v", repository.planned, repository.retention, repository.states)
	}
	repository.commitErr = nil
	manager.RetentionOnce(context.Background())
	if repository.planned || len(repository.retention) != 1 || repository.retention[0].Status != "deleted" || len(repository.states) != 1 {
		t.Fatalf("confirmation retry did not converge: planned=%v retention=%#v states=%#v", repository.planned, repository.retention, repository.states)
	}
}

func TestTemporaryCleanupOnlyRemovesOldUnreferencedCorePartial(t *testing.T) {
	cfg := testConfig(t)
	root := filepath.Join(cfg.MediaStorageDirectory(), "staging")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	unreferenced := string(make([]byte, 64)) + ".partial"
	unreferenced = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.partial"
	referenced := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.partial"
	unknown := "unknown.tmp"
	for _, name := range []string{unreferenced, referenced, unknown} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	repository := &fakeRepository{referenced: map[string]bool{"staging/" + referenced: true}}
	manager := NewWithProbe(cfg, repository, nil, fixedProbe{bytes: 1 << 50}, time.Now)
	manager.CleanupTempOnce(context.Background())
	if _, err := os.Stat(filepath.Join(root, unreferenced)); !os.IsNotExist(err) {
		t.Fatal("unreferenced Core partial was not cleaned")
	}
	for _, name := range []string{referenced, unknown} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("cleanup removed protected file %s", name)
		}
	}
}
