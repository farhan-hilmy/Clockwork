package scheduler

import (
	"slices"
	"testing"
	"time"

	"github.com/farhan-hilmy/clockwork/internal/schedule"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func mustSpec(t *testing.T, cron, tz string) schedule.Spec {
	t.Helper()
	s, err := schedule.Parse(cron, tz)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPlanDue(t *testing.T) {
	every5 := mustSpec(t, "*/5 * * * *", "UTC")
	hourly := mustSpec(t, "0 * * * *", "UTC")
	grace := time.Minute

	tests := []struct {
		name     string
		spec     schedule.Spec
		nextRun  string
		now      string
		misfire  string
		want     []string
		wantNext string
		missed   bool
		catchUp  bool
	}{
		{
			name: "on time", spec: every5, misfire: tasks.MisfireSkip,
			nextRun: "2026-11-06T10:05:00Z", now: "2026-11-06T10:05:00.4Z",
			want: []string{"2026-11-06T10:05:00Z"}, wantNext: "2026-11-06T10:10:00Z",
		},
		{
			name: "late but within grace", spec: every5, misfire: tasks.MisfireSkip,
			nextRun: "2026-11-06T10:05:00Z", now: "2026-11-06T10:05:59Z",
			want: []string{"2026-11-06T10:05:00Z"}, wantNext: "2026-11-06T10:10:00Z",
		},
		{
			name: "exactly at grace boundary is on time", spec: every5, misfire: tasks.MisfireSkip,
			nextRun: "2026-11-06T10:05:00Z", now: "2026-11-06T10:06:00Z",
			want: []string{"2026-11-06T10:05:00Z"}, wantNext: "2026-11-06T10:10:00Z",
		},
		{
			name: "every minute", spec: mustSpec(t, "* * * * *", "UTC"), misfire: tasks.MisfireSkip,
			nextRun: "2026-11-06T10:05:00Z", now: "2026-11-06T10:05:30Z",
			want: []string{"2026-11-06T10:05:00Z"}, wantNext: "2026-11-06T10:06:00Z",
		},
		{
			name: "outage with skip drops missed runs", spec: hourly, misfire: tasks.MisfireSkip,
			nextRun: "2026-11-06T08:00:00Z", now: "2026-11-06T10:30:00Z",
			want: nil, wantNext: "2026-11-06T11:00:00Z", missed: true,
		},
		{
			name: "outage with run_once enqueues one catch-up", spec: hourly, misfire: tasks.MisfireRunOnce,
			nextRun: "2026-11-06T08:00:00Z", now: "2026-11-06T10:30:00Z",
			want: []string{"2026-11-06T08:00:00Z"}, wantNext: "2026-11-06T11:00:00Z", missed: true, catchUp: true,
		},
		{
			name: "outage ending just after an occurrence: the on-time run replaces the catch-up", spec: hourly, misfire: tasks.MisfireRunOnce,
			nextRun: "2026-11-06T08:00:00Z", now: "2026-11-06T10:00:20Z",
			want: []string{"2026-11-06T10:00:00Z"}, wantNext: "2026-11-06T11:00:00Z", missed: true,
		},
		{
			name: "outage with skip still runs the on-time occurrence", spec: hourly, misfire: tasks.MisfireSkip,
			nextRun: "2026-11-06T08:00:00Z", now: "2026-11-06T10:00:20Z",
			want: []string{"2026-11-06T10:00:00Z"}, wantNext: "2026-11-06T11:00:00Z", missed: true,
		},
		{
			name: "timezone: next run evaluated in the task's zone", spec: mustSpec(t, "0 9 * * *", "Asia/Jakarta"), misfire: tasks.MisfireSkip,
			nextRun: "2026-11-06T02:00:00Z", now: "2026-11-06T02:00:01Z",
			want: []string{"2026-11-06T02:00:00Z"}, wantNext: "2026-11-07T02:00:00Z",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := planDue(tc.spec, at(tc.nextRun), at(tc.now), grace, tc.misfire)
			var got []string
			for _, o := range p.occurrences {
				got = append(got, o.Format(time.RFC3339))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("occurrences = %v, want %v", got, tc.want)
			}
			if !p.next.Equal(at(tc.wantNext)) {
				t.Errorf("next = %v, want %v", p.next, tc.wantNext)
			}
			if p.missed != tc.missed || p.catchUp != tc.catchUp {
				t.Errorf("missed=%v catchUp=%v, want %v %v", p.missed, p.catchUp, tc.missed, tc.catchUp)
			}
			if !p.next.After(at(tc.now)) {
				t.Errorf("next %v is not after now", p.next)
			}
		})
	}
}

func TestPlanDueLongGraceEnqueuesEveryOnTimeOccurrence(t *testing.T) {
	p := planDue(mustSpec(t, "* * * * *", "UTC"), at("2026-11-06T10:00:00Z"), at("2026-11-06T10:02:10Z"), 5*time.Minute, tasks.MisfireSkip)
	if len(p.occurrences) != 3 || p.missed || !p.next.Equal(at("2026-11-06T10:03:00Z")) {
		t.Fatalf("got %+v", p)
	}
}

func TestPlanDueNeverAgain(t *testing.T) {
	// Feb 30 never exists: the task stops being scheduled instead of looping.
	p := planDue(mustSpec(t, "0 0 30 2 *", "UTC"), at("2026-11-06T10:00:00Z"), at("2026-11-06T10:00:01Z"), time.Minute, tasks.MisfireSkip)
	if !p.next.IsZero() {
		t.Fatalf("next = %v, want zero", p.next)
	}
}
