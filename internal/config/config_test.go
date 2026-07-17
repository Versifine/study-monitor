package config

import (
	"os"
	"path/filepath"
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
		cfg.IdleTimeout() != 20*time.Second || cfg.ShutdownTimeout() != 15*time.Second {
		t.Fatalf("environment override not applied: %+v", cfg)
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
