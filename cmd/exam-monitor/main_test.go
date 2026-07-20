package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Versifine/study-monitor/internal/config"
)

func TestBinaryEmbedsIANAZoneData(t *testing.T) {
	// A frozen Windows deployment must not depend on a Go installation's
	// GOROOT/lib/time/zoneinfo.zip or an operator-provided ZONEINFO file.
	t.Setenv("GOROOT", t.TempDir())
	t.Setenv("ZONEINFO", filepath.Join(t.TempDir(), "missing-zoneinfo.zip"))
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load embedded IANA timezone: %v", err)
	}
	_, offset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC).In(location).Zone()
	if offset != 8*60*60 {
		t.Fatalf("Asia/Shanghai offset=%d", offset)
	}
}

func TestRunVersionIncludesRequiredFields(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--version"}, &stdout, &stderr, emptyEnvironment)
	if code != exitOK {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("invalid version JSON: %v", err)
	}
	for _, field := range []string{"version", "commit", "build_time_utc", "go_version"} {
		if value, ok := result[field].(string); !ok || value == "" {
			t.Fatalf("%s missing from version output: %#v", field, result[field])
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestRunCheckConfigDoesNotCreateRuntimeState(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := directoryEntries(t, directory)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--config", path, "--check-config"}, &stdout, &stderr, emptyEnvironment)
	if code != exitOK {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}
	after := directoryEntries(t, directory)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("check-config changed directory: before=%v after=%v", before, after)
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("invalid check JSON: %v", err)
	}
	if result["status"] != "ok" || result["listen_address"] != "127.0.0.1:47831" {
		t.Fatalf("unexpected check result: %#v", result)
	}
	wantDataDirectory := filepath.Join(testLocalAppData, "ExamMonitor", "data")
	if result["data_directory"] != wantDataDirectory {
		t.Fatalf("unexpected data directory: %#v", result["data_directory"])
	}
	if result["database_path"] != filepath.Join(wantDataDirectory, "exam-monitor.db") {
		t.Fatalf("unexpected database path: %#v", result["database_path"])
	}
}

func TestRunInvalidConfigUsesStableStructuredError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--config", path, "--check-config"}, &stdout, &stderr, emptyEnvironment)
	if code != exitUsage {
		t.Fatalf("exit code = %d", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("unexpected stdout: %s", stdout.String())
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stderr.Bytes()), &record); err != nil {
		t.Fatalf("stderr is not structured JSON: %v; output=%q", err, stderr.String())
	}
	if record["error_code"] != config.CodeUnsupportedSchema || record["event"] != "validation_failed" {
		t.Fatalf("unexpected error record: %#v", record)
	}
}

func TestRunRejectsConflictingActions(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--version", "--check-config"}, &stdout, &stderr, emptyEnvironment)
	if code != exitUsage || !strings.Contains(stderr.String(), codeCLIConflict) {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}

func TestRunRejectsInvalidRunFor(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--run-for=6m"}, &stdout, &stderr, emptyEnvironment)
	if code != exitUsage || !strings.Contains(stderr.String(), codeCLIRunFor) {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}

func TestRunHelpIsSideEffectFree(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--help"}, &stdout, &stderr, emptyEnvironment)
	if code != exitOK || !strings.Contains(stdout.String(), "Usage:") || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func directoryEntries(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

var testLocalAppData = filepath.Join(os.TempDir(), "exam-monitor-main-test-local-app-data")

func emptyEnvironment(key string) (string, bool) {
	if key == "LOCALAPPDATA" {
		return testLocalAppData, true
	}
	return "", false
}
