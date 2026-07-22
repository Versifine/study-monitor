package operations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Versifine/study-monitor/internal/config"
	"github.com/Versifine/study-monitor/internal/eventstore"
	"github.com/Versifine/study-monitor/internal/logging"
)

const (
	CodeDiskProbeFailed  = "STORAGE_DISK_PROBE_FAILED"
	CodeDiskWarning      = "STORAGE_DISK_WARNING"
	CodeDiskCritical     = "STORAGE_DISK_CRITICAL"
	CodeDiskReserve      = "STORAGE_DATABASE_RESERVE_THREATENED"
	CodeRetentionOff     = "RETENTION_DISABLED"
	CodeBackupMissing    = "RETENTION_FULL_BACKUP_REQUIRED"
	CodeBackupInvalid    = "RETENTION_BACKUP_INVALID"
	CodeRetentionCommit  = "RETENTION_CONFIRM_FAILED"
	CodeCleanupFailed    = "TEMP_CLEANUP_FAILED"
	CodeCheckpointFailed = "WAL_CHECKPOINT_FAILED"
)

type Repository interface {
	AppendFaultEvent(context.Context, eventstore.FaultEvent) error
	AppendModuleStateEvent(context.Context, eventstore.ModuleStateEvent) error
	AppendModeTransition(context.Context, string, string, string, string, string) error
	RetentionCandidates(context.Context, string, int) ([]eventstore.RetentionCandidate, error)
	AppendRetentionEvent(context.Context, eventstore.RetentionEvent) error
	RetentionDeletionPlanned(context.Context, int64, string) (bool, error)
	CommitRetentionDeletion(context.Context, eventstore.RetentionEvent) error
	TemporaryPathReferenced(context.Context, string) (bool, error)
	Checkpoint(context.Context, bool) error
}

type Status struct {
	SchemaVersion int              `json:"schema_version"`
	DiskLevel     string           `json:"disk_level"`
	FreeBytes     uint64           `json:"free_bytes"`
	ErrorCode     string           `json:"error_code,omitempty"`
	CheckedAtUTC  string           `json:"checked_at_utc,omitempty"`
	Retention     string           `json:"retention"`
	Runtime       RuntimeResources `json:"runtime"`
}

type RuntimeResources struct {
	Goroutines         int    `json:"goroutines"`
	HeapAllocBytes     uint64 `json:"heap_alloc_bytes"`
	HeapInUseBytes     uint64 `json:"heap_in_use_bytes"`
	StackInUseBytes    uint64 `json:"stack_in_use_bytes"`
	RuntimeSystemBytes uint64 `json:"runtime_system_bytes"`
}

type Manager struct {
	cfg               config.Config
	repository        Repository
	logger            *logging.Logger
	probe             DiskProbe
	now               func() time.Time
	mu                sync.RWMutex
	status            Status
	reportedDiskLevel string
	hashFile          func(string) (string, error)
}

func New(cfg config.Config, repository Repository, logger *logging.Logger) *Manager {
	return NewWithProbe(cfg, repository, logger, platformDiskProbe{}, time.Now)
}

func NewWithProbe(cfg config.Config, repository Repository, logger *logging.Logger, probe DiskProbe, now func() time.Time) *Manager {
	retention := "disabled"
	if cfg.Retention.Enabled {
		retention = "enabled"
	}
	return &Manager{cfg: cfg, repository: repository, logger: logger, probe: probe, now: now, hashFile: fileSHA256,
		status: Status{SchemaVersion: 1, DiskLevel: DiskNormal, Retention: retention}, reportedDiskLevel: DiskNormal}
}

func (manager *Manager) Status(context.Context) Status {
	manager.mu.RLock()
	status := manager.status
	manager.mu.RUnlock()
	return WithRuntimeResources(status)
}

// WithRuntimeResources adds bounded, read-only runtime counters required by
// the M6 stability certification. It does not affect readiness or write gates.
func WithRuntimeResources(status Status) Status {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	status.Runtime = RuntimeResources{
		Goroutines:         runtime.NumGoroutine(),
		HeapAllocBytes:     memory.HeapAlloc,
		HeapInUseBytes:     memory.HeapInuse,
		StackInUseBytes:    memory.StackInuse,
		RuntimeSystemBytes: memory.Sys,
	}
	return status
}

func (manager *Manager) MediaAllowed() (bool, string) {
	level, code, _ := manager.refreshDiskStatus()
	switch level {
	case DiskNormal:
		return true, ""
	case DiskWarning:
		return false, code
	case DiskCritical:
		return false, code
	default:
		return false, code
	}
}

func (manager *Manager) CoreWritesAllowed() (bool, string) {
	level, code, _ := manager.refreshDiskStatus()
	if level == DiskReserve {
		return false, code
	}
	return true, ""
}

func (manager *Manager) Initialize(ctx context.Context) {
	now := manager.now().UTC().Format(time.RFC3339Nano)
	reason := CodeRetentionOff
	state := "disabled"
	if manager.cfg.Retention.Enabled {
		reason, state = "RETENTION_ENABLED", "healthy"
	}
	manager.appendModule(ctx, "retention", state, reason, now)
	manager.ScanDiskOnce(ctx)
}

func (manager *Manager) RecordFault(ctx context.Context, module, severity, status, code, detail string) {
	manager.appendFault(ctx, module, severity, status, code, detail, manager.now().UTC().Format(time.RFC3339Nano))
}

func (manager *Manager) RecordModuleState(ctx context.Context, module, status, reason string) {
	manager.appendModule(ctx, module, status, reason, manager.now().UTC().Format(time.RFC3339Nano))
}

func (manager *Manager) RecordRuntimeMode(ctx context.Context, mode, operator, trigger, reason string) {
	if manager.repository == nil {
		return
	}
	now := manager.now().UTC().Format(time.RFC3339Nano)
	if err := manager.repository.AppendModeTransition(ctx, mode, operator, trigger, reason, now); err != nil {
		manager.recordError(ctx, "runtime_mode", "P2", "MODE_TRANSITION_WRITE_FAILED", err)
	}
}

func (manager *Manager) Run(ctx context.Context) {
	disk := time.NewTicker(manager.cfg.DiskCheckInterval())
	defer disk.Stop()
	checkpoint := time.NewTicker(manager.cfg.WALCheckpointInterval())
	defer checkpoint.Stop()
	cleanup := time.NewTicker(manager.cfg.TempCleanupInterval())
	defer cleanup.Stop()
	retention := time.NewTicker(manager.cfg.RetentionScanInterval())
	defer retention.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-disk.C:
			manager.ScanDiskOnce(ctx)
		case <-checkpoint.C:
			manager.CheckpointOnce(ctx)
		case <-cleanup.C:
			manager.CleanupTempOnce(ctx)
		case <-retention.C:
			manager.RetentionOnce(ctx)
		}
	}
}

func (manager *Manager) ScanDiskOnce(ctx context.Context) {
	level, code, probeErr := manager.refreshDiskStatus()
	manager.mu.Lock()
	previous := manager.reportedDiskLevel
	manager.reportedDiskLevel = level
	available := manager.status.FreeBytes
	now := manager.status.CheckedAtUTC
	manager.mu.Unlock()
	if previous != level || probeErr != nil {
		severity, status := "P3", "recovered"
		if level == DiskWarning {
			severity, status = "P2", "degraded"
		}
		if level == DiskCritical {
			severity, status = "P1", "active"
		}
		if level == DiskReserve {
			severity, status = "P0", "active"
		}
		if code == "" {
			code = "STORAGE_DISK_RECOVERED"
		}
		manager.appendFault(ctx, "storage", severity, status, code, fmt.Sprintf("free_bytes=%d", available), now)
	}
}

func (manager *Manager) refreshDiskStatus() (string, string, error) {
	available, err := manager.probe.FreeBytes(manager.cfg.Paths.DataDirectory)
	now := manager.now().UTC().Format(time.RFC3339Nano)
	level, code := DiskNormal, ""
	if err != nil {
		level, code = DiskWarning, CodeDiskProbeFailed
	} else {
		switch {
		case available <= uint64(manager.cfg.Storage.DatabaseReserveBytes):
			level, code = DiskReserve, CodeDiskReserve
		case available <= uint64(manager.cfg.Storage.CriticalFreeBytes):
			level, code = DiskCritical, CodeDiskCritical
		case available <= uint64(manager.cfg.Storage.WarningFreeBytes):
			level, code = DiskWarning, CodeDiskWarning
		}
	}
	manager.mu.Lock()
	manager.status.DiskLevel, manager.status.FreeBytes, manager.status.ErrorCode, manager.status.CheckedAtUTC = level, available, code, now
	manager.mu.Unlock()
	return level, code, err
}

func (manager *Manager) CheckpointOnce(ctx context.Context) {
	wal := manager.cfg.DatabasePath() + "-wal"
	truncate := false
	if info, err := os.Stat(wal); err == nil {
		truncate = info.Size() > manager.cfg.Operations.WALMaxBytes
	}
	if err := manager.repository.Checkpoint(ctx, truncate); err != nil {
		manager.recordError(ctx, "storage", "P2", CodeCheckpointFailed, err)
	}
}

var partialName = regexp.MustCompile(`^[0-9a-f]{64}\.partial$`)

func (manager *Manager) CleanupTempOnce(ctx context.Context) {
	root := filepath.Join(manager.cfg.MediaStorageDirectory(), "staging")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		manager.recordError(ctx, "media_ingest", "P2", CodeCleanupFailed, err)
		return
	}
	cutoff := manager.now().Add(-manager.cfg.TempMaxAge())
	removed := 0
	for _, entry := range entries {
		if removed >= manager.cfg.Operations.TempMaxFiles || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !partialName.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		relative := filepath.ToSlash(filepath.Join("staging", entry.Name()))
		referenced, err := manager.repository.TemporaryPathReferenced(ctx, relative)
		if err != nil || referenced {
			continue
		}
		if err := os.Remove(filepath.Join(root, entry.Name())); err == nil {
			removed++
		}
	}
}

func (manager *Manager) RetentionOnce(ctx context.Context) {
	if !manager.cfg.Retention.Enabled {
		return
	}
	allowed, _ := manager.MediaAllowed()
	if allowed {
		return
	}
	coverage, err := manager.loadBackupCoverage()
	if err != nil {
		manager.recordError(ctx, "retention", "P1", CodeBackupInvalid, err)
		return
	}
	cutoff := manager.now().Add(-manager.cfg.RetentionMinimumAge()).UTC().Format(time.RFC3339Nano)
	candidates, err := manager.repository.RetentionCandidates(ctx, cutoff, manager.cfg.Retention.MaxDeletesPerRun)
	if err != nil {
		manager.recordError(ctx, "retention", "P1", "RETENTION_QUERY_FAILED", err)
		return
	}
	acceptedRoot := filepath.Clean(filepath.Join(manager.cfg.MediaStorageDirectory(), "accepted"))
	for _, candidate := range candidates {
		clean := filepath.Clean(filepath.FromSlash(candidate.ManagedRelativePath))
		path := filepath.Clean(filepath.Join(manager.cfg.MediaStorageDirectory(), clean))
		rel, relErr := filepath.Rel(acceptedRoot, path)
		if relErr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if manager.cfg.Retention.RequireFullBackup {
			backup, covered := coverage[candidate.ManagedRelativePath]
			if !covered || backup.SHA256 != strings.ToLower(candidate.SHA256) || backup.SizeBytes != candidate.SizeBytes || !manager.backupBodyVerified(backup) {
				continue
			}
		}
		plannedBefore, planErr := manager.repository.RetentionDeletionPlanned(ctx, candidate.MediaSegmentID, candidate.ManagedRelativePath)
		if planErr != nil {
			continue
		}
		info, statErr := os.Lstat(path)
		if errors.Is(statErr, os.ErrNotExist) && plannedBefore {
			if err := manager.confirmRetentionDeletion(ctx, candidate); err != nil {
				manager.recordError(ctx, "retention", "P1", CodeRetentionCommit, err)
			}
			continue
		}
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != candidate.SizeBytes {
			continue
		}
		actualHash, hashErr := manager.hashFile(path)
		if hashErr != nil || actualHash != candidate.SHA256 {
			continue
		}
		now := manager.now().UTC().Format(time.RFC3339Nano)
		planned := eventstore.RetentionEvent{MediaSegmentID: candidate.MediaSegmentID, Status: "planned", ReasonCode: "RETENTION_POLICY", ManagedRelativePath: candidate.ManagedRelativePath, OccurredAtUTC: now}
		if err := manager.repository.AppendRetentionEvent(ctx, planned); err != nil {
			continue
		}
		if err := os.Remove(path); err != nil {
			manager.recordRetentionFailure(ctx, candidate, err)
			continue
		}
		if err := manager.confirmRetentionDeletion(ctx, candidate); err != nil {
			manager.recordError(ctx, "retention", "P1", CodeRetentionCommit, err)
		}
	}
}

func (manager *Manager) confirmRetentionDeletion(ctx context.Context, candidate eventstore.RetentionCandidate) error {
	deletedAt := manager.now().UTC().Format(time.RFC3339Nano)
	return manager.repository.CommitRetentionDeletion(ctx, eventstore.RetentionEvent{MediaSegmentID: candidate.MediaSegmentID, Status: "deleted", ReasonCode: "RETENTION_POLICY", ManagedRelativePath: candidate.ManagedRelativePath, OccurredAtUTC: deletedAt})
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

type backupMarker struct {
	SchemaVersion  int    `json:"schema_version"`
	ManifestPath   string `json:"manifest_path"`
	ManifestSHA256 string `json:"manifest_sha256"`
}
type backupFile struct {
	RelativePath string `json:"relative_path"`
	BackupPath   string `json:"backup_path"`
	SHA256       string `json:"sha256"`
	Kind         string `json:"kind"`
	SizeBytes    int64  `json:"size_bytes"`
	Included     bool   `json:"included"`
}
type backupManifest struct {
	SchemaVersion int          `json:"schema_version"`
	Type          string       `json:"type"`
	Files         []backupFile `json:"files"`
}

type backupCoverage struct {
	BackupPath string
	SHA256     string
	SizeBytes  int64
}

func (manager *Manager) loadBackupCoverage() (map[string]backupCoverage, error) {
	if !manager.cfg.Retention.RequireFullBackup {
		return map[string]backupCoverage{}, nil
	}
	markerPath := filepath.Join(manager.cfg.Paths.DataDirectory, "backup", "latest-full.json")
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", CodeBackupMissing, err)
	}
	var marker backupMarker
	if err := json.Unmarshal(raw, &marker); err != nil || marker.SchemaVersion != 1 || !filepath.IsAbs(marker.ManifestPath) || len(marker.ManifestSHA256) != 64 {
		return nil, errors.New("backup marker is invalid")
	}
	manifestInfo, err := os.Lstat(marker.ManifestPath)
	if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("backup manifest is not a regular file")
	}
	manifestRaw, err := os.ReadFile(marker.ManifestPath)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(manifestRaw)
	if hex.EncodeToString(digest[:]) != strings.ToLower(marker.ManifestSHA256) {
		return nil, errors.New("backup manifest checksum mismatch")
	}
	var manifest backupManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil || manifest.SchemaVersion != 1 || manifest.Type != "full" {
		return nil, errors.New("full backup manifest is invalid")
	}
	coverage := make(map[string]backupCoverage)
	for _, file := range manifest.Files {
		if file.Kind != "media" || !file.Included || len(file.SHA256) != 64 || file.SizeBytes < 1 || file.BackupPath == "" {
			continue
		}
		relativePath := filepath.ToSlash(filepath.Clean(filepath.FromSlash(file.RelativePath)))
		if relativePath != file.RelativePath || relativePath == "." || strings.HasPrefix(relativePath, "../") || !strings.HasPrefix(relativePath, "accepted/") {
			continue
		}
		clean := filepath.Clean(filepath.FromSlash(file.BackupPath))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			continue
		}
		backupPath := filepath.Join(filepath.Dir(marker.ManifestPath), clean)
		if _, duplicate := coverage[relativePath]; duplicate {
			return nil, errors.New("full backup manifest has duplicate media path")
		}
		coverage[relativePath] = backupCoverage{BackupPath: backupPath, SHA256: strings.ToLower(file.SHA256), SizeBytes: file.SizeBytes}
	}
	return coverage, nil
}

func (manager *Manager) backupBodyVerified(backup backupCoverage) bool {
	info, err := os.Lstat(backup.BackupPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != backup.SizeBytes {
		return false
	}
	actual, err := manager.hashFile(backup.BackupPath)
	return err == nil && actual == backup.SHA256
}

func (manager *Manager) recordRetentionFailure(ctx context.Context, candidate eventstore.RetentionCandidate, err error) {
	now := manager.now().UTC().Format(time.RFC3339Nano)
	_ = manager.repository.AppendRetentionEvent(ctx, eventstore.RetentionEvent{MediaSegmentID: candidate.MediaSegmentID, Status: "failed", ReasonCode: "RETENTION_DELETE_FAILED", ManagedRelativePath: candidate.ManagedRelativePath, OccurredAtUTC: now})
	manager.recordError(ctx, "retention", "P1", "RETENTION_DELETE_FAILED", err)
}

func (manager *Manager) recordError(ctx context.Context, module, severity, code string, err error) {
	now := manager.now().UTC().Format(time.RFC3339Nano)
	manager.appendFault(ctx, module, severity, "active", code, err.Error(), now)
	if manager.logger != nil {
		manager.logger.Error(module, "operations_fault", code, "operations module fault", err)
	}
}

func (manager *Manager) appendFault(ctx context.Context, module, severity, status, code, detail, now string) {
	detail = truncateRunes(detail, 1024)
	if manager.repository != nil {
		_ = manager.repository.AppendFaultEvent(ctx, eventstore.FaultEvent{Module: module, Severity: severity, Status: status, ErrorCode: code, Detail: detail, OccurredAtUTC: now})
	}
	if manager.logger != nil && status != "recovered" {
		manager.logger.Warn(module, "degraded", code, "module entered a protected state", slog.String("detail", detail))
	}
}

func truncateRunes(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum])
}

func (manager *Manager) appendModule(ctx context.Context, module, status, reason, now string) {
	if manager.repository != nil {
		_ = manager.repository.AppendModuleStateEvent(ctx, eventstore.ModuleStateEvent{Module: module, Status: status, ReasonCode: reason, OccurredAtUTC: now})
	}
}
