package mediaingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Versifine/study-monitor/internal/strictjson"
)

type Sidecar struct {
	SchemaVersion        int    `json:"schema_version"`
	Complete             bool   `json:"complete"`
	CollectorID          string `json:"collector_id"`
	SourceIdempotencyKey string `json:"source_idempotency_key"`
	DeviceStartRaw       string `json:"device_start_raw"`
	DeviceEndRaw         string `json:"device_end_raw"`
	ClockOffsetMS        *int64 `json:"clock_offset_ms"`
	ClockErrorMS         *int64 `json:"clock_error_ms"`
	SizeBytes            int64  `json:"size_bytes"`
	SHA256               string `json:"sha256"`
	MediaType            string `json:"media_type"`
}

type parsedSidecar struct {
	Sidecar
	DeviceStartUTC string
	DeviceEndUTC   string
	Fingerprint    string
}

var sidecarFields = []string{
	"schema_version",
	"complete",
	"collector_id",
	"source_idempotency_key",
	"device_start_raw",
	"device_end_raw",
	"clock_offset_ms",
	"clock_error_ms",
	"size_bytes",
	"sha256",
	"media_type",
}

func parseSidecar(raw []byte, maxDuration time.Duration) (parsedSidecar, error) {
	if err := strictjson.ValidateExactRootObjectRequired(raw, 0, sidecarFields...); err != nil {
		return parsedSidecar{}, &Error{Code: CodeSidecarInvalid, Err: errors.New("sidecar JSON does not match schema v1")}
	}
	var requiredTypes struct {
		Complete *bool `json:"complete"`
	}
	if err := json.Unmarshal(raw, &requiredTypes); err != nil || requiredTypes.Complete == nil {
		return parsedSidecar{}, &Error{Code: CodeSidecarInvalid, Err: errors.New("sidecar complete field must be a boolean")}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var sidecar Sidecar
	if err := decoder.Decode(&sidecar); err != nil {
		return parsedSidecar{}, &Error{Code: CodeSidecarInvalid, Err: errors.New("sidecar JSON is invalid")}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return parsedSidecar{}, &Error{Code: CodeSidecarInvalid, Err: errors.New("sidecar contains trailing JSON")}
	}
	if sidecar.SchemaVersion != SidecarSchemaVersion {
		return parsedSidecar{}, &Error{Code: CodeSidecarInvalid, Err: errors.New("sidecar schema version is unsupported")}
	}
	if !sidecar.Complete {
		return parsedSidecar{}, &Error{Code: CodeSidecarIncomplete, Err: errors.New("sidecar is not marked complete")}
	}
	if !validIdentifier(sidecar.CollectorID, 128) || !validIdentifier(sidecar.SourceIdempotencyKey, 256) {
		return parsedSidecar{}, &Error{Code: CodeSidecarInvalid, Err: errors.New("sidecar identity is invalid")}
	}
	start, err := parseDeviceTime(sidecar.DeviceStartRaw)
	if err != nil {
		return parsedSidecar{}, err
	}
	end, err := parseDeviceTime(sidecar.DeviceEndRaw)
	if err != nil {
		return parsedSidecar{}, err
	}
	if !end.After(start) || end.Sub(start) > maxDuration {
		return parsedSidecar{}, &Error{Code: CodeTimeInvalid, Err: errors.New("sidecar device time range is invalid or exceeds the segment limit")}
	}
	if sidecar.ClockOffsetMS == nil || *sidecar.ClockOffsetMS < -365*24*60*60*1000 || *sidecar.ClockOffsetMS > 365*24*60*60*1000 {
		return parsedSidecar{}, &Error{Code: CodeTimeInvalid, Err: errors.New("sidecar clock offset is missing or invalid")}
	}
	if sidecar.ClockErrorMS == nil || *sidecar.ClockErrorMS < 0 || *sidecar.ClockErrorMS > 24*60*60*1000 {
		return parsedSidecar{}, &Error{Code: CodeTimeInvalid, Err: errors.New("sidecar clock error is missing or invalid")}
	}
	if sidecar.SizeBytes <= 0 {
		return parsedSidecar{}, &Error{Code: CodeSidecarInvalid, Err: errors.New("sidecar size must be positive")}
	}
	if len(sidecar.SHA256) != 64 {
		return parsedSidecar{}, &Error{Code: CodeSidecarInvalid, Err: errors.New("sidecar SHA-256 must be lowercase hexadecimal")}
	}
	if _, err := hex.DecodeString(sidecar.SHA256); err != nil || strings.ToLower(sidecar.SHA256) != sidecar.SHA256 {
		return parsedSidecar{}, &Error{Code: CodeSidecarInvalid, Err: errors.New("sidecar SHA-256 must be lowercase hexadecimal")}
	}
	if sidecar.MediaType != "video" {
		return parsedSidecar{}, &Error{Code: CodeTypeInvalid, Err: errors.New("sidecar media_type must be video")}
	}
	canonical, err := json.Marshal(sidecar)
	if err != nil {
		return parsedSidecar{}, &Error{Code: CodeSidecarInvalid, Err: errors.New("sidecar cannot be canonicalized")}
	}
	digest := sha256.Sum256(canonical)
	return parsedSidecar{
		Sidecar:        sidecar,
		DeviceStartUTC: start.UTC().Format(time.RFC3339Nano),
		DeviceEndUTC:   end.UTC().Format(time.RFC3339Nano),
		Fingerprint:    hex.EncodeToString(digest[:]),
	}, nil
}

func parseDeviceTime(raw string) (time.Time, error) {
	if raw == "" || len(raw) > 128 || strings.TrimSpace(raw) != raw || strings.HasSuffix(raw, "-00:00") {
		return time.Time{}, &Error{Code: CodeTimeInvalid, Err: errors.New("sidecar device time must have an explicit RFC3339 offset")}
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, &Error{Code: CodeTimeInvalid, Err: errors.New("sidecar device time must have an explicit RFC3339 offset")}
	}
	return parsed, nil
}

func validIdentifier(value string, maximum int) bool {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func metadataHash(sidecar parsedSidecar, probe ProbeInfo) (string, error) {
	value := struct {
		DeviceStartRaw       string `json:"device_start_raw"`
		DeviceEndRaw         string `json:"device_end_raw"`
		DeviceStartUTC       string `json:"device_start_utc"`
		DeviceEndUTC         string `json:"device_end_utc"`
		ClockOffsetMS        int64  `json:"clock_offset_ms"`
		ClockErrorMS         int64  `json:"clock_error_ms"`
		SizeBytes            int64  `json:"size_bytes"`
		DurationMS           int64  `json:"duration_ms"`
		CodecName            string `json:"codec_name"`
		FormatName           string `json:"format_name"`
		MediaType            string `json:"media_type"`
		SHA256               string `json:"sha256"`
		SidecarSchemaVersion int    `json:"sidecar_schema_version"`
	}{
		DeviceStartRaw: sidecar.DeviceStartRaw, DeviceEndRaw: sidecar.DeviceEndRaw,
		DeviceStartUTC: sidecar.DeviceStartUTC, DeviceEndUTC: sidecar.DeviceEndUTC,
		ClockOffsetMS: *sidecar.ClockOffsetMS, ClockErrorMS: *sidecar.ClockErrorMS,
		SizeBytes: sidecar.SizeBytes, DurationMS: probe.DurationMS,
		CodecName: probe.CodecName, FormatName: probe.FormatName,
		MediaType: sidecar.MediaType, SHA256: sidecar.SHA256,
		SidecarSchemaVersion: sidecar.SchemaVersion,
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal media metadata: %w", err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
