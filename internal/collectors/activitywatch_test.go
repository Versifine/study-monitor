package collectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Versifine/study-monitor/internal/config"
	"github.com/Versifine/study-monitor/internal/eventstore"
	"github.com/Versifine/study-monitor/internal/logging"
)

func TestActivityWatchGETOnlyCheckpointRestartAndRescanAreIdempotent(t *testing.T) {
	bucketFixture, err := os.ReadFile(filepath.Join("testdata", "activitywatch_bucket.json"))
	if err != nil {
		t.Fatal(err)
	}
	eventsFixture, err := os.ReadFile(filepath.Join("testdata", "activitywatch_events.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	methods := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		methods = append(methods, request.Method)
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/0/buckets/aw-watcher-afk_test-host" {
			_, _ = writer.Write(bucketFixture)
			return
		}
		if request.URL.Path == "/api/0/buckets/aw-watcher-afk_test-host/events" {
			if request.URL.Query().Get("start") == "" || request.URL.Query().Get("end") == "" || request.URL.Query().Get("limit") != "10" {
				t.Errorf("unexpected ActivityWatch query: %s", request.URL.RawQuery)
			}
			_, _ = writer.Write(eventsFixture)
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()

	collector := testActivityWatchCollector(server.URL, "aw.afk", "aw-watcher-afk_test-host")
	path := filepath.Join(t.TempDir(), "events.db")
	store := openCollectorStore(t, path, collector)
	logger := testCollectorLogger(t)
	cfg := testCollectorConfig(t, collector)
	manager := New(cfg, store, logger)
	manager.now = func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) }
	manager.PollOnce(context.Background())

	page, err := store.QueryPage(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[0].EventType != "activitywatch.event" || page.Events[0].CollectorID != "aw.afk" {
		t.Fatalf("imported events=%#v", page.Events)
	}
	checkpoint, exists, err := store.LoadActivityWatchCheckpoint(context.Background(), "aw.afk", "aw-watcher-afk_test-host")
	if err != nil || !exists || checkpoint.SourceEventID != 2 || checkpoint.SourceTimeUTC != "2026-07-20T00:05:00.000000000Z" {
		t.Fatalf("checkpoint=%#v exists=%v err=%v", checkpoint, exists, err)
	}
	manager.PollOnce(context.Background())
	page, err = store.QueryPage(context.Background(), "", 10)
	if err != nil || len(page.Events) != 2 {
		t.Fatalf("rescan duplicated facts: count=%d err=%v", len(page.Events), err)
	}
	store.Close()

	store = openCollectorStore(t, path, collector)
	defer store.Close()
	manager = New(cfg, store, logger)
	manager.now = func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) }
	manager.PollOnce(context.Background())
	page, err = store.QueryPage(context.Background(), "", 10)
	if err != nil || len(page.Events) != 2 {
		t.Fatalf("restart duplicated facts: count=%d err=%v", len(page.Events), err)
	}
	statuses := manager.Status(context.Background())
	if len(statuses) != 1 || statuses[0].Status != StatusHealthy || statuses[0].CheckpointEventID != 2 || statuses[0].Duplicates != 2 {
		t.Fatalf("status=%#v", statuses)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, method := range methods {
		if method != http.MethodGet {
			t.Fatalf("ActivityWatch adapter used mutating method %q", method)
		}
	}
}

func TestActivityWatchPollClosesPerPollIdleConnections(t *testing.T) {
	collector := testActivityWatchCollector("http://127.0.0.1:5600", "aw.connections", "connections")
	store := openCollectorStore(t, filepath.Join(t.TempDir(), "events.db"), collector)
	defer store.Close()
	transport := &closeTrackingTransport{}
	manager := New(testCollectorConfig(t, collector), store, testCollectorLogger(t))
	manager.client = func(time.Duration) *http.Client { return &http.Client{Transport: transport} }
	manager.now = func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) }
	for poll := 0; poll < 5; poll++ {
		manager.PollOnce(context.Background())
	}
	if got := atomic.LoadInt32(&transport.closes); got != 5 {
		t.Fatalf("per-poll HTTP transports closed %d times, want 5", got)
	}
}

type closeTrackingTransport struct {
	closes int32
}

func (transport *closeTrackingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body := `{"id":"connections","type":"currentwindow","client":"test","hostname":"host"}`
	if strings.HasSuffix(request.URL.Path, "/events") {
		body = `[]`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}, nil
}

func (transport *closeTrackingTransport) CloseIdleConnections() {
	atomic.AddInt32(&transport.closes, 1)
}

func TestActivityWatchPaginationDeduplicatesBoundaryAndAdvancesStableTuple(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/0/buckets/paged" {
			_, _ = writer.Write([]byte(`{"id":"paged","type":"currentwindow","client":"test","hostname":"host"}`))
			return
		}
		calls++
		switch calls {
		case 1:
			_, _ = writer.Write([]byte(`[{"id":3,"timestamp":"2026-07-20T00:08:00Z","duration":1,"data":{"app":"three"}},{"id":2,"timestamp":"2026-07-20T00:07:00Z","duration":1,"data":{"app":"two"}}]`))
		case 2:
			if got := request.URL.Query().Get("end"); got != "2026-07-20T00:07:00.000000000Z" {
				t.Errorf("second page end=%q", got)
			}
			_, _ = writer.Write([]byte(`[{"id":2,"timestamp":"2026-07-20T00:07:00Z","duration":1,"data":{"app":"two"}},{"id":1,"timestamp":"2026-07-20T00:06:00Z","duration":1,"data":{"app":"one"}}]`))
		default:
			_, _ = writer.Write([]byte(`[]`))
		}
	}))
	defer server.Close()
	collector := testActivityWatchCollector(server.URL, "aw.paged", "paged")
	collector.ActivityWatch.PageSize = 2
	store := openCollectorStore(t, filepath.Join(t.TempDir(), "events.db"), collector)
	defer store.Close()
	manager := New(testCollectorConfig(t, collector), store, testCollectorLogger(t))
	manager.now = func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) }
	manager.PollOnce(context.Background())
	page, err := store.QueryPage(context.Background(), "", 10)
	if err != nil || len(page.Events) != 3 || page.Events[0].DeviceTimestampRaw != "2026-07-20T00:06:00Z" || page.Events[1].DeviceTimestampRaw != "2026-07-20T00:07:00Z" || page.Events[2].DeviceTimestampRaw != "2026-07-20T00:08:00Z" {
		t.Fatalf("paged import order/dedup=%#v err=%v", page.Events, err)
	}
	checkpoint, exists, err := store.LoadActivityWatchCheckpoint(context.Background(), "aw.paged", "paged")
	if err != nil || !exists || checkpoint.SourceEventID != 3 || checkpoint.SourceTimeUTC != "2026-07-20T00:08:00.000000000Z" || calls != 3 {
		t.Fatalf("paged checkpoint=%#v exists=%v calls=%d err=%v", checkpoint, exists, calls, err)
	}
}

func TestActivityWatchFullPageLimitFailsWithoutFactsOrCheckpoint(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/0/buckets/backlog" {
			_, _ = writer.Write([]byte(`{"id":"backlog","type":"currentwindow","client":"test","hostname":"host"}`))
			return
		}
		calls++
		if calls == 1 {
			_, _ = writer.Write([]byte(`[{"id":4,"timestamp":"2026-07-20T00:08:00Z","duration":1,"data":{}},{"id":3,"timestamp":"2026-07-20T00:07:00Z","duration":1,"data":{}}]`))
			return
		}
		_, _ = writer.Write([]byte(`[{"id":3,"timestamp":"2026-07-20T00:07:00Z","duration":1,"data":{}},{"id":2,"timestamp":"2026-07-20T00:06:00Z","duration":1,"data":{}}]`))
	}))
	defer server.Close()
	collector := testActivityWatchCollector(server.URL, "aw.backlog", "backlog")
	collector.ActivityWatch.PageSize = 2
	collector.ActivityWatch.MaxPagesPerPoll = 2
	store := openCollectorStore(t, filepath.Join(t.TempDir(), "events.db"), collector)
	defer store.Close()
	manager := New(testCollectorConfig(t, collector), store, testCollectorLogger(t))
	manager.now = func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) }
	manager.PollOnce(context.Background())
	assertActivityWatchFailureWithoutFacts(t, store, manager, "aw.backlog", "backlog", CodeBacklogLimit)
}

func TestActivityWatchPaginationStallFailsWithoutFactsOrCheckpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/0/buckets/stalled" {
			_, _ = writer.Write([]byte(`{"id":"stalled","type":"currentwindow","client":"test","hostname":"host"}`))
			return
		}
		_, _ = writer.Write([]byte(`[{"id":2,"timestamp":"2026-07-20T00:07:00Z","duration":1,"data":{}},{"id":1,"timestamp":"2026-07-20T00:07:00Z","duration":1,"data":{}}]`))
	}))
	defer server.Close()
	collector := testActivityWatchCollector(server.URL, "aw.stalled", "stalled")
	collector.ActivityWatch.PageSize = 2
	store := openCollectorStore(t, filepath.Join(t.TempDir(), "events.db"), collector)
	defer store.Close()
	manager := New(testCollectorConfig(t, collector), store, testCollectorLogger(t))
	manager.now = func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) }
	manager.PollOnce(context.Background())
	assertActivityWatchFailureWithoutFacts(t, store, manager, "aw.stalled", "stalled", CodePaginationStalled)
}

func TestActivityWatchDefersEventWhoseEndCrossesLatenessCutoff(t *testing.T) {
	var mu sync.Mutex
	duration := 90.0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/0/buckets/mutable" {
			_, _ = writer.Write([]byte(`{"id":"mutable","type":"currentwindow","client":"test","hostname":"host"}`))
			return
		}
		mu.Lock()
		currentDuration := duration
		mu.Unlock()
		_, _ = fmt.Fprintf(writer, `[{"id":7,"timestamp":"2026-07-20T00:08:30Z","duration":%g,"data":{"app":"editor"}}]`, currentDuration)
	}))
	defer server.Close()

	collector := testActivityWatchCollector(server.URL, "aw.mutable", "mutable")
	store := openCollectorStore(t, filepath.Join(t.TempDir(), "events.db"), collector)
	defer store.Close()
	manager := New(testCollectorConfig(t, collector), store, testCollectorLogger(t))
	currentNow := time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC)
	manager.now = func() time.Time { return currentNow }

	manager.PollOnce(context.Background())
	page, err := store.QueryPage(context.Background(), "", 10)
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("event crossing lateness cutoff was imported: count=%d err=%v", len(page.Events), err)
	}
	if _, exists, err := store.LoadActivityWatchCheckpoint(context.Background(), "aw.mutable", "mutable"); err != nil || exists {
		t.Fatalf("deferred event advanced checkpoint: exists=%v err=%v", exists, err)
	}

	mu.Lock()
	duration = 120
	mu.Unlock()
	currentNow = time.Date(2026, 7, 20, 0, 12, 0, 0, time.UTC)
	manager.PollOnce(context.Background())
	manager.PollOnce(context.Background())
	page, err = store.QueryPage(context.Background(), "", 10)
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("stable event did not import idempotently: count=%d err=%v", len(page.Events), err)
	}
	checkpoint, exists, err := store.LoadActivityWatchCheckpoint(context.Background(), "aw.mutable", "mutable")
	if err != nil || !exists || checkpoint.SourceEventID != 7 {
		t.Fatalf("stable event checkpoint=%#v exists=%v err=%v", checkpoint, exists, err)
	}
}

func TestActivityWatchCheckpointNeverPassesEarlierDeferredEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/0/buckets/overlap" {
			_, _ = writer.Write([]byte(`{"id":"overlap","type":"currentwindow","client":"test","hostname":"host"}`))
			return
		}
		start, _ := time.Parse(time.RFC3339Nano, request.URL.Query().Get("start"))
		if start.After(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)) {
			_, _ = writer.Write([]byte(`[{"id":2,"timestamp":"2026-07-20T00:08:00Z","duration":30,"data":{"app":"short"}}]`))
			return
		}
		_, _ = writer.Write([]byte(`[{"id":2,"timestamp":"2026-07-20T00:08:00Z","duration":30,"data":{"app":"short"}},{"id":1,"timestamp":"2026-07-20T00:00:00Z","duration":600,"data":{"app":"long"}}]`))
	}))
	defer server.Close()
	collector := testActivityWatchCollector(server.URL, "aw.overlap", "overlap")
	collector.ActivityWatch.RescanWindow = "1m"
	store := openCollectorStore(t, filepath.Join(t.TempDir(), "events.db"), collector)
	defer store.Close()
	manager := New(testCollectorConfig(t, collector), store, testCollectorLogger(t))
	currentNow := time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC)
	manager.now = func() time.Time { return currentNow }
	manager.PollOnce(context.Background())
	page, err := store.QueryPage(context.Background(), "", 10)
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("checkpoint jumped past earlier deferred event: count=%d err=%v", len(page.Events), err)
	}
	if _, exists, err := store.LoadActivityWatchCheckpoint(context.Background(), "aw.overlap", "overlap"); err != nil || exists {
		t.Fatalf("deferred stable prefix advanced checkpoint: exists=%v err=%v", exists, err)
	}
	currentNow = time.Date(2026, 7, 20, 0, 11, 0, 0, time.UTC)
	manager.PollOnce(context.Background())
	page, err = store.QueryPage(context.Background(), "", 10)
	if err != nil || len(page.Events) != 2 {
		t.Fatalf("earlier deferred event was not recovered: count=%d err=%v", len(page.Events), err)
	}
}

func TestActivityWatchCrossPageMemoryBudgetFailsWithoutBlockingEvidenceWrites(t *testing.T) {
	var calls int
	blob := strings.Repeat("x", 700)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/0/buckets/budget" {
			_, _ = writer.Write([]byte(`{"id":"budget","type":"currentwindow","client":"test","hostname":"host"}`))
			return
		}
		calls++
		if calls == 1 {
			_, _ = fmt.Fprintf(writer, `[{"id":2,"timestamp":"2026-07-20T00:08:00Z","duration":1,"data":{"blob":%q}}]`, blob)
			return
		}
		_, _ = fmt.Fprintf(writer, `[{"id":1,"timestamp":"2026-07-20T00:07:00Z","duration":1,"data":{"blob":%q}}]`, blob)
	}))
	defer server.Close()
	collector := testActivityWatchCollector(server.URL, "aw.budget", "budget")
	collector.ActivityWatch.PageSize = 1
	store := openCollectorStore(t, filepath.Join(t.TempDir(), "events.db"), collector)
	defer store.Close()
	manager := New(testCollectorConfig(t, collector), store, testCollectorLogger(t))
	manager.pollByteBudget = 1500
	manager.now = func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) }
	manager.PollOnce(context.Background())
	assertActivityWatchFailureWithoutFacts(t, store, manager, "aw.budget", "budget", CodeBacklogLimit)
	raw := json.RawMessage(`{"schema_version":1,"collector_id":"aw.budget","event_type":"generic.evidence","device_timestamp_raw":"2026-07-20T00:04:00Z","clock_offset_ms":0,"clock_error_ms":0,"idempotency_key":"after-aw-budget","payload":{}}`)
	if results, err := store.AppendBatch(context.Background(), []eventstore.Candidate{{Raw: raw}}); err != nil || results[0].Status != eventstore.StatusAccepted {
		t.Fatalf("ActivityWatch budget failure blocked Evidence write=%#v err=%v", results, err)
	}
}

func TestActivityWatchWorkersUseGlobalPollConcurrencyBudget(t *testing.T) {
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	var current, peak int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(request.URL.Path, "/api/0/buckets/")
		if !strings.HasSuffix(path, "/events") {
			_, _ = fmt.Fprintf(writer, `{"id":%q,"type":"currentwindow","client":"test","hostname":"host"}`, path)
			return
		}
		active := atomic.AddInt32(&current, 1)
		for {
			observed := atomic.LoadInt32(&peak)
			if active <= observed || atomic.CompareAndSwapInt32(&peak, observed, active) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		atomic.AddInt32(&current, -1)
		_, _ = writer.Write([]byte(`[]`))
	}))
	defer server.Close()
	collectorsConfig := []config.CollectorConfig{
		testActivityWatchCollector(server.URL, "aw.one", "one"),
		testActivityWatchCollector(server.URL, "aw.two", "two"),
		testActivityWatchCollector(server.URL, "aw.three", "three"),
	}
	store := openCollectorStore(t, filepath.Join(t.TempDir(), "events.db"), collectorsConfig...)
	defer store.Close()
	manager := New(testCollectorConfig(t, collectorsConfig...), store, testCollectorLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { manager.Run(ctx); close(done) }()
	for index := 0; index < activityWatchPollConcurrency; index++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("ActivityWatch workers did not fill the bounded poll slots")
		}
	}
	select {
	case <-entered:
		t.Fatal("more ActivityWatch polls ran concurrently than the fixed budget")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ActivityWatch workers did not stop")
	}
	if got := atomic.LoadInt32(&peak); got > activityWatchPollConcurrency {
		t.Fatalf("peak concurrent ActivityWatch polls=%d", got)
	}
}

func TestSlowActivityWatchPollsCannotStarveHealthyCollectorPastWholePollBudget(t *testing.T) {
	slowEntered := make(chan struct{}, 2)
	transport := &executionBudgetTransport{slowEntered: slowEntered}
	slowOne := testActivityWatchCollector("http://127.0.0.1:5600", "aw.slow-one", "slow-one")
	slowTwo := testActivityWatchCollector("http://127.0.0.1:5600", "aw.slow-two", "slow-two")
	healthy := testActivityWatchCollector("http://127.0.0.1:5600", "aw.healthy", "healthy")
	store := openCollectorStore(t, filepath.Join(t.TempDir(), "events.db"), slowOne, slowTwo, healthy)
	defer store.Close()
	manager := New(testCollectorConfig(t, slowOne, slowTwo, healthy), store, testCollectorLogger(t))
	manager.client = func(time.Duration) *http.Client { return &http.Client{Transport: transport} }
	manager.pollExecutionLimit = func(config.CollectorConfig) time.Duration { return 100 * time.Millisecond }
	var workers sync.WaitGroup
	workers.Add(2)
	go func() { defer workers.Done(); manager.pollAndRecord(context.Background(), slowOne) }()
	go func() { defer workers.Done(); manager.pollAndRecord(context.Background(), slowTwo) }()
	for index := 0; index < 2; index++ {
		select {
		case <-slowEntered:
		case <-time.After(2 * time.Second):
			t.Fatal("slow ActivityWatch polls did not occupy both worker slots")
		}
	}
	healthyDone := make(chan struct{})
	go func() { manager.pollAndRecord(context.Background(), healthy); close(healthyDone) }()
	select {
	case <-healthyDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("healthy ActivityWatch collector was starved by slow workers")
	}
	workers.Wait()
	statuses := manager.Status(context.Background())
	found := false
	for _, status := range statuses {
		if status.CollectorID == healthy.ID {
			found = true
			if status.Status != StatusHealthy {
				t.Fatalf("healthy collector status=%#v", status)
			}
		}
	}
	if !found {
		t.Fatal("healthy collector status is missing")
	}
}

type executionBudgetTransport struct {
	slowEntered chan<- struct{}
}

func (transport *executionBudgetTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	path := strings.TrimPrefix(request.URL.Path, "/api/0/buckets/")
	if strings.HasSuffix(path, "/events") {
		if strings.HasPrefix(path, "slow-") {
			transport.slowEntered <- struct{}{}
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		return jsonHTTPResponse(request, `[]`), nil
	}
	bucketID := strings.TrimSuffix(path, "/")
	return jsonHTTPResponse(request, fmt.Sprintf(`{"id":%q,"type":"currentwindow","client":"test","hostname":"host"}`, bucketID)), nil
}

func (transport *executionBudgetTransport) CloseIdleConnections() {}

func jsonHTTPResponse(request *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func TestQueuedActivityWatchPollUsesExecutionStartForCutoffAndHeartbeat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/0/buckets/queued" {
			_, _ = writer.Write([]byte(`{"id":"queued","type":"currentwindow","client":"test","hostname":"host"}`))
			return
		}
		_, _ = writer.Write([]byte(`[]`))
	}))
	defer server.Close()
	collector := testActivityWatchCollector(server.URL, "aw.queued", "queued")
	store := openCollectorStore(t, filepath.Join(t.TempDir(), "events.db"), collector)
	defer store.Close()
	manager := New(testCollectorConfig(t, collector), store, testCollectorLogger(t))
	manager.polls <- struct{}{}
	manager.polls <- struct{}{}
	currentNow := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	var nowMu sync.Mutex
	var nowCalls int32
	manager.now = func() time.Time {
		atomic.AddInt32(&nowCalls, 1)
		nowMu.Lock()
		defer nowMu.Unlock()
		return currentNow
	}
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		manager.pollAndRecord(context.Background(), collector)
		close(done)
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	if calls := atomic.LoadInt32(&nowCalls); calls != 0 {
		t.Fatalf("queued poll captured time before acquiring a worker slot: calls=%d", calls)
	}
	nowMu.Lock()
	currentNow = time.Date(2026, 7, 20, 0, 1, 0, 0, time.UTC)
	nowMu.Unlock()
	<-manager.polls
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("queued ActivityWatch poll did not finish after a slot was released")
	}
	<-manager.polls
	timeline, err := store.QueryTimeline(context.Background(), time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 20, 0, 2, 0, 0, time.UTC), "", 10, time.Second, 100)
	if err != nil || len(timeline.Entries) != 1 || timeline.Entries[0].SourceType != "heartbeat" || timeline.Entries[0].DeviceStartRaw != "2026-07-20T00:01:00.000000000Z" {
		t.Fatalf("queued poll heartbeat used queue time: %#v err=%v", timeline, err)
	}
}

func TestActivityWatchFailureIsIsolatedPerCollector(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/0/buckets/good" {
			_, _ = writer.Write([]byte(`{"id":"good","type":"afkstatus","client":"test","hostname":"host"}`))
			return
		}
		_, _ = writer.Write([]byte(`[]`))
	}))
	defer server.Close()
	closedURL := closedLoopbackURL(t)
	good := testActivityWatchCollector(server.URL, "aw.good", "good")
	bad := testActivityWatchCollector(closedURL, "aw.bad", "bad")
	cfg := testCollectorConfig(t, good, bad)
	path := filepath.Join(t.TempDir(), "events.db")
	store := openCollectorStore(t, path, good, bad)
	defer store.Close()
	manager := New(cfg, store, testCollectorLogger(t))
	manager.now = func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) }
	manager.PollOnce(context.Background())
	statuses := manager.Status(context.Background())
	if len(statuses) != 2 || statuses[0].CollectorID != "aw.bad" || statuses[0].Status != StatusUnavailable || statuses[1].CollectorID != "aw.good" || statuses[1].Status != StatusHealthy {
		t.Fatalf("isolated statuses=%#v", statuses)
	}
	readiness := store.Readiness(context.Background())
	if readiness.Status != eventstore.ReadinessWritable {
		t.Fatalf("one offline collector changed core readiness: %#v", readiness)
	}
}

func TestActivityWatchInvalidResponseDoesNotAdvanceCheckpointOrWriteFacts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/0/buckets/bad-json" {
			_, _ = writer.Write([]byte(`{"id":"bad-json","type":"afkstatus","client":"test","hostname":"host"}`))
			return
		}
		_, _ = writer.Write([]byte(`[{"id":1,"timestamp":"2026-07-20T00:00:00Z","duration":1,"data":{"status":"afk","status":"not-afk"}}]`))
	}))
	defer server.Close()
	collector := testActivityWatchCollector(server.URL, "aw.invalid", "bad-json")
	store := openCollectorStore(t, filepath.Join(t.TempDir(), "events.db"), collector)
	defer store.Close()
	manager := New(testCollectorConfig(t, collector), store, testCollectorLogger(t))
	manager.now = func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) }
	manager.PollOnce(context.Background())
	page, err := store.QueryPage(context.Background(), "", 10)
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("invalid response wrote facts: count=%d err=%v", len(page.Events), err)
	}
	_, exists, err := store.LoadActivityWatchCheckpoint(context.Background(), "aw.invalid", "bad-json")
	if err != nil || exists {
		t.Fatalf("invalid response advanced checkpoint: exists=%v err=%v", exists, err)
	}
	statuses := manager.Status(context.Background())
	if len(statuses) != 1 || statuses[0].Status != StatusUnavailable || statuses[0].ErrorCode != CodeResponseInvalid {
		t.Fatalf("invalid response status=%#v", statuses)
	}
}

func TestActivityWatchInvalidBucketTypeDoesNotAdvanceCheckpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/0/buckets/bad-type" {
			_, _ = writer.Write([]byte(`{"id":"bad-type","type":" bad","client":"test","hostname":"host"}`))
			return
		}
		_, _ = writer.Write([]byte(`[]`))
	}))
	defer server.Close()
	collector := testActivityWatchCollector(server.URL, "aw.bad-type", "bad-type")
	store := openCollectorStore(t, filepath.Join(t.TempDir(), "events.db"), collector)
	defer store.Close()
	manager := New(testCollectorConfig(t, collector), store, testCollectorLogger(t))
	manager.now = func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) }
	manager.PollOnce(context.Background())
	assertActivityWatchFailureWithoutFacts(t, store, manager, "aw.bad-type", "bad-type", CodeResponseInvalid)
}

func TestActivityWatchDoesNotFollowRedirects(t *testing.T) {
	var redirected int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected++ }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer source.Close()
	collector := testActivityWatchCollector(source.URL, "aw.redirect", "redirect")
	store := openCollectorStore(t, filepath.Join(t.TempDir(), "events.db"), collector)
	defer store.Close()
	manager := New(testCollectorConfig(t, collector), store, testCollectorLogger(t))
	manager.now = func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) }
	manager.PollOnce(context.Background())
	if redirected != 0 {
		t.Fatalf("ActivityWatch adapter followed a redirect outside its configured origin %d times", redirected)
	}
	statuses := manager.Status(context.Background())
	if len(statuses) != 1 || statuses[0].Status != StatusUnavailable || statuses[0].ErrorCode != CodeRequestFailed {
		t.Fatalf("redirect status=%#v", statuses)
	}
}

func TestActivityWatchFactCommitBeforeCheckpointFailureReplaysSafely(t *testing.T) {
	bucketFixture, err := os.ReadFile(filepath.Join("testdata", "activitywatch_bucket.json"))
	if err != nil {
		t.Fatal(err)
	}
	eventsFixture, err := os.ReadFile(filepath.Join("testdata", "activitywatch_events.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/0/buckets/aw-watcher-afk_test-host" {
			_, _ = writer.Write(bucketFixture)
			return
		}
		_, _ = writer.Write(eventsFixture)
	}))
	defer server.Close()
	collector := testActivityWatchCollector(server.URL, "aw.afk", "aw-watcher-afk_test-host")
	path := filepath.Join(t.TempDir(), "events.db")
	realStore := openCollectorStore(t, path, collector)
	store := &checkpointFailureStore{Store: realStore, failNext: true}
	manager := New(testCollectorConfig(t, collector), store, testCollectorLogger(t))
	manager.now = func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) }
	manager.PollOnce(context.Background())
	page, err := realStore.QueryPage(context.Background(), "", 10)
	if err != nil || len(page.Events) != 2 {
		t.Fatalf("facts were not committed before injected checkpoint failure: count=%d err=%v", len(page.Events), err)
	}
	if _, exists, err := realStore.LoadActivityWatchCheckpoint(context.Background(), "aw.afk", "aw-watcher-afk_test-host"); err != nil || exists {
		t.Fatalf("failed checkpoint unexpectedly persisted: exists=%v err=%v", exists, err)
	}
	if err := realStore.Close(); err != nil {
		t.Fatal(err)
	}
	realStore = openCollectorStore(t, path, collector)
	defer realStore.Close()
	manager = New(testCollectorConfig(t, collector), realStore, testCollectorLogger(t))
	manager.now = func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) }
	manager.PollOnce(context.Background())
	page, err = realStore.QueryPage(context.Background(), "", 10)
	if err != nil || len(page.Events) != 2 {
		t.Fatalf("checkpoint retry duplicated facts: count=%d err=%v", len(page.Events), err)
	}
	checkpoint, exists, err := realStore.LoadActivityWatchCheckpoint(context.Background(), "aw.afk", "aw-watcher-afk_test-host")
	if err != nil || !exists || checkpoint.SourceEventID != 2 {
		t.Fatalf("checkpoint retry did not converge: %#v exists=%v err=%v", checkpoint, exists, err)
	}
}

func assertActivityWatchFailureWithoutFacts(t *testing.T, store *eventstore.Store, manager *Manager, collectorID, bucketID, code string) {
	t.Helper()
	page, err := store.QueryPage(context.Background(), "", 10)
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("failed pagination wrote facts: count=%d err=%v", len(page.Events), err)
	}
	if _, exists, err := store.LoadActivityWatchCheckpoint(context.Background(), collectorID, bucketID); err != nil || exists {
		t.Fatalf("failed pagination advanced checkpoint: exists=%v err=%v", exists, err)
	}
	statuses := manager.Status(context.Background())
	if len(statuses) != 1 || statuses[0].Status != StatusUnavailable || statuses[0].ErrorCode != code {
		t.Fatalf("failed pagination status=%#v want=%s", statuses, code)
	}
}

type checkpointFailureStore struct {
	*eventstore.Store
	failNext bool
}

func (store *checkpointFailureStore) SaveActivityWatchCheckpoint(ctx context.Context, checkpoint eventstore.ActivityWatchCheckpoint) error {
	if store.failNext {
		store.failNext = false
		return errors.New("injected checkpoint write failure")
	}
	return store.Store.SaveActivityWatchCheckpoint(ctx, checkpoint)
}

func testActivityWatchCollector(baseURL, id, bucketID string) config.CollectorConfig {
	return config.CollectorConfig{
		ID: id, Kind: config.CollectorActivityWatch, Enabled: true,
		HeartbeatPeriod: "1m", AllowedLateness: "1m", OfflineAfter: "5m",
		PlannedSchedule: config.PlannedScheduleConfig{Timezone: "UTC", Windows: []config.ScheduleWindowConfig{{Days: []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}, StartLocal: "00:00", EndLocal: "24:00"}}},
		ActivityWatch:   &config.ActivityWatchConfig{BaseURL: baseURL, BucketID: bucketID, PollInterval: "30s", RequestTimeout: "1s", InitialLookback: "24h", RescanWindow: "1h", PageSize: 10, MaxPagesPerPoll: 4, MaxResponseBytes: 1 << 20, ClockErrorMS: 100},
	}
}

func testCollectorConfig(t *testing.T, collectors ...config.CollectorConfig) config.Config {
	t.Helper()
	cfg, err := config.Load("", func(key string) (string, bool) {
		if key == "LOCALAPPDATA" {
			return t.TempDir(), true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Collectors = collectors
	return cfg
}

func openCollectorStore(t *testing.T, path string, collectors ...config.CollectorConfig) *eventstore.Store {
	t.Helper()
	policies := make(map[string]eventstore.CollectorPolicy)
	for _, collector := range collectors {
		policies[collector.ID] = eventstore.CollectorPolicy{Kind: collector.Kind, HeartbeatPeriod: collector.HeartbeatPeriodDuration()}
	}
	store, err := eventstore.Open(context.Background(), path, eventstore.Options{BusyTimeout: time.Second, MaxOpenConnections: 4, MaxBatchEvents: 100, MaxEventBytes: 64 << 10, MaxPayloadDepth: 16, MaxPageSize: 100, CollectorPolicies: policies, Now: func() time.Time { return time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testCollectorLogger(t *testing.T) *logging.Logger {
	t.Helper()
	logger, err := logging.New(&bytes.Buffer{}, "info", "test")
	if err != nil {
		t.Fatal(err)
	}
	return logger
}

func closedLoopbackURL(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "http://" + listener.Addr().String()
	listener.Close()
	return address
}
