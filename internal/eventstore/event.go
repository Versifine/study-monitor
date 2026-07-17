package eventstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const EventSchemaVersion = 1

const (
	StatusAccepted  = "accepted"
	StatusDuplicate = "duplicate"
	StatusRejected  = "rejected"
	StatusConflict  = "conflict"
)

const (
	CodeBatchEmpty          = "EVENT_BATCH_EMPTY"
	CodeBatchTooLarge       = "EVENT_BATCH_TOO_LARGE"
	CodeEventTooLarge       = "EVENT_TOO_LARGE"
	CodeEventDecodeInvalid  = "EVENT_JSON_INVALID"
	CodeEventSchemaInvalid  = "EVENT_SCHEMA_UNSUPPORTED"
	CodeCollectorInvalid    = "EVENT_COLLECTOR_INVALID"
	CodeEventTypeInvalid    = "EVENT_TYPE_INVALID"
	CodeDeviceTimeInvalid   = "EVENT_DEVICE_TIME_INVALID"
	CodeClockOffsetInvalid  = "EVENT_CLOCK_OFFSET_INVALID"
	CodeClockErrorInvalid   = "EVENT_CLOCK_ERROR_INVALID"
	CodeIdempotencyInvalid  = "EVENT_IDEMPOTENCY_KEY_INVALID"
	CodePayloadInvalid      = "EVENT_PAYLOAD_INVALID"
	CodePayloadTooDeep      = "EVENT_PAYLOAD_TOO_DEEP"
	CodeIdempotencyConflict = "EVENT_IDEMPOTENCY_CONFLICT"
)

type Candidate struct {
	Raw json.RawMessage
}

type WriteResult struct {
	Index     int    `json:"index"`
	Status    string `json:"status"`
	EventID   int64  `json:"event_id,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
}

type eventInput struct {
	SchemaVersion      int             `json:"schema_version"`
	CollectorID        string          `json:"collector_id"`
	EventType          string          `json:"event_type"`
	DeviceTimestampRaw string          `json:"device_timestamp_raw"`
	ClockOffsetMS      *int64          `json:"clock_offset_ms"`
	ClockErrorMS       *int64          `json:"clock_error_ms"`
	IdempotencyKey     string          `json:"idempotency_key"`
	Payload            json.RawMessage `json:"payload"`
}

type preparedEvent struct {
	SchemaVersion      int
	CollectorID        string
	EventType          string
	DeviceTimestampRaw string
	DeviceTimeUTC      string
	ReceivedAtUTC      string
	ClockOffsetMS      int64
	ClockErrorMS       int64
	IdempotencyKey     string
	PayloadJSON        string
	PayloadHash        string
	ContentHash        string
}

type validationError struct {
	code string
	err  error
}

func (e *validationError) Error() string { return e.err.Error() }

func (store *Store) AppendBatch(ctx context.Context, candidates []Candidate) ([]WriteResult, error) {
	if len(candidates) == 0 {
		return nil, &validationError{code: CodeBatchEmpty, err: errors.New("event batch must contain at least one item")}
	}
	if len(candidates) > store.maxBatchEvents {
		return nil, &validationError{code: CodeBatchTooLarge, err: errors.New("event batch exceeds configured item limit")}
	}

	receivedAt := store.now().UTC().Format(time.RFC3339Nano)
	results := make([]WriteResult, len(candidates))
	prepared := make([]*preparedEvent, len(candidates))
	validCount := 0
	for index, candidate := range candidates {
		results[index].Index = index
		item, err := store.prepare(candidate.Raw, receivedAt)
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
		return nil, classifySQLiteError(CodeWriteFailed, "begin event batch", err)
	}
	defer tx.Rollback()
	for index, item := range prepared {
		if item == nil {
			continue
		}
		result, err := appendOne(ctx, tx, index, item)
		if err != nil {
			return nil, err
		}
		results[index] = result
	}
	if err := tx.Commit(); err != nil {
		return nil, classifySQLiteError(CodeWriteFailed, "commit event batch", err)
	}
	return results, nil
}

func appendOne(ctx context.Context, tx *sql.Tx, index int, item *preparedEvent) (WriteResult, error) {
	result, err := tx.ExecContext(ctx, `
INSERT INTO raw_events (
    collector_id, event_type, device_timestamp_raw, device_time_utc, received_at_utc,
    clock_offset_ms, clock_error_ms, idempotency_key, payload_json, payload_hash,
    content_hash, schema_version
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(collector_id, idempotency_key) DO NOTHING`,
		item.CollectorID,
		item.EventType,
		item.DeviceTimestampRaw,
		item.DeviceTimeUTC,
		item.ReceivedAtUTC,
		item.ClockOffsetMS,
		item.ClockErrorMS,
		item.IdempotencyKey,
		item.PayloadJSON,
		item.PayloadHash,
		item.ContentHash,
		item.SchemaVersion,
	)
	if err != nil {
		return WriteResult{}, classifySQLiteError(CodeWriteFailed, "insert raw event", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return WriteResult{}, wrap(CodeWriteFailed, "read insert result", err)
	}
	if rowsAffected == 1 {
		id, err := result.LastInsertId()
		if err != nil {
			return WriteResult{}, wrap(CodeWriteFailed, "read accepted event id", err)
		}
		return WriteResult{Index: index, Status: StatusAccepted, EventID: id}, nil
	}

	var id int64
	var existingHash string
	if err := tx.QueryRowContext(
		ctx,
		"SELECT id, content_hash FROM raw_events WHERE collector_id = ? AND idempotency_key = ?",
		item.CollectorID,
		item.IdempotencyKey,
	).Scan(&id, &existingHash); err != nil {
		return WriteResult{}, classifySQLiteError(CodeWriteFailed, "read idempotent event", err)
	}
	if existingHash == item.ContentHash {
		return WriteResult{Index: index, Status: StatusDuplicate, EventID: id}, nil
	}
	return WriteResult{Index: index, Status: StatusConflict, EventID: id, ErrorCode: CodeIdempotencyConflict}, nil
}

func (store *Store) prepare(raw json.RawMessage, receivedAt string) (*preparedEvent, error) {
	if len(raw) > store.maxEventBytes {
		return nil, &validationError{code: CodeEventTooLarge, err: errors.New("event exceeds configured byte limit")}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var input eventInput
	if err := decoder.Decode(&input); err != nil {
		return nil, &validationError{code: CodeEventDecodeInvalid, err: errors.New("event JSON is invalid")}
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, &validationError{code: CodeEventDecodeInvalid, err: errors.New("event JSON contains trailing data")}
	}
	if input.SchemaVersion != EventSchemaVersion {
		return nil, &validationError{code: CodeEventSchemaInvalid, err: errors.New("event schema version is unsupported")}
	}
	if !validIdentifier(input.CollectorID, 128) {
		return nil, &validationError{code: CodeCollectorInvalid, err: errors.New("collector_id is invalid")}
	}
	if !validIdentifier(input.EventType, 128) {
		return nil, &validationError{code: CodeEventTypeInvalid, err: errors.New("event_type is invalid")}
	}
	if len(input.DeviceTimestampRaw) == 0 || len(input.DeviceTimestampRaw) > 128 || strings.TrimSpace(input.DeviceTimestampRaw) != input.DeviceTimestampRaw {
		return nil, &validationError{code: CodeDeviceTimeInvalid, err: errors.New("device_timestamp_raw is invalid")}
	}
	deviceTime, err := time.Parse(time.RFC3339Nano, input.DeviceTimestampRaw)
	if err != nil {
		return nil, &validationError{code: CodeDeviceTimeInvalid, err: errors.New("device_timestamp_raw must include an RFC3339 offset")}
	}
	if input.ClockOffsetMS == nil || *input.ClockOffsetMS < -365*24*60*60*1000 || *input.ClockOffsetMS > 365*24*60*60*1000 {
		return nil, &validationError{code: CodeClockOffsetInvalid, err: errors.New("clock_offset_ms is missing or outside the supported range")}
	}
	if input.ClockErrorMS == nil || *input.ClockErrorMS < 0 || *input.ClockErrorMS > 24*60*60*1000 {
		return nil, &validationError{code: CodeClockErrorInvalid, err: errors.New("clock_error_ms is missing or outside the supported range")}
	}
	if !validIdentifier(input.IdempotencyKey, 256) {
		return nil, &validationError{code: CodeIdempotencyInvalid, err: errors.New("idempotency_key is invalid")}
	}
	payloadValue, err := parsePayload(input.Payload, store.maxPayloadDepth)
	if err != nil {
		return nil, err
	}
	canonicalPayload, err := json.Marshal(payloadValue)
	if err != nil {
		return nil, &validationError{code: CodePayloadInvalid, err: errors.New("payload cannot be canonicalized")}
	}
	payloadDigest := sha256.Sum256(canonicalPayload)
	payloadHash := hex.EncodeToString(payloadDigest[:])

	deviceTimeUTC := deviceTime.UTC().Format(time.RFC3339Nano)
	contentBytes, err := json.Marshal(struct {
		SchemaVersion      int    `json:"schema_version"`
		CollectorID        string `json:"collector_id"`
		EventType          string `json:"event_type"`
		DeviceTimestampRaw string `json:"device_timestamp_raw"`
		DeviceTimeUTC      string `json:"device_time_utc"`
		ClockOffsetMS      int64  `json:"clock_offset_ms"`
		ClockErrorMS       int64  `json:"clock_error_ms"`
		PayloadHash        string `json:"payload_hash"`
	}{
		SchemaVersion:      input.SchemaVersion,
		CollectorID:        input.CollectorID,
		EventType:          input.EventType,
		DeviceTimestampRaw: input.DeviceTimestampRaw,
		DeviceTimeUTC:      deviceTimeUTC,
		ClockOffsetMS:      *input.ClockOffsetMS,
		ClockErrorMS:       *input.ClockErrorMS,
		PayloadHash:        payloadHash,
	})
	if err != nil {
		return nil, wrap(CodeWriteFailed, "hash event content", err)
	}
	contentDigest := sha256.Sum256(contentBytes)

	return &preparedEvent{
		SchemaVersion:      input.SchemaVersion,
		CollectorID:        input.CollectorID,
		EventType:          input.EventType,
		DeviceTimestampRaw: input.DeviceTimestampRaw,
		DeviceTimeUTC:      deviceTimeUTC,
		ReceivedAtUTC:      receivedAt,
		ClockOffsetMS:      *input.ClockOffsetMS,
		ClockErrorMS:       *input.ClockErrorMS,
		IdempotencyKey:     input.IdempotencyKey,
		PayloadJSON:        string(input.Payload),
		PayloadHash:        payloadHash,
		ContentHash:        hex.EncodeToString(contentDigest[:]),
	}, nil
}

func parsePayload(raw json.RawMessage, maxDepth int) (any, error) {
	if len(raw) == 0 {
		return nil, &validationError{code: CodePayloadInvalid, err: errors.New("payload is required")}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := parseJSONValue(decoder, 1, maxDepth)
	if err != nil {
		return nil, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, &validationError{code: CodePayloadInvalid, err: errors.New("payload contains trailing data")}
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, &validationError{code: CodePayloadInvalid, err: errors.New("payload must be a JSON object")}
	}
	return value, nil
}

func parseJSONValue(decoder *json.Decoder, depth, maxDepth int) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, &validationError{code: CodePayloadInvalid, err: errors.New("payload JSON is invalid")}
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	if depth > maxDepth {
		return nil, &validationError{code: CodePayloadTooDeep, err: errors.New("payload exceeds configured nesting depth")}
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, &validationError{code: CodePayloadInvalid, err: errors.New("payload object key is invalid")}
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, &validationError{code: CodePayloadInvalid, err: errors.New("payload object key must be a string")}
			}
			if _, exists := object[key]; exists {
				return nil, &validationError{code: CodePayloadInvalid, err: errors.New("payload contains a duplicate object key")}
			}
			value, err := parseJSONValue(decoder, depth+1, maxDepth)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, &validationError{code: CodePayloadInvalid, err: errors.New("payload object is not closed")}
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := parseJSONValue(decoder, depth+1, maxDepth)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, &validationError{code: CodePayloadInvalid, err: errors.New("payload array is not closed")}
		}
		return array, nil
	default:
		return nil, &validationError{code: CodePayloadInvalid, err: errors.New("payload contains an unexpected delimiter")}
	}
}

func validIdentifier(value string, maximum int) bool {
	if value == "" || len(value) > maximum || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func ValidationCode(err error) string {
	var invalid *validationError
	if errors.As(err, &invalid) {
		return invalid.code
	}
	return ""
}
