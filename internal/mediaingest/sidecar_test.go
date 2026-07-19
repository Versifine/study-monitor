package mediaingest

import (
	"strings"
	"testing"
	"time"
)

func TestSidecarUsesExactSchemaAndUnambiguousTime(t *testing.T) {
	base := `{"schema_version":1,"complete":true,"collector_id":"desk.camera","source_idempotency_key":"segment-1","device_start_raw":"2026-07-18T10:00:00+08:00","device_end_raw":"2026-07-18T10:00:01+08:00","clock_offset_ms":0,"clock_error_ms":10,"size_bytes":100,"sha256":"` + strings.Repeat("a", 64) + `","media_type":"video"}`
	if _, err := parseSidecar([]byte(base), 10*time.Minute); err != nil {
		t.Fatalf("valid sidecar = %v", err)
	}
	tests := []struct {
		name string
		raw  string
		code string
	}{
		{name: "case alias", raw: strings.Replace(base, `"media_type":`, `"Media_Type":`, 1), code: CodeSidecarInvalid},
		{name: "duplicate field", raw: strings.Replace(base, `"media_type":"video"`, `"media_type":"video","media_type":"video"`, 1), code: CodeSidecarInvalid},
		{name: "unknown offset", raw: strings.Replace(base, "+08:00", "-00:00", 1), code: CodeTimeInvalid},
		{name: "incomplete", raw: strings.Replace(base, `"complete":true`, `"complete":false`, 1), code: CodeSidecarIncomplete},
		{name: "over ten minutes", raw: strings.Replace(base, "10:00:01", "10:11:00", 1), code: CodeTimeInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseSidecar([]byte(test.raw), 10*time.Minute); ErrorCode(err) != test.code {
				t.Fatalf("parseSidecar() error = %v, want %s", err, test.code)
			}
		})
	}
}
