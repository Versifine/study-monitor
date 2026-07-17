package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLoggerEmitsRequiredUTCFields(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(&output, "info", "0.1.0-test")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("app", "started", "service started", slog.String("listen_address", "127.0.0.1:47831"))

	record := decodeRecord(t, output.String())
	assertString(t, record, "level", "info")
	assertString(t, record, "service", "exam-monitor")
	assertString(t, record, "build_version", "0.1.0-test")
	assertString(t, record, "component", "app")
	assertString(t, record, "event", "started")
	assertString(t, record, "listen_address", "127.0.0.1:47831")

	timestamp, ok := record["time"].(string)
	if !ok {
		t.Fatalf("time is not a string: %#v", record["time"])
	}
	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		t.Fatalf("time is not RFC3339: %v", err)
	}
	if parsed.Location() != time.UTC {
		t.Fatalf("time location = %v, want UTC", parsed.Location())
	}
}

func TestErrorLogIncludesStableCode(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(&output, "info", "dev")
	if err != nil {
		t.Fatal(err)
	}
	logger.Error("config", "load_failed", "CONFIG_READ_FAILED", "configuration failed", errors.New("missing file"))

	record := decodeRecord(t, output.String())
	assertString(t, record, "error_code", "CONFIG_READ_FAILED")
	assertString(t, record, "error", "missing file")
}

func TestLoggerHonorsLevel(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(&output, "warn", "dev")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("app", "ignored", "not emitted")
	logger.Warn("app", "degraded", "APP_DEGRADED", "degraded")
	if lines := strings.Count(strings.TrimSpace(output.String()), "\n") + 1; lines != 1 {
		t.Fatalf("log line count = %d; output=%q", lines, output.String())
	}
	assertString(t, decodeRecord(t, output.String()), "level", "warn")
}

func TestLoggerRejectsUnknownLevel(t *testing.T) {
	if _, err := New(&bytes.Buffer{}, "trace", "dev"); err == nil {
		t.Fatal("New() unexpectedly succeeded")
	}
}

func decodeRecord(t *testing.T, line string) map[string]any {
	t.Helper()
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &record); err != nil {
		t.Fatalf("invalid JSON log: %v; line=%q", err, line)
	}
	return record
}

func assertString(t *testing.T, record map[string]any, key, want string) {
	t.Helper()
	if got, ok := record[key].(string); !ok || got != want {
		t.Fatalf("%s = %#v, want %q", key, record[key], want)
	}
}
