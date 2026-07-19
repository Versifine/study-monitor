package mediaingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Versifine/study-monitor/internal/config"
	"github.com/Versifine/study-monitor/internal/eventstore"
)

type fakeProber struct {
	version string
	info    ProbeInfo
	err     error
}

func (prober fakeProber) Version(context.Context, time.Duration) (string, error) {
	if prober.err != nil {
		return "", prober.err
	}
	if prober.version == "" {
		return SupportedFFprobeVersion, nil
	}
	return prober.version, nil
}

func (prober fakeProber) Probe(context.Context, string, time.Duration) (ProbeInfo, error) {
	if prober.err != nil {
		return ProbeInfo{}, prober.err
	}
	if prober.info.DurationMS == 0 {
		return ProbeInfo{DurationMS: 1000, CodecName: "h264", FormatName: "mov,mp4", MediaType: "video"}, nil
	}
	return prober.info, nil
}

func TestManagerAcceptsReplaysAndPreservesSource(t *testing.T) {
	manager, store, cfg := openTestManager(t, fakeProber{})
	defer store.Close()
	segment := writeSegment(t, cfg.MediaIngest.InboxDirectory, "valid.mp4", []byte(strings.Repeat("valid-video-", 100)), nil)

	accepted, err := manager.ProcessReady(context.Background(), segment.readyPath)
	if err != nil || !accepted {
		staging, _ := filepath.Glob(filepath.Join(manager.stagingRoot, "*"))
		managed, _ := filepath.Glob(filepath.Join(manager.acceptedRoot, "*"))
		t.Fatalf("first ProcessReady() = %t, %v status=%#v staging=%v accepted=%v", accepted, err, manager.Status(context.Background()), staging, managed)
	}
	if _, err := os.Stat(segment.confirmationPath); err != nil {
		t.Fatalf("confirmation missing: %v", err)
	}
	if _, err := os.Stat(segment.mediaPath); err != nil {
		t.Fatalf("source media was removed: %v", err)
	}
	summary, err := store.MediaIngestSummary(context.Background())
	if err != nil || summary.Accepted != 1 || summary.TotalSegments != 1 {
		t.Fatalf("summary after accept = %#v, %v", summary, err)
	}
	confirmationRaw, err := os.ReadFile(segment.confirmationPath)
	if err != nil {
		t.Fatal(err)
	}
	var first confirmation
	if err := json.Unmarshal(confirmationRaw, &first); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(segment.confirmationPath); err != nil {
		t.Fatal(err)
	}
	replayed, err := manager.ProcessReady(context.Background(), segment.readyPath)
	if err != nil || !replayed {
		t.Fatalf("replay ProcessReady() = %t, %v", replayed, err)
	}
	confirmationRaw, err = os.ReadFile(segment.confirmationPath)
	if err != nil {
		t.Fatal(err)
	}
	var second confirmation
	if err := json.Unmarshal(confirmationRaw, &second); err != nil {
		t.Fatal(err)
	}
	if second.MediaSegmentID != first.MediaSegmentID {
		t.Fatalf("replay segment id = %d, want %d", second.MediaSegmentID, first.MediaSegmentID)
	}
	summary, err = store.MediaIngestSummary(context.Background())
	if err != nil || summary.TotalSegments != 1 {
		t.Fatalf("summary after replay = %#v, %v", summary, err)
	}
}

func TestManagerKeepsIncompleteAndGrowingFilesPending(t *testing.T) {
	manager, store, cfg := openTestManager(t, fakeProber{})
	defer store.Close()
	mediaPath := filepath.Join(cfg.MediaIngest.InboxDirectory, "missing-sidecar.mp4")
	if err := os.WriteFile(mediaPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertMediaTableCount(t, store, "media_ingest_events", 0)
	readyPath := mediaPath + ReadySuffix
	if err := os.WriteFile(readyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	accepted, err := manager.ProcessReady(context.Background(), readyPath)
	if err != nil || accepted {
		t.Fatalf("missing sidecar = %t, %v", accepted, err)
	}

	manager.config.MediaIngest.SettleInterval = "100ms"
	segment := writeSegment(t, cfg.MediaIngest.InboxDirectory, "growing.mp4", []byte("initial"), func(sidecar *Sidecar) {
		sidecar.SizeBytes++
		sidecar.SHA256 = hashBytes([]byte("initial!"))
	})
	go func() {
		time.Sleep(20 * time.Millisecond)
		file, openErr := os.OpenFile(segment.mediaPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if openErr == nil {
			_, _ = file.Write([]byte("!"))
			_ = file.Close()
		}
	}()
	accepted, err = manager.ProcessReady(context.Background(), segment.readyPath)
	if err != nil || accepted {
		t.Fatalf("growing media = %t, %v", accepted, err)
	}
	summary, err := store.MediaIngestSummary(context.Background())
	if err != nil || summary.Pending != 2 || summary.TotalSegments != 0 {
		t.Fatalf("pending summary = %#v, %v", summary, err)
	}
}

func TestManagerReportsFilesystemBacklogBytesWithoutDoubleCounting(t *testing.T) {
	manager, store, cfg := openTestManager(t, fakeProber{})
	defer store.Close()
	contents := []byte(strings.Repeat("pending-", 17))
	mediaPath := filepath.Join(cfg.MediaIngest.InboxDirectory, "pending.mp4")
	if err := os.WriteFile(mediaPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaPath+ReadySuffix, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := manager.Status(context.Background())
	if status.FilesystemReadyBacklog != 1 || status.FilesystemReadyBytes != int64(len(contents)) {
		t.Fatalf("filesystem backlog status = %#v", status)
	}
	if status.Ingest.Pending != 1 || status.Ingest.Backlog != 1 {
		t.Fatalf("projection backlog was duplicated: %#v", status.Ingest)
	}
}

func TestManagerQuarantinesCorruptMediaAndPreservesSource(t *testing.T) {
	manager, store, cfg := openTestManager(t, fakeProber{})
	defer store.Close()
	segment := writeSegment(t, cfg.MediaIngest.InboxDirectory, "corrupt.mp4", []byte("corrupt-media"), func(sidecar *Sidecar) {
		sidecar.SHA256 = strings.Repeat("0", 64)
	})
	accepted, err := manager.ProcessReady(context.Background(), segment.readyPath)
	if err != nil || accepted {
		t.Fatalf("corrupt ProcessReady() = %t, %v", accepted, err)
	}
	if _, err := os.Stat(segment.mediaPath); err != nil {
		t.Fatalf("source media was not preserved: %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(manager.quarantineRoot, "*.reason.json")); len(matches) != 1 {
		t.Fatalf("quarantine reason files = %v", matches)
	}
	summary, err := store.MediaIngestSummary(context.Background())
	if err != nil || summary.Quarantined != 1 || summary.TotalSegments != 0 {
		t.Fatalf("quarantine summary = %#v, %v", summary, err)
	}
}

func TestManagerDistinguishesSourceAndMetadataConflicts(t *testing.T) {
	manager, store, cfg := openTestManager(t, fakeProber{})
	defer store.Close()
	firstContents := []byte(strings.Repeat("first-content-", 100))
	first := writeSegment(t, cfg.MediaIngest.InboxDirectory, "first.mp4", firstContents, func(sidecar *Sidecar) {
		sidecar.SourceIdempotencyKey = "stable-source-key"
	})
	if accepted, err := manager.ProcessReady(context.Background(), first.readyPath); err != nil || !accepted {
		t.Fatalf("first ProcessReady() = %t, %v", accepted, err)
	}

	sourceConflict := writeSegment(t, cfg.MediaIngest.InboxDirectory, "source-conflict.mp4", []byte(strings.Repeat("different-content-", 100)), func(sidecar *Sidecar) {
		sidecar.SourceIdempotencyKey = "stable-source-key"
	})
	if accepted, err := manager.ProcessReady(context.Background(), sourceConflict.readyPath); err != nil || accepted {
		t.Fatalf("source conflict ProcessReady() = %t, %v", accepted, err)
	}
	assertQuarantineReason(t, manager.quarantineRoot, CodeIdempotencyConflict)

	metadataConflict := writeSegment(t, cfg.MediaIngest.InboxDirectory, "metadata-conflict.mp4", firstContents, func(sidecar *Sidecar) {
		sidecar.ClockErrorMS = pointerInt64(999)
	})
	if accepted, err := manager.ProcessReady(context.Background(), metadataConflict.readyPath); err != nil || accepted {
		t.Fatalf("metadata conflict ProcessReady() = %t, %v", accepted, err)
	}
	assertQuarantineReason(t, manager.quarantineRoot, CodeMetadataConflict)

	summary, err := store.MediaIngestSummary(context.Background())
	if err != nil || summary.Accepted != 1 || summary.Quarantined != 2 || summary.TotalSegments != 1 {
		t.Fatalf("conflict summary = %#v, %v", summary, err)
	}
}

func TestManagerQuarantinesTruncatedAndInvalidProbeMedia(t *testing.T) {
	tests := []struct {
		name   string
		prober Prober
		mutate func(*Sidecar)
	}{
		{
			name:   "declared size mismatch",
			prober: fakeProber{},
			mutate: func(sidecar *Sidecar) { sidecar.SizeBytes++ },
		},
		{
			name:   "duration one millisecond over limit",
			prober: fakeProber{info: ProbeInfo{DurationMS: int64(10*time.Minute/time.Millisecond) + 1, CodecName: "h264", FormatName: "mov,mp4", MediaType: "video"}},
		},
		{
			name:   "wrong media type",
			prober: fakeProber{info: ProbeInfo{DurationMS: 1000, CodecName: "aac", FormatName: "wav", MediaType: "audio"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, store, cfg := openTestManager(t, test.prober)
			defer store.Close()
			segment := writeSegment(t, cfg.MediaIngest.InboxDirectory, "invalid.mp4", []byte("invalid-probe-media"), test.mutate)
			accepted, err := manager.ProcessReady(context.Background(), segment.readyPath)
			if err != nil || accepted {
				t.Fatalf("ProcessReady() = %t, %v", accepted, err)
			}
			summary, err := store.MediaIngestSummary(context.Background())
			if err != nil || summary.Quarantined != 1 || summary.TotalSegments != 0 {
				t.Fatalf("summary = %#v, %v", summary, err)
			}
		})
	}
}

func TestManagerQuarantinesGenuinelyTruncatedFixture(t *testing.T) {
	ffprobePath := requirePinnedFFprobe(t)
	media := loadPinnedMediaFixture(t)
	if len(media) <= 512 {
		t.Fatalf("pinned fixture is unexpectedly short: %d bytes", len(media))
	}
	truncated := append([]byte(nil), media[:512]...)
	manager, store, cfg := openTestManager(t, ExecProber{Path: ffprobePath})
	defer store.Close()
	segment := writeSegment(t, cfg.MediaIngest.InboxDirectory, "truncated.mp4", truncated, nil)

	accepted, err := manager.ProcessReady(context.Background(), segment.readyPath)
	if err != nil || accepted {
		t.Fatalf("truncated ProcessReady() = %t, %v", accepted, err)
	}
	assertQuarantineReason(t, manager.quarantineRoot, CodeProbeFailed)
	if _, err := os.Stat(segment.mediaPath); err != nil {
		t.Fatalf("truncated source media was not preserved: %v", err)
	}
	summary, err := store.MediaIngestSummary(context.Background())
	if err != nil || summary.Quarantined != 1 || summary.TotalSegments != 0 {
		t.Fatalf("truncated summary = %#v, %v", summary, err)
	}
}

func TestManagerRejectsOversizedAndPathTraversalInputs(t *testing.T) {
	manager, store, cfg := openTestManager(t, fakeProber{})
	defer store.Close()
	manager.config.MediaIngest.MaxSegmentBytes = 1024
	segment := writeSegment(t, cfg.MediaIngest.InboxDirectory, "oversized.mp4", make([]byte, 2048), nil)
	accepted, err := manager.ProcessReady(context.Background(), segment.readyPath)
	if err != nil || accepted {
		t.Fatalf("oversized ProcessReady() = %t, %v", accepted, err)
	}
	summary, err := store.MediaIngestSummary(context.Background())
	if err != nil || summary.Failed != 1 || summary.TotalSegments != 0 {
		t.Fatalf("oversized summary = %#v, %v", summary, err)
	}

	outside := filepath.Join(t.TempDir(), "outside.mp4.ready")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ProcessReady(context.Background(), outside); ErrorCode(err) != CodePathInvalid {
		t.Fatalf("path traversal error = %v", err)
	}
}

func TestManagerRetriesAfterInterruptedCopy(t *testing.T) {
	manager, store, cfg := openTestManager(t, fakeProber{})
	defer store.Close()
	segment := writeSegment(t, cfg.MediaIngest.InboxDirectory, "interrupted.mp4", make([]byte, 160<<10), nil)
	interrupted := false
	manager.afterCopyChunk = func(int64) error {
		if !interrupted {
			interrupted = true
			return errors.New("injected write interruption")
		}
		return nil
	}
	accepted, err := manager.ProcessReady(context.Background(), segment.readyPath)
	if err != nil || accepted {
		t.Fatalf("interrupted ProcessReady() = %t, %v", accepted, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(manager.stagingRoot, "*.partial")); len(matches) != 1 {
		t.Fatalf("staging remnants = %v", matches)
	}
	manager.afterCopyChunk = nil
	accepted, err = manager.ProcessReady(context.Background(), segment.readyPath)
	if err != nil || !accepted {
		t.Fatalf("retry ProcessReady() = %t, %v status=%#v", accepted, err, manager.Status(context.Background()))
	}
	summary, err := store.MediaIngestSummary(context.Background())
	if err != nil || summary.TotalSegments != 1 || summary.Accepted != 1 {
		t.Fatalf("retry summary = %#v, %v", summary, err)
	}
}

func TestManagerRecoversAfterAbruptProcessTermination(t *testing.T) {
	tests := []struct {
		name         string
		mode         string
		exitCode     int
		wantPartial  int
		wantAccepted int
	}{
		{name: "during staging copy", mode: "copy", exitCode: 91, wantPartial: 1},
		{name: "after rename before database commit", mode: "rename", exitCode: 92, wantAccepted: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dataDirectory := filepath.Join(root, "data")
			inboxDirectory := filepath.Join(root, "inbox")
			if err := os.MkdirAll(inboxDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			segment := writeSegment(t, inboxDirectory, "abrupt.mp4", make([]byte, 160<<10), nil)
			command := exec.Command(os.Args[0], "-test.run=^TestMediaIngestCrashHelper$")
			command.Env = append(os.Environ(),
				"EXAM_MONITOR_TEST_MEDIA_CRASH=1",
				"EXAM_MONITOR_TEST_MEDIA_CRASH_MODE="+test.mode,
				"EXAM_MONITOR_TEST_DATA_DIRECTORY="+dataDirectory,
				"EXAM_MONITOR_TEST_INBOX_DIRECTORY="+inboxDirectory,
			)
			output, err := command.CombinedOutput()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != test.exitCode {
				t.Fatalf("crash helper exit = %v, want %d\n%s", err, test.exitCode, output)
			}

			partial, err := filepath.Glob(filepath.Join(dataDirectory, "media", "staging", "*.partial"))
			if err != nil || len(partial) != test.wantPartial {
				t.Fatalf("partial files = %v, %v; want %d", partial, err, test.wantPartial)
			}
			acceptedFiles, err := filepath.Glob(filepath.Join(dataDirectory, "media", "accepted", "*.media"))
			if err != nil || len(acceptedFiles) != test.wantAccepted {
				t.Fatalf("accepted files = %v, %v; want %d", acceptedFiles, err, test.wantAccepted)
			}
			if _, err := os.Stat(segment.confirmationPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("confirmation exists after abrupt exit: %v", err)
			}
			if _, err := os.Stat(segment.mediaPath); err != nil {
				t.Fatalf("source media missing after abrupt exit: %v", err)
			}

			cfg := crashTestConfig(t, dataDirectory, inboxDirectory)
			store := openCrashTestStore(t, cfg)
			defer store.Close()
			beforeRetry, err := store.MediaIngestSummary(context.Background())
			if err != nil || beforeRetry.TotalSegments != 0 {
				t.Fatalf("summary before retry = %#v, %v", beforeRetry, err)
			}
			restarted := newManager(cfg, store, fakeProber{}, nil, fixedNow)
			if err := restarted.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			processed, err := restarted.ProcessReady(context.Background(), segment.readyPath)
			if err != nil || !processed {
				t.Fatalf("restart retry ProcessReady() = %t, %v", processed, err)
			}
			afterRetry, err := store.MediaIngestSummary(context.Background())
			if err != nil || afterRetry.TotalSegments != 1 || afterRetry.Accepted != 1 {
				t.Fatalf("summary after retry = %#v, %v", afterRetry, err)
			}
			if _, err := os.Stat(segment.confirmationPath); err != nil {
				t.Fatalf("confirmation missing after retry: %v", err)
			}
		})
	}
}

func TestMediaIngestCrashHelper(t *testing.T) {
	if os.Getenv("EXAM_MONITOR_TEST_MEDIA_CRASH") != "1" {
		return
	}
	dataDirectory := os.Getenv("EXAM_MONITOR_TEST_DATA_DIRECTORY")
	inboxDirectory := os.Getenv("EXAM_MONITOR_TEST_INBOX_DIRECTORY")
	cfg := crashTestConfig(t, dataDirectory, inboxDirectory)
	store := openCrashTestStore(t, cfg)
	manager := newManager(cfg, store, fakeProber{}, nil, fixedNow)
	if err := manager.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	switch os.Getenv("EXAM_MONITOR_TEST_MEDIA_CRASH_MODE") {
	case "copy":
		manager.afterCopyChunk = func(int64) error {
			os.Exit(91)
			return nil
		}
	case "rename":
		manager.afterRename = func(string) error {
			os.Exit(92)
			return nil
		}
	default:
		t.Fatal("unknown crash helper mode")
	}
	readyPath := filepath.Join(inboxDirectory, "abrupt.mp4"+ReadySuffix)
	_, _ = manager.ProcessReady(context.Background(), readyPath)
	t.Fatal("crash hook was not reached")
}

func TestManagerRecoversTargetRenameFollowedByDatabaseFailure(t *testing.T) {
	manager, store, cfg := openTestManager(t, fakeProber{})
	defer store.Close()
	segment := writeSegment(t, cfg.MediaIngest.InboxDirectory, "db-failure.mp4", []byte(strings.Repeat("database-window", 100)), nil)
	failing := &failAcceptRepository{Repository: store, remaining: 1}
	manager.repository = failing
	accepted, err := manager.ProcessReady(context.Background(), segment.readyPath)
	if err == nil || accepted || ErrorCode(err) != CodeDatabaseFailed {
		t.Fatalf("database failure ProcessReady() = %t, %v status=%#v", accepted, err, manager.Status(context.Background()))
	}
	if matches, _ := filepath.Glob(filepath.Join(manager.acceptedRoot, "*.media")); len(matches) != 1 {
		t.Fatalf("renamed media files = %v", matches)
	}
	assertMediaTableCount(t, store, "media_segments", 0)
	if _, err := os.Stat(segment.confirmationPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmation exists before database commit: %v", err)
	}

	restarted := newManager(cfg, store, fakeProber{}, nil, fixedNow)
	if err := restarted.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	accepted, err = restarted.ProcessReady(context.Background(), segment.readyPath)
	if err != nil || !accepted {
		t.Fatalf("restart recovery ProcessReady() = %t, %v", accepted, err)
	}
	assertMediaTableCount(t, store, "media_segments", 1)
}

func TestManagerExposesAndRecoversRuntimeScanFailure(t *testing.T) {
	manager, store, cfg := openTestManager(t, fakeProber{})
	defer store.Close()
	segment := writeSegment(t, cfg.MediaIngest.InboxDirectory, "scan-recovery.mp4", []byte(strings.Repeat("scan-recovery-", 100)), nil)
	manager.repository = &failAcceptRepository{Repository: store, remaining: 1}

	manager.scanAndLog(context.Background())
	status := manager.Status(context.Background())
	if status.Status != ModuleUnavailable || status.ErrorCode != CodeDatabaseFailed {
		t.Fatalf("failed scan status = %#v", status)
	}
	if _, err := os.Stat(segment.confirmationPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmation exists after failed scan: %v", err)
	}

	manager.scanAndLog(context.Background())
	status = manager.Status(context.Background())
	if status.Status != ModuleHealthy || status.ErrorCode != "" || status.Ingest.Accepted != 1 {
		t.Fatalf("recovered scan status = %#v", status)
	}
	if _, err := os.Stat(segment.confirmationPath); err != nil {
		t.Fatalf("confirmation missing after recovered scan: %v", err)
	}
}

func TestManagerRecordsQuarantineFailureWithoutAccepting(t *testing.T) {
	manager, store, cfg := openTestManager(t, fakeProber{})
	defer store.Close()
	segment := writeSegment(t, cfg.MediaIngest.InboxDirectory, "quarantine-failure.mp4", []byte("broken"), func(sidecar *Sidecar) {
		sidecar.SHA256 = strings.Repeat("f", 64)
	})
	manager.beforeQuarantine = func() error { return errors.New("injected quarantine failure") }
	accepted, err := manager.ProcessReady(context.Background(), segment.readyPath)
	if err == nil || accepted || ErrorCode(err) != CodeQuarantineFailed {
		t.Fatalf("quarantine failure ProcessReady() = %t, %v", accepted, err)
	}
	if _, err := os.Stat(segment.mediaPath); err != nil {
		t.Fatalf("source media was not preserved: %v", err)
	}
	summary, err := store.MediaIngestSummary(context.Background())
	if err != nil || summary.Failed != 1 || summary.TotalSegments != 0 {
		t.Fatalf("quarantine failure summary = %#v, %v", summary, err)
	}
}

func TestManagerRejectsReparsePointInbox(t *testing.T) {
	manager, store, cfg := uninitializedTestManager(t, fakeProber{})
	defer store.Close()
	realInbox := t.TempDir()
	junction := filepath.Join(t.TempDir(), "inbox-link")
	if runtime.GOOS == "windows" {
		command := exec.Command("cmd.exe", "/c", "mklink", "/J", junction, realInbox)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("create junction: %v\n%s", err, output)
		}
	} else if err := os.Symlink(realInbox, junction); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(junction)
	cfg.MediaIngest.InboxDirectory = junction
	manager = newManager(cfg, store, fakeProber{}, nil, fixedNow)
	if err := manager.Initialize(context.Background()); ErrorCode(err) != CodeReparsePoint {
		t.Fatalf("Initialize() error = %v", err)
	}
}

func TestManagerDisablesOnlyMediaOnProbeVersionMismatch(t *testing.T) {
	manager, store, _ := uninitializedTestManager(t, fakeProber{version: "different"})
	defer store.Close()
	if err := manager.Initialize(context.Background()); ErrorCode(err) != CodeProbeVersionMismatch {
		t.Fatalf("Initialize() error = %v", err)
	}
	status := manager.Status(context.Background())
	if status.Status != ModuleUnavailable || status.ErrorCode != CodeProbeVersionMismatch {
		t.Fatalf("status = %#v", status)
	}
}

type failAcceptRepository struct {
	Repository
	remaining int
}

func (repository *failAcceptRepository) AcceptMedia(ctx context.Context, metadata eventstore.MediaMetadata, event eventstore.MediaIngestEvent, stateEventKey string) (eventstore.MediaClaim, error) {
	if repository.remaining > 0 {
		repository.remaining--
		return eventstore.MediaClaim{}, errors.New("injected database failure")
	}
	return repository.Repository.AcceptMedia(ctx, metadata, event, stateEventKey)
}

type segmentFixture struct {
	mediaPath        string
	readyPath        string
	confirmationPath string
}

func writeSegment(t *testing.T, inbox, name string, contents []byte, mutate func(*Sidecar)) segmentFixture {
	t.Helper()
	mediaPath := filepath.Join(inbox, name)
	if err := os.WriteFile(mediaPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	offset, clockError := int64(0), int64(10)
	sidecar := Sidecar{
		SchemaVersion:        1,
		Complete:             true,
		CollectorID:          "desk.camera",
		SourceIdempotencyKey: name + "-source-key",
		DeviceStartRaw:       "2026-07-18T10:00:00+08:00",
		DeviceEndRaw:         "2026-07-18T10:00:01+08:00",
		ClockOffsetMS:        &offset,
		ClockErrorMS:         &clockError,
		SizeBytes:            int64(len(contents)),
		SHA256:               hex.EncodeToString(digest[:]),
		MediaType:            "video",
	}
	if mutate != nil {
		mutate(&sidecar)
	}
	raw, err := json.Marshal(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaPath+SidecarSuffix, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	readyPath := mediaPath + ReadySuffix
	if err := os.WriteFile(readyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return segmentFixture{mediaPath: mediaPath, readyPath: readyPath, confirmationPath: mediaPath + ConfirmationSuffix}
}

func openTestManager(t *testing.T, prober Prober) (*Manager, *eventstore.Store, config.Config) {
	t.Helper()
	manager, store, cfg := uninitializedTestManager(t, prober)
	if err := manager.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	return manager, store, cfg
}

func uninitializedTestManager(t *testing.T, prober Prober) (*Manager, *eventstore.Store, config.Config) {
	t.Helper()
	localAppData := t.TempDir()
	cfg, err := config.Load("", func(key string) (string, bool) {
		if key == "LOCALAPPDATA" {
			return localAppData, true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.MediaIngest.Enabled = true
	cfg.MediaIngest.InboxDirectory = filepath.Join(t.TempDir(), "inbox")
	cfg.MediaIngest.SettleInterval = "1ms"
	cfg.MediaIngest.ScanInterval = "10ms"
	cfg.MediaIngest.FFprobePath = filepath.Join(t.TempDir(), "ffprobe.exe")
	store, err := eventstore.Open(context.Background(), cfg.DatabasePath(), eventstore.Options{
		BusyTimeout:        5 * time.Second,
		MaxOpenConnections: 8,
		MaxBatchEvents:     100,
		MaxEventBytes:      64 << 10,
		MaxPayloadDepth:    16,
		MaxPageSize:        500,
		Now:                fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	return newManager(cfg, store, prober, nil, fixedNow), store, cfg
}

func crashTestConfig(t *testing.T, dataDirectory, inboxDirectory string) config.Config {
	t.Helper()
	cfg, err := config.Load("", func(key string) (string, bool) {
		if key == "LOCALAPPDATA" {
			return filepath.Dir(dataDirectory), true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Paths.DataDirectory = dataDirectory
	cfg.MediaIngest.Enabled = true
	cfg.MediaIngest.InboxDirectory = inboxDirectory
	cfg.MediaIngest.SettleInterval = "1ms"
	cfg.MediaIngest.ScanInterval = "10ms"
	cfg.MediaIngest.FFprobePath = filepath.Join(filepath.Dir(dataDirectory), "ffprobe.exe")
	return cfg
}

func openCrashTestStore(t *testing.T, cfg config.Config) *eventstore.Store {
	t.Helper()
	store, err := eventstore.Open(context.Background(), cfg.DatabasePath(), eventstore.Options{
		BusyTimeout:        5 * time.Second,
		MaxOpenConnections: 8,
		MaxBatchEvents:     100,
		MaxEventBytes:      64 << 10,
		MaxPayloadDepth:    16,
		MaxPageSize:        500,
		Now:                fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func fixedNow() time.Time {
	return time.Date(2026, 7, 18, 3, 4, 5, 0, time.UTC)
}

func pointerInt64(value int64) *int64 { return &value }

func assertQuarantineReason(t *testing.T, root, want string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "*.reason.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var reason struct {
			ReasonCode string `json:"reason_code"`
		}
		if err := json.Unmarshal(raw, &reason); err != nil {
			t.Fatal(err)
		}
		if reason.ReasonCode == want {
			return
		}
	}
	t.Fatalf("quarantine reason %s not found in %v", want, paths)
}

func assertMediaTableCount(t *testing.T, store *eventstore.Store, table string, want int) {
	t.Helper()
	summary, err := store.MediaIngestSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	switch table {
	case "media_segments":
		if summary.TotalSegments != int64(want) {
			t.Fatalf("%s count = %d, want %d", table, summary.TotalSegments, want)
		}
	case "media_ingest_events":
		// Projection count is sufficient for the zero-fact assertion in this package.
		if summary.Backlog+summary.Accepted+summary.Quarantined+summary.Failed != int64(want) {
			t.Fatalf("%s current count mismatch: %#v", table, summary)
		}
	default:
		t.Fatalf("unsupported table assertion %q", table)
	}
}
