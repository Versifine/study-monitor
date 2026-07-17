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

	"github.com/Versifine/study-monitor/internal/app"
	"github.com/Versifine/study-monitor/internal/config"
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
	configPath string
	check      bool
	show       bool
	help       bool
	runFor     time.Duration
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

	if opts.check {
		result := struct {
			Status        string `json:"status"`
			SchemaVersion int    `json:"schema_version"`
			ListenAddress string `json:"listen_address"`
			DataDirectory string `json:"data_directory"`
			LogLevel      string `json:"log_level"`
		}{
			Status:        "ok",
			SchemaVersion: cfg.SchemaVersion,
			ListenAddress: cfg.Server.ListenAddress,
			DataDirectory: cfg.Paths.DataDirectory,
			LogLevel:      cfg.Logging.Level,
		}
		if err := writeJSON(stdout, result); err != nil {
			logger.Error("cli", "config_output_failed", "CLI_OUTPUT_FAILED", "write config validation output", err)
			return exitRuntime
		}
		return exitOK
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
			"recorder core skeleton stopped unexpectedly",
			err,
			slog.String("mode", "record-only"),
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
	if err := flags.Parse(args); err != nil {
		return options{}, &cliError{code: codeCLIInvalid, err: err}
	}
	if flags.NArg() != 0 {
		return options{}, &cliError{code: codeCLIInvalid, err: fmt.Errorf("unexpected positional arguments")}
	}
	if countTrue(opts.check, opts.show, opts.help) > 1 {
		return options{}, &cliError{code: codeCLIConflict, err: errors.New("choose only one of --check-config, --version, or --help")}
	}
	if opts.runFor < 0 || opts.runFor > 5*time.Minute {
		return options{}, &cliError{code: codeCLIRunFor, err: errors.New("--run-for must be between 0 and 5m")}
	}
	if opts.runFor > 0 && (opts.check || opts.show || opts.help) {
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
	_, _ = fmt.Fprintln(output, "Usage: exam-monitor [--config path] [--check-config|--version|--help] [--run-for duration]")
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
