package eventstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/Versifine/study-monitor/internal/strictjson"
)

const HeartbeatSchemaVersion = 1

const fixedUTCLayout = "2006-01-02T15:04:05.000000000Z"

const (
	HeartbeatStateActive = "active"
	HeartbeatStateIdle   = "idle"

	CodeHeartbeatTooLarge      = "HEARTBEAT_TOO_LARGE"
	CodeHeartbeatJSONInvalid   = "HEARTBEAT_JSON_INVALID"
	CodeHeartbeatSchemaInvalid = "HEARTBEAT_SCHEMA_UNSUPPORTED"
	CodeHeartbeatCollector     = "HEARTBEAT_COLLECTOR_INVALID"
	CodeHeartbeatState         = "HEARTBEAT_STATE_INVALID"
	CodeHeartbeatTime          = "HEARTBEAT_TIME_INVALID"
	CodeHeartbeatDuration      = "HEARTBEAT_DURATION_INVALID"
	CodeHeartbeatClock         = "HEARTBEAT_CLOCK_INVALID"
	CodeHeartbeatIdempotency   = "HEARTBEAT_IDEMPOTENCY_KEY_INVALID"
	CodeHeartbeatQuality       = "HEARTBEAT_QUALITY_FLAGS_INVALID"
	CodeHeartbeatConflict      = "HEARTBEAT_IDEMPOTENCY_CONFLICT"
)

var heartbeatInputJSONFields = []string{
	"schema_version", "collector_id", "state", "device_start_raw", "device_end_raw",
	"clock_offset_ms", "clock_error_ms", "idempotency_key", "quality_flags",
}

type HeartbeatCandidate struct {
	Raw json.RawMessage
}

type HeartbeatWriteResult struct {
	Index       int    `json:"index"`
	Status      string `json:"status"`
	HeartbeatID int64  `json:"heartbeat_id,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
}

type heartbeatInput struct {
	SchemaVersion  int       `json:"schema_version"`
	CollectorID    string    `json:"collector_id"`
	State          string    `json:"state"`
	DeviceStartRaw string    `json:"device_start_raw"`
	DeviceEndRaw   string    `json:"device_end_raw"`
	ClockOffsetMS  *int64    `json:"clock_offset_ms"`
	ClockErrorMS   *int64    `json:"clock_error_ms"`
	IdempotencyKey string    `json:"idempotency_key"`
	QualityFlags   *[]string `json:"quality_flags"`
}

type preparedHeartbeat struct {
	SchemaVersion     int
	CollectorID       string
	State             string
	DeviceStartRaw    string
	DeviceEndRaw      string
	DeviceStartUTC    string
	DeviceEndUTC      string
	ReceivedAtUTC     string
	CorrectedStartUTC string
	CorrectedEndUTC   string
	ClockOffsetMS     int64
	ClockErrorMS      int64
	IdempotencyKey    string
	QualityFlagsJSON  string
	ContentHash       string
}

func (store *Store) AppendHeartbeatBatch(ctx context.Context, candidates []HeartbeatCandidate) ([]HeartbeatWriteResult, error) {
	if len(candidates) == 0 {
		return nil, &validationError{code: CodeBatchEmpty, err: errors.New("heartbeat batch must contain at least one item")}
	}
	if len(candidates) > store.maxBatchEvents {
		return nil, &validationError{code: CodeBatchTooLarge, err: errors.New("heartbeat batch exceeds configured item limit")}
	}
	receivedAt := store.now().UTC()
	results := make([]HeartbeatWriteResult, len(candidates))
	prepared := make([]*preparedHeartbeat, len(candidates))
	validCount := 0
	for index, candidate := range candidates {
		results[index].Index = index
		item, err := store.prepareHeartbeat(candidate.Raw, receivedAt)
		if err != nil {
			var invalid *validationError
			if !errors.As(err, &invalid) {
				return nil, err
			}
			results[index].Status = StatusRejected
			results[index].ErrorCode = invalid.code
			continue
		}
		prepared[index] = item
		validCount++
	}
	if validCount == 0 {
		return results, nil
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, classifySQLiteError(CodeWriteFailed, "begin heartbeat batch", err)
	}
	defer tx.Rollback()
	for index, item := range prepared {
		if item == nil {
			continue
		}
		result, err := appendHeartbeat(ctx, tx, index, item)
		if err != nil {
			return nil, err
		}
		results[index] = result
	}
	if err := tx.Commit(); err != nil {
		return nil, classifySQLiteError(CodeWriteFailed, "commit heartbeat batch", err)
	}
	return results, nil
}

func appendHeartbeat(ctx context.Context, tx *sql.Tx, index int, item *preparedHeartbeat) (HeartbeatWriteResult, error) {
	result, err := tx.ExecContext(ctx, `
INSERT INTO collector_heartbeats (
    collector_id, idempotency_key, state, device_start_raw, device_end_raw,
    device_start_utc, device_end_utc, received_at_utc, corrected_start_utc,
    corrected_end_utc, clock_offset_ms, clock_error_ms, quality_flags_json,
    content_hash, schema_version
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(collector_id, idempotency_key) DO NOTHING`,
		item.CollectorID, item.IdempotencyKey, item.State, item.DeviceStartRaw, item.DeviceEndRaw,
		item.DeviceStartUTC, item.DeviceEndUTC, item.ReceivedAtUTC, item.CorrectedStartUTC,
		item.CorrectedEndUTC, item.ClockOffsetMS, item.ClockErrorMS, item.QualityFlagsJSON,
		item.ContentHash, item.SchemaVersion)
	if err != nil {
		return HeartbeatWriteResult{}, classifySQLiteError(CodeWriteFailed, "insert collector heartbeat", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return HeartbeatWriteResult{}, wrap(CodeWriteFailed, "read heartbeat insert result", err)
	}
	if rows == 1 {
		id, err := result.LastInsertId()
		if err != nil {
			return HeartbeatWriteResult{}, wrap(CodeWriteFailed, "read accepted heartbeat id", err)
		}
		return HeartbeatWriteResult{Index: index, Status: StatusAccepted, HeartbeatID: id}, nil
	}
	var id int64
	var contentHash string
	if err := tx.QueryRowContext(ctx, `SELECT id, content_hash FROM collector_heartbeats WHERE collector_id = ? AND idempotency_key = ?`, item.CollectorID, item.IdempotencyKey).Scan(&id, &contentHash); err != nil {
		return HeartbeatWriteResult{}, classifySQLiteError(CodeWriteFailed, "read idempotent heartbeat", err)
	}
	if contentHash == item.ContentHash {
		return HeartbeatWriteResult{Index: index, Status: StatusDuplicate, HeartbeatID: id}, nil
	}
	return HeartbeatWriteResult{Index: index, Status: StatusConflict, HeartbeatID: id, ErrorCode: CodeHeartbeatConflict}, nil
}

func (store *Store) prepareHeartbeat(raw json.RawMessage, receivedAt time.Time) (*preparedHeartbeat, error) {
	if len(raw) > store.maxEventBytes {
		return nil, &validationError{code: CodeHeartbeatTooLarge, err: errors.New("heartbeat exceeds configured byte limit")}
	}
	if err := strictjson.ValidateExactRootObject(raw, 0, heartbeatInputJSONFields...); err != nil {
		return nil, &validationError{code: CodeHeartbeatJSONInvalid, err: errors.New("heartbeat JSON contains a duplicate or unexpected key, or is invalid")}
	}
	var input heartbeatInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || ensureJSONEOF(decoder) != nil {
		return nil, &validationError{code: CodeHeartbeatJSONInvalid, err: errors.New("heartbeat JSON is invalid")}
	}
	if input.SchemaVersion != HeartbeatSchemaVersion {
		return nil, &validationError{code: CodeHeartbeatSchemaInvalid, err: errors.New("heartbeat schema version is unsupported")}
	}
	policy, configured := store.collectorPolicies[input.CollectorID]
	if !configured || !validIdentifier(input.CollectorID, 128) {
		return nil, &validationError{code: CodeHeartbeatCollector, err: errors.New("heartbeat collector is not enabled or is invalid")}
	}
	if input.State != HeartbeatStateActive && input.State != HeartbeatStateIdle {
		return nil, &validationError{code: CodeHeartbeatState, err: errors.New("heartbeat state must be active or idle")}
	}
	start, err := parseKnownOffsetTime(input.DeviceStartRaw)
	if err != nil {
		return nil, &validationError{code: CodeHeartbeatTime, err: errors.New("device_start_raw must be RFC3339 with a known offset")}
	}
	end, err := parseKnownOffsetTime(input.DeviceEndRaw)
	if err != nil {
		return nil, &validationError{code: CodeHeartbeatTime, err: errors.New("device_end_raw must be RFC3339 with a known offset")}
	}
	if !end.After(start) || end.Sub(start) > policy.HeartbeatPeriod {
		return nil, &validationError{code: CodeHeartbeatDuration, err: errors.New("heartbeat interval must be positive and no longer than the configured heartbeat period")}
	}
	if input.ClockOffsetMS == nil || *input.ClockOffsetMS < -365*24*60*60*1000 || *input.ClockOffsetMS > 365*24*60*60*1000 || input.ClockErrorMS == nil || *input.ClockErrorMS < 0 || *input.ClockErrorMS > 24*60*60*1000 {
		return nil, &validationError{code: CodeHeartbeatClock, err: errors.New("heartbeat clock metadata is missing or outside the supported range")}
	}
	if !validIdentifier(input.IdempotencyKey, 256) {
		return nil, &validationError{code: CodeHeartbeatIdempotency, err: errors.New("heartbeat idempotency_key is invalid")}
	}
	if input.QualityFlags == nil {
		return nil, &validationError{code: CodeHeartbeatQuality, err: errors.New("quality_flags is required")}
	}
	flags, err := normalizeQualityFlags(*input.QualityFlags)
	if err != nil {
		return nil, &validationError{code: CodeHeartbeatQuality, err: err}
	}
	flagsJSON, _ := json.Marshal(flags)
	content, _ := json.Marshal(struct {
		SchemaVersion  int      `json:"schema_version"`
		CollectorID    string   `json:"collector_id"`
		State          string   `json:"state"`
		DeviceStartRaw string   `json:"device_start_raw"`
		DeviceEndRaw   string   `json:"device_end_raw"`
		StartUTC       string   `json:"start_utc"`
		EndUTC         string   `json:"end_utc"`
		ClockOffsetMS  int64    `json:"clock_offset_ms"`
		ClockErrorMS   int64    `json:"clock_error_ms"`
		QualityFlags   []string `json:"quality_flags"`
	}{input.SchemaVersion, input.CollectorID, input.State, input.DeviceStartRaw, input.DeviceEndRaw, fixedUTC(start), fixedUTC(end), *input.ClockOffsetMS, *input.ClockErrorMS, flags})
	digest := sha256.Sum256(content)
	offset := time.Duration(*input.ClockOffsetMS) * time.Millisecond
	return &preparedHeartbeat{
		SchemaVersion: input.SchemaVersion, CollectorID: input.CollectorID, State: input.State,
		DeviceStartRaw: input.DeviceStartRaw, DeviceEndRaw: input.DeviceEndRaw,
		DeviceStartUTC: start.UTC().Format(time.RFC3339Nano), DeviceEndUTC: end.UTC().Format(time.RFC3339Nano),
		ReceivedAtUTC: receivedAt.UTC().Format(time.RFC3339Nano), CorrectedStartUTC: fixedUTC(start.Add(offset)),
		CorrectedEndUTC: fixedUTC(end.Add(offset)), ClockOffsetMS: *input.ClockOffsetMS,
		ClockErrorMS: *input.ClockErrorMS, IdempotencyKey: input.IdempotencyKey,
		QualityFlagsJSON: string(flagsJSON), ContentHash: hex.EncodeToString(digest[:]),
	}, nil
}

func parseKnownOffsetTime(value string) (time.Time, error) {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value || strings.HasSuffix(value, "-00:00") {
		return time.Time{}, errors.New("invalid timestamp")
	}
	return time.Parse(time.RFC3339Nano, value)
}

func fixedUTC(value time.Time) string {
	return value.UTC().Format(fixedUTCLayout)
}

func normalizeQualityFlags(values []string) ([]string, error) {
	allowed := map[string]bool{"obscured": true, "corrupt": true, "clock_uncertain": true, "incomplete": true}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !allowed[value] || seen[value] {
			return nil, errors.New("quality_flags contains an unsupported or duplicate value")
		}
		seen[value] = true
	}
	result := make([]string, 0, len(values))
	result = append(result, values...)
	sort.Strings(result)
	return result, nil
}
