package collectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Versifine/study-monitor/internal/config"
	"github.com/Versifine/study-monitor/internal/eventstore"
	"github.com/Versifine/study-monitor/internal/logging"
	"github.com/Versifine/study-monitor/internal/strictjson"
)

const (
	StatusHealthy     = "healthy"
	StatusDisabled    = "disabled"
	StatusUnavailable = "unavailable"

	CodeRequestFailed     = "ACTIVITYWATCH_REQUEST_FAILED"
	CodeResponseTooLarge  = "ACTIVITYWATCH_RESPONSE_TOO_LARGE"
	CodeResponseInvalid   = "ACTIVITYWATCH_RESPONSE_INVALID"
	CodePaginationStalled = "ACTIVITYWATCH_PAGINATION_STALLED"
	CodeBacklogLimit      = "ACTIVITYWATCH_BACKLOG_LIMIT"
	CodeWriteFailed       = "ACTIVITYWATCH_WRITE_FAILED"

	activityWatchPollByteBudget  = 32 << 20
	activityWatchPollEventLimit  = 10000
	activityWatchPollConcurrency = 2
)

type Store interface {
	AppendBatch(context.Context, []eventstore.Candidate) ([]eventstore.WriteResult, error)
	AppendHeartbeatBatch(context.Context, []eventstore.HeartbeatCandidate) ([]eventstore.HeartbeatWriteResult, error)
	LoadActivityWatchCheckpoint(context.Context, string, string) (eventstore.ActivityWatchCheckpoint, bool, error)
	SaveActivityWatchCheckpoint(context.Context, eventstore.ActivityWatchCheckpoint) error
}

type Status struct {
	CollectorID       string `json:"collector_id"`
	Kind              string `json:"kind"`
	Status            string `json:"status"`
	ErrorCode         string `json:"error_code,omitempty"`
	LastAttemptUTC    string `json:"last_attempt_utc,omitempty"`
	LastSuccessUTC    string `json:"last_success_utc,omitempty"`
	CheckpointUTC     string `json:"checkpoint_utc,omitempty"`
	CheckpointEventID int64  `json:"checkpoint_event_id,omitempty"`
	Imported          int64  `json:"imported"`
	Duplicates        int64  `json:"duplicates"`
}

type Manager struct {
	configs []config.CollectorConfig
	store   Store
	logger  *logging.Logger
	now     func() time.Time
	client  func(time.Duration) *http.Client
	polls   chan struct{}

	pollByteBudget     int
	pollEventLimit     int
	pollExecutionLimit func(config.CollectorConfig) time.Duration

	mu       sync.RWMutex
	statuses map[string]Status
}

func New(cfg config.Config, store Store, logger *logging.Logger) *Manager {
	collectors := make([]config.CollectorConfig, 0)
	statuses := make(map[string]Status)
	minOffline := time.Duration(0)
	for _, collector := range cfg.Collectors {
		if collector.Enabled && collector.Kind == config.CollectorActivityWatch {
			collectors = append(collectors, collector)
			statuses[collector.ID] = Status{CollectorID: collector.ID, Kind: collector.Kind, Status: StatusUnavailable}
			if minOffline == 0 || collector.OfflineAfterDuration() < minOffline {
				minOffline = collector.OfflineAfterDuration()
			}
		}
	}
	waves := (len(collectors) + activityWatchPollConcurrency - 1) / activityWatchPollConcurrency
	if waves < 1 {
		waves = 1
	}
	executionLimit := minOffline / time.Duration(waves)
	return &Manager{
		configs: collectors, store: store, logger: logger, now: time.Now,
		client:         localHTTPClient,
		polls:          make(chan struct{}, activityWatchPollConcurrency),
		pollByteBudget: activityWatchPollByteBudget,
		pollEventLimit: activityWatchPollEventLimit,
		pollExecutionLimit: func(config.CollectorConfig) time.Duration {
			return executionLimit
		},
		statuses: statuses,
	}
}

func localHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("ActivityWatch redirects are disabled")
		},
	}
}

func (manager *Manager) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for _, collector := range manager.configs {
		collector := collector
		workers.Add(1)
		go func() {
			defer workers.Done()
			manager.runCollector(ctx, collector)
		}()
	}
	workers.Wait()
}

func (manager *Manager) PollOnce(ctx context.Context) {
	for _, collector := range manager.configs {
		manager.pollAndRecord(ctx, collector)
	}
}

func (manager *Manager) Status(context.Context) []Status {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	result := make([]Status, 0, len(manager.statuses))
	for _, status := range manager.statuses {
		result = append(result, status)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CollectorID < result[j].CollectorID })
	return result
}

func (manager *Manager) runCollector(ctx context.Context, collector config.CollectorConfig) {
	manager.pollAndRecord(ctx, collector)
	ticker := time.NewTicker(collector.ActivityWatch.PollIntervalDuration())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			manager.pollAndRecord(ctx, collector)
		}
	}
}

func (manager *Manager) pollAndRecord(ctx context.Context, collector config.CollectorConfig) {
	attemptCtx, cancelAttempt := context.WithTimeout(ctx, collector.OfflineAfterDuration())
	defer cancelAttempt()
	var imported, duplicates int
	var checkpoint eventstore.ActivityWatchCheckpoint
	var err error
	var now time.Time
	select {
	case manager.polls <- struct{}{}:
		now = manager.now().UTC()
		executionCtx, cancelExecution := context.WithTimeout(attemptCtx, manager.pollExecutionLimit(collector))
		imported, duplicates, checkpoint, err = manager.poll(executionCtx, collector, now)
		cancelExecution()
		<-manager.polls
	case <-attemptCtx.Done():
		if ctx.Err() != nil {
			return
		}
		now = manager.now().UTC()
		err = &adapterError{CodeRequestFailed, errors.New("ActivityWatch poll could not acquire the bounded worker slot before offline_after")}
	}
	manager.mu.Lock()
	status := manager.statuses[collector.ID]
	status.LastAttemptUTC = fixedUTC(now)
	if err != nil {
		status.Status = StatusUnavailable
		status.ErrorCode = errorCode(err)
	} else {
		status.Status = StatusHealthy
		status.ErrorCode = ""
		status.LastSuccessUTC = fixedUTC(now)
		status.CheckpointUTC = checkpoint.SourceTimeUTC
		status.CheckpointEventID = checkpoint.SourceEventID
		status.Imported += int64(imported)
		status.Duplicates += int64(duplicates)
	}
	manager.statuses[collector.ID] = status
	manager.mu.Unlock()
	if err != nil {
		manager.logger.Error("activitywatch", "poll_failed", errorCode(err), "ActivityWatch collector poll failed", err, slog.String("collector_id", collector.ID))
	}
}

type adapterError struct {
	code string
	err  error
}

func (err *adapterError) Error() string { return err.err.Error() }
func (err *adapterError) Unwrap() error { return err.err }

func errorCode(err error) string {
	var adapterErr *adapterError
	if errors.As(err, &adapterErr) {
		return adapterErr.code
	}
	return CodeWriteFailed
}

type awBucket struct {
	ID          string          `json:"id"`
	Created     string          `json:"created"`
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Client      string          `json:"client"`
	Hostname    string          `json:"hostname"`
	Data        json.RawMessage `json:"data"`
	FirstSeen   string          `json:"first_seen"`
	LastUpdated string          `json:"last_updated"`
}

type awEvent struct {
	ID             *int64          `json:"id"`
	Timestamp      string          `json:"timestamp"`
	Duration       *float64        `json:"duration"`
	Data           json.RawMessage `json:"data"`
	parsed         time.Time
	sourceID       int64
	sourceDuration float64
}

func (manager *Manager) poll(ctx context.Context, collector config.CollectorConfig, now time.Time) (int, int, eventstore.ActivityWatchCheckpoint, error) {
	aw := *collector.ActivityWatch
	client := manager.client(aw.RequestTimeoutDuration())
	defer client.CloseIdleConnections()
	bucket, err := fetchBucket(ctx, client, aw)
	if err != nil {
		return 0, 0, eventstore.ActivityWatchCheckpoint{}, err
	}
	checkpoint, exists, err := manager.store.LoadActivityWatchCheckpoint(ctx, collector.ID, aw.BucketID)
	if err != nil {
		return 0, 0, eventstore.ActivityWatchCheckpoint{}, &adapterError{CodeWriteFailed, err}
	}
	cutoff := now.Add(-collector.AllowedLatenessDuration())
	start := now.Add(-aw.InitialLookbackDuration())
	if exists {
		parsed, parseErr := time.Parse(fixedLayout, checkpoint.SourceTimeUTC)
		if parseErr != nil {
			return 0, 0, checkpoint, &adapterError{CodeResponseInvalid, errors.New("stored ActivityWatch checkpoint time is invalid")}
		}
		start = parsed.Add(-aw.RescanWindowDuration())
	}
	if !start.Before(cutoff) {
		if err := manager.appendPollHeartbeat(ctx, collector, now, manager.now().UTC()); err != nil {
			return 0, 0, checkpoint, err
		}
		return 0, 0, checkpoint, nil
	}
	events, err := fetchEvents(ctx, client, aw, start, cutoff, manager.pollEventLimit, manager.pollByteBudget)
	if err != nil {
		return 0, 0, checkpoint, err
	}
	imported, duplicates := 0, 0
	for offset := 0; offset < len(events); offset += aw.PageSize {
		end := offset + aw.PageSize
		if end > len(events) {
			end = len(events)
		}
		candidates := make([]eventstore.Candidate, 0, end-offset)
		for _, event := range events[offset:end] {
			candidate, err := activityWatchCandidate(collector.ID, bucket, event, aw.ClockErrorMS)
			if err != nil {
				return imported, duplicates, checkpoint, err
			}
			candidates = append(candidates, candidate)
		}
		results, err := manager.store.AppendBatch(ctx, candidates)
		if err != nil {
			return imported, duplicates, checkpoint, &adapterError{CodeWriteFailed, err}
		}
		for _, result := range results {
			switch result.Status {
			case eventstore.StatusAccepted:
				imported++
			case eventstore.StatusDuplicate:
				duplicates++
			default:
				return imported, duplicates, checkpoint, &adapterError{CodeWriteFailed, fmt.Errorf("ActivityWatch event write returned %s (%s)", result.Status, result.ErrorCode)}
			}
		}
		last := events[end-1]
		if !exists || tupleAfter(last.parsed, last.sourceID, checkpoint) {
			checkpoint = eventstore.ActivityWatchCheckpoint{CollectorID: collector.ID, BucketID: aw.BucketID, SourceTimeUTC: fixedUTC(last.parsed), SourceEventID: last.sourceID}
			if err := manager.store.SaveActivityWatchCheckpoint(ctx, checkpoint); err != nil {
				return imported, duplicates, checkpoint, &adapterError{CodeWriteFailed, err}
			}
			exists = true
		}
	}
	if err := manager.appendPollHeartbeat(ctx, collector, now, manager.now().UTC()); err != nil {
		return imported, duplicates, checkpoint, err
	}
	return imported, duplicates, checkpoint, nil
}

func tupleAfter(at time.Time, id int64, checkpoint eventstore.ActivityWatchCheckpoint) bool {
	stored, err := time.Parse(fixedLayout, checkpoint.SourceTimeUTC)
	return err != nil || at.After(stored) || (at.Equal(stored) && id > checkpoint.SourceEventID)
}

func (manager *Manager) appendPollHeartbeat(ctx context.Context, collector config.CollectorConfig, start, end time.Time) error {
	if !end.After(start) {
		end = start.Add(time.Nanosecond)
	}
	if end.Sub(start) > collector.HeartbeatPeriodDuration() {
		end = start.Add(collector.HeartbeatPeriodDuration())
	}
	raw, _ := json.Marshal(map[string]any{
		"schema_version": 1, "collector_id": collector.ID, "state": eventstore.HeartbeatStateActive,
		"device_start_raw": fixedUTC(start), "device_end_raw": fixedUTC(end),
		"clock_offset_ms": int64(0), "clock_error_ms": collector.ActivityWatch.ClockErrorMS,
		"idempotency_key": "activitywatch-poll:" + fixedUTC(start) + ":" + fixedUTC(end), "quality_flags": []string{},
	})
	results, err := manager.store.AppendHeartbeatBatch(ctx, []eventstore.HeartbeatCandidate{{Raw: raw}})
	if err != nil {
		return &adapterError{CodeWriteFailed, err}
	}
	if len(results) != 1 || (results[0].Status != eventstore.StatusAccepted && results[0].Status != eventstore.StatusDuplicate) {
		return &adapterError{CodeWriteFailed, errors.New("ActivityWatch poll heartbeat was rejected")}
	}
	return nil
}

func fetchBucket(ctx context.Context, client *http.Client, aw config.ActivityWatchConfig) (awBucket, error) {
	var bucket awBucket
	endpoint := strings.TrimSuffix(aw.BaseURL, "/") + "/api/0/buckets/" + url.PathEscape(aw.BucketID)
	if err := getJSON(ctx, client, endpoint, aw.MaxResponseBytes, &bucket); err != nil {
		return awBucket{}, err
	}
	if bucket.ID != aw.BucketID || !validActivityWatchIdentifier(bucket.Type, 128) {
		return awBucket{}, &adapterError{CodeResponseInvalid, errors.New("ActivityWatch bucket metadata is invalid")}
	}
	return bucket, nil
}

func fetchEvents(ctx context.Context, client *http.Client, aw config.ActivityWatchConfig, start, end time.Time, eventLimit, byteBudget int) ([]awEvent, error) {
	seen := make(map[int64]awEvent)
	usedBytes := 0
	pageEnd := end.UTC()
	fullLastPage := false
	for pageIndex := 0; pageIndex < aw.MaxPagesPerPoll; pageIndex++ {
		endpoint, _ := url.Parse(strings.TrimSuffix(aw.BaseURL, "/") + "/api/0/buckets/" + url.PathEscape(aw.BucketID) + "/events")
		query := endpoint.Query()
		query.Set("start", fixedUTC(start))
		query.Set("end", fixedUTC(pageEnd))
		query.Set("limit", strconv.Itoa(aw.PageSize))
		endpoint.RawQuery = query.Encode()
		var page []awEvent
		if err := getJSON(ctx, client, endpoint.String(), aw.MaxResponseBytes, &page); err != nil {
			return nil, err
		}
		if len(page) == 0 {
			fullLastPage = false
			break
		}
		oldest := pageEnd
		newIDs := 0
		for index := range page {
			event := &page[index]
			if event.ID == nil || event.Duration == nil || *event.ID < 0 || math.IsNaN(*event.Duration) || math.IsInf(*event.Duration, 0) || *event.Duration < 0 || *event.Duration > 366*24*60*60 {
				return nil, &adapterError{CodeResponseInvalid, errors.New("ActivityWatch event identity or duration is invalid")}
			}
			event.sourceID = *event.ID
			event.sourceDuration = *event.Duration
			parsed, err := time.Parse(time.RFC3339Nano, event.Timestamp)
			if err != nil || strings.HasSuffix(event.Timestamp, "-00:00") {
				return nil, &adapterError{CodeResponseInvalid, errors.New("ActivityWatch event timestamp is invalid")}
			}
			event.parsed = parsed.UTC()
			if event.parsed.Before(start) || event.parsed.After(end) {
				return nil, &adapterError{CodeResponseInvalid, errors.New("ActivityWatch returned an event outside the requested range")}
			}
			if err := validateDataObject(event.Data); err != nil {
				return nil, err
			}
			if event.parsed.Before(oldest) {
				oldest = event.parsed
			}
			if previous, exists := seen[event.sourceID]; exists {
				if previous.Timestamp != event.Timestamp || previous.sourceDuration != event.sourceDuration || !bytes.Equal(previous.Data, event.Data) {
					return nil, &adapterError{CodeResponseInvalid, errors.New("ActivityWatch repeated an event id with different content")}
				}
			} else {
				eventBytes := 256 + len(event.Timestamp) + len(event.Data)
				if len(seen) >= eventLimit || eventBytes > byteBudget-usedBytes {
					return nil, &adapterError{CodeBacklogLimit, errors.New("ActivityWatch poll exceeds the fixed event or memory budget; no checkpoint was changed")}
				}
				seen[event.sourceID] = *event
				usedBytes += eventBytes
				newIDs++
			}
		}
		fullLastPage = len(page) == aw.PageSize
		if !fullLastPage {
			break
		}
		if !oldest.Before(pageEnd) || newIDs == 0 {
			return nil, &adapterError{CodePaginationStalled, errors.New("ActivityWatch pagination did not advance; no checkpoint was changed")}
		}
		pageEnd = oldest
	}
	if fullLastPage {
		return nil, &adapterError{CodeBacklogLimit, errors.New("ActivityWatch backlog exceeds the bounded pages per poll; no checkpoint was changed")}
	}
	result := make([]awEvent, 0, len(seen))
	for _, event := range seen {
		result = append(result, event)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].parsed.Equal(result[j].parsed) {
			return result[i].sourceID < result[j].sourceID
		}
		return result[i].parsed.Before(result[j].parsed)
	})
	stable := result[:0]
	for _, event := range result {
		// ActivityWatch can keep extending an earlier event while later,
		// overlapping events are already closed. Once the first mutable tuple is
		// found, stop the stable prefix so the checkpoint can never jump past it.
		duration := time.Duration(event.sourceDuration * float64(time.Second))
		if event.parsed.Add(duration).After(end) {
			break
		}
		stable = append(stable, event)
	}
	return stable, nil
}

func activityWatchCandidate(collectorID string, bucket awBucket, event awEvent, clockErrorMS int64) (eventstore.Candidate, error) {
	data := json.RawMessage(event.Data)
	payload, err := json.Marshal(struct {
		BucketID        string          `json:"bucket_id"`
		BucketType      string          `json:"bucket_type"`
		BucketClient    string          `json:"bucket_client"`
		BucketHostname  string          `json:"bucket_hostname"`
		SourceEventID   int64           `json:"source_event_id"`
		DurationSeconds float64         `json:"duration_seconds"`
		Data            json.RawMessage `json:"data"`
	}{bucket.ID, bucket.Type, bucket.Client, bucket.Hostname, event.sourceID, event.sourceDuration, data})
	if err != nil {
		return eventstore.Candidate{}, &adapterError{CodeResponseInvalid, err}
	}
	bucketDigest := sha256.Sum256([]byte(bucket.ID))
	key := "activitywatch:" + hex.EncodeToString(bucketDigest[:8]) + ":" + strconv.FormatInt(event.sourceID, 10)
	raw, err := json.Marshal(struct {
		SchemaVersion      int             `json:"schema_version"`
		CollectorID        string          `json:"collector_id"`
		EventType          string          `json:"event_type"`
		DeviceTimestampRaw string          `json:"device_timestamp_raw"`
		ClockOffsetMS      int64           `json:"clock_offset_ms"`
		ClockErrorMS       int64           `json:"clock_error_ms"`
		IdempotencyKey     string          `json:"idempotency_key"`
		Payload            json.RawMessage `json:"payload"`
	}{1, collectorID, "activitywatch.event", event.Timestamp, 0, clockErrorMS, key, payload})
	if err != nil {
		return eventstore.Candidate{}, &adapterError{CodeResponseInvalid, err}
	}
	return eventstore.Candidate{Raw: raw}, nil
}

func validateDataObject(raw json.RawMessage) error {
	if err := strictjson.ValidateObjectKeys(raw, 0); err != nil {
		return &adapterError{CodeResponseInvalid, errors.New("ActivityWatch event data contains duplicate keys or invalid JSON")}
	}
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil || value == nil {
		return &adapterError{CodeResponseInvalid, errors.New("ActivityWatch event data must be a JSON object")}
	}
	return nil
}

func validActivityWatchIdentifier(value string, maximum int) bool {
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

func getJSON(ctx context.Context, client *http.Client, endpoint string, limit int64, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return &adapterError{CodeRequestFailed, err}
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return &adapterError{CodeRequestFailed, err}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &adapterError{CodeRequestFailed, fmt.Errorf("ActivityWatch GET returned HTTP %d", response.StatusCode)}
	}
	limited := io.LimitReader(response.Body, limit+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return &adapterError{CodeRequestFailed, err}
	}
	if int64(len(raw)) > limit {
		return &adapterError{CodeResponseTooLarge, errors.New("ActivityWatch response exceeds the configured byte limit")}
	}
	if !utf8.Valid(raw) {
		return &adapterError{CodeResponseInvalid, errors.New("ActivityWatch response must be valid UTF-8")}
	}
	if err := strictjson.ValidateObjectKeys(raw, 0); err != nil {
		return &adapterError{CodeResponseInvalid, errors.New("ActivityWatch response contains duplicate keys or invalid JSON")}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return &adapterError{CodeResponseInvalid, errors.New("ActivityWatch response does not match the supported schema")}
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return &adapterError{CodeResponseInvalid, errors.New("ActivityWatch response contains trailing JSON")}
	}
	return nil
}

const fixedLayout = "2006-01-02T15:04:05.000000000Z"

func fixedUTC(value time.Time) string { return value.UTC().Format(fixedLayout) }
