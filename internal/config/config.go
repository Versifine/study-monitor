package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
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
	CodeInvalidLogLevel   = "CONFIG_LOG_LEVEL_INVALID"
	CodeInvalidTimeout    = "CONFIG_TIMEOUT_INVALID"
)

const (
	EnvListenAddress    = "EXAM_MONITOR_LISTEN_ADDRESS"
	EnvAllowNonLoopback = "EXAM_MONITOR_ALLOW_NON_LOOPBACK"
	EnvDataDirectory    = "EXAM_MONITOR_DATA_DIRECTORY"
	EnvLogLevel         = "EXAM_MONITOR_LOG_LEVEL"
	EnvReadHeader       = "EXAM_MONITOR_READ_HEADER_TIMEOUT"
	EnvShutdown         = "EXAM_MONITOR_SHUTDOWN_TIMEOUT"
)

// LookupEnv matches os.LookupEnv and makes environment overrides deterministic in tests.
type LookupEnv func(string) (string, bool)

// Config is the complete M0 configuration contract.
type Config struct {
	SchemaVersion int           `json:"schema_version"`
	Server        ServerConfig  `json:"server"`
	Paths         PathsConfig   `json:"paths"`
	Logging       LoggingConfig `json:"logging"`
}

type ServerConfig struct {
	ListenAddress    string `json:"listen_address"`
	AllowNonLoopback bool   `json:"allow_non_loopback"`
	ReadHeader       string `json:"read_header_timeout"`
	Shutdown         string `json:"shutdown_timeout"`
}

type LoggingConfig struct {
	Level string `json:"level"`
}

type PathsConfig struct {
	DataDirectory string `json:"data_directory"`
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

func Default() Config {
	return Config{
		SchemaVersion: CurrentSchemaVersion,
		Server: ServerConfig{
			ListenAddress:    "127.0.0.1:47831",
			AllowNonLoopback: false,
			ReadHeader:       "5s",
			Shutdown:         "10s",
		},
		Paths:   PathsConfig{DataDirectory: "data"},
		Logging: LoggingConfig{Level: "info"},
	}
}

// Load applies defaults, an optional JSON file, then environment overrides.
// It validates the final result without creating directories or other runtime state.
func Load(path string, lookup LookupEnv) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, &Error{Code: CodeReadFailed, Err: fmt.Errorf("read config %q: %w", path, err)}
		}
		if err := decodeFile(raw, &cfg); err != nil {
			return Config{}, err
		}
	}

	if lookup == nil {
		lookup = os.LookupEnv
	}
	if err := applyEnvironment(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func decodeFile(raw []byte, cfg *Config) error {
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return &Error{Code: CodeDecodeFailed, Err: fmt.Errorf("decode config: %w", err)}
	}
	if _, ok := fields["schema_version"]; !ok {
		return &Error{Code: CodeMissingSchema, Err: errors.New("config must declare schema_version")}
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

func applyEnvironment(cfg *Config, lookup LookupEnv) error {
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
	if value, ok := lookup(EnvReadHeader); ok {
		cfg.Server.ReadHeader = value
	}
	if value, ok := lookup(EnvShutdown); ok {
		cfg.Server.Shutdown = value
	}
	return nil
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
	if strings.TrimSpace(cfg.Paths.DataDirectory) == "" || strings.ContainsRune(cfg.Paths.DataDirectory, '\x00') {
		return &Error{Code: CodeInvalidDataDir, Err: errors.New("paths.data_directory must be a non-empty filesystem path")}
	}

	switch strings.ToLower(cfg.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		return &Error{Code: CodeInvalidLogLevel, Err: errors.New("logging.level must be debug, info, warn, or error")}
	}

	if _, err := validateDuration("server.read_header_timeout", cfg.Server.ReadHeader); err != nil {
		return err
	}
	if _, err := validateDuration("server.shutdown_timeout", cfg.Server.Shutdown); err != nil {
		return err
	}
	return nil
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

func (cfg Config) ShutdownTimeout() time.Duration {
	duration, _ := time.ParseDuration(cfg.Server.Shutdown)
	return duration
}
