package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultsAreSafe(t *testing.T) {
	cfg, err := Load("", emptyEnvironment)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion = %d", cfg.SchemaVersion)
	}
	if cfg.Server.ListenAddress != "127.0.0.1:47831" {
		t.Fatalf("ListenAddress = %q", cfg.Server.ListenAddress)
	}
	if cfg.Server.AllowNonLoopback {
		t.Fatal("AllowNonLoopback must default to false")
	}
	wantDataDirectory := filepath.Join(testLocalAppData, "ExamMonitor", "data")
	if cfg.Paths.DataDirectory != wantDataDirectory {
		t.Fatalf("DataDirectory = %q", cfg.Paths.DataDirectory)
	}
	if cfg.ReadHeaderTimeout() != 5*time.Second || cfg.ReadTimeout() != 10*time.Second ||
		cfg.WriteTimeout() != 10*time.Second || cfg.IdleTimeout() != 30*time.Second {
		t.Fatalf("unsafe server timeout defaults: %+v", cfg.Server)
	}
	if cfg.Logging.Level != "info" {
		t.Fatalf("Log level = %q", cfg.Logging.Level)
	}
	if cfg.BusyTimeout() != 5*time.Second || cfg.Storage.MaxOpenConnections != 8 {
		t.Fatalf("unsafe storage defaults: %+v", cfg.Storage)
	}
	if cfg.Storage.WarningFreeBytes != 10<<30 || cfg.Storage.CriticalFreeBytes != 5<<30 || cfg.Storage.DatabaseReserveBytes != 1<<30 || cfg.Retention.Enabled || !cfg.Retention.RequireFullBackup || cfg.RetentionMinimumAge() != 168*time.Hour {
		t.Fatalf("unsafe M4 storage/retention defaults: storage=%+v retention=%+v", cfg.Storage, cfg.Retention)
	}
	if !cfg.Logging.FileEnabled || cfg.Logging.MaxFileBytes != 10<<20 || cfg.Logging.MaxFiles != 5 || cfg.Operations.WALMaxBytes != 64<<20 || cfg.TempMaxAge() != 24*time.Hour {
		t.Fatalf("unsafe M4 operations defaults: logging=%+v operations=%+v", cfg.Logging, cfg.Operations)
	}
	if cfg.API.MaxRequestBytes != 1<<20 || cfg.API.MaxBatchEvents != 100 || cfg.API.MaxEventBytes != 64<<10 ||
		cfg.API.MaxPayloadDepth != 16 || cfg.API.MaxConcurrentWrites != 4 || cfg.API.DefaultPageSize != 100 || cfg.API.MaxPageSize != 500 {
		t.Fatalf("unsafe API defaults: %+v", cfg.API)
	}
	if cfg.MediaIngest.Enabled || cfg.MediaIngest.InboxDirectory != filepath.Join(wantDataDirectory, "media-inbox") ||
		cfg.MediaIngest.MaxSegmentBytes != 2<<30 || cfg.MediaMaxSegmentDuration() != 10*time.Minute ||
		cfg.MediaIngest.MaxSidecarBytes != 64<<10 || cfg.MediaIngest.MaxScanEntries != 1000 ||
		cfg.MediaScanInterval() != time.Second || cfg.MediaSettleInterval() != time.Second || cfg.FFprobeTimeout() != 30*time.Second {
		t.Fatalf("unsafe media defaults: %+v", cfg.MediaIngest)
	}
	if cfg.MediaStorageDirectory() != filepath.Join(wantDataDirectory, "media") {
		t.Fatalf("MediaStorageDirectory = %q", cfg.MediaStorageDirectory())
	}
	if cfg.DatabasePath() != filepath.Join(wantDataDirectory, "exam-monitor.db") {
		t.Fatalf("DatabasePath = %q", cfg.DatabasePath())
	}
}

func TestLoadPartialFileRetainsDefaults(t *testing.T) {
	path := writeConfig(t, `{"schema_version":1,"logging":{"level":"debug"}}`)
	cfg, err := Load(path, emptyEnvironment)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Logging.Level != "debug" {
		t.Fatalf("Log level = %q", cfg.Logging.Level)
	}
	if cfg.Server.ListenAddress != "127.0.0.1:47831" {
		t.Fatalf("default listen address lost: %q", cfg.Server.ListenAddress)
	}
}

func TestLoadEnvironmentOverridesFile(t *testing.T) {
	path := writeConfig(t, `{"schema_version":1}`)
	lookup := mapEnvironment(map[string]string{
		EnvListenAddress:    "0.0.0.0:49000",
		EnvAllowNonLoopback: "true",
		EnvDataDirectory:    `D:\exam-monitor-data`,
		EnvLogLevel:         "warn",
		EnvReadHeader:       "3s",
		EnvRead:             "4s",
		EnvWrite:            "6s",
		EnvIdle:             "20s",
		EnvShutdown:         "15s",
		EnvBusyTimeout:      "2s",
		EnvMaxOpenConns:     "6",
		EnvMaxRequestBytes:  "2097152",
		EnvMaxBatchEvents:   "50",
		EnvMaxEventBytes:    "32768",
		EnvMaxPayloadDepth:  "12",
		EnvMaxWrites:        "3",
		EnvDefaultPageSize:  "25",
		EnvMaxPageSize:      "250",
		EnvMediaEnabled:     "true",
		EnvMediaInbox:       `D:\media-inbox`,
		EnvMediaScan:        "2s",
		EnvMediaSettle:      "3s",
		EnvMediaMaxBytes:    "10485760",
		EnvMediaMaxDuration: "5m",
		EnvMediaSidecar:     "32768",
		EnvMediaScanEntries: "250",
		EnvFFprobePath:      `D:\tools\ffprobe.exe`,
		EnvFFprobeTimeout:   "20s",
	})
	cfg, err := Load(path, lookup)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.ListenAddress != "0.0.0.0:49000" || !cfg.Server.AllowNonLoopback {
		t.Fatalf("server override not applied: %+v", cfg.Server)
	}
	if cfg.Paths.DataDirectory != `D:\exam-monitor-data` {
		t.Fatalf("data directory override not applied: %q", cfg.Paths.DataDirectory)
	}
	if cfg.Logging.Level != "warn" || cfg.ReadHeaderTimeout() != 3*time.Second ||
		cfg.ReadTimeout() != 4*time.Second || cfg.WriteTimeout() != 6*time.Second ||
		cfg.IdleTimeout() != 20*time.Second || cfg.ShutdownTimeout() != 15*time.Second ||
		cfg.BusyTimeout() != 2*time.Second || cfg.Storage.MaxOpenConnections != 6 ||
		cfg.API.MaxRequestBytes != 2097152 || cfg.API.MaxBatchEvents != 50 || cfg.API.MaxEventBytes != 32768 ||
		cfg.API.MaxPayloadDepth != 12 || cfg.API.MaxConcurrentWrites != 3 || cfg.API.DefaultPageSize != 25 || cfg.API.MaxPageSize != 250 {
		t.Fatalf("environment override not applied: %+v", cfg)
	}
	if !cfg.MediaIngest.Enabled || cfg.MediaIngest.InboxDirectory != `D:\media-inbox` || cfg.MediaScanInterval() != 2*time.Second ||
		cfg.MediaSettleInterval() != 3*time.Second || cfg.MediaIngest.MaxSegmentBytes != 10485760 || cfg.MediaMaxSegmentDuration() != 5*time.Minute ||
		cfg.MediaIngest.MaxSidecarBytes != 32768 || cfg.MediaIngest.MaxScanEntries != 250 ||
		cfg.MediaIngest.FFprobePath != `D:\tools\ffprobe.exe` || cfg.FFprobeTimeout() != 20*time.Second {
		t.Fatalf("media environment override not applied: %+v", cfg.MediaIngest)
	}
}

func TestLoadResolvesRelativeDataDirectoryAgainstStableApplicationRoot(t *testing.T) {
	configDirectory := t.TempDir()
	path := filepath.Join(configDirectory, "config.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"paths":{"data_directory":"nested-data"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	applicationData := filepath.Join(t.TempDir(), "local-app-data")
	lookup := mapEnvironment(map[string]string{envLocalAppData: applicationData})

	firstWorkingDirectory := t.TempDir()
	t.Chdir(firstWorkingDirectory)
	first, err := Load(path, lookup)
	if err != nil {
		t.Fatalf("first Load() error = %v", err)
	}
	secondWorkingDirectory := t.TempDir()
	t.Chdir(secondWorkingDirectory)
	second, err := Load(path, lookup)
	if err != nil {
		t.Fatalf("second Load() error = %v", err)
	}

	want := filepath.Join(applicationData, "ExamMonitor", "nested-data")
	if first.Paths.DataDirectory != want || second.Paths.DataDirectory != want {
		t.Fatalf("data directory depends on working/config directory: first=%q second=%q want=%q", first.Paths.DataDirectory, second.Paths.DataDirectory, want)
	}
}

func TestLoadAcceptsUTF8BOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"schema_version":1}`)...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, emptyEnvironment); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadMissingFileHasStableCode(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.json"), emptyEnvironment)
	if err == nil {
		t.Fatal("Load() unexpectedly succeeded")
	}
	if got := ErrorCode(err); got != CodeReadFailed {
		t.Fatalf("ErrorCode() = %q, want %q", got, CodeReadFailed)
	}
}

func TestLoadRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		json string
		code string
	}{
		{name: "missing schema", json: `{}`, code: CodeMissingSchema},
		{name: "unknown field", json: `{"schema_version":1,"extra":true}`, code: CodeDecodeFailed},
		{name: "case variant root field", json: `{"schema_version":1,"Logging":{"level":"debug"}}`, code: CodeDecodeFailed},
		{name: "duplicate root field", json: `{"schema_version":1,"logging":{},"logging":{"level":"debug"}}`, code: CodeDecodeFailed},
		{name: "case variant nested field", json: `{"schema_version":1,"media_ingest":{"Enabled":true}}`, code: CodeDecodeFailed},
		{name: "duplicate nested field", json: `{"schema_version":1,"media_ingest":{"enabled":false,"enabled":true}}`, code: CodeDecodeFailed},
		{name: "trailing value", json: `{"schema_version":1} {}`, code: CodeDecodeFailed},
		{name: "unsupported schema", json: `{"schema_version":2}`, code: CodeUnsupportedSchema},
		{name: "hostname instead of IP", json: `{"schema_version":1,"server":{"listen_address":"localhost:47831"}}`, code: CodeInvalidAddress},
		{name: "zero port", json: `{"schema_version":1,"server":{"listen_address":"127.0.0.1:0"}}`, code: CodeInvalidAddress},
		{name: "non-loopback without opt in", json: `{"schema_version":1,"server":{"listen_address":"0.0.0.0:47831"}}`, code: CodeNonLoopback},
		{name: "empty data directory", json: `{"schema_version":1,"paths":{"data_directory":" "}}`, code: CodeInvalidDataDir},
		{name: "relative data traversal", json: `{"schema_version":1,"paths":{"data_directory":"..\\outside"}}`, code: CodeInvalidDataDir},
		{name: "drive relative data path", json: `{"schema_version":1,"paths":{"data_directory":"C:relative"}}`, code: CodeInvalidDataDir},
		{name: "invalid level", json: `{"schema_version":1,"logging":{"level":"trace"}}`, code: CodeInvalidLogLevel},
		{name: "short timeout", json: `{"schema_version":1,"server":{"shutdown_timeout":"10ms"}}`, code: CodeInvalidTimeout},
		{name: "read timeout before header timeout", json: `{"schema_version":1,"server":{"read_header_timeout":"8s","read_timeout":"5s"}}`, code: CodeInvalidTimeout},
		{name: "short busy timeout", json: `{"schema_version":1,"storage":{"busy_timeout":"10ms"}}`, code: CodeInvalidStorage},
		{name: "too many database connections", json: `{"schema_version":1,"storage":{"max_open_connections":33}}`, code: CodeInvalidStorage},
		{name: "unordered disk thresholds", json: `{"schema_version":1,"storage":{"warning_free_bytes":100000000,"critical_free_bytes":200000000}}`, code: CodeInvalidStorage},
		{name: "short retention", json: `{"schema_version":1,"retention":{"minimum_age":"1h"}}`, code: CodeInvalidRetention},
		{name: "retention without full backup", json: `{"schema_version":1,"retention":{"enabled":true,"require_full_backup":false}}`, code: CodeInvalidRetention},
		{name: "tiny wal limit", json: `{"schema_version":1,"operations":{"wal_max_bytes":1024}}`, code: CodeInvalidOperations},
		{name: "single log file", json: `{"schema_version":1,"logging":{"max_files":1}}`, code: CodeInvalidLogLevel},
		{name: "small request limit", json: `{"schema_version":1,"api":{"max_request_bytes":1024}}`, code: CodeInvalidAPILimit},
		{name: "event limit exceeds request", json: `{"schema_version":1,"api":{"max_request_bytes":65536,"max_event_bytes":65536}}`, code: CodeInvalidAPILimit},
		{name: "page default exceeds maximum", json: `{"schema_version":1,"api":{"default_page_size":501,"max_page_size":500}}`, code: CodeInvalidAPILimit},
		{name: "media duration exceeds ten minutes", json: `{"schema_version":1,"media_ingest":{"max_segment_duration":"11m"}}`, code: CodeInvalidMedia},
		{name: "media storage overlap", json: `{"schema_version":1,"media_ingest":{"inbox_directory":"media"}}`, code: CodeInvalidMedia},
		{name: "enabled media relative ffprobe", json: `{"schema_version":1,"media_ingest":{"enabled":true,"ffprobe_path":"ffprobe.exe"}}`, code: CodeInvalidMedia},
		{name: "small media sidecar limit", json: `{"schema_version":1,"media_ingest":{"max_sidecar_bytes":128}}`, code: CodeInvalidMedia},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, test.json)
			_, err := Load(path, emptyEnvironment)
			if err == nil {
				t.Fatal("Load() unexpectedly succeeded")
			}
			if got := ErrorCode(err); got != test.code {
				t.Fatalf("ErrorCode() = %q, want %q (err=%v)", got, test.code, err)
			}
		})
	}
}

func TestLoadRejectsInvalidBooleanEnvironmentWithoutEchoingValue(t *testing.T) {
	secretLikeValue := "not-a-bool-secret"
	_, err := Load("", mapEnvironment(map[string]string{EnvAllowNonLoopback: secretLikeValue}))
	if err == nil {
		t.Fatal("Load() unexpectedly succeeded")
	}
	if got := ErrorCode(err); got != CodeInvalidEnv {
		t.Fatalf("ErrorCode() = %q", got)
	}
	if strings.Contains(err.Error(), secretLikeValue) {
		t.Fatal("error must not echo environment value")
	}
}

func TestLoadRejectsRelativeDataDirectoryWithoutAbsoluteApplicationRoot(t *testing.T) {
	_, err := Load("", mapEnvironment(map[string]string{envLocalAppData: "relative-root"}))
	if err == nil {
		t.Fatal("Load() unexpectedly succeeded")
	}
	if got := ErrorCode(err); got != CodeInvalidDataDir {
		t.Fatalf("ErrorCode() = %q", got)
	}
}

func TestM3CollectorConfigurationAndModes(t *testing.T) {
	validCollectors := `[
      {"id":"aw.afk","kind":"activitywatch","enabled":true,"heartbeat_period":"1m","allowed_lateness":"1m","offline_after":"5m","planned_schedule":{"timezone":"Asia/Shanghai","windows":[{"days":["monday","tuesday","wednesday","thursday","friday","saturday","sunday"],"start_local":"00:00","end_local":"24:00"}]},"activitywatch":{"base_url":"http://127.0.0.1:5600","bucket_id":"aw-watcher-afk_host","poll_interval":"30s","request_timeout":"2s","initial_lookback":"24h","rescan_window":"1h","page_size":100,"max_pages_per_poll":10,"max_response_bytes":1048576,"clock_error_ms":100}},
      {"id":"desk.media","kind":"media","enabled":true,"heartbeat_period":"5m","allowed_lateness":"5m","offline_after":"15m","planned_schedule":{"timezone":"Asia/Shanghai","windows":[{"days":["monday","tuesday","wednesday","thursday","friday","saturday","sunday"],"start_local":"00:00","end_local":"24:00"}]}}
    ]`

	t.Run("record-only accepts frozen collectors", func(t *testing.T) {
		path := writeConfig(t, `{"schema_version":1,"collectors":`+validCollectors+`}`)
		cfg, err := Load(path, emptyEnvironment)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Runtime.Mode != ModeRecordOnly || len(cfg.Collectors) != 2 || cfg.Collectors[0].ActivityWatch == nil {
			t.Fatalf("unexpected M3 config: %#v", cfg)
		}
	})

	t.Run("minimum enables only required recorder modules", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "minimum")
		ffprobe := filepath.Join(root, "ffprobe.exe")
		json := `{"schema_version":1,"runtime":{"mode":"minimum","backup_interface_enabled":true},"paths":{"data_directory":` + strconv.Quote(root) + `},"media_ingest":{"enabled":true,"ffprobe_path":` + strconv.Quote(ffprobe) + `},"collectors":` + validCollectors + `}`
		cfg, err := Load(writeConfig(t, json), emptyEnvironment)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Runtime.Mode != ModeMinimum || !cfg.Runtime.BackupInterfaceEnabled || !cfg.MediaIngest.Enabled {
			t.Fatalf("unexpected minimum config: %#v", cfg)
		}
	})

	tests := []struct {
		name string
		json string
		code string
	}{
		{name: "ActivityWatch SLA", json: strings.Replace(validCollectors, `"offline_after":"5m"`, `"offline_after":"6m"`, 1), code: CodeInvalidCollector},
		{name: "heartbeat relationship", json: strings.Replace(validCollectors, `"allowed_lateness":"1m"`, `"allowed_lateness":"5m"`, 1), code: CodeInvalidCollector},
		{name: "non-loopback ActivityWatch", json: strings.Replace(validCollectors, `http://127.0.0.1:5600`, `http://192.0.2.1:5600`, 1), code: CodeInvalidCollector},
		{name: "zero ActivityWatch port", json: strings.Replace(validCollectors, `http://127.0.0.1:5600`, `http://127.0.0.1:0`, 1), code: CodeInvalidCollector},
		{name: "out of range ActivityWatch port", json: strings.Replace(validCollectors, `http://127.0.0.1:5600`, `http://127.0.0.1:99999`, 1), code: CodeInvalidCollector},
		{name: "non-numeric ActivityWatch port", json: strings.Replace(validCollectors, `http://127.0.0.1:5600`, `http://127.0.0.1:abc`, 1), code: CodeInvalidCollector},
		{name: "poll slower than heartbeat", json: strings.Replace(validCollectors, `"poll_interval":"30s"`, `"poll_interval":"2m"`, 1), code: CodeInvalidCollector},
		{name: "ActivityWatch response page too large", json: strings.Replace(validCollectors, `"max_response_bytes":1048576`, `"max_response_bytes":8388609`, 1), code: CodeInvalidCollector},
		{name: "unknown timezone", json: strings.Replace(validCollectors, `Asia/Shanghai`, `Missing/Zone`, 1), code: CodeInvalidCollector},
		{name: "machine-local timezone", json: strings.Replace(validCollectors, `Asia/Shanghai`, `Local`, 1), code: CodeInvalidCollector},
		{name: "collector id control character", json: strings.Replace(validCollectors, `aw.afk`, `aw\nafk`, 1), code: CodeInvalidCollector},
		{name: "bucket id control character", json: strings.Replace(validCollectors, `aw-watcher-afk_host`, `aw-watcher-\u0001-host`, 1), code: CodeInvalidCollector},
		{name: "missing explicit ActivityWatch clock error", json: strings.Replace(validCollectors, `,"clock_error_ms":100`, ``, 1), code: CodeDecodeFailed},
		{name: "minimum generic collector", json: strings.Replace(validCollectors, `"kind":"media"`, `"kind":"generic_json"`, 1), code: CodeInvalidRuntime},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prefix := `{"schema_version":1,"collectors":`
			if test.name == "minimum generic collector" {
				prefix = `{"schema_version":1,"runtime":{"mode":"minimum","backup_interface_enabled":true},"media_ingest":{"enabled":true,"ffprobe_path":"C:\\ffprobe.exe"},"collectors":`
			}
			_, err := Load(writeConfig(t, prefix+test.json+`}`), emptyEnvironment)
			if err == nil || ErrorCode(err) != test.code {
				t.Fatalf("Load() error=%v code=%q, want %q", err, ErrorCode(err), test.code)
			}
		})
	}

	t.Run("invalid UTF-8 is rejected before JSON decoding", func(t *testing.T) {
		invalid := strings.Replace(validCollectors, "aw.afk", "aw."+string([]byte{0xff}), 1)
		_, err := Load(writeConfig(t, `{"schema_version":1,"collectors":`+invalid+`}`), emptyEnvironment)
		if err == nil || ErrorCode(err) != CodeDecodeFailed {
			t.Fatalf("Load() error=%v code=%q", err, ErrorCode(err))
		}
	})

	t.Run("enabled ActivityWatch count is bounded by offline SLA", func(t *testing.T) {
		cfg, err := Load(writeConfig(t, `{"schema_version":1,"collectors":`+validCollectors+`}`), emptyEnvironment)
		if err != nil {
			t.Fatal(err)
		}
		template := cfg.Collectors[0]
		cfg.Collectors = cfg.Collectors[1:]
		for index := 0; index < 5; index++ {
			collector := template
			collector.ID = fmt.Sprintf("aw.%d", index)
			activityWatch := *template.ActivityWatch
			activityWatch.BucketID = fmt.Sprintf("bucket-%d", index)
			collector.ActivityWatch = &activityWatch
			cfg.Collectors = append(cfg.Collectors, collector)
		}
		if err := cfg.Validate(); err == nil || ErrorCode(err) != CodeInvalidCollector {
			t.Fatalf("Validate() error=%v code=%q", err, ErrorCode(err))
		}
	})
}

func TestM3CollectorJSONRejectsUnknownAndDuplicateNestedFields(t *testing.T) {
	tests := []string{
		`{"schema_version":1,"collectors":[{"id":"x","kind":"generic_json","enabled":false,"extra":true}]}`,
		`{"schema_version":1,"collectors":[{"id":"x","kind":"generic_json","enabled":false,"planned_schedule":{"timezone":"UTC","timezone":"Asia/Shanghai","windows":[]}}]}`,
		`{"schema_version":1,"collectors":[{"id":"x","kind":"activitywatch","enabled":false,"activitywatch":{"base_url":"http://127.0.0.1:5600","unknown":true}}]}`,
	}
	for _, raw := range tests {
		_, err := Load(writeConfig(t, raw), emptyEnvironment)
		if err == nil || ErrorCode(err) != CodeDecodeFailed {
			t.Fatalf("Load(%s) error=%v code=%q", raw, err, ErrorCode(err))
		}
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var testLocalAppData = filepath.Join(os.TempDir(), "exam-monitor-config-test-local-app-data")

func emptyEnvironment(key string) (string, bool) {
	if key == envLocalAppData {
		return testLocalAppData, true
	}
	return "", false
}

func mapEnvironment(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
