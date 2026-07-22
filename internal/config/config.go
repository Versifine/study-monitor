package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Versifine/study-monitor/internal/strictjson"
)

const (
	CurrentSchemaVersion = 1

	CodeReadFailed        = "CONFIG_READ_FAILED"
	CodeDecodeFailed      = "CONFIG_DECODE_FAILED"
	CodeMissingSchema     = "CONFIG_SCHEMA_REQUIRED"
	CodeUnsupportedSchema = "CONFIG_SCHEMA_UNSUPPORTED"
	CodeInvalidEnv        = "CONFIG_ENV_INVALID"
	CodeInvalidAddress    = "CONFIG_ADDRESS_INVALID"
	CodeNonLoopback       = "CONFIG_NON_LOOPBACK_DISABLED"
	CodeInvalidDataDir    = "CONFIG_DATA_DIRECTORY_INVALID"
	CodeInvalidStorage    = "CONFIG_STORAGE_INVALID"
	CodeInvalidAPILimit   = "CONFIG_API_LIMIT_INVALID"
	CodeInvalidMedia      = "CONFIG_MEDIA_INGEST_INVALID"
	CodeInvalidRuntime    = "CONFIG_RUNTIME_INVALID"
	CodeInvalidCollector  = "CONFIG_COLLECTOR_INVALID"
	CodeInvalidTimeline   = "CONFIG_TIMELINE_INVALID"
	CodeInvalidOperations = "CONFIG_OPERATIONS_INVALID"
	CodeInvalidRetention  = "CONFIG_RETENTION_INVALID"
	CodeInvalidLogLevel   = "CONFIG_LOG_LEVEL_INVALID"
	CodeInvalidTimeout    = "CONFIG_TIMEOUT_INVALID"
)

const (
	EnvListenAddress    = "EXAM_MONITOR_LISTEN_ADDRESS"
	EnvAllowNonLoopback = "EXAM_MONITOR_ALLOW_NON_LOOPBACK"
	EnvDataDirectory    = "EXAM_MONITOR_DATA_DIRECTORY"
	EnvLogLevel         = "EXAM_MONITOR_LOG_LEVEL"
	EnvReadHeader       = "EXAM_MONITOR_READ_HEADER_TIMEOUT"
	EnvRead             = "EXAM_MONITOR_READ_TIMEOUT"
	EnvWrite            = "EXAM_MONITOR_WRITE_TIMEOUT"
	EnvIdle             = "EXAM_MONITOR_IDLE_TIMEOUT"
	EnvShutdown         = "EXAM_MONITOR_SHUTDOWN_TIMEOUT"
	EnvBusyTimeout      = "EXAM_MONITOR_BUSY_TIMEOUT"
	EnvMaxOpenConns     = "EXAM_MONITOR_MAX_OPEN_CONNECTIONS"
	EnvMaxRequestBytes  = "EXAM_MONITOR_MAX_REQUEST_BYTES"
	EnvMaxBatchEvents   = "EXAM_MONITOR_MAX_BATCH_EVENTS"
	EnvMaxEventBytes    = "EXAM_MONITOR_MAX_EVENT_BYTES"
	EnvMaxPayloadDepth  = "EXAM_MONITOR_MAX_PAYLOAD_DEPTH"
	EnvMaxWrites        = "EXAM_MONITOR_MAX_CONCURRENT_WRITES"
	EnvDefaultPageSize  = "EXAM_MONITOR_DEFAULT_PAGE_SIZE"
	EnvMaxPageSize      = "EXAM_MONITOR_MAX_PAGE_SIZE"
	EnvMediaEnabled     = "EXAM_MONITOR_MEDIA_INGEST_ENABLED"
	EnvMediaInbox       = "EXAM_MONITOR_MEDIA_INBOX_DIRECTORY"
	EnvMediaScan        = "EXAM_MONITOR_MEDIA_SCAN_INTERVAL"
	EnvMediaSettle      = "EXAM_MONITOR_MEDIA_SETTLE_INTERVAL"
	EnvMediaMaxBytes    = "EXAM_MONITOR_MEDIA_MAX_SEGMENT_BYTES"
	EnvMediaMaxDuration = "EXAM_MONITOR_MEDIA_MAX_SEGMENT_DURATION"
	EnvMediaSidecar     = "EXAM_MONITOR_MEDIA_MAX_SIDECAR_BYTES"
	EnvMediaScanEntries = "EXAM_MONITOR_MEDIA_MAX_SCAN_ENTRIES"
	EnvFFprobePath      = "EXAM_MONITOR_FFPROBE_PATH"
	EnvFFprobeTimeout   = "EXAM_MONITOR_FFPROBE_TIMEOUT"
	EnvRuntimeMode      = "EXAM_MONITOR_MODE"

	envLocalAppData = "LOCALAPPDATA"
)

// LookupEnv matches os.LookupEnv and makes environment overrides deterministic in tests.
type LookupEnv func(string) (string, bool)

// Config is the complete Recorder Core configuration contract through M4.
type Config struct {
	SchemaVersion int               `json:"schema_version"`
	Runtime       RuntimeConfig     `json:"runtime"`
	Server        ServerConfig      `json:"server"`
	Paths         PathsConfig       `json:"paths"`
	Storage       StorageConfig     `json:"storage"`
	API           APIConfig         `json:"api"`
	MediaIngest   MediaIngestConfig `json:"media_ingest"`
	Collectors    []CollectorConfig `json:"collectors"`
	Timeline      TimelineConfig    `json:"timeline"`
	Operations    OperationsConfig  `json:"operations"`
	Retention     RetentionConfig   `json:"retention"`
	Logging       LoggingConfig     `json:"logging"`
}

const (
	ModeRecordOnly = "record-only"
	ModeMinimum    = "minimum"

	CollectorActivityWatch = "activitywatch"
	CollectorGenericJSON   = "generic_json"
	CollectorMedia         = "media"
)

type RuntimeConfig struct {
	Mode                   string `json:"mode"`
	BackupInterfaceEnabled bool   `json:"backup_interface_enabled"`
}

type CollectorConfig struct {
	ID              string                `json:"id"`
	Kind            string                `json:"kind"`
	Enabled         bool                  `json:"enabled"`
	HeartbeatPeriod string                `json:"heartbeat_period"`
	AllowedLateness string                `json:"allowed_lateness"`
	OfflineAfter    string                `json:"offline_after"`
	PlannedSchedule PlannedScheduleConfig `json:"planned_schedule"`
	ActivityWatch   *ActivityWatchConfig  `json:"activitywatch,omitempty"`
}

type PlannedScheduleConfig struct {
	Timezone string                 `json:"timezone"`
	Windows  []ScheduleWindowConfig `json:"windows"`
}

type ScheduleWindowConfig struct {
	Days       []string `json:"days"`
	StartLocal string   `json:"start_local"`
	EndLocal   string   `json:"end_local"`
}

type ActivityWatchConfig struct {
	BaseURL          string `json:"base_url"`
	BucketID         string `json:"bucket_id"`
	PollInterval     string `json:"poll_interval"`
	RequestTimeout   string `json:"request_timeout"`
	InitialLookback  string `json:"initial_lookback"`
	RescanWindow     string `json:"rescan_window"`
	PageSize         int    `json:"page_size"`
	MaxPagesPerPoll  int    `json:"max_pages_per_poll"`
	MaxResponseBytes int64  `json:"max_response_bytes"`
	ClockErrorMS     int64  `json:"clock_error_ms"`
}

type TimelineConfig struct {
	ClockUncertainAfter string `json:"clock_uncertain_after"`
	MaxQueryRange       string `json:"max_query_range"`
	MaxProjectionFacts  int    `json:"max_projection_facts"`
}

type ServerConfig struct {
	ListenAddress    string `json:"listen_address"`
	AllowNonLoopback bool   `json:"allow_non_loopback"`
	ReadHeader       string `json:"read_header_timeout"`
	Read             string `json:"read_timeout"`
	Write            string `json:"write_timeout"`
	Idle             string `json:"idle_timeout"`
	Shutdown         string `json:"shutdown_timeout"`
}

type LoggingConfig struct {
	Level        string `json:"level"`
	FileEnabled  bool   `json:"file_enabled"`
	MaxFileBytes int64  `json:"max_file_bytes"`
	MaxFiles     int    `json:"max_files"`
}

type PathsConfig struct {
	DataDirectory string `json:"data_directory"`
}

type StorageConfig struct {
	BusyTimeout          string `json:"busy_timeout"`
	MaxOpenConnections   int    `json:"max_open_connections"`
	WarningFreeBytes     int64  `json:"warning_free_bytes"`
	CriticalFreeBytes    int64  `json:"critical_free_bytes"`
	DatabaseReserveBytes int64  `json:"database_reserve_bytes"`
}

type OperationsConfig struct {
	DiskCheckInterval     string `json:"disk_check_interval"`
	WALCheckpointInterval string `json:"wal_checkpoint_interval"`
	WALMaxBytes           int64  `json:"wal_max_bytes"`
	TempCleanupInterval   string `json:"temp_cleanup_interval"`
	TempMaxAge            string `json:"temp_max_age"`
	TempMaxFiles          int    `json:"temp_max_files"`
}

type RetentionConfig struct {
	Enabled           bool   `json:"enabled"`
	ScanInterval      string `json:"scan_interval"`
	MinimumAge        string `json:"minimum_age"`
	RequireFullBackup bool   `json:"require_full_backup"`
	MaxDeletesPerRun  int    `json:"max_deletes_per_run"`
}

type APIConfig struct {
	MaxRequestBytes     int64 `json:"max_request_bytes"`
	MaxBatchEvents      int   `json:"max_batch_events"`
	MaxEventBytes       int   `json:"max_event_bytes"`
	MaxPayloadDepth     int   `json:"max_payload_depth"`
	MaxConcurrentWrites int   `json:"max_concurrent_writes"`
	DefaultPageSize     int   `json:"default_page_size"`
	MaxPageSize         int   `json:"max_page_size"`
}

type MediaIngestConfig struct {
	Enabled            bool   `json:"enabled"`
	InboxDirectory     string `json:"inbox_directory"`
	ScanInterval       string `json:"scan_interval"`
	SettleInterval     string `json:"settle_interval"`
	MaxSegmentBytes    int64  `json:"max_segment_bytes"`
	MaxSegmentDuration string `json:"max_segment_duration"`
	MaxSidecarBytes    int64  `json:"max_sidecar_bytes"`
	MaxScanEntries     int    `json:"max_scan_entries"`
	FFprobePath        string `json:"ffprobe_path"`
	FFprobeTimeout     string `json:"ffprobe_timeout"`
}

// Error carries a stable error code without forcing callers to inspect text.
type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// ErrorCode returns the stable code for a configuration error.
func ErrorCode(err error) string {
	var configErr *Error
	if errors.As(err, &configErr) {
		return configErr.Code
	}
	return CodeDecodeFailed
}

func defaultConfig() Config {
	return Config{
		SchemaVersion: CurrentSchemaVersion,
		Runtime: RuntimeConfig{
			Mode:                   ModeRecordOnly,
			BackupInterfaceEnabled: true,
		},
		Server: ServerConfig{
			ListenAddress:    "127.0.0.1:47831",
			AllowNonLoopback: false,
			ReadHeader:       "5s",
			Read:             "10s",
			Write:            "10s",
			Idle:             "30s",
			Shutdown:         "10s",
		},
		Paths: PathsConfig{DataDirectory: "data"},
		Storage: StorageConfig{
			BusyTimeout:          "5s",
			MaxOpenConnections:   8,
			WarningFreeBytes:     10 << 30,
			CriticalFreeBytes:    5 << 30,
			DatabaseReserveBytes: 1 << 30,
		},
		API: APIConfig{
			MaxRequestBytes:     1 << 20,
			MaxBatchEvents:      100,
			MaxEventBytes:       64 << 10,
			MaxPayloadDepth:     16,
			MaxConcurrentWrites: 4,
			DefaultPageSize:     100,
			MaxPageSize:         500,
		},
		MediaIngest: MediaIngestConfig{
			Enabled:            false,
			InboxDirectory:     "media-inbox",
			ScanInterval:       "1s",
			SettleInterval:     "1s",
			MaxSegmentBytes:    2 << 30,
			MaxSegmentDuration: "10m",
			MaxSidecarBytes:    64 << 10,
			MaxScanEntries:     1000,
			FFprobeTimeout:     "30s",
		},
		Collectors: []CollectorConfig{},
		Timeline: TimelineConfig{
			ClockUncertainAfter: "1s",
			MaxQueryRange:       "744h",
			MaxProjectionFacts:  100000,
		},
		Operations: OperationsConfig{
			DiskCheckInterval:     "30s",
			WALCheckpointInterval: "5m",
			WALMaxBytes:           64 << 20,
			TempCleanupInterval:   "1h",
			TempMaxAge:            "24h",
			TempMaxFiles:          1000,
		},
		Retention: RetentionConfig{
			Enabled: false, ScanInterval: "1h", MinimumAge: "168h",
			RequireFullBackup: true, MaxDeletesPerRun: 100,
		},
		Logging: LoggingConfig{Level: "info", FileEnabled: true, MaxFileBytes: 10 << 20, MaxFiles: 5},
	}
}

// Load applies defaults, an optional JSON file, then environment overrides.
// It validates the final result without creating directories or other runtime state.
func Load(path string, lookup LookupEnv) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	cfg := defaultConfig()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, &Error{Code: CodeReadFailed, Err: fmt.Errorf("read config %q: %w", path, err)}
		}
		if err := decodeFile(raw, &cfg); err != nil {
			return Config{}, err
		}
	}

	if err := applyEnvironment(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if err := resolveDataDirectory(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func decodeFile(raw []byte, cfg *Config) error {
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if !utf8.Valid(raw) {
		return &Error{Code: CodeDecodeFailed, Err: errors.New("config JSON must be valid UTF-8")}
	}
	if err := strictjson.ValidateObjectKeys(raw, 0); err != nil {
		return &Error{Code: CodeDecodeFailed, Err: errors.New("config JSON contains a duplicate key or is invalid")}
	}
	rootFields := []string{"schema_version", "runtime", "server", "paths", "storage", "api", "media_ingest", "collectors", "timeline", "operations", "retention", "logging"}
	if err := strictjson.ValidateExactRootObject(raw, 1, rootFields...); err != nil {
		return &Error{Code: CodeDecodeFailed, Err: errors.New("config root fields must exactly match the versioned schema")}
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return &Error{Code: CodeDecodeFailed, Err: fmt.Errorf("decode config: %w", err)}
	}
	if _, ok := fields["schema_version"]; !ok {
		return &Error{Code: CodeMissingSchema, Err: errors.New("config must declare schema_version")}
	}
	sections := []struct {
		name    string
		allowed []string
	}{
		{name: "runtime", allowed: []string{"mode", "backup_interface_enabled"}},
		{name: "server", allowed: []string{"listen_address", "allow_non_loopback", "read_header_timeout", "read_timeout", "write_timeout", "idle_timeout", "shutdown_timeout"}},
		{name: "paths", allowed: []string{"data_directory"}},
		{name: "storage", allowed: []string{"busy_timeout", "max_open_connections", "warning_free_bytes", "critical_free_bytes", "database_reserve_bytes"}},
		{name: "api", allowed: []string{"max_request_bytes", "max_batch_events", "max_event_bytes", "max_payload_depth", "max_concurrent_writes", "default_page_size", "max_page_size"}},
		{name: "media_ingest", allowed: []string{"enabled", "inbox_directory", "scan_interval", "settle_interval", "max_segment_bytes", "max_segment_duration", "max_sidecar_bytes", "max_scan_entries", "ffprobe_path", "ffprobe_timeout"}},
		{name: "timeline", allowed: []string{"clock_uncertain_after", "max_query_range", "max_projection_facts"}},
		{name: "operations", allowed: []string{"disk_check_interval", "wal_checkpoint_interval", "wal_max_bytes", "temp_cleanup_interval", "temp_max_age", "temp_max_files"}},
		{name: "retention", allowed: []string{"enabled", "scan_interval", "minimum_age", "require_full_backup", "max_deletes_per_run"}},
		{name: "logging", allowed: []string{"level", "file_enabled", "max_file_bytes", "max_files"}},
	}
	for _, section := range sections {
		if value, ok := fields[section.name]; ok {
			if err := strictjson.ValidateExactRootObject(value, 1, section.allowed...); err != nil {
				return &Error{Code: CodeDecodeFailed, Err: fmt.Errorf("config section %s fields must exactly match the versioned schema", section.name)}
			}
		}
	}
	if value, ok := fields["collectors"]; ok {
		if err := validateCollectorJSON(value); err != nil {
			return &Error{Code: CodeDecodeFailed, Err: err}
		}
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return &Error{Code: CodeDecodeFailed, Err: fmt.Errorf("decode config: %w", err)}
	}
	if err := ensureEOF(decoder); err != nil {
		return &Error{Code: CodeDecodeFailed, Err: err}
	}
	return nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("config contains multiple JSON values")
		}
		return fmt.Errorf("decode trailing config data: %w", err)
	}
	return nil
}

func validateCollectorJSON(raw json.RawMessage) error {
	var collectors []json.RawMessage
	if err := json.Unmarshal(raw, &collectors); err != nil {
		return errors.New("config collectors must be an array")
	}
	for index, collectorRaw := range collectors {
		if err := strictjson.ValidateExactRootObject(collectorRaw, 1,
			"id", "kind", "enabled", "heartbeat_period", "allowed_lateness", "offline_after", "planned_schedule", "activitywatch"); err != nil {
			return fmt.Errorf("config collector %d fields must match the versioned schema", index)
		}
		var collector map[string]json.RawMessage
		if err := json.Unmarshal(collectorRaw, &collector); err != nil {
			return fmt.Errorf("config collector %d is invalid", index)
		}
		var collectorHeader struct {
			Kind    string `json:"kind"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.Unmarshal(collectorRaw, &collectorHeader); err != nil {
			return fmt.Errorf("config collector %d is invalid", index)
		}
		if scheduleRaw, ok := collector["planned_schedule"]; ok {
			if err := strictjson.ValidateExactRootObject(scheduleRaw, 1, "timezone", "windows"); err != nil {
				return fmt.Errorf("config collector %d planned_schedule fields must match the versioned schema", index)
			}
			var schedule struct {
				Windows []json.RawMessage `json:"windows"`
			}
			if err := json.Unmarshal(scheduleRaw, &schedule); err != nil {
				return fmt.Errorf("config collector %d planned_schedule is invalid", index)
			}
			for windowIndex, windowRaw := range schedule.Windows {
				if err := strictjson.ValidateExactRootObject(windowRaw, 1, "days", "start_local", "end_local"); err != nil {
					return fmt.Errorf("config collector %d schedule window %d fields must match the versioned schema", index, windowIndex)
				}
			}
		}
		if activityWatchRaw, ok := collector["activitywatch"]; ok {
			activityWatchFields := []string{"base_url", "bucket_id", "poll_interval", "request_timeout", "initial_lookback", "rescan_window", "page_size", "max_pages_per_poll", "max_response_bytes", "clock_error_ms"}
			if err := strictjson.ValidateExactRootObject(activityWatchRaw, 1, activityWatchFields...); err != nil {
				return fmt.Errorf("config collector %d activitywatch fields must match the versioned schema", index)
			}
			if collectorHeader.Enabled && collectorHeader.Kind == CollectorActivityWatch {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(activityWatchRaw, &fields); err != nil {
					return fmt.Errorf("config collector %d activitywatch settings are invalid", index)
				}
				for _, field := range activityWatchFields {
					if _, exists := fields[field]; !exists {
						return fmt.Errorf("config collector %d activitywatch field %s is required", index, field)
					}
				}
			}
		}
	}
	return nil
}

func applyEnvironment(cfg *Config, lookup LookupEnv) error {
	if value, ok := lookup(EnvRuntimeMode); ok {
		cfg.Runtime.Mode = value
	}
	if value, ok := lookup(EnvListenAddress); ok {
		cfg.Server.ListenAddress = value
	}
	if value, ok := lookup(EnvAllowNonLoopback); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return &Error{Code: CodeInvalidEnv, Err: fmt.Errorf("%s must be a boolean", EnvAllowNonLoopback)}
		}
		cfg.Server.AllowNonLoopback = parsed
	}
	if value, ok := lookup(EnvDataDirectory); ok {
		cfg.Paths.DataDirectory = value
	}
	if value, ok := lookup(EnvLogLevel); ok {
		cfg.Logging.Level = value
	}
	if value, ok := lookup(EnvMediaEnabled); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return &Error{Code: CodeInvalidEnv, Err: fmt.Errorf("%s must be a boolean", EnvMediaEnabled)}
		}
		cfg.MediaIngest.Enabled = parsed
	}
	if value, ok := lookup(EnvMediaInbox); ok {
		cfg.MediaIngest.InboxDirectory = value
	}
	if value, ok := lookup(EnvMediaScan); ok {
		cfg.MediaIngest.ScanInterval = value
	}
	if value, ok := lookup(EnvMediaSettle); ok {
		cfg.MediaIngest.SettleInterval = value
	}
	if value, ok := lookup(EnvMediaMaxDuration); ok {
		cfg.MediaIngest.MaxSegmentDuration = value
	}
	if value, ok := lookup(EnvFFprobePath); ok {
		cfg.MediaIngest.FFprobePath = value
	}
	if value, ok := lookup(EnvFFprobeTimeout); ok {
		cfg.MediaIngest.FFprobeTimeout = value
	}
	if value, ok := lookup(EnvReadHeader); ok {
		cfg.Server.ReadHeader = value
	}
	if value, ok := lookup(EnvRead); ok {
		cfg.Server.Read = value
	}
	if value, ok := lookup(EnvWrite); ok {
		cfg.Server.Write = value
	}
	if value, ok := lookup(EnvIdle); ok {
		cfg.Server.Idle = value
	}
	if value, ok := lookup(EnvShutdown); ok {
		cfg.Server.Shutdown = value
	}
	if value, ok := lookup(EnvBusyTimeout); ok {
		cfg.Storage.BusyTimeout = value
	}
	integerOverrides := []struct {
		name   string
		target *int
	}{
		{name: EnvMaxOpenConns, target: &cfg.Storage.MaxOpenConnections},
		{name: EnvMaxBatchEvents, target: &cfg.API.MaxBatchEvents},
		{name: EnvMaxEventBytes, target: &cfg.API.MaxEventBytes},
		{name: EnvMaxPayloadDepth, target: &cfg.API.MaxPayloadDepth},
		{name: EnvMaxWrites, target: &cfg.API.MaxConcurrentWrites},
		{name: EnvDefaultPageSize, target: &cfg.API.DefaultPageSize},
		{name: EnvMaxPageSize, target: &cfg.API.MaxPageSize},
		{name: EnvMediaScanEntries, target: &cfg.MediaIngest.MaxScanEntries},
	}
	for _, override := range integerOverrides {
		if value, ok := lookup(override.name); ok {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return &Error{Code: CodeInvalidEnv, Err: fmt.Errorf("%s must be an integer", override.name)}
			}
			*override.target = parsed
		}
	}
	if value, ok := lookup(EnvMaxRequestBytes); ok {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return &Error{Code: CodeInvalidEnv, Err: fmt.Errorf("%s must be an integer", EnvMaxRequestBytes)}
		}
		cfg.API.MaxRequestBytes = parsed
	}
	int64Overrides := []struct {
		name   string
		target *int64
	}{
		{name: EnvMediaMaxBytes, target: &cfg.MediaIngest.MaxSegmentBytes},
		{name: EnvMediaSidecar, target: &cfg.MediaIngest.MaxSidecarBytes},
	}
	for _, override := range int64Overrides {
		if value, ok := lookup(override.name); ok {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return &Error{Code: CodeInvalidEnv, Err: fmt.Errorf("%s must be an integer", override.name)}
			}
			*override.target = parsed
		}
	}
	return nil
}

func resolveDataDirectory(cfg *Config, lookup LookupEnv) error {
	value := cfg.Paths.DataDirectory
	if strings.TrimSpace(value) != value || value == "" || strings.ContainsRune(value, '\x00') {
		return &Error{Code: CodeInvalidDataDir, Err: errors.New("paths.data_directory must be a non-empty filesystem path without surrounding whitespace")}
	}

	cleaned := filepath.Clean(value)
	if filepath.IsAbs(cleaned) {
		cfg.Paths.DataDirectory = cleaned
		return resolveMediaInbox(cfg)
	}
	if !filepath.IsLocal(cleaned) {
		return &Error{Code: CodeInvalidDataDir, Err: errors.New("relative paths.data_directory must be a local path within the application data root")}
	}

	localAppData, ok := lookup(envLocalAppData)
	if !ok || strings.TrimSpace(localAppData) != localAppData || !filepath.IsAbs(localAppData) {
		return &Error{Code: CodeInvalidDataDir, Err: errors.New("LOCALAPPDATA must provide an absolute application data root for relative paths")}
	}
	applicationRoot := filepath.Join(filepath.Clean(localAppData), "ExamMonitor")
	resolved := filepath.Clean(filepath.Join(applicationRoot, cleaned))
	relative, err := filepath.Rel(applicationRoot, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return &Error{Code: CodeInvalidDataDir, Err: errors.New("relative paths.data_directory must remain within the application data root")}
	}
	cfg.Paths.DataDirectory = resolved
	return resolveMediaInbox(cfg)
}

func resolveMediaInbox(cfg *Config) error {
	value := cfg.MediaIngest.InboxDirectory
	if strings.TrimSpace(value) != value || value == "" || strings.ContainsRune(value, '\x00') {
		return &Error{Code: CodeInvalidMedia, Err: errors.New("media_ingest.inbox_directory must be a non-empty filesystem path without surrounding whitespace")}
	}
	cleaned := filepath.Clean(value)
	if !filepath.IsAbs(cleaned) {
		if !filepath.IsLocal(cleaned) {
			return &Error{Code: CodeInvalidMedia, Err: errors.New("relative media_ingest.inbox_directory must remain inside the data directory")}
		}
		cleaned = filepath.Join(cfg.Paths.DataDirectory, cleaned)
	}
	cfg.MediaIngest.InboxDirectory = filepath.Clean(cleaned)
	managed := cfg.MediaStorageDirectory()
	if pathsOverlap(cfg.MediaIngest.InboxDirectory, managed) {
		return &Error{Code: CodeInvalidMedia, Err: errors.New("media inbox and managed media storage must not overlap")}
	}
	return nil
}

func pathsOverlap(first, second string) bool {
	return pathContains(first, second) || pathContains(second, first)
}

func pathContains(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func (cfg Config) Validate() error {
	if cfg.SchemaVersion != CurrentSchemaVersion {
		return &Error{
			Code: CodeUnsupportedSchema,
			Err:  fmt.Errorf("unsupported config schema_version %d", cfg.SchemaVersion),
		}
	}

	host, port, err := net.SplitHostPort(cfg.Server.ListenAddress)
	if err != nil || strings.TrimSpace(host) != host || strings.TrimSpace(port) != port {
		return &Error{Code: CodeInvalidAddress, Err: errors.New("server.listen_address must be an IP literal and port")}
	}
	ip := net.ParseIP(host)
	portNumber, portErr := strconv.Atoi(port)
	if ip == nil || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return &Error{Code: CodeInvalidAddress, Err: errors.New("server.listen_address must be an IP literal and port between 1 and 65535")}
	}
	if !ip.IsLoopback() && !cfg.Server.AllowNonLoopback {
		return &Error{Code: CodeNonLoopback, Err: errors.New("non-loopback listen address requires server.allow_non_loopback=true")}
	}
	if !filepath.IsAbs(cfg.Paths.DataDirectory) {
		return &Error{Code: CodeInvalidDataDir, Err: errors.New("paths.data_directory must resolve to an absolute filesystem path")}
	}
	if cfg.Runtime.Mode != ModeRecordOnly && cfg.Runtime.Mode != ModeMinimum {
		return &Error{Code: CodeInvalidRuntime, Err: errors.New("runtime.mode must be record-only or minimum")}
	}

	switch strings.ToLower(cfg.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		return &Error{Code: CodeInvalidLogLevel, Err: errors.New("logging.level must be debug, info, warn, or error")}
	}
	if cfg.Logging.MaxFileBytes < 1<<20 || cfg.Logging.MaxFileBytes > 1<<30 || cfg.Logging.MaxFiles < 2 || cfg.Logging.MaxFiles > 100 {
		return &Error{Code: CodeInvalidLogLevel, Err: errors.New("logging rotation must satisfy 1MiB <= max_file_bytes <= 1GiB and 2 <= max_files <= 100")}
	}

	readHeaderTimeout, err := validateDuration("server.read_header_timeout", cfg.Server.ReadHeader)
	if err != nil {
		return err
	}
	readTimeout, err := validateDuration("server.read_timeout", cfg.Server.Read)
	if err != nil {
		return err
	}
	if readTimeout < readHeaderTimeout {
		return &Error{Code: CodeInvalidTimeout, Err: errors.New("server.read_timeout must be greater than or equal to server.read_header_timeout")}
	}
	if _, err := validateDuration("server.write_timeout", cfg.Server.Write); err != nil {
		return err
	}
	if _, err := validateDuration("server.idle_timeout", cfg.Server.Idle); err != nil {
		return err
	}
	if _, err := validateDuration("server.shutdown_timeout", cfg.Server.Shutdown); err != nil {
		return err
	}
	busyTimeout, err := time.ParseDuration(cfg.Storage.BusyTimeout)
	if err != nil || busyTimeout < 100*time.Millisecond || busyTimeout > 30*time.Second {
		return &Error{Code: CodeInvalidStorage, Err: errors.New("storage.busy_timeout must be between 100ms and 30s")}
	}
	if cfg.Storage.MaxOpenConnections < 1 || cfg.Storage.MaxOpenConnections > 32 {
		return &Error{Code: CodeInvalidStorage, Err: errors.New("storage.max_open_connections must be between 1 and 32")}
	}
	if cfg.Storage.DatabaseReserveBytes < 64<<20 || cfg.Storage.CriticalFreeBytes <= cfg.Storage.DatabaseReserveBytes || cfg.Storage.WarningFreeBytes <= cfg.Storage.CriticalFreeBytes {
		return &Error{Code: CodeInvalidStorage, Err: errors.New("storage thresholds must satisfy warning_free_bytes > critical_free_bytes > database_reserve_bytes >= 64MiB")}
	}
	if cfg.Storage.WarningFreeBytes > 1<<50 {
		return &Error{Code: CodeInvalidStorage, Err: errors.New("storage.warning_free_bytes must not exceed 1PiB")}
	}
	if _, err := validateOperationsDuration("operations.disk_check_interval", cfg.Operations.DiskCheckInterval, time.Second, 10*time.Minute); err != nil {
		return err
	}
	if _, err := validateOperationsDuration("operations.wal_checkpoint_interval", cfg.Operations.WALCheckpointInterval, time.Second, 24*time.Hour); err != nil {
		return err
	}
	if _, err := validateOperationsDuration("operations.temp_cleanup_interval", cfg.Operations.TempCleanupInterval, time.Minute, 24*time.Hour); err != nil {
		return err
	}
	if _, err := validateOperationsDuration("operations.temp_max_age", cfg.Operations.TempMaxAge, time.Hour, 30*24*time.Hour); err != nil {
		return err
	}
	if cfg.Operations.WALMaxBytes < 1<<20 || cfg.Operations.WALMaxBytes > 4<<30 || cfg.Operations.TempMaxFiles < 1 || cfg.Operations.TempMaxFiles > 100000 {
		return &Error{Code: CodeInvalidOperations, Err: errors.New("operations WAL or temporary-file limits are invalid")}
	}
	if _, err := validateRetentionDuration("retention.scan_interval", cfg.Retention.ScanInterval, time.Minute, 24*time.Hour); err != nil {
		return err
	}
	if _, err := validateRetentionDuration("retention.minimum_age", cfg.Retention.MinimumAge, 24*time.Hour, 10*365*24*time.Hour); err != nil {
		return err
	}
	if cfg.Retention.MaxDeletesPerRun < 1 || cfg.Retention.MaxDeletesPerRun > 10000 {
		return &Error{Code: CodeInvalidRetention, Err: errors.New("retention.max_deletes_per_run must be between 1 and 10000")}
	}
	if cfg.Retention.Enabled && !cfg.Retention.RequireFullBackup {
		return &Error{Code: CodeInvalidRetention, Err: errors.New("enabled retention requires a verified full backup")}
	}
	if cfg.API.MaxRequestBytes < 64<<10 || cfg.API.MaxRequestBytes > 16<<20 {
		return &Error{Code: CodeInvalidAPILimit, Err: errors.New("api.max_request_bytes must be between 65536 and 16777216")}
	}
	if cfg.API.MaxBatchEvents < 1 || cfg.API.MaxBatchEvents > 1000 {
		return &Error{Code: CodeInvalidAPILimit, Err: errors.New("api.max_batch_events must be between 1 and 1000")}
	}
	if cfg.API.MaxEventBytes < 1024 || cfg.API.MaxEventBytes > 1<<20 || int64(cfg.API.MaxEventBytes) >= cfg.API.MaxRequestBytes {
		return &Error{Code: CodeInvalidAPILimit, Err: errors.New("api.max_event_bytes must be between 1024 and 1048576 and below api.max_request_bytes")}
	}
	if cfg.API.MaxPayloadDepth < 1 || cfg.API.MaxPayloadDepth > 64 {
		return &Error{Code: CodeInvalidAPILimit, Err: errors.New("api.max_payload_depth must be between 1 and 64")}
	}
	if cfg.API.MaxConcurrentWrites < 1 || cfg.API.MaxConcurrentWrites > 32 {
		return &Error{Code: CodeInvalidAPILimit, Err: errors.New("api.max_concurrent_writes must be between 1 and 32")}
	}
	if cfg.API.DefaultPageSize < 1 || cfg.API.MaxPageSize < cfg.API.DefaultPageSize || cfg.API.MaxPageSize > 1000 {
		return &Error{Code: CodeInvalidAPILimit, Err: errors.New("api page sizes must satisfy 1 <= default_page_size <= max_page_size <= 1000")}
	}
	if !filepath.IsAbs(cfg.MediaIngest.InboxDirectory) || pathsOverlap(cfg.MediaIngest.InboxDirectory, cfg.MediaStorageDirectory()) {
		return &Error{Code: CodeInvalidMedia, Err: errors.New("media ingest paths must be absolute and non-overlapping")}
	}
	if _, err := validateMediaDuration("media_ingest.scan_interval", cfg.MediaIngest.ScanInterval, 100*time.Millisecond, time.Minute); err != nil {
		return err
	}
	if _, err := validateMediaDuration("media_ingest.settle_interval", cfg.MediaIngest.SettleInterval, 100*time.Millisecond, time.Minute); err != nil {
		return err
	}
	if _, err := validateMediaDuration("media_ingest.max_segment_duration", cfg.MediaIngest.MaxSegmentDuration, time.Second, 10*time.Minute); err != nil {
		return err
	}
	if _, err := validateMediaDuration("media_ingest.ffprobe_timeout", cfg.MediaIngest.FFprobeTimeout, time.Second, 2*time.Minute); err != nil {
		return err
	}
	if cfg.MediaIngest.MaxSegmentBytes < 1024 || cfg.MediaIngest.MaxSegmentBytes > 64<<30 {
		return &Error{Code: CodeInvalidMedia, Err: errors.New("media_ingest.max_segment_bytes must be between 1024 and 68719476736")}
	}
	if cfg.MediaIngest.MaxSidecarBytes < 1024 || cfg.MediaIngest.MaxSidecarBytes > 1<<20 {
		return &Error{Code: CodeInvalidMedia, Err: errors.New("media_ingest.max_sidecar_bytes must be between 1024 and 1048576")}
	}
	if cfg.MediaIngest.MaxScanEntries < 1 || cfg.MediaIngest.MaxScanEntries > 10000 {
		return &Error{Code: CodeInvalidMedia, Err: errors.New("media_ingest.max_scan_entries must be between 1 and 10000")}
	}
	if cfg.MediaIngest.Enabled && !filepath.IsAbs(cfg.MediaIngest.FFprobePath) {
		return &Error{Code: CodeInvalidMedia, Err: errors.New("media_ingest.ffprobe_path must be absolute when media ingest is enabled")}
	}
	if err := cfg.validateTimeline(); err != nil {
		return err
	}
	if err := cfg.validateCollectors(); err != nil {
		return err
	}
	return nil
}

func (cfg Config) validateTimeline() error {
	uncertain, err := time.ParseDuration(cfg.Timeline.ClockUncertainAfter)
	if err != nil || uncertain < time.Millisecond || uncertain > 24*time.Hour {
		return &Error{Code: CodeInvalidTimeline, Err: errors.New("timeline.clock_uncertain_after must be between 1ms and 24h")}
	}
	queryRange, err := time.ParseDuration(cfg.Timeline.MaxQueryRange)
	if err != nil || queryRange < time.Minute || queryRange > 366*24*time.Hour {
		return &Error{Code: CodeInvalidTimeline, Err: errors.New("timeline.max_query_range must be between 1m and 8784h")}
	}
	if cfg.Timeline.MaxProjectionFacts < 100 || cfg.Timeline.MaxProjectionFacts > 1000000 {
		return &Error{Code: CodeInvalidTimeline, Err: errors.New("timeline.max_projection_facts must be between 100 and 1000000")}
	}
	return nil
}

func (cfg Config) validateCollectors() error {
	if len(cfg.Collectors) > 64 {
		return &Error{Code: CodeInvalidCollector, Err: errors.New("collectors cannot contain more than 64 entries")}
	}
	seen := make(map[string]struct{}, len(cfg.Collectors))
	enabledActivityWatch := 0
	enabledMedia := 0
	for index := range cfg.Collectors {
		collector := &cfg.Collectors[index]
		if !validConfigIdentifier(collector.ID, 128) {
			return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("collectors[%d].id is invalid", index)}
		}
		if _, exists := seen[collector.ID]; exists {
			return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("collector id %q is duplicated", collector.ID)}
		}
		seen[collector.ID] = struct{}{}
		switch collector.Kind {
		case CollectorActivityWatch, CollectorGenericJSON, CollectorMedia:
		default:
			return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("collectors[%d].kind is unsupported", index)}
		}
		if !collector.Enabled {
			continue
		}
		heartbeat, err := parseCollectorDuration("heartbeat_period", collector.HeartbeatPeriod, time.Second, 10*time.Minute)
		if err != nil {
			return err
		}
		lateness, err := parseCollectorDuration("allowed_lateness", collector.AllowedLateness, 0, time.Hour)
		if err != nil {
			return err
		}
		offline, err := parseCollectorDuration("offline_after", collector.OfflineAfter, time.Second, 24*time.Hour)
		if err != nil {
			return err
		}
		if heartbeat+lateness > offline {
			return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("collector %q must satisfy heartbeat_period + allowed_lateness <= offline_after", collector.ID)}
		}
		if err := validateSchedule(collector.ID, collector.PlannedSchedule); err != nil {
			return err
		}
		switch collector.Kind {
		case CollectorActivityWatch:
			enabledActivityWatch++
			if offline > 5*time.Minute {
				return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("ActivityWatch collector %q offline_after exceeds the V1 5m SLA", collector.ID)}
			}
			if err := validateActivityWatch(collector.ID, collector.ActivityWatch, cfg.API.MaxBatchEvents, heartbeat); err != nil {
				return err
			}
		case CollectorMedia:
			enabledMedia++
			if offline > 15*time.Minute {
				return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("media collector %q offline_after exceeds the V1 15m SLA", collector.ID)}
			}
			if collector.ActivityWatch != nil {
				return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("media collector %q must not configure activitywatch", collector.ID)}
			}
		case CollectorGenericJSON:
			if collector.ActivityWatch != nil {
				return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("generic JSON collector %q must not configure activitywatch", collector.ID)}
			}
		}
	}
	if cfg.Runtime.Mode == ModeMinimum {
		if !cfg.Runtime.BackupInterfaceEnabled {
			return &Error{Code: CodeInvalidRuntime, Err: errors.New("minimum mode requires the backup interface")}
		}
		if enabledActivityWatch == 0 || enabledMedia == 0 || !cfg.MediaIngest.Enabled {
			return &Error{Code: CodeInvalidRuntime, Err: errors.New("minimum mode requires enabled ActivityWatch and media collectors plus media ingest")}
		}
		for _, collector := range cfg.Collectors {
			if collector.Enabled && collector.Kind != CollectorActivityWatch && collector.Kind != CollectorMedia {
				return &Error{Code: CodeInvalidRuntime, Err: errors.New("minimum mode may enable only ActivityWatch and media collectors")}
			}
		}
	}
	if enabledActivityWatch > 4 {
		return &Error{Code: CodeInvalidCollector, Err: errors.New("Version 1 supports at most 4 enabled ActivityWatch collectors so every poll stays within the offline SLA")}
	}
	return nil
}

func validateActivityWatch(collectorID string, aw *ActivityWatchConfig, maxBatch int, heartbeatPeriod time.Duration) error {
	if aw == nil {
		return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("ActivityWatch collector %q requires activitywatch settings", collectorID)}
	}
	parsed, err := url.Parse(aw.BaseURL)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("ActivityWatch collector %q base_url must be a plain loopback HTTP origin", collectorID)}
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	port, portErr := strconv.Atoi(parsed.Port())
	if ip == nil || !ip.IsLoopback() || portErr != nil || port < 1 || port > 65535 {
		return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("ActivityWatch collector %q base_url must use a loopback IP and explicit port", collectorID)}
	}
	if !validConfigIdentifier(aw.BucketID, 256) || strings.ContainsAny(aw.BucketID, "/\\?#") {
		return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("ActivityWatch collector %q bucket_id is invalid", collectorID)}
	}
	for name, value := range map[string]string{
		"poll_interval": aw.PollInterval, "request_timeout": aw.RequestTimeout,
		"initial_lookback": aw.InitialLookback, "rescan_window": aw.RescanWindow,
	} {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 || duration > 366*24*time.Hour {
			return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("ActivityWatch collector %q %s is invalid", collectorID, name)}
		}
	}
	if poll, _ := time.ParseDuration(aw.PollInterval); poll < time.Second || poll > 10*time.Minute {
		return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("ActivityWatch collector %q poll_interval must be between 1s and 10m", collectorID)}
	}
	if poll, _ := time.ParseDuration(aw.PollInterval); poll > heartbeatPeriod {
		return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("ActivityWatch collector %q poll_interval must not exceed heartbeat_period", collectorID)}
	}
	if timeout, _ := time.ParseDuration(aw.RequestTimeout); timeout < 100*time.Millisecond || timeout > time.Minute {
		return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("ActivityWatch collector %q request_timeout must be between 100ms and 1m", collectorID)}
	}
	if aw.PageSize < 1 || aw.PageSize > maxBatch || aw.MaxPagesPerPoll < 1 || aw.MaxPagesPerPoll > 100 || aw.MaxResponseBytes < 1024 || aw.MaxResponseBytes > 8<<20 || aw.ClockErrorMS < 0 || aw.ClockErrorMS > 24*60*60*1000 {
		return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("ActivityWatch collector %q paging, response, or clock limits are invalid", collectorID)}
	}
	return nil
}

func validateSchedule(collectorID string, schedule PlannedScheduleConfig) error {
	if schedule.Timezone == "" || schedule.Timezone == "Local" {
		return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("collector %q planned_schedule.timezone is required", collectorID)}
	}
	if _, err := time.LoadLocation(schedule.Timezone); err != nil {
		return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("collector %q planned_schedule.timezone is invalid", collectorID)}
	}
	if len(schedule.Windows) == 0 || len(schedule.Windows) > 64 {
		return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("collector %q planned_schedule.windows must contain 1 to 64 entries", collectorID)}
	}
	validDays := map[string]bool{"monday": true, "tuesday": true, "wednesday": true, "thursday": true, "friday": true, "saturday": true, "sunday": true}
	for index, window := range schedule.Windows {
		if len(window.Days) == 0 || len(window.Days) > 7 {
			return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("collector %q schedule window %d days are invalid", collectorID, index)}
		}
		seenDays := map[string]bool{}
		for _, day := range window.Days {
			if !validDays[day] || seenDays[day] {
				return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("collector %q schedule window %d contains an invalid or duplicate day", collectorID, index)}
			}
			seenDays[day] = true
		}
		start, ok := parseClockMinute(window.StartLocal, false)
		if !ok {
			return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("collector %q schedule window %d start_local is invalid", collectorID, index)}
		}
		end, ok := parseClockMinute(window.EndLocal, true)
		if !ok || start >= end {
			return &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("collector %q schedule window %d must satisfy start_local < end_local", collectorID, index)}
		}
	}
	return nil
}

func parseClockMinute(value string, allowEndOfDay bool) (int, bool) {
	if allowEndOfDay && value == "24:00" {
		return 24 * 60, true
	}
	parsed, err := time.Parse("15:04", value)
	if err != nil || parsed.Format("15:04") != value {
		return 0, false
	}
	return parsed.Hour()*60 + parsed.Minute(), true
}

func parseCollectorDuration(name, value string, minimum, maximum time.Duration) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration < minimum || duration > maximum {
		return 0, &Error{Code: CodeInvalidCollector, Err: fmt.Errorf("collector %s must be between %s and %s", name, minimum, maximum)}
	}
	return duration, nil
}

func validConfigIdentifier(value string, maximum int) bool {
	if value == "" || len(value) > maximum || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validateMediaDuration(name, value string, minimum, maximum time.Duration) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration < minimum || duration > maximum {
		return 0, &Error{Code: CodeInvalidMedia, Err: fmt.Errorf("%s must be between %s and %s", name, minimum, maximum)}
	}
	return duration, nil
}

func validateOperationsDuration(name, value string, minimum, maximum time.Duration) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration < minimum || duration > maximum {
		return 0, &Error{Code: CodeInvalidOperations, Err: fmt.Errorf("%s must be between %s and %s", name, minimum, maximum)}
	}
	return duration, nil
}

func validateRetentionDuration(name, value string, minimum, maximum time.Duration) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration < minimum || duration > maximum {
		return 0, &Error{Code: CodeInvalidRetention, Err: fmt.Errorf("%s must be between %s and %s", name, minimum, maximum)}
	}
	return duration, nil
}

func validateDuration(name, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 100*time.Millisecond || duration > 2*time.Minute {
		return 0, &Error{
			Code: CodeInvalidTimeout,
			Err:  fmt.Errorf("%s must be between 100ms and 2m", name),
		}
	}
	return duration, nil
}

func (cfg Config) ReadHeaderTimeout() time.Duration {
	duration, _ := time.ParseDuration(cfg.Server.ReadHeader)
	return duration
}

func (cfg Config) ReadTimeout() time.Duration {
	duration, _ := time.ParseDuration(cfg.Server.Read)
	return duration
}

func (cfg Config) WriteTimeout() time.Duration {
	duration, _ := time.ParseDuration(cfg.Server.Write)
	return duration
}

func (cfg Config) IdleTimeout() time.Duration {
	duration, _ := time.ParseDuration(cfg.Server.Idle)
	return duration
}

func (cfg Config) ShutdownTimeout() time.Duration {
	duration, _ := time.ParseDuration(cfg.Server.Shutdown)
	return duration
}

func (cfg Config) BusyTimeout() time.Duration {
	duration, _ := time.ParseDuration(cfg.Storage.BusyTimeout)
	return duration
}

func (cfg Config) DiskCheckInterval() time.Duration {
	duration, _ := time.ParseDuration(cfg.Operations.DiskCheckInterval)
	return duration
}

func (cfg Config) WALCheckpointInterval() time.Duration {
	duration, _ := time.ParseDuration(cfg.Operations.WALCheckpointInterval)
	return duration
}

func (cfg Config) TempCleanupInterval() time.Duration {
	duration, _ := time.ParseDuration(cfg.Operations.TempCleanupInterval)
	return duration
}

func (cfg Config) TempMaxAge() time.Duration {
	duration, _ := time.ParseDuration(cfg.Operations.TempMaxAge)
	return duration
}

func (cfg Config) RetentionScanInterval() time.Duration {
	duration, _ := time.ParseDuration(cfg.Retention.ScanInterval)
	return duration
}

func (cfg Config) RetentionMinimumAge() time.Duration {
	duration, _ := time.ParseDuration(cfg.Retention.MinimumAge)
	return duration
}

func (cfg Config) LogDirectory() string { return filepath.Join(cfg.Paths.DataDirectory, "logs") }

func (cfg Config) DatabasePath() string {
	return filepath.Join(cfg.Paths.DataDirectory, "exam-monitor.db")
}

func (cfg Config) MediaStorageDirectory() string {
	return filepath.Join(cfg.Paths.DataDirectory, "media")
}

func (cfg Config) MediaScanInterval() time.Duration {
	duration, _ := time.ParseDuration(cfg.MediaIngest.ScanInterval)
	return duration
}

func (cfg Config) MediaSettleInterval() time.Duration {
	duration, _ := time.ParseDuration(cfg.MediaIngest.SettleInterval)
	return duration
}

func (cfg Config) MediaMaxSegmentDuration() time.Duration {
	duration, _ := time.ParseDuration(cfg.MediaIngest.MaxSegmentDuration)
	return duration
}

func (cfg Config) FFprobeTimeout() time.Duration {
	duration, _ := time.ParseDuration(cfg.MediaIngest.FFprobeTimeout)
	return duration
}

func (cfg Config) ClockUncertainAfter() time.Duration {
	duration, _ := time.ParseDuration(cfg.Timeline.ClockUncertainAfter)
	return duration
}

func (cfg Config) TimelineMaxQueryRange() time.Duration {
	duration, _ := time.ParseDuration(cfg.Timeline.MaxQueryRange)
	return duration
}

func (collector CollectorConfig) HeartbeatPeriodDuration() time.Duration {
	duration, _ := time.ParseDuration(collector.HeartbeatPeriod)
	return duration
}

func (collector CollectorConfig) AllowedLatenessDuration() time.Duration {
	duration, _ := time.ParseDuration(collector.AllowedLateness)
	return duration
}

func (collector CollectorConfig) OfflineAfterDuration() time.Duration {
	duration, _ := time.ParseDuration(collector.OfflineAfter)
	return duration
}

func (aw ActivityWatchConfig) PollIntervalDuration() time.Duration {
	duration, _ := time.ParseDuration(aw.PollInterval)
	return duration
}

func (aw ActivityWatchConfig) RequestTimeoutDuration() time.Duration {
	duration, _ := time.ParseDuration(aw.RequestTimeout)
	return duration
}

func (aw ActivityWatchConfig) InitialLookbackDuration() time.Duration {
	duration, _ := time.ParseDuration(aw.InitialLookback)
	return duration
}

func (aw ActivityWatchConfig) RescanWindowDuration() time.Duration {
	duration, _ := time.ParseDuration(aw.RescanWindow)
	return duration
}
