package mediaingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Versifine/study-monitor/internal/config"
	"github.com/Versifine/study-monitor/internal/eventstore"
	"github.com/Versifine/study-monitor/internal/logging"
	"github.com/Versifine/study-monitor/internal/strictjson"
)

type Manager struct {
	config     config.Config
	repository Repository
	prober     Prober
	logger     *logging.Logger
	now        func() time.Time
	state      *statusState

	storageRoot    string
	stagingRoot    string
	acceptedRoot   string
	quarantineRoot string

	afterCopyChunk   func(int64) error
	afterRename      func(string) error
	beforeQuarantine func() error
}

type processIdentity struct {
	ingestKey            string
	sourceName           string
	sourceFingerprint    string
	collectorID          string
	sourceIdempotencyKey string
}

type confirmation struct {
	SchemaVersion        int    `json:"schema_version"`
	CollectorID          string `json:"collector_id"`
	SourceIdempotencyKey string `json:"source_idempotency_key"`
	MediaSegmentID       int64  `json:"media_segment_id"`
	SHA256               string `json:"sha256"`
	MetadataHash         string `json:"metadata_hash"`
	AcceptedAtUTC        string `json:"accepted_at_utc"`
}

var confirmationFields = []string{
	"schema_version", "collector_id", "source_idempotency_key", "media_segment_id",
	"sha256", "metadata_hash", "accepted_at_utc",
}

func New(cfg config.Config, repository Repository, logger *logging.Logger) *Manager {
	return newManager(cfg, repository, ExecProber{Path: cfg.MediaIngest.FFprobePath}, logger, time.Now)
}

func newManager(cfg config.Config, repository Repository, prober Prober, logger *logging.Logger, now func() time.Time) *Manager {
	state := ModuleDisabled
	if cfg.MediaIngest.Enabled {
		state = ModuleUnavailable
	}
	storageRoot := cfg.MediaStorageDirectory()
	return &Manager{
		config: cfg, repository: repository, prober: prober, logger: logger, now: now,
		state:          newStatus(state, ""),
		storageRoot:    storageRoot,
		stagingRoot:    filepath.Join(storageRoot, "staging"),
		acceptedRoot:   filepath.Join(storageRoot, "accepted"),
		quarantineRoot: filepath.Join(storageRoot, "quarantine"),
	}
}

func (manager *Manager) Initialize(ctx context.Context) error {
	if !manager.config.MediaIngest.Enabled {
		return nil
	}
	if manager.repository == nil || manager.prober == nil {
		return manager.setUnavailable(CodeDatabaseFailed, errors.New("media ingest dependencies are unavailable"))
	}
	for _, directory := range []string{
		manager.config.MediaIngest.InboxDirectory,
		manager.storageRoot,
		manager.stagingRoot,
		manager.acceptedRoot,
		manager.quarantineRoot,
	} {
		if err := secureMkdirAll(directory); err != nil {
			return manager.setUnavailable(ErrorCode(err), err)
		}
	}
	version, err := manager.prober.Version(ctx, manager.config.FFprobeTimeout())
	if err != nil {
		return manager.setUnavailable(ErrorCode(err), err)
	}
	if version != SupportedFFprobeVersion {
		return manager.setUnavailable(CodeProbeVersionMismatch, fmt.Errorf("unsupported ffprobe version %q", version))
	}
	if err := manager.repository.RebuildMediaProjections(ctx); err != nil {
		return manager.setUnavailable(CodeDatabaseFailed, err)
	}
	manager.state.Lock()
	manager.state.status.Status = ModuleHealthy
	manager.state.status.ErrorCode = ""
	manager.state.initialized = true
	manager.state.Unlock()
	return nil
}

func (manager *Manager) setUnavailable(code string, err error) error {
	manager.state.Lock()
	manager.state.status.Status = ModuleUnavailable
	manager.state.status.ErrorCode = code
	manager.state.Unlock()
	return &Error{Code: code, Err: err}
}

func (manager *Manager) Run(ctx context.Context) {
	if manager.state.snapshot().Status != ModuleHealthy {
		return
	}
	manager.scanAndLog(ctx)
	ticker := time.NewTicker(manager.config.MediaScanInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			manager.scanAndLog(ctx)
		}
	}
}

func (manager *Manager) scanAndLog(ctx context.Context) {
	err := manager.ScanOnce(ctx)
	if ctx.Err() != nil {
		return
	}
	manager.state.Lock()
	if err != nil {
		manager.state.status.Status = ModuleUnavailable
		manager.state.status.ErrorCode = ErrorCode(err)
	} else {
		manager.state.status.Status = ModuleHealthy
		manager.state.status.ErrorCode = ""
	}
	manager.state.Unlock()
	if err != nil && manager.logger != nil {
		manager.logger.Error("media_ingest", "scan_failed", ErrorCode(err), "media ingest scan failed", err)
	}
}

func (manager *Manager) Status(ctx context.Context) Status {
	status := manager.state.snapshot()
	if manager.repository == nil {
		return status
	}
	summary, err := manager.repository.MediaIngestSummary(ctx)
	if err != nil {
		status.Status = ModuleUnavailable
		status.ErrorCode = CodeDatabaseFailed
		return status
	}
	status.Ingest = summary
	return status
}

func (manager *Manager) ScanOnce(ctx context.Context) error {
	if !manager.state.isInitialized() {
		return &Error{Code: CodeProbeUnavailable, Err: errors.New("media ingest module is not healthy")}
	}
	directory, err := os.Open(manager.config.MediaIngest.InboxDirectory)
	if err != nil {
		return &Error{Code: CodeStorageFailed, Err: errors.New("media inbox cannot be opened")}
	}
	defer directory.Close()
	processed := 0
	remaining := int64(0)
	remainingBytes := int64(0)
	var firstError error
	for {
		entries, err := directory.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return &Error{Code: CodeStorageFailed, Err: errors.New("media inbox cannot be enumerated")}
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ReadySuffix) {
				continue
			}
			readyPath := filepath.Join(manager.config.MediaIngest.InboxDirectory, entry.Name())
			confirmed, confirmationErr := manager.confirmationValid(ctx, strings.TrimSuffix(readyPath, ReadySuffix)+ConfirmationSuffix)
			if confirmationErr == nil && confirmed {
				continue
			}
			if processed >= manager.config.MediaIngest.MaxScanEntries {
				remaining++
				remainingBytes = saturatedAdd(remainingBytes, manager.readySourceSize(readyPath))
				continue
			}
			processed++
			accepted, processErr := manager.ProcessReady(ctx, readyPath)
			if processErr != nil || !accepted {
				remaining++
				remainingBytes = saturatedAdd(remainingBytes, manager.readySourceSize(readyPath))
			}
			if processErr != nil && firstError == nil {
				firstError = processErr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	manager.state.Lock()
	manager.state.status.LastScanUTC = manager.now().UTC().Format(time.RFC3339Nano)
	manager.state.status.FilesystemReadyBacklog = remaining
	manager.state.status.FilesystemReadyBytes = remainingBytes
	manager.state.Unlock()
	return firstError
}

func (manager *Manager) readySourceSize(readyPath string) int64 {
	mediaPath := strings.TrimSuffix(readyPath, ReadySuffix)
	if err := ensureSafePath(manager.config.MediaIngest.InboxDirectory, mediaPath, true); err != nil {
		return 0
	}
	info, err := os.Lstat(mediaPath)
	if err != nil || info.Size() <= 0 {
		return 0
	}
	return info.Size()
}

func saturatedAdd(left, right int64) int64 {
	if right <= 0 {
		return left
	}
	if left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func (manager *Manager) ProcessReady(ctx context.Context, readyPath string) (bool, error) {
	if err := ensureSafePath(manager.config.MediaIngest.InboxDirectory, readyPath, true); err != nil {
		return false, err
	}
	if !strings.HasSuffix(filepath.Base(readyPath), ReadySuffix) {
		return false, &Error{Code: CodePathInvalid, Err: errors.New("media ready marker suffix is invalid")}
	}
	sourceName := strings.TrimSuffix(filepath.Base(readyPath), ReadySuffix)
	if sourceName == "" || len(sourceName) > 512 {
		return false, &Error{Code: CodePathInvalid, Err: errors.New("media source filename is invalid")}
	}
	mediaPath := filepath.Join(manager.config.MediaIngest.InboxDirectory, sourceName)
	sidecarPath := mediaPath + SidecarSuffix
	confirmationPath := mediaPath + ConfirmationSuffix
	identity := processIdentity{
		ingestKey:         hashText(strings.ToLower(filepath.Clean(manager.config.MediaIngest.InboxDirectory)) + "\x00" + sourceName),
		sourceName:        sourceName,
		sourceFingerprint: hashText(sourceName),
	}
	if err := manager.record(ctx, identity, "discovered", "", "", 0); err != nil {
		return false, err
	}

	sidecarRaw, err := manager.readInboxFile(sidecarPath, manager.config.MediaIngest.MaxSidecarBytes)
	if errors.Is(err, os.ErrNotExist) {
		return false, manager.record(ctx, identity, "pending", CodeSidecarMissing, "", 0)
	}
	if err != nil {
		return false, manager.record(ctx, identity, "failed", ErrorCode(err), "", 0)
	}
	identity.sourceFingerprint = hashBytes(sidecarRaw)
	sidecar, err := parseSidecar(sidecarRaw, manager.config.MediaMaxSegmentDuration())
	if err != nil {
		if ErrorCode(err) == CodeSidecarIncomplete {
			return false, manager.record(ctx, identity, "pending", CodeSidecarIncomplete, "", 0)
		}
		return false, manager.quarantine(ctx, identity, mediaPath, sidecarRaw, "", ErrorCode(err))
	}
	identity.sourceFingerprint = sidecar.Fingerprint
	identity.collectorID = sidecar.CollectorID
	identity.sourceIdempotencyKey = sidecar.SourceIdempotencyKey

	first, err := manager.stableSourceStat(mediaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, manager.record(ctx, identity, "pending", CodeSourceMissing, "", 0)
		}
		return false, manager.record(ctx, identity, "failed", ErrorCode(err), "", 0)
	}
	if first.Size() > manager.config.MediaIngest.MaxSegmentBytes || sidecar.SizeBytes > manager.config.MediaIngest.MaxSegmentBytes {
		return false, manager.record(ctx, identity, "failed", CodeTooLarge, "", 0)
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(manager.config.MediaSettleInterval()):
	}
	second, err := manager.stableSourceStat(mediaPath)
	if err != nil {
		return false, manager.record(ctx, identity, "pending", CodeSourceMissing, "", 0)
	}
	if first.Size() != second.Size() || !first.ModTime().Equal(second.ModTime()) {
		return false, manager.record(ctx, identity, "pending", CodeFileGrowing, "", 0)
	}
	if second.Size() != sidecar.SizeBytes {
		return false, manager.quarantine(ctx, identity, mediaPath, sidecarRaw, "", CodeSizeMismatch)
	}

	stagingPath := filepath.Join(manager.stagingRoot, identity.ingestKey+".partial")
	copyResult, err := manager.copyToStaging(mediaPath, stagingPath)
	if err != nil {
		return false, manager.record(ctx, identity, "failed", ErrorCode(err), manager.relativeManaged(stagingPath), 0)
	}
	if copyResult.size != sidecar.SizeBytes {
		return false, manager.quarantine(ctx, identity, mediaPath, sidecarRaw, stagingPath, CodeSizeMismatch)
	}
	if copyResult.sha256 != sidecar.SHA256 {
		return false, manager.quarantine(ctx, identity, mediaPath, sidecarRaw, stagingPath, CodeHashMismatch)
	}
	probe, err := manager.prober.Probe(ctx, stagingPath, manager.config.FFprobeTimeout())
	if err != nil {
		return false, manager.quarantine(ctx, identity, mediaPath, sidecarRaw, stagingPath, ErrorCode(err))
	}
	if probe.MediaType != sidecar.MediaType {
		return false, manager.quarantine(ctx, identity, mediaPath, sidecarRaw, stagingPath, CodeTypeInvalid)
	}
	if time.Duration(probe.DurationMS)*time.Millisecond > manager.config.MediaMaxSegmentDuration() {
		return false, manager.quarantine(ctx, identity, mediaPath, sidecarRaw, stagingPath, CodeDurationInvalid)
	}
	metadataDigest, err := metadataHash(sidecar, probe)
	if err != nil {
		return false, manager.record(ctx, identity, "failed", CodeStorageFailed, manager.relativeManaged(stagingPath), 0)
	}
	if err := manager.record(ctx, identity, "validated", "", manager.relativeManaged(stagingPath), 0); err != nil {
		return false, err
	}

	claim, err := manager.repository.ResolveMediaClaim(ctx, sidecar.CollectorID, sidecar.SourceIdempotencyKey, sidecar.SHA256, metadataDigest)
	if err != nil {
		return false, manager.record(ctx, identity, "failed", CodeDatabaseFailed, manager.relativeManaged(stagingPath), 0)
	}
	if claim.Status == eventstore.MediaWriteConflict {
		conflictCode := CodeMetadataConflict
		if claim.SHA256 != sidecar.SHA256 {
			conflictCode = CodeIdempotencyConflict
		}
		return false, manager.quarantine(ctx, identity, mediaPath, sidecarRaw, stagingPath, conflictCode)
	}

	managedRelativePath := claim.ManagedRelativePath
	if claim.Status == eventstore.MediaWriteDuplicate {
		existingPath := filepath.Join(manager.storageRoot, filepath.FromSlash(claim.ManagedRelativePath))
		if err := manager.verifyManagedFile(existingPath, sidecar.SizeBytes, sidecar.SHA256); err != nil {
			return false, manager.record(ctx, identity, "failed", CodeStorageFailed, manager.relativeManaged(stagingPath), claim.SegmentID)
		}
		_ = os.Remove(stagingPath)
	} else {
		acceptedPath := filepath.Join(manager.acceptedRoot, sidecar.SHA256+".media")
		managedRelativePath = manager.relativeManaged(acceptedPath)
		if err := manager.installAccepted(stagingPath, acceptedPath, sidecar.SizeBytes, sidecar.SHA256); err != nil {
			return false, manager.record(ctx, identity, "failed", ErrorCode(err), manager.relativeManaged(stagingPath), 0)
		}
		if manager.afterRename != nil {
			if err := manager.afterRename(acceptedPath); err != nil {
				return false, manager.record(ctx, identity, "failed", CodeDatabaseFailed, managedRelativePath, 0)
			}
		}
	}

	receivedAt := manager.now().UTC().Format(time.RFC3339Nano)
	metadata := eventstore.MediaMetadata{
		CollectorID:          sidecar.CollectorID,
		SourceIdempotencyKey: sidecar.SourceIdempotencyKey,
		ManagedRelativePath:  managedRelativePath,
		DeviceStartRaw:       sidecar.DeviceStartRaw,
		DeviceEndRaw:         sidecar.DeviceEndRaw,
		DeviceStartUTC:       sidecar.DeviceStartUTC,
		DeviceEndUTC:         sidecar.DeviceEndUTC,
		ReceivedAtUTC:        receivedAt,
		ClockOffsetMS:        *sidecar.ClockOffsetMS,
		ClockErrorMS:         *sidecar.ClockErrorMS,
		SizeBytes:            sidecar.SizeBytes,
		DurationMS:           probe.DurationMS,
		CodecName:            probe.CodecName,
		FormatName:           probe.FormatName,
		MediaType:            sidecar.MediaType,
		SHA256:               sidecar.SHA256,
		MetadataHash:         metadataDigest,
		SidecarSchemaVersion: sidecar.SchemaVersion,
	}
	acceptEvent := manager.makeEvent(identity, "accepted", "", managedRelativePath, claim.SegmentID)
	stateEventKey := hashText("media-state\x00accepted\x00" + sidecar.CollectorID + "\x00" + sidecar.SourceIdempotencyKey + "\x00" + metadataDigest)
	claim, err = manager.repository.AcceptMedia(ctx, metadata, acceptEvent, stateEventKey)
	if err != nil {
		return false, &Error{Code: CodeDatabaseFailed, Err: err}
	}
	if claim.Status == eventstore.MediaWriteConflict {
		return false, manager.quarantine(ctx, identity, mediaPath, sidecarRaw, "", CodeIdempotencyConflict)
	}
	acceptedAt := receivedAt
	if err := manager.writeConfirmation(confirmationPath, confirmation{
		SchemaVersion:        StatusSchemaVersion,
		CollectorID:          sidecar.CollectorID,
		SourceIdempotencyKey: sidecar.SourceIdempotencyKey,
		MediaSegmentID:       claim.SegmentID,
		SHA256:               sidecar.SHA256,
		MetadataHash:         metadataDigest,
		AcceptedAtUTC:        acceptedAt,
	}); err != nil {
		return false, err
	}
	return true, nil
}

func (manager *Manager) stableSourceStat(path string) (os.FileInfo, error) {
	if err := ensureSafePath(manager.config.MediaIngest.InboxDirectory, path, true); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	return info, nil
}

func (manager *Manager) readInboxFile(path string, maximum int64) ([]byte, error) {
	if err := ensureSafePath(manager.config.MediaIngest.InboxDirectory, path, true); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	value, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, &Error{Code: CodeStorageFailed, Err: errors.New("media inbox file cannot be read")}
	}
	if int64(len(value)) > maximum {
		return nil, &Error{Code: CodeSidecarInvalid, Err: errors.New("media sidecar exceeds configured size")}
	}
	return value, nil
}

func (manager *Manager) record(ctx context.Context, identity processIdentity, status, errorCode, temporaryPath string, segmentID int64) error {
	event := manager.makeEvent(identity, status, errorCode, temporaryPath, segmentID)
	if err := manager.repository.AppendMediaIngestEvent(ctx, event); err != nil {
		return &Error{Code: CodeDatabaseFailed, Err: err}
	}
	return nil
}

func (manager *Manager) makeEvent(identity processIdentity, status, errorCode, temporaryPath string, segmentID int64) eventstore.MediaIngestEvent {
	eventKey := hashText(strings.Join([]string{
		identity.ingestKey, identity.sourceFingerprint, status, errorCode, temporaryPath,
	}, "\x00"))
	return eventstore.MediaIngestEvent{
		EventKey:              eventKey,
		IngestKey:             identity.ingestKey,
		CollectorID:           identity.collectorID,
		SourceIdempotencyKey:  identity.sourceIdempotencyKey,
		SourceName:            identity.sourceName,
		SourceFingerprint:     identity.sourceFingerprint,
		Status:                status,
		TemporaryRelativePath: temporaryPath,
		MediaSegmentID:        segmentID,
		ErrorCode:             errorCode,
		OccurredAtUTC:         manager.now().UTC().Format(time.RFC3339Nano),
	}
}

func (manager *Manager) relativeManaged(path string) string {
	if path == "" {
		return ""
	}
	relative, err := filepath.Rel(manager.storageRoot, path)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(relative)
}

func hashText(value string) string { return hashBytes([]byte(value)) }

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func (manager *Manager) confirmationValid(ctx context.Context, path string) (bool, error) {
	raw, err := manager.readInboxFile(path, manager.config.MediaIngest.MaxSidecarBytes)
	if err != nil {
		return false, err
	}
	if err := validateConfirmationJSON(raw); err != nil {
		return false, err
	}
	var value confirmation
	if err := json.Unmarshal(raw, &value); err != nil || value.SchemaVersion != StatusSchemaVersion || value.MediaSegmentID <= 0 {
		return false, &Error{Code: CodeSidecarInvalid, Err: errors.New("media confirmation is invalid")}
	}
	claim, err := manager.repository.ResolveMediaClaim(ctx, value.CollectorID, value.SourceIdempotencyKey, value.SHA256, value.MetadataHash)
	if err != nil {
		return false, err
	}
	return claim.Status == eventstore.MediaWriteDuplicate && claim.SegmentID == value.MediaSegmentID, nil
}

func validateConfirmationJSON(raw []byte) error {
	if err := strictjson.ValidateExactRootObject(raw, 0, confirmationFields...); err != nil {
		return &Error{Code: CodeSidecarInvalid, Err: errors.New("media confirmation JSON is invalid")}
	}
	return nil
}

func (manager *Manager) writeConfirmation(path string, value confirmation) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return &Error{Code: CodeStorageFailed, Err: errors.New("media confirmation cannot be encoded")}
	}
	if err := writeAtomicFile(path, raw); err != nil {
		return &Error{Code: CodeStorageFailed, Err: errors.New("media confirmation cannot be committed")}
	}
	return nil
}
