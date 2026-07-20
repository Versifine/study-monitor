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
	"strings"
	"time"

	"github.com/Versifine/study-monitor/internal/collectors"
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

	CodeMethodNotAllowed = "API_METHOD_NOT_ALLOWED"
	CodeMediaTypeInvalid = "API_MEDIA_TYPE_INVALID"
	CodeOriginForbidden  = "API_ORIGIN_FORBIDDEN"
	CodeBodyTooLarge     = "API_BODY_TOO_LARGE"
	CodeJSONInvalid      = "API_JSON_INVALID"
	CodeSchemaInvalid    = "API_SCHEMA_UNSUPPORTED"
	CodeQueryInvalid     = "API_QUERY_INVALID"
	CodeWriteLimit       = "API_WRITE_CONCURRENCY_LIMIT"
	CodeProjectionLimit  = "API_PROJECTION_CONCURRENCY_LIMIT"
	CodeModuleDisabled   = "API_MODULE_DISABLED"
	CodeStorageOffline   = "API_STORAGE_UNAVAILABLE"
)

var eventBatchEnvelopeJSONFields = []string{
	"schema_version",
	"events",
}

var heartbeatBatchEnvelopeJSONFields = []string{"schema_version", "heartbeats"}

type Store interface {
	AppendBatch(context.Context, []eventstore.Candidate) ([]eventstore.WriteResult, error)
	AppendHeartbeatBatch(context.Context, []eventstore.HeartbeatCandidate) ([]eventstore.HeartbeatWriteResult, error)
	QueryPage(context.Context, string, int) (eventstore.Page, error)
	QueryTimeline(context.Context, time.Time, time.Time, string, int, time.Duration, int) (eventstore.TimelinePage, error)
	RebuildCoverage(context.Context, []config.CollectorConfig, time.Time, time.Time, time.Time, time.Duration, int) (eventstore.CoverageResult, error)
	Readiness(context.Context) eventstore.Readiness
}

type MediaStatusProvider interface {
	Status(context.Context) mediaingest.Status
}

type CollectorStatusProvider interface {
	Status(context.Context) []collectors.Status
}

type StorageFailure struct {
	Status        string
	SchemaVersion int
	ErrorCode     string
}

type Handler struct {
	config          config.Config
	logger          *logging.Logger
	build           version.Info
	store           Store
	storageFailure  StorageFailure
	mediaStatus     MediaStatusProvider
	collectorStatus CollectorStatusProvider
	writes          chan struct{}
	projections     chan struct{}
	mux             *http.ServeMux
}

func New(cfg config.Config, logger *logging.Logger, build version.Info, store Store, failure StorageFailure, providers ...any) *Handler {
	var mediaStatus MediaStatusProvider = mediaingest.NewFixedStatusProvider(mediaingest.ModuleDisabled, "")
	var collectorStatus CollectorStatusProvider = fixedCollectorStatus{}
	for _, provider := range providers {
		if typed, ok := provider.(MediaStatusProvider); ok && typed != nil {
			mediaStatus = typed
		}
		if typed, ok := provider.(CollectorStatusProvider); ok && typed != nil {
			collectorStatus = typed
		}
	}
	handler := &Handler{
		config:          cfg,
		logger:          logger,
		build:           build,
		store:           store,
		storageFailure:  failure,
		mediaStatus:     mediaStatus,
		collectorStatus: collectorStatus,
		writes:          make(chan struct{}, cfg.API.MaxConcurrentWrites),
		projections:     make(chan struct{}, 1),
		mux:             http.NewServeMux(),
	}
	handler.mux.HandleFunc("/health/live", handler.handleLiveness)
	handler.mux.HandleFunc("/health/ready", handler.handleReadiness)
	handler.mux.HandleFunc("/api/v1/events/batch", handler.handleEventBatch)
	handler.mux.HandleFunc("/api/v1/evidence/batch", handler.handleEventBatch)
	handler.mux.HandleFunc("/api/v1/events", handler.handleEventQuery)
	handler.mux.HandleFunc("/api/v1/collectors/heartbeats/batch", handler.handleHeartbeatBatch)
	handler.mux.HandleFunc("/api/v1/collectors/status", handler.handleCollectorStatus)
	handler.mux.HandleFunc("/api/v1/timeline", handler.handleTimeline)
	handler.mux.HandleFunc("/api/v1/coverage", handler.handleCoverage)
	handler.mux.HandleFunc("/api/v1/media/ingest/status", handler.handleMediaIngestStatus)
	return handler
}

type fixedCollectorStatus struct{}

func (fixedCollectorStatus) Status(context.Context) []collectors.Status { return []collectors.Status{} }

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
	}{Status: "ok", Service: serviceName, Version: handler.build.Version, Mode: handler.config.Runtime.Mode})
}

func (handler *Handler) handleCollectorStatus(writer http.ResponseWriter, request *http.Request) {
	if !allowReadMethod(writer, request) {
		return
	}
	writeJSON(writer, request, http.StatusOK, struct {
		SchemaVersion int                 `json:"schema_version"`
		Collectors    []collectors.Status `json:"collectors"`
	}{APISchemaVersion, handler.collectorStatus.Status(request.Context())})
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
	if handler.config.Runtime.Mode == config.ModeMinimum {
		writeError(writer, request, http.StatusNotFound, CodeModuleDisabled, "generic Evidence input is disabled in minimum mode")
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

func (handler *Handler) handleHeartbeatBatch(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeMethodError(writer, request, http.MethodPost)
		return
	}
	if handler.config.Runtime.Mode == config.ModeMinimum {
		writeError(writer, request, http.StatusNotFound, CodeModuleDisabled, "external heartbeat input is disabled in minimum mode")
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
	if err := strictjson.ValidateExactRootObject(rawBody, 1, heartbeatBatchEnvelopeJSONFields...); err != nil {
		writeError(writer, request, http.StatusBadRequest, CodeJSONInvalid, "request JSON is invalid")
		return
	}
	var envelope struct {
		SchemaVersion int               `json:"schema_version"`
		Heartbeats    []json.RawMessage `json:"heartbeats"`
	}
	decoder := json.NewDecoder(bytes.NewReader(rawBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || ensureJSONEOF(decoder) != nil {
		writeError(writer, request, http.StatusBadRequest, CodeJSONInvalid, "request JSON is invalid")
		return
	}
	if envelope.SchemaVersion != APISchemaVersion {
		writeError(writer, request, http.StatusBadRequest, CodeSchemaInvalid, "API schema version is unsupported")
		return
	}
	if len(envelope.Heartbeats) == 0 || len(envelope.Heartbeats) > handler.config.API.MaxBatchEvents {
		code := eventstore.CodeBatchEmpty
		if len(envelope.Heartbeats) > handler.config.API.MaxBatchEvents {
			code = eventstore.CodeBatchTooLarge
		}
		writeError(writer, request, http.StatusBadRequest, code, "heartbeat batch size is invalid")
		return
	}
	candidates := make([]eventstore.HeartbeatCandidate, len(envelope.Heartbeats))
	for index := range envelope.Heartbeats {
		candidates[index] = eventstore.HeartbeatCandidate{Raw: envelope.Heartbeats[index]}
	}
	results, err := handler.store.AppendHeartbeatBatch(request.Context(), candidates)
	if err != nil {
		handler.writeStoreError(writer, request, "heartbeat_batch_failed", err)
		return
	}
	writeJSON(writer, request, http.StatusOK, struct {
		SchemaVersion int                               `json:"schema_version"`
		Results       []eventstore.HeartbeatWriteResult `json:"results"`
	}{APISchemaVersion, results})
}

func (handler *Handler) handleTimeline(writer http.ResponseWriter, request *http.Request) {
	if !allowReadMethod(writer, request) {
		return
	}
	if handler.store == nil {
		writeError(writer, request, http.StatusServiceUnavailable, CodeStorageOffline, "event storage is unavailable")
		return
	}
	start, end, limit, cursor, ok := handler.parseRangeQuery(writer, request, true)
	if !ok {
		return
	}
	if cursor == "" {
		if !handler.acquireProjection(writer, request) {
			return
		}
		defer handler.releaseProjection()
	}
	page, err := handler.store.QueryTimeline(request.Context(), start, end, cursor, limit, handler.config.ClockUncertainAfter(), handler.config.Timeline.MaxProjectionFacts)
	if err != nil {
		code := eventstore.ErrorCode(err)
		if code == eventstore.CodeTimelineRangeInvalid || code == eventstore.CodeTimelineCursor || code == eventstore.CodePageLimitInvalid || code == eventstore.CodeTimelineFactLimit || code == eventstore.CodeTimelineByteLimit {
			writeError(writer, request, http.StatusBadRequest, code, "timeline query is invalid")
			return
		}
		handler.writeStoreError(writer, request, "timeline_query_failed", err)
		return
	}
	writeJSON(writer, request, http.StatusOK, struct {
		SchemaVersion int `json:"schema_version"`
		eventstore.TimelinePage
	}{APISchemaVersion, page})
}

func (handler *Handler) handleCoverage(writer http.ResponseWriter, request *http.Request) {
	if !allowReadMethod(writer, request) {
		return
	}
	if handler.store == nil {
		writeError(writer, request, http.StatusServiceUnavailable, CodeStorageOffline, "event storage is unavailable")
		return
	}
	start, end, _, _, ok := handler.parseRangeQuery(writer, request, false)
	if !ok {
		return
	}
	collectorsConfig := handler.config.Collectors
	if collectorID := request.URL.Query().Get("collector_id"); collectorID != "" {
		collectorsConfig = nil
		for _, collector := range handler.config.Collectors {
			if collector.Enabled && collector.ID == collectorID {
				collectorsConfig = append(collectorsConfig, collector)
			}
		}
		if len(collectorsConfig) == 0 {
			writeError(writer, request, http.StatusBadRequest, CodeQueryInvalid, "collector_id is not an enabled collector")
			return
		}
	}
	if !handler.acquireProjection(writer, request) {
		return
	}
	defer handler.releaseProjection()
	result, err := handler.store.RebuildCoverage(request.Context(), collectorsConfig, start, end, time.Now().UTC(), handler.config.ClockUncertainAfter(), handler.config.Timeline.MaxProjectionFacts)
	if err != nil {
		code := eventstore.ErrorCode(err)
		if code == eventstore.CodeCoverageRangeInvalid {
			writeError(writer, request, http.StatusBadRequest, code, "coverage query is invalid")
			return
		}
		handler.writeStoreError(writer, request, "coverage_query_failed", err)
		return
	}
	writeJSON(writer, request, http.StatusOK, struct {
		SchemaVersion int `json:"schema_version"`
		eventstore.CoverageResult
	}{APISchemaVersion, result})
}

func (handler *Handler) parseRangeQuery(writer http.ResponseWriter, request *http.Request, allowCursor bool) (time.Time, time.Time, int, string, bool) {
	allowed := map[string]bool{"start": true, "end": true, "limit": allowCursor, "cursor": allowCursor, "collector_id": !allowCursor}
	for key, values := range request.URL.Query() {
		if !allowed[key] || len(values) != 1 {
			writeError(writer, request, http.StatusBadRequest, CodeQueryInvalid, "query parameters are invalid")
			return time.Time{}, time.Time{}, 0, "", false
		}
	}
	startText, endText := request.URL.Query().Get("start"), request.URL.Query().Get("end")
	if strings.HasSuffix(startText, "-00:00") || strings.HasSuffix(endText, "-00:00") {
		writeError(writer, request, http.StatusBadRequest, CodeQueryInvalid, "range timestamps require known RFC3339 offsets")
		return time.Time{}, time.Time{}, 0, "", false
	}
	start, startErr := time.Parse(time.RFC3339Nano, startText)
	end, endErr := time.Parse(time.RFC3339Nano, endText)
	if startErr != nil || endErr != nil || !start.Before(end) || end.Sub(start) > handler.config.TimelineMaxQueryRange() {
		writeError(writer, request, http.StatusBadRequest, CodeQueryInvalid, "range is invalid or exceeds the configured maximum")
		return time.Time{}, time.Time{}, 0, "", false
	}
	limit := handler.config.API.DefaultPageSize
	if text := request.URL.Query().Get("limit"); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 || parsed > handler.config.API.MaxPageSize {
			writeError(writer, request, http.StatusBadRequest, eventstore.CodePageLimitInvalid, "page limit is invalid")
			return time.Time{}, time.Time{}, 0, "", false
		}
		limit = parsed
	}
	return start, end, limit, request.URL.Query().Get("cursor"), true
}

func (handler *Handler) writeStoreError(writer http.ResponseWriter, request *http.Request, event string, err error) {
	code := eventstore.ErrorCode(err)
	status := http.StatusInternalServerError
	if code == eventstore.CodeBusy || code == eventstore.CodeReadOnly || code == eventstore.CodeCanceled || code == eventstore.CodeWriteFailed || code == eventstore.CodeQueryFailed || code == eventstore.CodeTimelineSyncBusy {
		status = http.StatusServiceUnavailable
	}
	handler.logger.Error("http_api", event, code, "event storage operation failed", err,
		slog.String("method", request.Method), slog.String("path", request.URL.Path))
	writeError(writer, request, status, code, "event storage operation failed")
}

func (handler *Handler) acquireProjection(writer http.ResponseWriter, request *http.Request) bool {
	select {
	case handler.projections <- struct{}{}:
		return true
	default:
		writeError(writer, request, http.StatusTooManyRequests, CodeProjectionLimit, "timeline or coverage projection is already running")
		return false
	}
}

func (handler *Handler) releaseProjection() { <-handler.projections }

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
