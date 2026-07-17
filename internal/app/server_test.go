package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Versifine/study-monitor/internal/config"
	"github.com/Versifine/study-monitor/internal/eventstore"
	"github.com/Versifine/study-monitor/internal/httpapi"
	"github.com/Versifine/study-monitor/internal/logging"
	"github.com/Versifine/study-monitor/internal/version"
)

func TestLivenessEndpointIsInfrastructureOnly(t *testing.T) {
	server := newTestServer(t, &bytes.Buffer{})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("Cache-Control = %q", cache)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"status": "ok", "service": "exam-monitor", "version": "0.1.0-test", "mode": "record-only",
	}
	for key, value := range want {
		if response[key] != value {
			t.Fatalf("%s = %#v, want %q", key, response[key], value)
		}
	}
	for _, forbidden := range []string{"ready", "database", "storage", "events"} {
		if _, exists := response[forbidden]; exists {
			t.Fatalf("liveness leaked storage/readiness field %q", forbidden)
		}
	}
}

func TestLivenessMethods(t *testing.T) {
	server := newTestServer(t, &bytes.Buffer{})

	head := httptest.NewRecorder()
	server.Handler().ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/health/live", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD response: status=%d body=%q", head.Code, head.Body.String())
	}

	post := httptest.NewRecorder()
	server.Handler().ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/health/live", nil))
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST response: status=%d allow=%q", post.Code, post.Header().Get("Allow"))
	}
}

func TestHTTPServerHasBoundedConnectionLifecycle(t *testing.T) {
	server := newTestServer(t, &bytes.Buffer{})
	httpServer := server.newHTTPServer()
	if httpServer.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("ReadHeaderTimeout = %s", httpServer.ReadHeaderTimeout)
	}
	if httpServer.ReadTimeout != 10*time.Second {
		t.Fatalf("ReadTimeout = %s", httpServer.ReadTimeout)
	}
	if httpServer.WriteTimeout != 10*time.Second {
		t.Fatalf("WriteTimeout = %s", httpServer.WriteTimeout)
	}
	if httpServer.IdleTimeout != 30*time.Second {
		t.Fatalf("IdleTimeout = %s", httpServer.IdleTimeout)
	}
}

func TestUnknownRouteIsNotFound(t *testing.T) {
	server := newTestServer(t, &bytes.Buffer{})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/events", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

func TestRunReportsOccupiedAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	cfg := loadTestConfig(t)
	cfg.Server.ListenAddress = listener.Addr().String()
	logger, err := logging.New(&bytes.Buffer{}, "info", "test")
	if err != nil {
		t.Fatal(err)
	}
	err = Run(context.Background(), cfg, logger, version.Info{Version: "test"})
	if err == nil {
		t.Fatal("Run() unexpectedly succeeded")
	}
	if got := ErrorCode(err); got != CodeListenFailed {
		t.Fatalf("ErrorCode() = %q, want %q", got, CodeListenFailed)
	}
}

func TestStorageInitializationFailureClassification(t *testing.T) {
	tests := []struct {
		code   string
		status string
	}{
		{code: eventstore.CodeMigrationFailed, status: eventstore.ReadinessMigrationFailed},
		{code: eventstore.CodeMigrationUnsupported, status: eventstore.ReadinessMigrationFailed},
		{code: eventstore.CodeReadOnly, status: eventstore.ReadinessReadOnly},
		{code: eventstore.CodeBusy, status: eventstore.ReadinessBusy},
		{code: eventstore.CodeOpenFailed, status: eventstore.ReadinessUnavailable},
	}
	for _, test := range tests {
		failure := classifyStorageFailure(&eventstore.Error{Code: test.code, Err: errors.New("test")})
		if failure.Status != test.status || failure.ErrorCode != test.code {
			t.Fatalf("classifyStorageFailure(%q) = %#v", test.code, failure)
		}
	}
}

func TestServeStopsCleanlyWhenContextIsCanceled(t *testing.T) {
	var logs bytes.Buffer
	server := newTestServer(t, &logs)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()

	url := "http://" + listener.Addr().String() + "/health/live"
	waitForLiveness(t, url)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve() did not stop after cancellation")
	}
	if !bytes.Contains(logs.Bytes(), []byte(`"event":"stopped"`)) {
		t.Fatalf("stopped event missing from logs: %s", logs.String())
	}
}

func newTestServer(t *testing.T, output io.Writer) *Server {
	t.Helper()
	logger, err := logging.New(output, "info", "0.1.0-test")
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(loadTestConfig(t), logger, version.Info{Version: "0.1.0-test"}, nil, httpapi.StorageFailure{})
}

func loadTestConfig(t *testing.T) config.Config {
	t.Helper()
	localAppData := t.TempDir()
	cfg, err := config.Load("", func(key string) (string, bool) {
		if key == "LOCALAPPDATA" {
			return localAppData, true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func waitForLiveness(t *testing.T, url string) {
	t.Helper()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("liveness endpoint did not become ready: %s", url)
}
