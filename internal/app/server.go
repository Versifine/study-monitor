package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/Versifine/study-monitor/internal/config"
	"github.com/Versifine/study-monitor/internal/eventstore"
	"github.com/Versifine/study-monitor/internal/httpapi"
	"github.com/Versifine/study-monitor/internal/logging"
	"github.com/Versifine/study-monitor/internal/version"
)

const (
	CodeListenFailed   = "APP_LISTEN_FAILED"
	CodeServeFailed    = "APP_SERVE_FAILED"
	CodeShutdownFailed = "APP_SHUTDOWN_FAILED"

	runtimeMode = "record-only"
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

func NewServer(cfg config.Config, logger *logging.Logger, build version.Info, store httpapi.Store, failure httpapi.StorageFailure) *Server {
	return &Server{
		config:  cfg,
		logger:  logger,
		version: build,
		handler: httpapi.New(cfg, logger, build, store, failure),
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

	store, storeErr := eventstore.Open(ctx, cfg.DatabasePath(), eventstore.Options{
		BusyTimeout: cfg.BusyTimeout(), MaxOpenConnections: cfg.Storage.MaxOpenConnections,
		MaxBatchEvents: cfg.API.MaxBatchEvents, MaxEventBytes: cfg.API.MaxEventBytes,
		MaxPayloadDepth: cfg.API.MaxPayloadDepth, MaxPageSize: cfg.API.MaxPageSize,
	})
	failure := httpapi.StorageFailure{}
	if storeErr != nil {
		failure = classifyStorageFailure(storeErr)
		logger.Error("storage", "initialization_failed", failure.ErrorCode, "event storage initialization failed", storeErr)
	} else {
		defer store.Close()
	}
	return NewServer(cfg, logger, build, store, failure).Serve(ctx, listener)
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
		slog.String("mode", runtimeMode),
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
