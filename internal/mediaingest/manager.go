package mediaingest

import (
	"container/heap"
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
	"sort"
	"strings"
	"sync"
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
	gate       StorageGate
	faults     FaultRecorder

	storageRoot    string
	stagingRoot    string
	acceptedRoot   string
	quarantineRoot string

	afterCopyChunk   func(int64) error
	afterRename      func(string) error
	beforeQuarantine func() error
	afterFirstStat   func()

	scanMu     sync.Mutex
	scanCursor string
}

type coreWriteGate interface {
	CoreWritesAllowed() (bool, string)
}

type fileStamp struct {
	size       int64
	modifiedNS int64
	identity   string
	changeTime int64
}

type readyCandidate struct {
	path string
	key  string
	size int64
}

type candidateHeap []readyCandidate

func (candidates candidateHeap) Len() int { return len(candidates) }
func (candidates candidateHeap) Less(left, right int) bool {
	return candidates[left].key > candidates[right].key
}
func (candidates candidateHeap) Swap(left, right int) {
	candidates[left], candidates[right] = candidates[right], candidates[left]
}
func (candidates *candidateHeap) Push(value any) {
	*candidates = append(*candidates, value.(readyCandidate))
}
func (candidates *candidateHeap) Pop() any {
	old := *candidates
	last := old[len(old)-1]
	*candidates = old[:len(old)-1]
	return last
}

func retainSmallest(candidates *candidateHeap, candidate readyCandidate, limit int) {
	if candidates.Len() < limit {
		heap.Push(candidates, candidate)
		return
	}
	if limit > 0 && candidate.key < (*candidates)[0].key {
		heap.Pop(candidates)
		heap.Push(candidates, candidate)
	}
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
	SidecarFingerprint   string `json:"sidecar_fingerprint"`
	MediaSegmentID       int64  `json:"media_segment_id"`
	SHA256               string `json:"sha256"`
	MetadataHash         string `json:"metadata_hash"`
	AcceptedAtUTC        string `json:"accepted_at_utc"`
}

var confirmationFields = []string{
	"schema_version", "collector_id", "source_idempotency_key", "media_segment_id",
	"sidecar_fingerprint", "sha256", "metadata_hash", "accepted_at_utc",
}

func New(cfg config.Config, repository Repository, logger *logging.Logger) *Manager {
	return newManager(cfg, repository, ExecProber{Path: cfg.MediaIngest.FFprobePath}, logger, time.Now)
}

func NewWithGate(cfg config.Config, repository Repository, logger *logging.Logger, gate StorageGate) *Manager {
	manager := newManager(cfg, repository, ExecProber{Path: cfg.MediaIngest.FFprobePath}, logger, time.Now)
	manager.gate = gate
	if recorder, ok := gate.(FaultRecorder); ok {
		manager.faults = recorder
	}
	return manager
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
	if err := manager.ensureCoreWritesAllowed(); err != nil {
		return manager.setUnavailable(CodeStorageProtected, err)
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

func (manager *Manager) ensureMediaAllowed() error {
	if manager.gate == nil {
		return nil
	}
	if allowed, reason := manager.gate.MediaAllowed(); !allowed {
		return &Error{Code: CodeStorageProtected, Err: fmt.Errorf("media ingest paused by storage protection: %s", reason)}
	}
	return nil
}

func (manager *Manager) ensureCoreWritesAllowed() error {
	gate, ok := manager.gate.(coreWriteGate)
	if !ok {
		return nil
	}
	if allowed, reason := gate.CoreWritesAllowed(); !allowed {
		return &Error{Code: CodeStorageProtected, Err: fmt.Errorf("media metadata writes paused by storage protection: %s", reason)}
	}
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
	previousStatus, previousCode := manager.state.status.Status, manager.state.status.ErrorCode
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
	if manager.faults != nil {
		if err != nil && (previousStatus != ModuleUnavailable || previousCode != ErrorCode(err)) {
			manager.faults.RecordFault(ctx, "media_ingest", "P2", "degraded", ErrorCode(err), err.Error())
		} else if err == nil && previousStatus == ModuleUnavailable && previousCode != "" {
			manager.faults.RecordFault(ctx, "media_ingest", "P3", "recovered", "MEDIA_INGEST_RECOVERED", "media ingest recovered")
		}
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
	manager.scanMu.Lock()
	defer manager.scanMu.Unlock()
	if !manager.state.isInitialized() {
		return &Error{Code: CodeProbeUnavailable, Err: errors.New("media ingest module is not healthy")}
	}
	if err := manager.ensureMediaAllowed(); err != nil {
		return err
	}
	version, err := manager.prober.Version(ctx, manager.config.FFprobeTimeout())
	if err != nil {
		return err
	}
	if version != SupportedFFprobeVersion {
		return &Error{Code: CodeProbeVersionMismatch, Err: fmt.Errorf("unsupported ffprobe version %q", version)}
	}
	directory, err := os.Open(manager.config.MediaIngest.InboxDirectory)
	if err != nil {
		return &Error{Code: CodeStorageFailed, Err: errors.New("media inbox cannot be opened")}
	}
	defer directory.Close()
	limit := manager.config.MediaIngest.MaxScanEntries
	afterCursor := make(candidateHeap, 0, limit)
	wrapped := make(candidateHeap, 0, limit)
	remaining := int64(0)
	remainingBytes := int64(0)
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
			confirmed, confirmationErr := manager.confirmationCached(ctx, strings.TrimSuffix(readyPath, ReadySuffix)+ConfirmationSuffix)
			if confirmationErr != nil {
				return confirmationErr
			}
			if confirmed {
				continue
			}
			size := manager.readySourceSize(readyPath)
			name := filepath.Base(readyPath)
			key := strings.ToLower(name) + "\x00" + name
			candidate := readyCandidate{path: readyPath, key: key, size: size}
			if key > manager.scanCursor {
				retainSmallest(&afterCursor, candidate, limit)
			} else {
				retainSmallest(&wrapped, candidate, limit)
			}
			remaining++
			remainingBytes = saturatedAdd(remainingBytes, size)
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	sort.Slice(afterCursor, func(left, right int) bool {
		return afterCursor[left].key < afterCursor[right].key
	})
	sort.Slice(wrapped, func(left, right int) bool {
		return wrapped[left].key < wrapped[right].key
	})
	selected := make([]readyCandidate, 0, limit)
	for _, candidate := range afterCursor {
		if len(selected) == limit {
			break
		}
		selected = append(selected, candidate)
	}
	for _, candidate := range wrapped {
		if len(selected) == limit {
			break
		}
		selected = append(selected, candidate)
	}
	var firstError error
	for _, candidate := range selected {
		if gateErr := manager.ensureMediaAllowed(); gateErr != nil {
			if firstError == nil {
				firstError = gateErr
			}
			break
		}
		readyPath := candidate.path
		manager.scanCursor = candidate.key
		confirmed, confirmationErr := manager.confirmationValid(ctx, strings.TrimSuffix(readyPath, ReadySuffix)+ConfirmationSuffix)
		if confirmationErr == nil && confirmed {
			remaining--
			remainingBytes -= candidate.size
			continue
		}
		if confirmationErr != nil && ErrorCode(confirmationErr) == CodeDatabaseFailed {
			if firstError == nil {
				firstError = confirmationErr
			}
			continue
		}
		accepted, processErr := manager.ProcessReady(ctx, readyPath)
		if accepted {
			remaining--
			remainingBytes -= candidate.size
		}
		if processErr != nil && firstError == nil {
			firstError = processErr
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
	if err := manager.ensureMediaAllowed(); err != nil {
		return false, err
	}
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
	unchangedTerminal, terminalErr := manager.terminalUnchanged(ctx, identity, mediaPath)
	if terminalErr != nil {
		return false, terminalErr
	}
	if unchangedTerminal {
		return false, nil
	}
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
	unchangedTerminal, err = manager.terminalUnchanged(ctx, identity, mediaPath)
	if err != nil {
		return false, err
	}
	if unchangedTerminal {
		return false, nil
	}

	first, err := manager.stableSourceStat(mediaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, manager.record(ctx, identity, "pending", CodeSourceMissing, "", 0)
		}
		return false, manager.record(ctx, identity, "failed", ErrorCode(err), "", 0)
	}
	if manager.afterFirstStat != nil {
		manager.afterFirstStat()
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
		if ErrorCode(err) == CodeStorageProtected {
			return false, err
		}
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
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if code := ErrorCode(err); code == CodeProbeUnavailable || code == CodeProbeVersionMismatch {
			if recordErr := manager.record(ctx, identity, "pending", code, manager.relativeManaged(stagingPath), 0); recordErr != nil {
				return false, recordErr
			}
			return false, err
		}
		return false, manager.quarantine(ctx, identity, mediaPath, sidecarRaw, stagingPath, ErrorCode(err))
	}
	if probe.MediaType != sidecar.MediaType {
		return false, manager.quarantine(ctx, identity, mediaPath, sidecarRaw, stagingPath, CodeTypeInvalid)
	}
	if probe.DurationMS > manager.config.MediaMaxSegmentDuration().Milliseconds() {
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
			if _, inspectErr := os.Lstat(existingPath); !errors.Is(inspectErr, os.ErrNotExist) {
				return false, manager.record(ctx, identity, "failed", CodeStorageFailed, manager.relativeManaged(stagingPath), claim.SegmentID)
			}
			expectedPath := filepath.Join(manager.acceptedRoot, sidecar.SHA256+".media")
			if filepath.Clean(existingPath) != filepath.Clean(expectedPath) {
				return false, manager.record(ctx, identity, "failed", CodeStorageFailed, manager.relativeManaged(stagingPath), claim.SegmentID)
			}
			if err := manager.installAccepted(stagingPath, existingPath, sidecar.SizeBytes, sidecar.SHA256); err != nil {
				return false, manager.record(ctx, identity, "failed", ErrorCode(err), manager.relativeManaged(stagingPath), claim.SegmentID)
			}
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
	if err := manager.ensureCoreWritesAllowed(); err != nil {
		return false, err
	}
	claim, err = manager.repository.AcceptMedia(ctx, metadata, acceptEvent, stateEventKey)
	if err != nil {
		return false, &Error{Code: CodeDatabaseFailed, Err: err}
	}
	if claim.Status == eventstore.MediaWriteConflict {
		return false, manager.quarantine(ctx, identity, mediaPath, sidecarRaw, "", CodeIdempotencyConflict)
	}
	acceptedAt := receivedAt
	if err := manager.ensureMediaAllowed(); err != nil {
		return false, err
	}
	if err := manager.writeConfirmation(confirmationPath, confirmation{
		SchemaVersion:        StatusSchemaVersion,
		CollectorID:          sidecar.CollectorID,
		SourceIdempotencyKey: sidecar.SourceIdempotencyKey,
		SidecarFingerprint:   sidecar.Fingerprint,
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
	if err := manager.ensureCoreWritesAllowed(); err != nil {
		return err
	}
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

func stampFile(path string) (fileStamp, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fileStamp{}, err
	}
	if !info.Mode().IsRegular() {
		return fileStamp{}, errors.New("path is not a regular file")
	}
	identity, changeTime, err := platformFileStamp(path, info)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{size: info.Size(), modifiedNS: info.ModTime().UnixNano(), identity: identity, changeTime: changeTime}, nil
}

func (manager *Manager) terminalFileSignature(identity processIdentity, sourcePath string) (string, error) {
	base := identity.ingestKey + "-" + identity.sourceFingerprint
	paths := []string{
		sourcePath,
		sourcePath + SidecarSuffix,
		filepath.Join(manager.quarantineRoot, base+".media"),
		filepath.Join(manager.quarantineRoot, base+SidecarSuffix),
		filepath.Join(manager.quarantineRoot, base+".reason.json"),
	}
	stamps := make([]fileStamp, 0, len(paths))
	for _, path := range paths {
		stamp, err := stampFile(path)
		if err != nil {
			return "", err
		}
		stamps = append(stamps, stamp)
	}
	return hashText(identity.sourceFingerprint + "\x00" + fmt.Sprintf("%#v", stamps)), nil
}

func (manager *Manager) terminalUnchanged(ctx context.Context, identity processIdentity, sourcePath string) (bool, error) {
	signature, err := manager.terminalFileSignature(identity, sourcePath)
	if err != nil {
		return false, nil
	}
	matches, err := manager.repository.MediaFileCheckMatches(ctx, identity.ingestKey, "quarantined", signature)
	if err != nil {
		return false, &Error{Code: CodeDatabaseFailed, Err: err}
	}
	return matches, nil
}

func (manager *Manager) rememberTerminal(ctx context.Context, identity processIdentity, sourcePath string) error {
	signature, err := manager.terminalFileSignature(identity, sourcePath)
	if err != nil {
		return &Error{Code: CodeQuarantineFailed, Err: errors.New("quarantine artifacts cannot be verified")}
	}
	if err := manager.ensureCoreWritesAllowed(); err != nil {
		return err
	}
	if err := manager.repository.SaveMediaFileCheck(ctx, identity.ingestKey, "quarantined", signature, manager.now().UTC().Format(time.RFC3339Nano)); err != nil {
		return &Error{Code: CodeDatabaseFailed, Err: err}
	}
	return nil
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
	acceptedAt, err := time.Parse(time.RFC3339Nano, value.AcceptedAtUTC)
	_, offset := acceptedAt.Zone()
	if err != nil || strings.TrimSpace(value.AcceptedAtUTC) != value.AcceptedAtUTC || strings.HasSuffix(value.AcceptedAtUTC, "-00:00") || offset != 0 {
		return false, &Error{Code: CodeSidecarInvalid, Err: errors.New("media confirmation accepted_at_utc is invalid")}
	}
	mediaPath := strings.TrimSuffix(path, ConfirmationSuffix)
	sidecarPath := mediaPath + SidecarSuffix
	sidecarRaw, err := manager.readInboxFile(sidecarPath, manager.config.MediaIngest.MaxSidecarBytes)
	if err != nil {
		return false, err
	}
	sidecar, err := parseSidecar(sidecarRaw, manager.config.MediaMaxSegmentDuration())
	if err != nil {
		return false, err
	}
	if value.CollectorID != sidecar.CollectorID || value.SourceIdempotencyKey != sidecar.SourceIdempotencyKey || value.SidecarFingerprint != sidecar.Fingerprint || value.SHA256 != sidecar.SHA256 {
		return false, &Error{Code: CodeSidecarInvalid, Err: errors.New("media confirmation does not describe the current sidecar")}
	}
	if err := ensureSafePath(manager.config.MediaIngest.InboxDirectory, mediaPath, true); err != nil {
		return false, err
	}
	sourceStamp, err := stampFile(mediaPath)
	if err != nil || sourceStamp.size != sidecar.SizeBytes {
		return false, &Error{Code: CodeStorageFailed, Err: errors.New("confirmed source media is missing or changed")}
	}
	claim, err := manager.repository.ResolveMediaClaim(ctx, value.CollectorID, value.SourceIdempotencyKey, value.SHA256, value.MetadataHash)
	if err != nil {
		return false, &Error{Code: CodeDatabaseFailed, Err: err}
	}
	if claim.Status != eventstore.MediaWriteDuplicate || claim.SegmentID != value.MediaSegmentID || claim.ManagedRelativePath == "" || claim.SidecarFingerprint != sidecar.Fingerprint {
		return false, nil
	}
	source, err := hashFile(mediaPath, manager.config.MediaIngest.MaxSegmentBytes)
	if err != nil || source.size != sidecar.SizeBytes || source.sha256 != sidecar.SHA256 {
		return false, &Error{Code: CodeStorageFailed, Err: errors.New("confirmed source media failed verification")}
	}
	managedPath := filepath.Join(manager.storageRoot, filepath.FromSlash(claim.ManagedRelativePath))
	if err := manager.verifyManagedFile(managedPath, sidecar.SizeBytes, sidecar.SHA256); err != nil {
		return false, err
	}
	signature, err := manager.confirmationFileSignature(path)
	if err != nil {
		return false, err
	}
	ingestKey := hashText(strings.ToLower(filepath.Clean(manager.config.MediaIngest.InboxDirectory)) + "\x00" + filepath.Base(mediaPath))
	if err := manager.ensureCoreWritesAllowed(); err != nil {
		return false, err
	}
	if err := manager.repository.SaveMediaFileCheck(ctx, ingestKey, "confirmed", signature, manager.now().UTC().Format(time.RFC3339Nano)); err != nil {
		return false, &Error{Code: CodeDatabaseFailed, Err: err}
	}
	return true, nil
}

func (manager *Manager) confirmationFileSignature(path string) (string, error) {
	raw, err := manager.readInboxFile(path, manager.config.MediaIngest.MaxSidecarBytes)
	if err != nil {
		return "", err
	}
	if err := validateConfirmationJSON(raw); err != nil {
		return "", err
	}
	var value confirmation
	if err := json.Unmarshal(raw, &value); err != nil || value.SchemaVersion != StatusSchemaVersion || value.MediaSegmentID <= 0 {
		return "", &Error{Code: CodeSidecarInvalid, Err: errors.New("media confirmation is invalid")}
	}
	acceptedAt, err := time.Parse(time.RFC3339Nano, value.AcceptedAtUTC)
	_, offset := acceptedAt.Zone()
	if err != nil || strings.TrimSpace(value.AcceptedAtUTC) != value.AcceptedAtUTC || strings.HasSuffix(value.AcceptedAtUTC, "-00:00") || offset != 0 {
		return "", &Error{Code: CodeSidecarInvalid, Err: errors.New("media confirmation accepted_at_utc is invalid")}
	}
	mediaPath := strings.TrimSuffix(path, ConfirmationSuffix)
	sidecarPath := mediaPath + SidecarSuffix
	sidecarRaw, err := manager.readInboxFile(sidecarPath, manager.config.MediaIngest.MaxSidecarBytes)
	if err != nil {
		return "", err
	}
	sidecar, err := parseSidecar(sidecarRaw, manager.config.MediaMaxSegmentDuration())
	if err != nil {
		return "", err
	}
	if value.CollectorID != sidecar.CollectorID || value.SourceIdempotencyKey != sidecar.SourceIdempotencyKey || value.SidecarFingerprint != sidecar.Fingerprint || value.SHA256 != sidecar.SHA256 {
		return "", &Error{Code: CodeSidecarInvalid, Err: errors.New("media confirmation does not describe the current sidecar")}
	}
	if err := ensureSafePath(manager.config.MediaIngest.InboxDirectory, mediaPath, true); err != nil {
		return "", err
	}
	paths := []string{
		path,
		sidecarPath,
		mediaPath,
		filepath.Join(manager.acceptedRoot, sidecar.SHA256+".media"),
	}
	stamps := make([]fileStamp, 0, len(paths))
	for _, candidatePath := range paths {
		stamp, err := stampFile(candidatePath)
		if err != nil {
			return "", err
		}
		stamps = append(stamps, stamp)
	}
	if stamps[2].size != sidecar.SizeBytes || stamps[3].size != sidecar.SizeBytes {
		return "", &Error{Code: CodeStorageFailed, Err: errors.New("confirmed media size changed")}
	}
	return hashText(hashBytes(raw) + "\x00" + sidecar.Fingerprint + "\x00" + fmt.Sprintf("%#v", stamps)), nil
}

func (manager *Manager) confirmationCached(ctx context.Context, path string) (bool, error) {
	signature, err := manager.confirmationFileSignature(path)
	if err != nil {
		return false, nil
	}
	mediaPath := strings.TrimSuffix(path, ConfirmationSuffix)
	ingestKey := hashText(strings.ToLower(filepath.Clean(manager.config.MediaIngest.InboxDirectory)) + "\x00" + filepath.Base(mediaPath))
	matches, err := manager.repository.MediaFileCheckMatches(ctx, ingestKey, "confirmed", signature)
	if err != nil {
		return false, &Error{Code: CodeDatabaseFailed, Err: err}
	}
	return matches, nil
}

func validateConfirmationJSON(raw []byte) error {
	if err := strictjson.ValidateExactRootObjectRequired(raw, 0, confirmationFields...); err != nil {
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
