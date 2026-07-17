package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/Versifine/study-monitor/internal/config"
	"github.com/Versifine/study-monitor/internal/logging"
	"github.com/Versifine/study-monitor/internal/version"
)

const (
	CodeListenFailed   = "APP_LISTEN_FAILED"
	CodeServeFailed    = "APP_SERVE_FAILED"
	CodeShutdownFailed = "APP_SHUTDOWN_FAILED"

	serviceName = "exam-monitor"
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

func NewServer(cfg config.Config, logger *logging.Logger, build version.Info) *Server {
	server := &Server{config: cfg, logger: logger, version: build}
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", server.handleLiveness)
	server.handler = mux
	return server
}

func (server *Server) Handler() http.Handler { return server.handler }

// Run binds the configured address and serves until the context is canceled.
func Run(ctx context.Context, cfg config.Config, logger *logging.Logger, build version.Info) error {
	listener, err := net.Listen("tcp", cfg.Server.ListenAddress)
	if err != nil {
		return &Error{Code: CodeListenFailed, Err: fmt.Errorf("listen on configured address: %w", err)}
	}
	defer listener.Close()
	return NewServer(cfg, logger, build).Serve(ctx, listener)
}

// Serve accepts a listener so graceful shutdown can be tested without fixed ports.
func (server *Server) Serve(ctx context.Context, listener net.Listener) error {
	httpServer := &http.Server{
		Handler:           server.handler,
		ReadHeaderTimeout: server.config.ReadHeaderTimeout(),
	}

	server.logger.Info(
		"app",
		"started",
		"recorder core skeleton started",
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
		server.logger.Info("app", "stopped", "recorder core skeleton stopped")
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
		server.logger.Info("app", "stopped", "recorder core skeleton stopped")
		return nil
	}
}

func (server *Server) handleLiveness(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		writer.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(writer).Encode(map[string]string{"status": "method_not_allowed"})
		return
	}

	writer.WriteHeader(http.StatusOK)
	if request.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(writer).Encode(struct {
		Status  string `json:"status"`
		Service string `json:"service"`
		Version string `json:"version"`
		Mode    string `json:"mode"`
	}{
		Status:  "ok",
		Service: serviceName,
		Version: server.version.Version,
		Mode:    runtimeMode,
	})
}
