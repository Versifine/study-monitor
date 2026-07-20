package coverage

import (
	"testing"
	"time"

	"github.com/Versifine/study-monitor/internal/config"
)

func TestBuildDistinguishesExplicitIdleFromOfflineAndNeverInfersIdle(t *testing.T) {
	collector := testCollector()
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(15 * time.Minute)
	facts := []Fact{
		{Start: start, End: start.Add(time.Minute), Availability: ConfirmedIdle, ReasonCode: "HEARTBEAT_IDLE", Priority: 30, StableID: "idle"},
		{Start: start.Add(10 * time.Minute), End: start.Add(11 * time.Minute), Availability: Covered, ReasonCode: "HEARTBEAT_ACTIVE", Priority: 30, StableID: "active"},
	}
	intervals, err := Build(collector, start, end, end, facts)
	if err != nil {
		t.Fatal(err)
	}
	assertNonOverlapping(t, intervals)
	if stateAt(intervals, start.Add(30*time.Second)) != ConfirmedIdle {
		t.Fatalf("explicit idle was not preserved: %#v", intervals)
	}
	if stateAt(intervals, start.Add(7*time.Minute)) != Offline {
		t.Fatalf("missing heartbeat was not exposed as offline: %#v", intervals)
	}
	for _, interval := range intervals {
		if interval.Availability == ConfirmedIdle && interval.ReasonCode != "HEARTBEAT_IDLE" {
			t.Fatalf("confirmed_idle was inferred without an idle fact: %#v", interval)
		}
		if interval.Availability == Pending && !interval.End.After(end) {
			t.Fatalf("settled historical gap remained pending: %#v", interval)
		}
	}
}

func TestBuildTouchingAndOverlappingFactsProduceOneAvailabilityPerInstant(t *testing.T) {
	collector := testCollector()
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	facts := []Fact{
		{Start: start, End: start.Add(2 * time.Minute), Availability: Covered, QualityFlags: []string{"incomplete"}, ReasonCode: "EVENT", Priority: 20, StableID: "a"},
		{Start: start.Add(2 * time.Minute), End: start.Add(4 * time.Minute), Availability: Covered, ReasonCode: "EVENT", Priority: 20, StableID: "b"},
		{Start: start.Add(time.Minute), End: start.Add(3 * time.Minute), Availability: ConfirmedIdle, QualityFlags: []string{"clock_uncertain"}, ReasonCode: "IDLE", Priority: 40, StableID: "c"},
	}
	intervals, err := Build(collector, start, start.Add(5*time.Minute), start.Add(5*time.Minute), facts)
	if err != nil {
		t.Fatal(err)
	}
	assertNonOverlapping(t, intervals)
	if stateAt(intervals, start.Add(90*time.Second)) != ConfirmedIdle || stateAt(intervals, start.Add(210*time.Second)) != Covered {
		t.Fatalf("overlap priority or touching boundary is wrong: %#v", intervals)
	}
	flags := flagsAt(intervals, start.Add(90*time.Second))
	if len(flags) != 2 || flags[0] != "clock_uncertain" || flags[1] != "incomplete" {
		t.Fatalf("independent quality flags were not merged: %#v", flags)
	}
}

func TestLateFactRebuildChangesProjectionWithoutChangingFacts(t *testing.T) {
	collector := testCollector()
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	before, err := Build(collector, start, end, end, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stateAt(before, start.Add(7*time.Minute)) != Offline {
		t.Fatalf("expected an offline gap before late data: %#v", before)
	}
	late := []Fact{{Start: start.Add(6 * time.Minute), End: start.Add(8 * time.Minute), Availability: Covered, ReasonCode: "LATE", Priority: 20, StableID: "late"}}
	after, err := Build(collector, start, end, end, late)
	if err != nil {
		t.Fatal(err)
	}
	if stateAt(after, start.Add(7*time.Minute)) != Covered {
		t.Fatalf("late data did not repair the projection: %#v", after)
	}
	if len(late) != 1 || late[0].StableID != "late" {
		t.Fatal("projection build mutated authoritative input facts")
	}
}

func TestFactBeforePlannedWindowAddsExactStatusDeadlines(t *testing.T) {
	collector := testCollector()
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(3 * time.Minute)
	facts := []Fact{{Start: start.Add(-90 * time.Second), End: start.Add(-30 * time.Second), Availability: Covered, StableID: "prior"}}
	intervals, err := Build(collector, start, end, end.Add(time.Minute), facts)
	if err != nil {
		t.Fatal(err)
	}
	if stateAt(intervals, start.Add(15*time.Second)) != Unknown || stateAt(intervals, start.Add(45*time.Second)) != Delayed {
		t.Fatalf("pre-window fact deadlines were not split exactly: %#v", intervals)
	}
}

func TestMidScheduleRangeDoesNotResetMissingHeartbeatClock(t *testing.T) {
	collector := testCollector()
	start := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	intervals, err := Build(collector, start, end, end, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stateAt(intervals, start.Add(time.Second)) != Offline {
		t.Fatalf("mid-schedule query reset missing heartbeat clock: %#v", intervals)
	}
}

func TestClippedScheduleUsesOriginalDeadlineBoundaries(t *testing.T) {
	collector := testCollector()
	collector.PlannedSchedule.Windows[0].StartLocal = "12:00"
	collector.PlannedSchedule.Windows[0].EndLocal = "13:00"
	scheduleStart := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	start := scheduleStart.Add(30 * time.Second)
	intervals, err := Build(collector, start, start.Add(3*time.Minute), scheduleStart.Add(10*time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stateAt(intervals, scheduleStart.Add(45*time.Second)) != Unknown || stateAt(intervals, scheduleStart.Add(75*time.Second)) != Delayed {
		t.Fatalf("clipped schedule shifted original deadlines: %#v", intervals)
	}
}

func TestPlannedScheduleHandlesDSTWithoutOverlap(t *testing.T) {
	collector := testCollector()
	collector.PlannedSchedule = config.PlannedScheduleConfig{Timezone: "America/New_York", Windows: []config.ScheduleWindowConfig{{Days: []string{"sunday"}, StartLocal: "00:30", EndLocal: "02:30"}}}
	start := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	intervals, err := Build(collector, start, end, end, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertNonOverlapping(t, intervals)
	if len(intervals) == 0 || intervals[0].Start.Before(start) || intervals[len(intervals)-1].End.After(end) {
		t.Fatalf("DST schedule escaped the requested range: %#v", intervals)
	}
}

func testCollector() config.CollectorConfig {
	return config.CollectorConfig{
		ID: "test", Kind: config.CollectorGenericJSON, Enabled: true,
		HeartbeatPeriod: "1m", AllowedLateness: "1m", OfflineAfter: "5m",
		PlannedSchedule: config.PlannedScheduleConfig{Timezone: "UTC", Windows: []config.ScheduleWindowConfig{{Days: []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}, StartLocal: "00:00", EndLocal: "24:00"}}},
	}
}

func stateAt(intervals []Interval, at time.Time) string {
	for _, interval := range intervals {
		if !at.Before(interval.Start) && at.Before(interval.End) {
			return interval.Availability
		}
	}
	return ""
}

func flagsAt(intervals []Interval, at time.Time) []string {
	for _, interval := range intervals {
		if !at.Before(interval.Start) && at.Before(interval.End) {
			return interval.QualityFlags
		}
	}
	return nil
}

func assertNonOverlapping(t *testing.T, intervals []Interval) {
	t.Helper()
	for index, interval := range intervals {
		if !interval.Start.Before(interval.End) {
			t.Fatalf("empty interval: %#v", interval)
		}
		if index > 0 && interval.Start.Before(intervals[index-1].End) {
			t.Fatalf("overlapping intervals: %#v then %#v", intervals[index-1], interval)
		}
	}
}
