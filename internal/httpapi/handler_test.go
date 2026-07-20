package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Versifine/study-monitor/internal/collectors"
	"github.com/Versifine/study-monitor/internal/config"
	"github.com/Versifine/study-monitor/internal/eventstore"
	"github.com/Versifine/study-monitor/internal/logging"
	"github.com/Versifine/study-monitor/internal/mediaingest"
	"github.com/Versifine/study-monitor/internal/version"
)

func TestBatchEndpointReturnsOrderedAcceptedRejectedDuplicateAndConflict(t *testing.T) {
	handler, store, _ := newIntegratedHandler(t)
	defer store.Close()

	valid := testEventJSON("api-key", `{"window":"notes"}`)
	bad := `{"schema_version":1,"collector_id":"desktop","event_type":"study.activity","device_timestamp_raw":"missing-offset","clock_offset_ms":0,"clock_error_ms":1,"idempotency_key":"bad","payload":{}}`
	first := performJSON(handler, http.MethodPost, "/api/v1/events/batch", fmt.Sprintf(`{"schema_version":1,"events":[%s,%s]}`, valid, bad), nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	var firstResponse batchResponse
	decodeResponse(t, first, &firstResponse)
	if len(firstResponse.Results) != 2 || firstResponse.Results[0].Status != eventstore.StatusAccepted || firstResponse.Results[1].Status != eventstore.StatusRejected {
		t.Fatalf("first results = %#v", firstResponse.Results)
	}

	conflict := testEventJSON("api-key", `{"window":"different"}`)
	second := performJSON(handler, http.MethodPost, "/api/v1/events/batch", fmt.Sprintf(`{"schema_version":1,"events":[%s,%s]}`, valid, conflict), nil)
	var secondResponse batchResponse
	decodeResponse(t, second, &secondResponse)
	if second.Code != http.StatusOK || secondResponse.Results[0].Status != eventstore.StatusDuplicate || secondResponse.Results[1].Status != eventstore.StatusConflict {
		t.Fatalf("second status=%d results=%#v", second.Code, secondResponse.Results)
	}
	if secondResponse.Results[0].EventID != firstResponse.Results[0].EventID || secondResponse.Results[1].EventID != firstResponse.Results[0].EventID {
		t.Fatalf("original event id changed: first=%#v second=%#v", firstResponse.Results, secondResponse.Results)
	}
}

func TestBatchEndpointEnforcesVersionedLocalWriteContractAndLimits(t *testing.T) {
	handler, store, cfg := newIntegratedHandler(t)
	defer store.Close()
	valid := testEventJSON("policy-key", `{}`)

	tests := []struct {
		name    string
		body    string
		headers map[string]string
		status  int
		code    string
	}{
		{name: "browser origin", body: fmt.Sprintf(`{"schema_version":1,"events":[%s]}`, valid), headers: map[string]string{"Origin": "http://127.0.0.1:3000"}, status: http.StatusForbidden, code: CodeOriginForbidden},
		{name: "wrong media type", body: `{}`, headers: map[string]string{"Content-Type": "text/plain"}, status: http.StatusUnsupportedMediaType, code: CodeMediaTypeInvalid},
		{name: "unknown top-level field", body: fmt.Sprintf(`{"schema_version":1,"events":[%s],"extra":true}`, valid), status: http.StatusBadRequest, code: CodeJSONInvalid},
		{name: "unsupported API schema", body: fmt.Sprintf(`{"schema_version":2,"events":[%s]}`, valid), status: http.StatusBadRequest, code: CodeSchemaInvalid},
		{name: "empty batch", body: `{"schema_version":1,"events":[]}`, status: http.StatusBadRequest, code: eventstore.CodeBatchEmpty},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			headers := map[string]string{"Content-Type": "application/json"}
			for key, value := range test.headers {
				headers[key] = value
			}
			response := performJSON(handler, http.MethodPost, "/api/v1/events/batch", test.body, headers)
			if response.Code != test.status || responseErrorCode(t, response) != test.code {
				t.Fatalf("status=%d code=%q body=%s", response.Code, responseErrorCode(t, response), response.Body.String())
			}
		})
	}

	limitedConfig := cfg
	limitedConfig.API.MaxRequestBytes = 128
	limited := New(limitedConfig, testLogger(t), version.Info{Version: "test"}, store, StorageFailure{})
	largeBody := fmt.Sprintf(`{"schema_version":1,"events":[%s],"padding":"%s"}`, valid, strings.Repeat("x", 256))
	response := performJSON(limited, http.MethodPost, "/api/v1/events/batch", largeBody, nil)
	if response.Code != http.StatusRequestEntityTooLarge || responseErrorCode(t, response) != CodeBodyTooLarge {
		t.Fatalf("oversized response: status=%d body=%s", response.Code, response.Body.String())
	}

	method := performJSON(handler, http.MethodGet, "/api/v1/events/batch", "", nil)
	if method.Code != http.StatusMethodNotAllowed || method.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("method response: status=%d allow=%q", method.Code, method.Header().Get("Allow"))
	}
}

func TestBatchEndpointRejectsDuplicateEnvelopeKeyWithoutWriting(t *testing.T) {
	handler, store, _ := newIntegratedHandler(t)
	defer store.Close()
	first := testEventJSON("first", `{}`)
	second := testEventJSON("second", `{}`)
	body := fmt.Sprintf(`{"schema_version":1,"events":[%s],"events":[%s]}`, first, second)
	response := performJSON(handler, http.MethodPost, "/api/v1/events/batch", body, nil)
	if response.Code != http.StatusBadRequest || responseErrorCode(t, response) != CodeJSONInvalid {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	page, err := store.QueryPage(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 0 {
		t.Fatalf("duplicate envelope silently wrote events: %#v", page.Events)
	}
}

func TestBatchEndpointRejectsCaseVariantEnvelopeKeyWithoutWriting(t *testing.T) {
	handler, store, _ := newIntegratedHandler(t)
	defer store.Close()
	first := testEventJSON("first", `{}`)
	second := testEventJSON("second", `{}`)
	body := fmt.Sprintf(`{"schema_version":1,"events":[%s],"Events":[%s]}`, first, second)
	response := performJSON(handler, http.MethodPost, "/api/v1/events/batch", body, nil)
	if response.Code != http.StatusBadRequest || responseErrorCode(t, response) != CodeJSONInvalid {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	page, err := store.QueryPage(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 0 {
		t.Fatalf("case-variant envelope silently wrote events: %#v", page.Events)
	}
}

func TestBatchEndpointRejectsDuplicateEventFieldIndependently(t *testing.T) {
	handler, store, _ := newIntegratedHandler(t)
	defer store.Close()
	duplicate := strings.Replace(
		testEventJSON("duplicate", `{}`),
		`"idempotency_key":"duplicate"`,
		`"idempotency_key":"discarded","idempotency_key":"duplicate"`,
		1,
	)
	valid := testEventJSON("valid", `{"kept":true}`)
	body := fmt.Sprintf(`{"schema_version":1,"events":[%s,%s]}`, duplicate, valid)
	response := performJSON(handler, http.MethodPost, "/api/v1/events/batch", body, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result batchResponse
	decodeResponse(t, response, &result)
	if len(result.Results) != 2 || result.Results[0].Status != eventstore.StatusRejected ||
		result.Results[0].ErrorCode != eventstore.CodeEventDecodeInvalid || result.Results[1].Status != eventstore.StatusAccepted {
		t.Fatalf("results = %#v", result.Results)
	}
	page, err := store.QueryPage(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].IdempotencyKey != "valid" {
		t.Fatalf("stored events = %#v", page.Events)
	}
}

func TestBatchEndpointRejectsCaseVariantEventFieldIndependently(t *testing.T) {
	handler, store, _ := newIntegratedHandler(t)
	defer store.Close()
	caseVariant := strings.Replace(
		testEventJSON("case-variant", `{}`),
		`"idempotency_key":"case-variant"`,
		`"idempotency_key":"discarded","IDEMPOTENCY_KEY":"case-variant"`,
		1,
	)
	valid := testEventJSON("valid-after-case-variant", `{"kept":true}`)
	body := fmt.Sprintf(`{"schema_version":1,"events":[%s,%s]}`, caseVariant, valid)
	response := performJSON(handler, http.MethodPost, "/api/v1/events/batch", body, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result batchResponse
	decodeResponse(t, response, &result)
	if len(result.Results) != 2 || result.Results[0].Status != eventstore.StatusRejected ||
		result.Results[0].ErrorCode != eventstore.CodeEventDecodeInvalid || result.Results[1].Status != eventstore.StatusAccepted {
		t.Fatalf("results = %#v", result.Results)
	}
	page, err := store.QueryPage(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].IdempotencyKey != "valid-after-case-variant" {
		t.Fatalf("stored events = %#v", page.Events)
	}
}

func TestQueryEndpointUsesStableSnapshotCursor(t *testing.T) {
	handler, store, _ := newIntegratedHandler(t)
	defer store.Close()
	for index := 1; index <= 3; index++ {
		appendEvent(t, store, fmt.Sprintf("query-%d", index))
	}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/events?limit=2", nil))
	var firstPage queryResponse
	decodeResponse(t, first, &firstPage)
	if first.Code != http.StatusOK || len(firstPage.Events) != 2 || firstPage.NextCursor == "" || firstPage.SnapshotID != 3 {
		t.Fatalf("first page status=%d response=%#v", first.Code, firstPage)
	}
	appendEvent(t, store, "query-4")

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/v1/events?limit=2&cursor="+url.QueryEscape(firstPage.NextCursor), nil))
	var secondPage queryResponse
	decodeResponse(t, second, &secondPage)
	if second.Code != http.StatusOK || len(secondPage.Events) != 1 || secondPage.Events[0].ID != 3 || secondPage.SnapshotID != 3 {
		t.Fatalf("second page status=%d response=%#v", second.Code, secondPage)
	}

	for _, target := range []string{"/api/v1/events?cursor=bad", "/api/v1/events?limit=9999", "/api/v1/events?unknown=1"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
}

func TestReadinessReportsWritableAndInitializationFailure(t *testing.T) {
	handler, store, cfg := newIntegratedHandler(t)
	defer store.Close()
	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("writable status=%d body=%s", ready.Code, ready.Body.String())
	}
	var readyBody map[string]any
	decodeResponse(t, ready, &readyBody)
	if readyBody["status"] != eventstore.ReadinessWritable || readyBody["schema_version"] != float64(eventstore.CurrentSchemaVersion) {
		t.Fatalf("writable readiness = %#v", readyBody)
	}

	failed := New(cfg, testLogger(t), version.Info{Version: "test"}, nil, StorageFailure{
		Status: eventstore.ReadinessMigrationFailed, SchemaVersion: 2, ErrorCode: eventstore.CodeMigrationUnsupported,
	})
	response := httptest.NewRecorder()
	failed.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed readiness status=%d body=%s", response.Code, response.Body.String())
	}
	var failedBody map[string]any
	decodeResponse(t, response, &failedBody)
	if failedBody["status"] != eventstore.ReadinessMigrationFailed || failedBody["error_code"] != eventstore.CodeMigrationUnsupported {
		t.Fatalf("failed readiness = %#v", failedBody)
	}

	live := httptest.NewRecorder()
	failed.ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if live.Code != http.StatusOK {
		t.Fatalf("liveness must survive storage failure: %d %s", live.Code, live.Body.String())
	}
}

func TestMediaStatusIsReadOnlyAndDoesNotChangeCoreReadiness(t *testing.T) {
	handler, store, cfg := newIntegratedHandler(t)
	defer store.Close()
	provider := &mediaStatusStub{status: mediaingest.Status{
		SchemaVersion:  1,
		Status:         mediaingest.ModuleUnavailable,
		ErrorCode:      mediaingest.CodeProbeUnavailable,
		FFprobeVersion: mediaingest.SupportedFFprobeVersion,
		Ingest:         eventstore.MediaIngestSummary{Pending: 2, Backlog: 2},
	}}
	handler = New(cfg, testLogger(t), version.Info{Version: "test"}, store, StorageFailure{}, provider)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/media/ingest/status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("media status=%d body=%s", response.Code, response.Body.String())
	}
	var status mediaingest.Status
	decodeResponse(t, response, &status)
	if status.Status != mediaingest.ModuleUnavailable || status.ErrorCode != mediaingest.CodeProbeUnavailable || status.Ingest.Pending != 2 {
		t.Fatalf("media response = %#v", status)
	}

	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("media failure changed core readiness: %d %s", ready.Code, ready.Body.String())
	}
	post := httptest.NewRecorder()
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/api/v1/media/ingest/status", nil))
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("media POST status=%d allow=%q", post.Code, post.Header().Get("Allow"))
	}
}

func TestWriteConcurrencyLimitFailsFast(t *testing.T) {
	cfg := testConfig(t)
	cfg.API.MaxConcurrentWrites = 1
	store := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	handler := New(cfg, testLogger(t), version.Info{Version: "test"}, store, StorageFailure{})
	body := fmt.Sprintf(`{"schema_version":1,"events":[%s]}`, testEventJSON("blocking", `{}`))

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- performJSON(handler, http.MethodPost, "/api/v1/events/batch", body, nil)
	}()
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first write did not enter store")
	}
	second := performJSON(handler, http.MethodPost, "/api/v1/events/batch", body, nil)
	if second.Code != http.StatusTooManyRequests || responseErrorCode(t, second) != CodeWriteLimit {
		t.Fatalf("second write status=%d body=%s", second.Code, second.Body.String())
	}
	close(store.release)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK {
			t.Fatalf("first write status=%d body=%s", first.Code, first.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first write did not finish")
	}
}

func TestProjectionConcurrencyLimitFailsFastAcrossTimelineAndCoverage(t *testing.T) {
	cfg := testConfig(t)
	store := &blockingStore{queryEntered: make(chan struct{}), queryRelease: make(chan struct{})}
	handler := New(cfg, testLogger(t), version.Info{Version: "test"}, store, StorageFailure{})
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/timeline?start=2026-07-20T00:00:00Z&end=2026-07-20T00:10:00Z", nil))
		firstDone <- response
	}()
	select {
	case <-store.queryEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first projection did not enter the store")
	}
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/v1/coverage?start=2026-07-20T00:00:00Z&end=2026-07-20T00:10:00Z", nil))
	if second.Code != http.StatusTooManyRequests || responseErrorCode(t, second) != CodeProjectionLimit {
		t.Fatalf("concurrent projection status=%d body=%s", second.Code, second.Body.String())
	}
	close(store.queryRelease)
	if first := <-firstDone; first.Code != http.StatusOK {
		t.Fatalf("first projection status=%d body=%s", first.Code, first.Body.String())
	}
}

func TestMinimumModeDisablesBothGenericEvidenceAliasesAndExternalHeartbeats(t *testing.T) {
	cfg := testConfig(t)
	cfg.Runtime.Mode = config.ModeMinimum
	handler := New(cfg, testLogger(t), version.Info{Version: "test"}, &blockingStore{}, StorageFailure{})
	for _, path := range []string{"/api/v1/events/batch", "/api/v1/evidence/batch", "/api/v1/collectors/heartbeats/batch"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"schema_version":1}`))
		request.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound || responseErrorCode(t, response) != CodeModuleDisabled {
			t.Fatalf("minimum endpoint %s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}

func TestM3EvidenceHeartbeatTimelineCoverageAndStatusEndpoints(t *testing.T) {
	handler, store, cfg := newM3IntegratedHandler(t)
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Second)
	eventTime := now.Add(-10 * time.Minute).Format(time.RFC3339Nano)
	event := fmt.Sprintf(`{"schema_version":1,"collector_id":"generic.one","event_type":"generic.evidence","device_timestamp_raw":%q,"clock_offset_ms":250,"clock_error_ms":5000,"idempotency_key":"m3-api-event","payload":{"source":"phone-json"}}`, eventTime)
	evidence := performJSON(handler, http.MethodPost, "/api/v1/evidence/batch", fmt.Sprintf(`{"schema_version":1,"events":[%s]}`, event), nil)
	if evidence.Code != http.StatusOK {
		t.Fatalf("evidence status=%d body=%s", evidence.Code, evidence.Body.String())
	}
	var evidenceResult batchResponse
	decodeResponse(t, evidence, &evidenceResult)
	if evidenceResult.Results[0].Status != eventstore.StatusAccepted {
		t.Fatalf("evidence results=%#v", evidenceResult.Results)
	}

	heartbeatStart := now.Add(-9 * time.Minute)
	heartbeatEnd := heartbeatStart.Add(time.Minute)
	heartbeat := fmt.Sprintf(`{"schema_version":1,"collector_id":"generic.one","state":"idle","device_start_raw":%q,"device_end_raw":%q,"clock_offset_ms":0,"clock_error_ms":25,"idempotency_key":"m3-api-heartbeat","quality_flags":[]}`, heartbeatStart.Format(time.RFC3339Nano), heartbeatEnd.Format(time.RFC3339Nano))
	body := fmt.Sprintf(`{"schema_version":1,"heartbeats":[%s]}`, heartbeat)
	accepted := performJSON(handler, http.MethodPost, "/api/v1/collectors/heartbeats/batch", body, nil)
	duplicate := performJSON(handler, http.MethodPost, "/api/v1/collectors/heartbeats/batch", body, nil)
	var acceptedBody, duplicateBody struct {
		Results []eventstore.HeartbeatWriteResult `json:"results"`
	}
	decodeResponse(t, accepted, &acceptedBody)
	decodeResponse(t, duplicate, &duplicateBody)
	if accepted.Code != http.StatusOK || duplicate.Code != http.StatusOK || acceptedBody.Results[0].Status != eventstore.StatusAccepted || duplicateBody.Results[0].Status != eventstore.StatusDuplicate || duplicateBody.Results[0].HeartbeatID != acceptedBody.Results[0].HeartbeatID {
		t.Fatalf("heartbeat accepted=%#v duplicate=%#v", acceptedBody, duplicateBody)
	}

	start := url.QueryEscape(now.Add(-15 * time.Minute).Format(time.RFC3339Nano))
	end := url.QueryEscape(now.Add(time.Minute).Format(time.RFC3339Nano))
	timelineResponse := httptest.NewRecorder()
	handler.ServeHTTP(timelineResponse, httptest.NewRequest(http.MethodGet, "/api/v1/timeline?start="+start+"&end="+end+"&limit=10", nil))
	var timeline struct {
		SchemaVersion int                        `json:"schema_version"`
		Entries       []eventstore.TimelineEntry `json:"entries"`
	}
	decodeResponse(t, timelineResponse, &timeline)
	if timelineResponse.Code != http.StatusOK || timeline.SchemaVersion != 1 || len(timeline.Entries) != 2 {
		t.Fatalf("timeline status=%d body=%s", timelineResponse.Code, timelineResponse.Body.String())
	}
	if timeline.Entries[0].CorrectedStartUTC == "" || timeline.Entries[0].ReceivedAtUTC == "" || timeline.Entries[0].DeviceStartRaw == "" || timeline.Entries[0].DeviceStartUTC == "" || timeline.Entries[0].StableID == "" {
		t.Fatalf("timeline omits clock evidence or stable identity: %#v", timeline.Entries[0])
	}
	if !timeline.Entries[0].ClockUncertain {
		t.Fatalf("large clock error was not explicit: %#v", timeline.Entries[0])
	}

	coverageResponse := httptest.NewRecorder()
	handler.ServeHTTP(coverageResponse, httptest.NewRequest(http.MethodGet, "/api/v1/coverage?start="+start+"&end="+end+"&collector_id=generic.one", nil))
	var coverage struct {
		SchemaVersion int                                   `json:"schema_version"`
		Projections   []eventstore.CoverageProjectionStatus `json:"projections"`
		Intervals     []eventstore.CoverageInterval         `json:"intervals"`
	}
	decodeResponse(t, coverageResponse, &coverage)
	if coverageResponse.Code != http.StatusOK || len(coverage.Projections) != 1 || coverage.Projections[0].Status != "fresh" || len(coverage.Intervals) == 0 {
		t.Fatalf("coverage status=%d body=%s", coverageResponse.Code, coverageResponse.Body.String())
	}
	for index := 1; index < len(coverage.Intervals); index++ {
		if coverage.Intervals[index].StartUTC < coverage.Intervals[index-1].EndUTC {
			t.Fatalf("coverage intervals overlap: %#v", coverage.Intervals)
		}
	}

	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/api/v1/collectors/status", nil))
	var statuses struct {
		Collectors []collectors.Status `json:"collectors"`
	}
	decodeResponse(t, statusResponse, &statuses)
	if statusResponse.Code != http.StatusOK || len(statuses.Collectors) != 1 || statuses.Collectors[0].CollectorID != "generic.one" {
		t.Fatalf("collector status=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}

	badRange := httptest.NewRecorder()
	handler.ServeHTTP(badRange, httptest.NewRequest(http.MethodGet, "/api/v1/timeline?start=2026-01-01T00:00:00-00:00&end=2026-01-01T01:00:00Z", nil))
	if badRange.Code != http.StatusBadRequest || responseErrorCode(t, badRange) != CodeQueryInvalid {
		t.Fatalf("unknown-offset range status=%d body=%s", badRange.Code, badRange.Body.String())
	}
	_ = cfg
}

type batchResponse struct {
	SchemaVersion int                      `json:"schema_version"`
	Results       []eventstore.WriteResult `json:"results"`
}

type queryResponse struct {
	SchemaVersion int                `json:"schema_version"`
	SnapshotID    int64              `json:"snapshot_id"`
	Events        []eventstore.Event `json:"events"`
	NextCursor    string             `json:"next_cursor"`
}

type blockingStore struct {
	entered      chan struct{}
	release      chan struct{}
	queryEntered chan struct{}
	queryRelease chan struct{}
}

type mediaStatusStub struct {
	status mediaingest.Status
}

type collectorStatusStub struct{ statuses []collectors.Status }

func (stub collectorStatusStub) Status(context.Context) []collectors.Status { return stub.statuses }

func (stub *mediaStatusStub) Status(context.Context) mediaingest.Status { return stub.status }

func (store *blockingStore) AppendBatch(_ context.Context, _ []eventstore.Candidate) ([]eventstore.WriteResult, error) {
	close(store.entered)
	<-store.release
	return []eventstore.WriteResult{{Index: 0, Status: eventstore.StatusAccepted, EventID: 1}}, nil
}

func (*blockingStore) AppendHeartbeatBatch(context.Context, []eventstore.HeartbeatCandidate) ([]eventstore.HeartbeatWriteResult, error) {
	return nil, nil
}

func (store *blockingStore) QueryTimeline(context.Context, time.Time, time.Time, string, int, time.Duration, int) (eventstore.TimelinePage, error) {
	if store.queryEntered != nil {
		close(store.queryEntered)
		<-store.queryRelease
	}
	return eventstore.TimelinePage{}, nil
}

func (*blockingStore) RebuildCoverage(context.Context, []config.CollectorConfig, time.Time, time.Time, time.Time, time.Duration, int) (eventstore.CoverageResult, error) {
	return eventstore.CoverageResult{}, nil
}

func (*blockingStore) QueryPage(context.Context, string, int) (eventstore.Page, error) {
	return eventstore.Page{}, nil
}

func (*blockingStore) Readiness(context.Context) eventstore.Readiness {
	return eventstore.Readiness{Status: eventstore.ReadinessWritable, SchemaVersion: eventstore.CurrentSchemaVersion}
}

func newIntegratedHandler(t *testing.T) (*Handler, *eventstore.Store, config.Config) {
	t.Helper()
	cfg := testConfig(t)
	store, err := eventstore.Open(context.Background(), cfg.DatabasePath(), eventstore.Options{
		BusyTimeout: cfg.BusyTimeout(), MaxOpenConnections: cfg.Storage.MaxOpenConnections,
		MaxBatchEvents: cfg.API.MaxBatchEvents, MaxEventBytes: cfg.API.MaxEventBytes,
		MaxPayloadDepth: cfg.API.MaxPayloadDepth, MaxPageSize: cfg.API.MaxPageSize,
		Now: func() time.Time { return time.Date(2026, 7, 18, 3, 4, 5, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, testLogger(t), version.Info{Version: "0.2.0-test"}, store, StorageFailure{}), store, cfg
}

func newM3IntegratedHandler(t *testing.T) (*Handler, *eventstore.Store, config.Config) {
	t.Helper()
	cfg := testConfig(t)
	cfg.Collectors = []config.CollectorConfig{{
		ID: "generic.one", Kind: config.CollectorGenericJSON, Enabled: true,
		HeartbeatPeriod: "1m", AllowedLateness: "1m", OfflineAfter: "5m",
		PlannedSchedule: config.PlannedScheduleConfig{Timezone: "UTC", Windows: []config.ScheduleWindowConfig{{Days: []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}, StartLocal: "00:00", EndLocal: "24:00"}}},
	}}
	store, err := eventstore.Open(context.Background(), cfg.DatabasePath(), eventstore.Options{
		BusyTimeout: cfg.BusyTimeout(), MaxOpenConnections: cfg.Storage.MaxOpenConnections,
		MaxBatchEvents: cfg.API.MaxBatchEvents, MaxEventBytes: cfg.API.MaxEventBytes,
		MaxPayloadDepth: cfg.API.MaxPayloadDepth, MaxPageSize: cfg.API.MaxPageSize,
		CollectorPolicies: map[string]eventstore.CollectorPolicy{"generic.one": {Kind: config.CollectorGenericJSON, HeartbeatPeriod: time.Minute}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status := collectorStatusStub{statuses: []collectors.Status{{CollectorID: "generic.one", Kind: config.CollectorGenericJSON, Status: collectors.StatusHealthy}}}
	return New(cfg, testLogger(t), version.Info{Version: "0.4.0-test"}, store, StorageFailure{}, status), store, cfg
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	cfg, err := config.Load("", func(key string) (string, bool) {
		if key == "LOCALAPPDATA" {
			return root, true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func testLogger(t *testing.T) *logging.Logger {
	t.Helper()
	logger, err := logging.New(&bytes.Buffer{}, "info", "test")
	if err != nil {
		t.Fatal(err)
	}
	return logger
}

func testEventJSON(key, payload string) string {
	return fmt.Sprintf(`{"schema_version":1,"collector_id":"desktop","event_type":"study.activity","device_timestamp_raw":"2026-07-18T10:00:00+08:00","clock_offset_ms":0,"clock_error_ms":25,"idempotency_key":%q,"payload":%s}`, key, payload)
}

func appendEvent(t *testing.T, store *eventstore.Store, key string) {
	t.Helper()
	results, err := store.AppendBatch(context.Background(), []eventstore.Candidate{{Raw: json.RawMessage(testEventJSON(key, `{}`))}})
	if err != nil || results[0].Status != eventstore.StatusAccepted {
		t.Fatalf("AppendBatch(%q) = %#v, %v", key, results, err)
	}
}

func performJSON(handler http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, response.Body.String())
	}
}

func responseErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeResponse(t, response, &body)
	return body.Error.Code
}
