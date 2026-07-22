package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/Versifine/study-monitor/internal/app"
	"github.com/Versifine/study-monitor/internal/config"
	"github.com/Versifine/study-monitor/internal/eventstore"
	"github.com/Versifine/study-monitor/internal/logging"
	"github.com/Versifine/study-monitor/internal/version"
)

const (
	exitOK      = 0
	exitRuntime = 1
	exitUsage   = 2

	codeCLIInvalid    = "CLI_ARGUMENT_INVALID"
	codeCLIConflict   = "CLI_ACTION_CONFLICT"
	codeCLIRunFor     = "CLI_RUN_FOR_INVALID"
	codeLoggingFailed = "LOGGING_INIT_FAILED"
)

type options struct {
	configPath            string
	check                 bool
	show                  bool
	help                  bool
	runFor                time.Duration
	backupDatabase        string
	verifyDatabase        string
	mediaManifestDatabase string
	schemaInfo            bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, lookup config.LookupEnv) int {
	build := version.Current()
	bootstrapLogger, _ := logging.New(stderr, "info", build.Version)

	opts, err := parseOptions(args)
	if err != nil {
		bootstrapLogger.Error("cli", "arguments_invalid", cliErrorCode(err), "invalid command line", err)
		return exitUsage
	}
	if opts.help {
		writeUsage(stdout)
		return exitOK
	}
	if opts.show {
		if err := writeJSON(stdout, build); err != nil {
			bootstrapLogger.Error("cli", "version_output_failed", "CLI_OUTPUT_FAILED", "write version output", err)
			return exitRuntime
		}
		return exitOK
	}

	cfg, err := config.Load(opts.configPath, lookup)
	if err != nil {
		bootstrapLogger.Error("config", "validation_failed", config.ErrorCode(err), "configuration is invalid", err)
		return exitUsage
	}
	logger, err := logging.New(stderr, cfg.Logging.Level, build.Version)
	if err != nil {
		bootstrapLogger.Error("logging", "initialization_failed", codeLoggingFailed, "initialize structured logging", err)
		return exitRuntime
	}
	if opts.verifyDatabase != "" {
		if err := eventstore.VerifyDatabase(ctx, opts.verifyDatabase); err != nil {
			logger.Error("backup", "database_verification_failed", eventstore.ErrorCode(err), "verify database snapshot", err)
			return exitRuntime
		}
		if err := writeJSON(stdout, struct {
			Status string `json:"status"`
		}{"ok"}); err != nil {
			return exitRuntime
		}
		return exitOK
	}
	if opts.mediaManifestDatabase != "" {
		media, manifestErr := eventstore.ReadMediaManifest(ctx, opts.mediaManifestDatabase)
		if manifestErr != nil {
			logger.Error("backup", "media_manifest_failed", eventstore.ErrorCode(manifestErr), "read snapshot media manifest", manifestErr)
			return exitRuntime
		}
		result := struct {
			SchemaVersion int                             `json:"schema_version"`
			Media         []eventstore.MediaManifestEntry `json:"media"`
		}{1, media}
		if result.Media == nil {
			result.Media = []eventstore.MediaManifestEntry{}
		}
		if err := writeJSON(stdout, result); err != nil {
			return exitRuntime
		}
		return exitOK
	}
	if opts.schemaInfo {
		info, infoErr := eventstore.ReadSchemaInfo(ctx, cfg.DatabasePath())
		if infoErr != nil {
			logger.Error("storage", "schema_info_failed", eventstore.ErrorCode(infoErr), "read schema compatibility", infoErr)
			return exitRuntime
		}
		if err := writeJSON(stdout, info); err != nil {
			return exitRuntime
		}
		return exitOK
	}
	if opts.backupDatabase != "" {
		store, openErr := openMaintenanceStore(ctx, cfg)
		if openErr != nil {
			logger.Error("storage", "maintenance_open_failed", eventstore.ErrorCode(openErr), "open storage for maintenance", openErr)
			return exitRuntime
		}
		defer store.Close()
		if err := store.BackupTo(ctx, opts.backupDatabase); err != nil {
			logger.Error("backup", "snapshot_failed", eventstore.ErrorCode(err), "create database snapshot", err)
			return exitRuntime
		}
		if err := writeJSON(stdout, struct{ Status, Path string }{"ok", opts.backupDatabase}); err != nil {
			return exitRuntime
		}
		return exitOK
	}

	if opts.check {
		enabledCollectors := 0
		enabledActivityWatch := 0
		for _, collector := range cfg.Collectors {
			if collector.Enabled {
				enabledCollectors++
				if collector.Kind == config.CollectorActivityWatch {
					enabledActivityWatch++
				}
			}
		}
		result := struct {
			Status                  string `json:"status"`
			SchemaVersion           int    `json:"schema_version"`
			ListenAddress           string `json:"listen_address"`
			DataDirectory           string `json:"data_directory"`
			DatabasePath            string `json:"database_path"`
			LogLevel                string `json:"log_level"`
			MediaEnabled            bool   `json:"media_ingest_enabled"`
			MediaInbox              string `json:"media_inbox_directory"`
			MediaStorage            string `json:"media_storage_directory"`
			FFprobePath             string `json:"ffprobe_path,omitempty"`
			Mode                    string `json:"mode"`
			EnabledCollectors       int    `json:"enabled_collectors"`
			ActivityWatchCollectors int    `json:"activitywatch_collectors"`
		}{
			Status:                  "ok",
			SchemaVersion:           cfg.SchemaVersion,
			ListenAddress:           cfg.Server.ListenAddress,
			DataDirectory:           cfg.Paths.DataDirectory,
			DatabasePath:            cfg.DatabasePath(),
			LogLevel:                cfg.Logging.Level,
			MediaEnabled:            cfg.MediaIngest.Enabled,
			MediaInbox:              cfg.MediaIngest.InboxDirectory,
			MediaStorage:            cfg.MediaStorageDirectory(),
			FFprobePath:             cfg.MediaIngest.FFprobePath,
			Mode:                    cfg.Runtime.Mode,
			EnabledCollectors:       enabledCollectors,
			ActivityWatchCollectors: enabledActivityWatch,
		}
		if err := writeJSON(stdout, result); err != nil {
			logger.Error("cli", "config_output_failed", "CLI_OUTPUT_FAILED", "write config validation output", err)
			return exitRuntime
		}
		return exitOK
	}
	var rotatingLog *logging.RotatingFile
	if cfg.Logging.FileEnabled {
		rotatingLog, err = logging.NewRotatingFile(cfg.LogDirectory(), cfg.Logging.MaxFileBytes, cfg.Logging.MaxFiles)
		if err != nil {
			bootstrapLogger.Error("logging", "initialization_failed", codeLoggingFailed, "initialize rotating log", err)
			return exitRuntime
		}
		defer rotatingLog.Close()
		logger, err = logging.New(io.MultiWriter(stderr, rotatingLog), cfg.Logging.Level, build.Version)
		if err != nil {
			return exitRuntime
		}
	}

	runContext := ctx
	cancel := func() {}
	if opts.runFor > 0 {
		runContext, cancel = context.WithTimeout(ctx, opts.runFor)
	}
	defer cancel()
	if err := app.Run(runContext, cfg, logger, build); err != nil {
		logger.Error(
			"app",
			"runtime_failed",
			app.ErrorCode(err),
			"recorder core stopped unexpectedly",
			err,
			slog.String("mode", cfg.Runtime.Mode),
		)
		return exitRuntime
	}
	return exitOK
}

func parseOptions(args []string) (options, error) {
	var opts options
	flags := flag.NewFlagSet("exam-monitor", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.configPath, "config", "", "path to a versioned JSON config")
	flags.BoolVar(&opts.check, "check-config", false, "validate configuration and exit")
	flags.BoolVar(&opts.show, "version", false, "print build version and exit")
	flags.BoolVar(&opts.help, "help", false, "print usage and exit")
	flags.DurationVar(&opts.runFor, "run-for", 0, "development/smoke only: stop cleanly after a duration")
	flags.StringVar(&opts.backupDatabase, "backup-database", "", "create a consistent verified database snapshot and exit")
	flags.StringVar(&opts.verifyDatabase, "verify-database", "", "verify a database snapshot and exit")
	flags.StringVar(&opts.mediaManifestDatabase, "media-manifest-database", "", "read accepted media manifest from a verified database snapshot and exit")
	flags.BoolVar(&opts.schemaInfo, "schema-info", false, "print database schema compatibility and exit")
	if err := flags.Parse(args); err != nil {
		return options{}, &cliError{code: codeCLIInvalid, err: err}
	}
	if flags.NArg() != 0 {
		return options{}, &cliError{code: codeCLIInvalid, err: fmt.Errorf("unexpected positional arguments")}
	}
	actions := countTrue(opts.check, opts.show, opts.help, opts.schemaInfo)
	if opts.backupDatabase != "" {
		actions++
	}
	if opts.verifyDatabase != "" {
		actions++
	}
	if opts.mediaManifestDatabase != "" {
		actions++
	}
	if actions > 1 {
		return options{}, &cliError{code: codeCLIConflict, err: errors.New("choose only one CLI action")}
	}
	if opts.runFor < 0 || opts.runFor > 5*time.Minute {
		return options{}, &cliError{code: codeCLIRunFor, err: errors.New("--run-for must be between 0 and 5m")}
	}
	if opts.runFor > 0 && actions > 0 {
		return options{}, &cliError{code: codeCLIConflict, err: errors.New("--run-for can only be used while serving")}
	}
	return opts, nil
}

type cliError struct {
	code string
	err  error
}

func (e *cliError) Error() string { return e.err.Error() }
func (e *cliError) Unwrap() error { return e.err }

func cliErrorCode(err error) string {
	var typed *cliError
	if errors.As(err, &typed) {
		return typed.code
	}
	return codeCLIInvalid
}

func countTrue(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func writeUsage(output io.Writer) {
	_, _ = fmt.Fprintln(output, "Usage: exam-monitor [--config path] [--check-config|--version|--schema-info|--backup-database path|--verify-database path|--media-manifest-database path|--help] [--run-for duration]")
}

func openMaintenanceStore(ctx context.Context, cfg config.Config) (*eventstore.Store, error) {
	policies := make(map[string]eventstore.CollectorPolicy)
	for _, collector := range cfg.Collectors {
		if collector.Enabled {
			policies[collector.ID] = eventstore.CollectorPolicy{Kind: collector.Kind, HeartbeatPeriod: collector.HeartbeatPeriodDuration()}
		}
	}
	return eventstore.Open(ctx, cfg.DatabasePath(), eventstore.Options{
		BusyTimeout: cfg.BusyTimeout(), MaxOpenConnections: cfg.Storage.MaxOpenConnections,
		MaxBatchEvents: cfg.API.MaxBatchEvents, MaxEventBytes: cfg.API.MaxEventBytes,
		MaxPayloadDepth: cfg.API.MaxPayloadDepth, MaxPageSize: cfg.API.MaxPageSize,
		CollectorPolicies: policies,
	})
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
