package eventstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
)

const (
	CodePageLimitInvalid = "QUERY_PAGE_LIMIT_INVALID"
	CodeCursorInvalid    = "QUERY_CURSOR_INVALID"
)

type Event struct {
	ID                 int64           `json:"id"`
	CollectorID        string          `json:"collector_id"`
	EventType          string          `json:"event_type"`
	DeviceTimestampRaw string          `json:"device_timestamp_raw"`
	DeviceTimeUTC      string          `json:"device_time_utc"`
	ReceivedAtUTC      string          `json:"received_at_utc"`
	ClockOffsetMS      int64           `json:"clock_offset_ms"`
	ClockErrorMS       int64           `json:"clock_error_ms"`
	IdempotencyKey     string          `json:"idempotency_key"`
	Payload            json.RawMessage `json:"payload"`
	PayloadHash        string          `json:"payload_hash"`
	SchemaVersion      int             `json:"schema_version"`
}

type Page struct {
	SnapshotID int64   `json:"snapshot_id"`
	Events     []Event `json:"events"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type pageCursor struct {
	Version    int   `json:"v"`
	SnapshotID int64 `json:"snapshot_id"`
	LastID     int64 `json:"last_id"`
}

func (store *Store) QueryPage(ctx context.Context, cursorText string, limit int) (Page, error) {
	if limit < 1 || limit > store.maxPageSize {
		return Page{}, &Error{Code: CodePageLimitInvalid, Err: errors.New("query page limit is outside the configured range")}
	}

	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Page{}, classifySQLiteError(CodeQueryFailed, "begin event query", err)
	}
	defer tx.Rollback()

	cursor := pageCursor{Version: 1}
	if cursorText == "" {
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM raw_events").Scan(&cursor.SnapshotID); err != nil {
			return Page{}, classifySQLiteError(CodeQueryFailed, "create query snapshot", err)
		}
	} else {
		decoded, err := decodeCursor(cursorText)
		if err != nil {
			return Page{}, err
		}
		cursor = decoded
	}

	rows, err := tx.QueryContext(ctx, `
SELECT id, collector_id, event_type, device_timestamp_raw, device_time_utc, received_at_utc,
       clock_offset_ms, clock_error_ms, idempotency_key, payload_json, payload_hash, schema_version
FROM raw_events
WHERE id > ? AND id <= ?
ORDER BY id ASC
LIMIT ?`, cursor.LastID, cursor.SnapshotID, limit+1)
	if err != nil {
		return Page{}, classifySQLiteError(CodeQueryFailed, "query raw events", err)
	}
	events := make([]Event, 0, limit+1)
	for rows.Next() {
		var event Event
		var payload string
		if err := rows.Scan(
			&event.ID,
			&event.CollectorID,
			&event.EventType,
			&event.DeviceTimestampRaw,
			&event.DeviceTimeUTC,
			&event.ReceivedAtUTC,
			&event.ClockOffsetMS,
			&event.ClockErrorMS,
			&event.IdempotencyKey,
			&payload,
			&event.PayloadHash,
			&event.SchemaVersion,
		); err != nil {
			rows.Close()
			return Page{}, wrap(CodeQueryFailed, "scan raw event", err)
		}
		event.Payload = json.RawMessage(payload)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Page{}, classifySQLiteError(CodeQueryFailed, "iterate event query", err)
	}
	if err := rows.Close(); err != nil {
		return Page{}, wrap(CodeQueryFailed, "close event query", err)
	}

	nextCursor := ""
	if len(events) > limit {
		events = events[:limit]
		nextCursor, err = encodeCursor(pageCursor{Version: 1, SnapshotID: cursor.SnapshotID, LastID: events[len(events)-1].ID})
		if err != nil {
			return Page{}, wrap(CodeQueryFailed, "encode query cursor", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Page{}, classifySQLiteError(CodeQueryFailed, "finish event query", err)
	}
	return Page{SnapshotID: cursor.SnapshotID, Events: events, NextCursor: nextCursor}, nil
}

func encodeCursor(cursor pageCursor) (string, error) {
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeCursor(value string) (pageCursor, error) {
	if len(value) > 512 {
		return pageCursor{}, &Error{Code: CodeCursorInvalid, Err: errors.New("query cursor is too large")}
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return pageCursor{}, &Error{Code: CodeCursorInvalid, Err: errors.New("query cursor is not valid base64url")}
	}
	var cursor pageCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || ensureJSONEOF(decoder) != nil {
		return pageCursor{}, &Error{Code: CodeCursorInvalid, Err: errors.New("query cursor is not valid JSON")}
	}
	if cursor.Version != 1 || cursor.SnapshotID < 0 || cursor.LastID < 0 || cursor.LastID > cursor.SnapshotID {
		return pageCursor{}, &Error{Code: CodeCursorInvalid, Err: errors.New("query cursor values are invalid")}
	}
	return cursor, nil
}
