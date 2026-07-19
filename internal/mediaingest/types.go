package mediaingest

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Versifine/study-monitor/internal/eventstore"
)

const (
	SidecarSchemaVersion    = 1
	StatusSchemaVersion     = 1
	SupportedFFprobeVersion = "N-117599-ge1d1ba4cbc-20241017"

	ReadySuffix        = ".ready"
	SidecarSuffix      = ".sidecar.json"
	ConfirmationSuffix = ".accepted.json"
)

const (
	ModuleDisabled    = "disabled"
	ModuleHealthy     = "healthy"
	ModuleUnavailable = "unavailable"
)

const (
	CodePathInvalid          = "MEDIA_PATH_INVALID"
	CodeReparsePoint         = "MEDIA_REPARSE_POINT_REJECTED"
	CodeReadyMissing         = "MEDIA_READY_MARKER_MISSING"
	CodeSourceMissing        = "MEDIA_SOURCE_MISSING"
	CodeSidecarMissing       = "MEDIA_SIDECAR_MISSING"
	CodeSidecarInvalid       = "MEDIA_SIDECAR_INVALID"
	CodeSidecarIncomplete    = "MEDIA_SIDECAR_INCOMPLETE"
	CodeFileGrowing          = "MEDIA_FILE_GROWING"
	CodeTooLarge             = "MEDIA_TOO_LARGE"
	CodeSizeMismatch         = "MEDIA_SIZE_MISMATCH"
	CodeHashMismatch         = "MEDIA_HASH_MISMATCH"
	CodeTimeInvalid          = "MEDIA_TIME_INVALID"
	CodeDurationInvalid      = "MEDIA_DURATION_INVALID"
	CodeTypeInvalid          = "MEDIA_TYPE_INVALID"
	CodeProbeUnavailable     = "MEDIA_FFPROBE_UNAVAILABLE"
	CodeProbeVersionMismatch = "MEDIA_FFPROBE_VERSION_MISMATCH"
	CodeProbeFailed          = "MEDIA_FFPROBE_FAILED"
	CodeIdempotencyConflict  = "MEDIA_IDEMPOTENCY_CONFLICT"
	CodeMetadataConflict     = "MEDIA_METADATA_CONFLICT"
	CodeStorageFailed        = "MEDIA_STORAGE_FAILED"
	CodeDatabaseFailed       = "MEDIA_DATABASE_FAILED"
	CodeQuarantineFailed     = "MEDIA_QUARANTINE_FAILED"
)

type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func ErrorCode(err error) string {
	var mediaError *Error
	if errors.As(err, &mediaError) {
		return mediaError.Code
	}
	return CodeStorageFailed
}

type Repository interface {
	ResolveMediaClaim(context.Context, string, string, string, string) (eventstore.MediaClaim, error)
	AppendMediaIngestEvent(context.Context, eventstore.MediaIngestEvent) error
	AcceptMedia(context.Context, eventstore.MediaMetadata, eventstore.MediaIngestEvent, string) (eventstore.MediaClaim, error)
	RebuildMediaProjections(context.Context) error
	MediaIngestSummary(context.Context) (eventstore.MediaIngestSummary, error)
}

type Prober interface {
	Version(context.Context, time.Duration) (string, error)
	Probe(context.Context, string, time.Duration) (ProbeInfo, error)
}

type ProbeInfo struct {
	DurationMS int64
	CodecName  string
	FormatName string
	MediaType  string
}

type Status struct {
	SchemaVersion          int                           `json:"schema_version"`
	Status                 string                        `json:"status"`
	ErrorCode              string                        `json:"error_code,omitempty"`
	FFprobeVersion         string                        `json:"ffprobe_version"`
	LastScanUTC            string                        `json:"last_scan_utc,omitempty"`
	FilesystemReadyBacklog int64                         `json:"filesystem_ready_backlog"`
	FilesystemReadyBytes   int64                         `json:"filesystem_ready_bytes"`
	Ingest                 eventstore.MediaIngestSummary `json:"ingest"`
}

type statusState struct {
	sync.RWMutex
	status      Status
	initialized bool
}

func newStatus(state, errorCode string) *statusState {
	return &statusState{status: Status{
		SchemaVersion:  StatusSchemaVersion,
		Status:         state,
		ErrorCode:      errorCode,
		FFprobeVersion: SupportedFFprobeVersion,
	}}
}

func (state *statusState) snapshot() Status {
	state.RLock()
	defer state.RUnlock()
	return state.status
}

func (state *statusState) isInitialized() bool {
	state.RLock()
	defer state.RUnlock()
	return state.initialized
}

type FixedStatusProvider struct {
	state *statusState
}

func NewFixedStatusProvider(status, errorCode string) *FixedStatusProvider {
	return &FixedStatusProvider{state: newStatus(status, errorCode)}
}

func (provider *FixedStatusProvider) Status(context.Context) Status {
	return provider.state.snapshot()
}
