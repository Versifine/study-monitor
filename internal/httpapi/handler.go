package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"

	"github.com/Versifine/study-monitor/internal/config"
	"github.com/Versifine/study-monitor/internal/eventstore"
	"github.com/Versifine/study-monitor/internal/logging"
	"github.com/Versifine/study-monitor/internal/mediaingest"
	"github.com/Versifine/study-monitor/internal/strictjson"
	"github.com/Versifine/study-monitor/internal/version"
)

const (
	APISchemaVersion = 1
	serviceName      = "exam-monitor"
	runtimeMode      = "record-only"

	CodeMethodNotAllowed = "API_METHOD_NOT_ALLOWED"
	CodeMediaTypeInvalid = "API_MEDIA_TYPE_INVALID"
	CodeOriginForbidden  = "API_ORIGIN_FORBIDDEN"
	CodeBodyTooLarge     = "API_BODY_TOO_LARGE"
	CodeJSONInvalid      = "API_JSON_INVALID"
	CodeSchemaInvalid    = "API_SCHEMA_UNSUPPORTED"
	CodeQueryInvalid     = "API_QUERY_INVALID"
	CodeWriteLimit       = "API_WRITE_CONCURRENCY_LIMIT"
	CodeStorageOffline   = "API_STORAGE_UNAVAILABLE"
)

var eventBatchEnvelopeJSONFields = []string{
	"schema_version",
	"events",
}

type Store interface {
	AppendBatch(context.Context, []eventstore.Candidate) ([]eventstore.WriteResult, error)
	QueryPage(context.Context, string, int) (eventstore.Page, error)
	Readiness(context.Context) eventstore.Readiness
}

type MediaStatusProvider interface {
	Status(context.Context) mediaingest.Status
}

type StorageFailure struct {
	Status        string
	SchemaVersion int
	ErrorCode     string
}

type Handler struct {
	config         config.Config
	logger         *logging.Logger
	build          version.Info
	store          Store
	storageFailure StorageFailure
	mediaStatus    MediaStatusProvider
	writes         chan struct{}
	mux            *http.ServeMux
}

func New(cfg config.Config, logger *logging.Logger, build version.Info, store Store, failure StorageFailure, mediaProviders ...MediaStatusProvider) *Handler {
	var mediaStatus MediaStatusProvider = mediaingest.NewFixedStatusProvider(mediaingest.ModuleDisabled, "")
	if len(mediaProviders) > 0 && mediaProviders[0] != nil {
		mediaStatus = mediaProviders[0]
	}
	handler := &Handler{
		config:         cfg,
		logger:         logger,
		build:          build,
		store:          store,
		storageFailure: failure,
		mediaStatus:    mediaStatus,
		writes:         make(chan struct{}, cfg.API.MaxConcurrentWrites),
		mux:            http.NewServeMux(),
	}
	handler.mux.HandleFunc("/health/live", handler.handleLiveness)
	handler.mux.HandleFunc("/health/ready", handler.handleReadiness)
	handler.mux.HandleFunc("/api/v1/events/batch", handler.handleEventBatch)
	handler.mux.HandleFunc("/api/v1/events", handler.handleEventQuery)
	handler.mux.HandleFunc("/api/v1/media/ingest/status", handler.handleMediaIngestStatus)
	return handler
}

func (handler *Handler) handleMediaIngestStatus(writer http.ResponseWriter, request *http.Request) {
	if !allowReadMethod(writer, request) {
		return
	}
	writeJSON(writer, request, http.StatusOK, handler.mediaStatus.Status(request.Context()))
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	handler.mux.ServeHTTP(writer, request)
}

func (handler *Handler) handleLiveness(writer http.ResponseWriter, request *http.Request) {
	if !allowReadMethod(writer, request) {
		return
	}
	writeJSON(writer, request, http.StatusOK, struct {
		Status  string `json:"status"`
		Service string `json:"service"`
		Version string `json:"version"`
		Mode    string `json:"mode"`
	}{Status: "ok", Service: serviceName, Version: handler.build.Version, Mode: runtimeMode})
}

func (handler *Handler) handleReadiness(writer http.ResponseWriter, request *http.Request) {
	if !allowReadMethod(writer, request) {
		return
	}
	readiness := eventstore.Readiness{
		Status:        handler.storageFailure.Status,
		SchemaVersion: handler.storageFailure.SchemaVersion,
		ErrorCode:     handler.storageFailure.ErrorCode,
	}
	if handler.store != nil {
		readiness = handler.store.Readiness(request.Context())
	}
	if readiness.Status == "" {
		readiness.Status = eventstore.ReadinessUnavailable
		readiness.ErrorCode = CodeStorageOffline
	}
	status := http.StatusOK
	if readiness.Status != eventstore.ReadinessWritable {
		status = http.StatusServiceUnavailable
	}
	writeJSON(writer, request, status, struct {
		Status        string `json:"status"`
		SchemaVersion int    `json:"schema_version"`
		ErrorCode     string `json:"error_code,omitempty"`
	}{Status: readiness.Status, SchemaVersion: readiness.SchemaVersion, ErrorCode: readiness.ErrorCode})
}

func (handler *Handler) handleEventBatch(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeMethodError(writer, request, http.MethodPost)
		return
	}
	if request.Header.Get("Origin") != "" {
		writeError(writer, request, http.StatusForbidden, CodeOriginForbidden, "browser-origin writes are not accepted")
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(writer, request, http.StatusUnsupportedMediaType, CodeMediaTypeInvalid, "Content-Type must be application/json")
		return
	}
	select {
	case handler.writes <- struct{}{}:
		defer func() { <-handler.writes }()
	default:
		writeError(writer, request, http.StatusTooManyRequests, CodeWriteLimit, "write concurrency limit reached")
		return
	}
	if handler.store == nil {
		writeError(writer, request, http.StatusServiceUnavailable, CodeStorageOffline, "event storage is unavailable")
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, handler.config.API.MaxRequestBytes)
	rawBody, err := io.ReadAll(request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(writer, request, http.StatusRequestEntityTooLarge, CodeBodyTooLarge, "request body exceeds configured limit")
			return
		}
		writeError(writer, request, http.StatusBadRequest, CodeJSONInvalid, "request JSON cannot be read")
		return
	}
	if err := strictjson.ValidateExactRootObject(rawBody, 1, eventBatchEnvelopeJSONFields...); err != nil {
		writeError(writer, request, http.StatusBadRequest, CodeJSONInvalid, "request JSON is invalid")
		return
	}
	var envelope struct {
		SchemaVersion int               `json:"schema_version"`
		Events        []json.RawMessage `json:"events"`
	}
	decoder := json.NewDecoder(bytes.NewReader(rawBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		writeError(writer, request, http.StatusBadRequest, CodeJSONInvalid, "request JSON is invalid")
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		writeError(writer, request, http.StatusBadRequest, CodeJSONInvalid, "request JSON contains trailing data")
		return
	}
	if envelope.SchemaVersion != APISchemaVersion {
		writeError(writer, request, http.StatusBadRequest, CodeSchemaInvalid, "API schema version is unsupported")
		return
	}
	if len(envelope.Events) == 0 || len(envelope.Events) > handler.config.API.MaxBatchEvents {
		code := eventstore.CodeBatchEmpty
		if len(envelope.Events) > handler.config.API.MaxBatchEvents {
			code = eventstore.CodeBatchTooLarge
		}
		writeError(writer, request, http.StatusBadRequest, code, "event batch size is invalid")
		return
	}
	candidates := make([]eventstore.Candidate, len(envelope.Events))
	for index := range envelope.Events {
		candidates[index] = eventstore.Candidate{Raw: envelope.Events[index]}
	}
	results, err := handler.store.AppendBatch(request.Context(), candidates)
	if err != nil {
		handler.writeStoreError(writer, request, "event_batch_failed", err)
		return
	}
	writeJSON(writer, request, http.StatusOK, struct {
		SchemaVersion int                      `json:"schema_version"`
		Results       []eventstore.WriteResult `json:"results"`
	}{SchemaVersion: APISchemaVersion, Results: results})
}

func (handler *Handler) handleEventQuery(writer http.ResponseWriter, request *http.Request) {
	if !allowReadMethod(writer, request) {
		return
	}
	if handler.store == nil {
		writeError(writer, request, http.StatusServiceUnavailable, CodeStorageOffline, "event storage is unavailable")
		return
	}
	query := request.URL.Query()
	for key, values := range query {
		if (key != "cursor" && key != "limit") || len(values) != 1 {
			writeError(writer, request, http.StatusBadRequest, CodeQueryInvalid, "query parameters are invalid")
			return
		}
	}
	limit := handler.config.API.DefaultPageSize
	if text := query.Get("limit"); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 || parsed > handler.config.API.MaxPageSize {
			writeError(writer, request, http.StatusBadRequest, eventstore.CodePageLimitInvalid, "page limit is invalid")
			return
		}
		limit = parsed
	}
	page, err := handler.store.QueryPage(request.Context(), query.Get("cursor"), limit)
	if err != nil {
		code := eventstore.ErrorCode(err)
		if code == eventstore.CodeCursorInvalid || code == eventstore.CodePageLimitInvalid {
			writeError(writer, request, http.StatusBadRequest, code, "event query is invalid")
			return
		}
		handler.writeStoreError(writer, request, "event_query_failed", err)
		return
	}
	writeJSON(writer, request, http.StatusOK, struct {
		SchemaVersion int                `json:"schema_version"`
		SnapshotID    int64              `json:"snapshot_id"`
		Events        []eventstore.Event `json:"events"`
		NextCursor    string             `json:"next_cursor,omitempty"`
	}{SchemaVersion: APISchemaVersion, SnapshotID: page.SnapshotID, Events: page.Events, NextCursor: page.NextCursor})
}

func (handler *Handler) writeStoreError(writer http.ResponseWriter, request *http.Request, event string, err error) {
	code := eventstore.ErrorCode(err)
	status := http.StatusInternalServerError
	if code == eventstore.CodeBusy || code == eventstore.CodeReadOnly || code == eventstore.CodeCanceled || code == eventstore.CodeWriteFailed || code == eventstore.CodeQueryFailed {
		status = http.StatusServiceUnavailable
	}
	handler.logger.Error("http_api", event, code, "event storage operation failed", err,
		slog.String("method", request.Method), slog.String("path", request.URL.Path))
	writeError(writer, request, status, code, "event storage operation failed")
}

func allowReadMethod(writer http.ResponseWriter, request *http.Request) bool {
	if request.Method == http.MethodGet || request.Method == http.MethodHead {
		return true
	}
	writeMethodError(writer, request, "GET, HEAD")
	return false
}

func writeMethodError(writer http.ResponseWriter, request *http.Request, allow string) {
	writer.Header().Set("Allow", allow)
	writeError(writer, request, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method is not allowed")
}

func writeError(writer http.ResponseWriter, request *http.Request, status int, code, message string) {
	writeJSON(writer, request, status, struct {
		SchemaVersion int `json:"schema_version"`
		Error         struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{SchemaVersion: APISchemaVersion, Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}})
}

func writeJSON(writer http.ResponseWriter, request *http.Request, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	if request.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(writer).Encode(value)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
