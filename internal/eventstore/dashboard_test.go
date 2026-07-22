package eventstore

import (
	"context"
	"path/filepath"
	"testing"
)

func TestQueryDashboardHistoryIsBoundedStableAndExplicitlyEmpty(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "dashboard.db"))
	defer store.Close()
	ctx := context.Background()

	empty, err := store.QueryDashboardHistory(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if empty.RecentFaults == nil || empty.Modules == nil || len(empty.RecentFaults) != 0 || len(empty.Modules) != 0 || empty.Mode != nil {
		t.Fatalf("empty dashboard history is ambiguous: %#v", empty)
	}

	faults := []FaultEvent{
		{Module: "coverage", Severity: "P2", Status: "degraded", ErrorCode: "COVERAGE_DELAYED", Detail: "first", OccurredAtUTC: "2026-07-23T00:00:01Z"},
		{Module: "media", Severity: "P1", Status: "active", ErrorCode: "MEDIA_OFFLINE", Detail: "second", OccurredAtUTC: "2026-07-23T00:00:02Z"},
		{Module: "coverage", Severity: "P3", Status: "recovered", ErrorCode: "COVERAGE_RECOVERED", Detail: "third", OccurredAtUTC: "2026-07-23T00:00:03Z"},
	}
	for _, fault := range faults {
		if err := store.AppendFaultEvent(ctx, fault); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range []ModuleStateEvent{
		{Module: "coverage", Status: "degraded", ReasonCode: "COVERAGE_DELAYED", OccurredAtUTC: "2026-07-23T00:00:01Z"},
		{Module: "coverage", Status: "healthy", ReasonCode: "COVERAGE_RECOVERED", OccurredAtUTC: "2026-07-23T00:00:03Z"},
		{Module: "media", Status: "unavailable", ReasonCode: "MEDIA_OFFLINE", OccurredAtUTC: "2026-07-23T00:00:02Z"},
	} {
		if err := store.AppendModuleStateEvent(ctx, state); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendModeTransition(ctx, "record-only", "test", "test", "TEST_MODE", "2026-07-23T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	history, err := store.QueryDashboardHistory(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.RecentFaults) != 2 || history.RecentFaults[0].ErrorCode != "COVERAGE_RECOVERED" || history.RecentFaults[1].ErrorCode != "MEDIA_OFFLINE" {
		t.Fatalf("fault ordering/limit = %#v", history.RecentFaults)
	}
	if len(history.Modules) != 2 || history.Modules[0].Module != "coverage" || history.Modules[0].Status != "healthy" || history.Modules[1].Module != "media" {
		t.Fatalf("latest module states = %#v", history.Modules)
	}
	if history.Mode == nil || history.Mode.NewMode != "record-only" || history.Mode.ReasonCode != "TEST_MODE" {
		t.Fatalf("mode = %#v", history.Mode)
	}
	if _, err := store.QueryDashboardHistory(ctx, 0); ErrorCode(err) != CodePageLimitInvalid {
		t.Fatalf("invalid limit error = %v code=%q", err, ErrorCode(err))
	}
}
