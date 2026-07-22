package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/Versifine/study-monitor/internal/collectors"
	"github.com/Versifine/study-monitor/internal/config"
	"github.com/Versifine/study-monitor/internal/eventstore"
	"github.com/Versifine/study-monitor/internal/httpapi"
	"github.com/Versifine/study-monitor/internal/logging"
	"github.com/Versifine/study-monitor/internal/mediaingest"
	"github.com/Versifine/study-monitor/internal/operations"
	"github.com/Versifine/study-monitor/internal/version"
)

const (
	CodeListenFailed   = "APP_LISTEN_FAILED"
	CodeServeFailed    = "APP_SERVE_FAILED"
	CodeShutdownFailed = "APP_SHUTDOWN_FAILED"
)

type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func ErrorCode(err error) string {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return CodeServeFailed
}

type Server struct {
	config  config.Config
	logger  *logging.Logger
	version version.Info
	handler http.Handler
}

func NewServer(cfg config.Config, logger *logging.Logger, build version.Info, store httpapi.Store, failure httpapi.StorageFailure, providers ...any) *Server {
	return &Server{
		config:  cfg,
		logger:  logger,
		version: build,
		handler: httpapi.New(cfg, logger, build, store, failure, providers...),
	}
}

func (server *Server) Handler() http.Handler { return server.handler }

// Run binds the configured address and serves until the context is canceled.
func Run(ctx context.Context, cfg config.Config, logger *logging.Logger, build version.Info) error {
	listener, err := net.Listen("tcp", cfg.Server.ListenAddress)
	if err != nil {
		return &Error{Code: CodeListenFailed, Err: fmt.Errorf("listen on configured address: %w", err)}
	}
	defer listener.Close()

	collectorPolicies := make(map[string]eventstore.CollectorPolicy)
	for _, collector := range cfg.Collectors {
		if collector.Enabled {
			collectorPolicies[collector.ID] = eventstore.CollectorPolicy{Kind: collector.Kind, HeartbeatPeriod: collector.HeartbeatPeriodDuration()}
		}
	}
	store, storeErr := eventstore.Open(ctx, cfg.DatabasePath(), eventstore.Options{
		BusyTimeout: cfg.BusyTimeout(), MaxOpenConnections: cfg.Storage.MaxOpenConnections,
		MaxBatchEvents: cfg.API.MaxBatchEvents, MaxEventBytes: cfg.API.MaxEventBytes,
		MaxPayloadDepth: cfg.API.MaxPayloadDepth, MaxPageSize: cfg.API.MaxPageSize,
		CollectorPolicies: collectorPolicies,
	})
	failure := httpapi.StorageFailure{}
	var mediaStatus httpapi.MediaStatusProvider = mediaingest.NewFixedStatusProvider(mediaingest.ModuleDisabled, "")
	var mediaManager *mediaingest.Manager
	var collectorManager *collectors.Manager
	var operationsManager *operations.Manager
	if storeErr != nil {
		failure = classifyStorageFailure(storeErr)
		logger.Error("storage", "initialization_failed", failure.ErrorCode, "event storage initialization failed", storeErr)
	} else {
		defer store.Close()
		operationsManager = operations.New(cfg, store, logger)
		operationsManager.Initialize(ctx)
		operationsManager.RecordRuntimeMode(ctx, cfg.Runtime.Mode, "current-user", "startup_config", "CONFIG_MODE")
		operationsManager.RecordModuleState(ctx, "dashboard", "disabled", "DASHBOARD_NOT_INSTALLED")
		operationsManager.RecordModuleState(ctx, "coverage", "healthy", "ON_DEMAND_PROJECTION_ENABLED")
		genericState, genericReason := "healthy", "GENERIC_JSON_ENABLED"
		if cfg.Runtime.Mode == config.ModeMinimum {
			genericState, genericReason = "disabled", "MINIMUM_MODE"
		}
		operationsManager.RecordModuleState(ctx, "generic_json", genericState, genericReason)
		for _, collector := range cfg.Collectors {
			state, reason := "healthy", "CONFIG_ENABLED"
			if !collector.Enabled {
				state, reason = "disabled", "CONFIG_DISABLED"
			}
			operationsManager.RecordModuleState(ctx, "collector:"+collector.ID, state, reason)
		}
		if cfg.MediaIngest.Enabled {
			mediaManager = mediaingest.NewWithGate(cfg, store, logger, operationsManager)
			mediaStatus = mediaManager
			operationsManager.RecordModuleState(ctx, "media_ingest", "unavailable", "INITIALIZING")
		} else {
			operationsManager.RecordModuleState(ctx, "media_ingest", "disabled", "CONFIG_DISABLED")
		}
		collectorManager = collectors.NewWithFaultRecorder(cfg, store, logger, operationsManager)
	}
	if cfg.MediaIngest.Enabled && storeErr != nil {
		mediaStatus = mediaingest.NewFixedStatusProvider(mediaingest.ModuleUnavailable, eventstore.ErrorCode(storeErr))
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	mediaDone := make(chan struct{})
	collectorDone := make(chan struct{})
	operationsDone := make(chan struct{})
	if operationsManager != nil {
		go func() { defer close(operationsDone); operationsManager.Run(runContext) }()
	} else {
		close(operationsDone)
	}
	if mediaManager != nil {
		go func() {
			defer close(mediaDone)
			if err := mediaManager.Initialize(runContext); err != nil {
				logger.Error("media_ingest", "initialization_failed", mediaingest.ErrorCode(err), "media ingest disabled after initialization failure", err)
				operationsManager.RecordModuleState(runContext, "media_ingest", "unavailable", mediaingest.ErrorCode(err))
				operationsManager.RecordFault(runContext, "media_ingest", "P2", "degraded", mediaingest.ErrorCode(err), err.Error())
				return
			}
			operationsManager.RecordModuleState(runContext, "media_ingest", "healthy", "INITIALIZED")
			mediaManager.Run(runContext)
		}()
	} else {
		close(mediaDone)
	}
	if collectorManager != nil {
		go func() {
			defer close(collectorDone)
			collectorManager.Run(runContext)
		}()
	} else {
		close(collectorDone)
	}
	providers := []any{mediaStatus}
	if operationsManager != nil {
		providers = append(providers, operationsManager)
	}
	if collectorManager != nil {
		providers = append(providers, collectorManager)
	}
	err = NewServer(cfg, logger, build, store, failure, providers...).Serve(runContext, listener)
	cancel()
	<-mediaDone
	<-collectorDone
	<-operationsDone
	return err
}

func classifyStorageFailure(err error) httpapi.StorageFailure {
	code := eventstore.ErrorCode(err)
	status := eventstore.ReadinessUnavailable
	switch code {
	case eventstore.CodeMigrationFailed, eventstore.CodeMigrationUnsupported:
		status = eventstore.ReadinessMigrationFailed
	case eventstore.CodeReadOnly:
		status = eventstore.ReadinessReadOnly
	case eventstore.CodeBusy:
		status = eventstore.ReadinessBusy
	}
	return httpapi.StorageFailure{Status: status, ErrorCode: code}
}

// Serve accepts a listener so graceful shutdown can be tested without fixed ports.
func (server *Server) Serve(ctx context.Context, listener net.Listener) error {
	httpServer := server.newHTTPServer()

	server.logger.Info(
		"app",
		"started",
		"recorder core started",
		slog.String("listen_address", listener.Addr().String()),
		slog.String("mode", server.config.Runtime.Mode),
	)

	serveResult := make(chan error, 1)
	go func() {
		serveResult <- httpServer.Serve(listener)
	}()

	select {
	case err := <-serveResult:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return &Error{Code: CodeServeFailed, Err: fmt.Errorf("serve liveness endpoint: %w", err)}
		}
		server.logger.Info("app", "stopped", "recorder core stopped")
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), server.config.ShutdownTimeout())
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
			return &Error{Code: CodeShutdownFailed, Err: fmt.Errorf("graceful shutdown: %w", err)}
		}
		err := <-serveResult
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return &Error{Code: CodeServeFailed, Err: fmt.Errorf("serve liveness endpoint: %w", err)}
		}
		server.logger.Info("app", "stopped", "recorder core stopped")
		return nil
	}
}

func (server *Server) newHTTPServer() *http.Server {
	return &http.Server{
		Handler:           server.handler,
		ReadHeaderTimeout: server.config.ReadHeaderTimeout(),
		ReadTimeout:       server.config.ReadTimeout(),
		WriteTimeout:      server.config.WriteTimeout(),
		IdleTimeout:       server.config.IdleTimeout(),
	}
}
