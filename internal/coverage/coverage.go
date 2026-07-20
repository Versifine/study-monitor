package coverage

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/Versifine/study-monitor/internal/config"
)

const (
	Covered       = "covered"
	ConfirmedIdle = "confirmed_idle"
	Pending       = "pending"
	Delayed       = "delayed"
	Offline       = "offline"
	Unknown       = "unknown"
)

type Fact struct {
	Start        time.Time
	End          time.Time
	Availability string
	QualityFlags []string
	ReasonCode   string
	Priority     int
	StableID     string
}

type Interval struct {
	Start         time.Time
	End           time.Time
	ScheduleStart time.Time
	Availability  string
	QualityFlags  []string
	ReasonCode    string
}

func Build(collector config.CollectorConfig, rangeStart, rangeEnd, now time.Time, facts []Fact) ([]Interval, error) {
	if !rangeStart.Before(rangeEnd) {
		return nil, errors.New("coverage range must be positive")
	}
	planned, err := plannedIntervals(collector.PlannedSchedule, rangeStart.UTC(), rangeEnd.UTC())
	if err != nil {
		return nil, err
	}
	for index := range facts {
		facts[index].Start = facts[index].Start.UTC()
		facts[index].End = facts[index].End.UTC()
		if !facts[index].Start.Before(facts[index].End) || (facts[index].Availability != Covered && facts[index].Availability != ConfirmedIdle) {
			return nil, errors.New("coverage fact is invalid")
		}
		facts[index].QualityFlags = normalizeFlags(facts[index].QualityFlags)
	}
	sort.Slice(facts, func(i, j int) bool {
		if facts[i].Start.Equal(facts[j].Start) {
			if facts[i].Priority == facts[j].Priority {
				return facts[i].StableID < facts[j].StableID
			}
			return facts[i].Priority > facts[j].Priority
		}
		return facts[i].Start.Before(facts[j].Start)
	})

	result := make([]Interval, 0)
	for _, plan := range planned {
		points := []time.Time{plan.Start, plan.End}
		for _, fact := range facts {
			if fact.End.After(plan.Start) && fact.Start.Before(plan.End) {
				points = append(points, maxTime(plan.Start, fact.Start), minTime(plan.End, fact.End))
			}
			// A fact immediately before a planned window still determines when the
			// collector becomes expected, delayed, and offline inside that window.
			for _, deadline := range []time.Time{
				fact.End.Add(collector.HeartbeatPeriodDuration()),
				fact.End.Add(collector.HeartbeatPeriodDuration() + collector.AllowedLatenessDuration()),
				fact.End.Add(collector.OfflineAfterDuration()),
			} {
				if deadline.After(plan.Start) && deadline.Before(plan.End) {
					points = append(points, deadline)
				}
			}
		}
		for _, deadline := range []time.Time{
			plan.ScheduleStart.Add(collector.HeartbeatPeriodDuration()),
			plan.ScheduleStart.Add(collector.HeartbeatPeriodDuration() + collector.AllowedLatenessDuration()),
			plan.ScheduleStart.Add(collector.OfflineAfterDuration()),
			now.UTC(),
		} {
			if deadline.After(plan.Start) && deadline.Before(plan.End) {
				points = append(points, deadline)
			}
		}
		points = uniqueTimes(points)
		for index := 0; index+1 < len(points); index++ {
			start, end := points[index], points[index+1]
			if !start.Before(end) {
				continue
			}
			availability, flags, reason := classifySegment(collector, plan.ScheduleStart, start, end, now.UTC(), facts)
			result = appendMerged(result, Interval{Start: start, End: end, Availability: availability, QualityFlags: flags, ReasonCode: reason})
		}
	}
	return result, nil
}

func classifySegment(collector config.CollectorConfig, planStart, start, end, now time.Time, facts []Fact) (string, []string, string) {
	covering := make([]Fact, 0)
	var lastEvidenceEnd time.Time
	for _, fact := range facts {
		if !fact.Start.After(start) && !fact.End.Before(end) {
			covering = append(covering, fact)
		}
		if !fact.End.After(start) && fact.End.After(lastEvidenceEnd) {
			lastEvidenceEnd = fact.End
		}
	}
	if len(covering) > 0 {
		sort.Slice(covering, func(i, j int) bool {
			if covering[i].Priority == covering[j].Priority {
				return covering[i].StableID < covering[j].StableID
			}
			return covering[i].Priority > covering[j].Priority
		})
		flags := make([]string, 0)
		for _, fact := range covering {
			flags = append(flags, fact.QualityFlags...)
		}
		return covering[0].Availability, normalizeFlags(flags), covering[0].ReasonCode
	}
	if !start.Before(now) {
		return Pending, nil, "SCHEDULED_FUTURE"
	}
	reference := lastEvidenceEnd
	if reference.IsZero() {
		reference = planStart
	}
	expected := reference.Add(collector.HeartbeatPeriodDuration())
	lateDeadline := expected.Add(collector.AllowedLatenessDuration())
	offlineDeadline := reference.Add(collector.OfflineAfterDuration())
	if now.Before(lateDeadline) {
		return Pending, nil, "AWAITING_HEARTBEAT"
	}
	if start.Before(expected) {
		return Unknown, nil, "HEARTBEAT_MISSING"
	}
	if start.Before(offlineDeadline) {
		return Delayed, nil, "HEARTBEAT_LATE"
	}
	return Offline, nil, "HEARTBEAT_OFFLINE"
}

func plannedIntervals(schedule config.PlannedScheduleConfig, rangeStart, rangeEnd time.Time) ([]Interval, error) {
	location, err := time.LoadLocation(schedule.Timezone)
	if err != nil {
		return nil, err
	}
	localStart := rangeStart.In(location)
	firstDate := time.Date(localStart.Year(), localStart.Month(), localStart.Day(), 0, 0, 0, 0, location).AddDate(0, 0, -1)
	localEnd := rangeEnd.In(location)
	lastDate := time.Date(localEnd.Year(), localEnd.Month(), localEnd.Day(), 0, 0, 0, 0, location).AddDate(0, 0, 1)
	intervals := make([]Interval, 0)
	for date := firstDate; !date.After(lastDate); date = date.AddDate(0, 0, 1) {
		day := strings.ToLower(date.Weekday().String())
		for _, window := range schedule.Windows {
			if !contains(window.Days, day) {
				continue
			}
			startMinute, _ := parseMinute(window.StartLocal)
			endMinute, _ := parseMinute(window.EndLocal)
			start := time.Date(date.Year(), date.Month(), date.Day(), startMinute/60, startMinute%60, 0, 0, location)
			var end time.Time
			if endMinute == 24*60 {
				end = time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, location).AddDate(0, 0, 1)
			} else {
				end = time.Date(date.Year(), date.Month(), date.Day(), endMinute/60, endMinute%60, 0, 0, location)
			}
			scheduleStart := start.UTC()
			start, end = maxTime(scheduleStart, rangeStart), minTime(end.UTC(), rangeEnd)
			if start.Before(end) {
				intervals = append(intervals, Interval{Start: start, End: end, ScheduleStart: scheduleStart})
			}
		}
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].Start.Before(intervals[j].Start) })
	merged := make([]Interval, 0, len(intervals))
	for _, interval := range intervals {
		if len(merged) == 0 || interval.Start.After(merged[len(merged)-1].End) {
			merged = append(merged, interval)
			continue
		}
		if interval.ScheduleStart.Before(merged[len(merged)-1].ScheduleStart) {
			merged[len(merged)-1].ScheduleStart = interval.ScheduleStart
		}
		if interval.End.After(merged[len(merged)-1].End) {
			merged[len(merged)-1].End = interval.End
		}
	}
	return merged, nil
}

func appendMerged(intervals []Interval, next Interval) []Interval {
	if len(intervals) > 0 {
		last := &intervals[len(intervals)-1]
		if last.End.Equal(next.Start) && last.Availability == next.Availability && last.ReasonCode == next.ReasonCode && equalFlags(last.QualityFlags, next.QualityFlags) {
			last.End = next.End
			return intervals
		}
	}
	return append(intervals, next)
}

func uniqueTimes(values []time.Time) []time.Time {
	sort.Slice(values, func(i, j int) bool { return values[i].Before(values[j]) })
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || !result[len(result)-1].Equal(value) {
			result = append(result, value)
		}
	}
	return result
}

func normalizeFlags(values []string) []string {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		seen[value] = true
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func equalFlags(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func parseMinute(value string) (int, error) {
	if value == "24:00" {
		return 24 * 60, nil
	}
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return 0, err
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func minTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}

func maxTime(first, second time.Time) time.Time {
	if first.After(second) {
		return first
	}
	return second
}
