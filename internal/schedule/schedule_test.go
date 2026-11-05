package schedule

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseRejectsInvalid(t *testing.T) {
	cases := map[string][2]string{
		"empty":         {"", "UTC"},
		"garbage":       {"not a cron", "UTC"},
		"six fields":    {"0 */5 * * * *", "UTC"},
		"every":         {"@every 5m", "UTC"},
		"embedded tz":   {"CRON_TZ=UTC 0 * * * *", "UTC"},
		"bad timezone":  {"0 * * * *", "Mars/Olympus"},
		"local tz":      {"0 * * * *", "Local"},
		"out of range":  {"61 * * * *", "UTC"},
	}
	for name, c := range cases {
		if _, err := Parse(c[0], c[1]); err == nil {
			t.Errorf("%s: expected error for %q / %q", name, c[0], c[1])
		}
	}
}

func TestNextEveryFiveMinutes(t *testing.T) {
	s, err := Parse("*/5 * * * *", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	got := s.Next(mustTime(t, "2026-11-05T14:02:30Z"))
	if want := mustTime(t, "2026-11-05T14:05:00Z"); !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
	// Strictly after: exactly on a boundary moves to the next one.
	got = s.Next(mustTime(t, "2026-11-05T14:05:00Z"))
	if want := mustTime(t, "2026-11-05T14:10:00Z"); !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestNextUsesScheduleTimezone(t *testing.T) {
	// 09:00 in Jakarta (UTC+7, no DST) is 02:00 UTC.
	s, err := Parse("0 9 * * *", "Asia/Jakarta")
	if err != nil {
		t.Fatal(err)
	}
	got := s.Next(mustTime(t, "2026-11-05T03:00:00Z"))
	if want := mustTime(t, "2026-11-06T02:00:00Z"); !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
	if got.Location() != time.UTC {
		t.Fatalf("expected UTC result, got %s", got.Location())
	}
}

func TestNextAcrossDSTSpringForward(t *testing.T) {
	// America/New_York jumps 02:00 -> 03:00 on 2026-03-08; 02:30 does not exist that day.
	s, err := Parse("30 2 * * *", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	got := s.Next(mustTime(t, "2026-03-08T00:00:00Z"))
	// 02:30 local does not exist on the 8th, so that day is skipped;
	// the next run is 02:30 EDT (UTC-4) on the 9th = 06:30 UTC.
	if want := mustTime(t, "2026-03-09T06:30:00Z"); !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestNextAcrossDSTFallBackRunsOnce(t *testing.T) {
	// America/New_York repeats 01:00-02:00 on 2026-11-01. A 01:30 job must run once that day.
	s, err := Parse("30 1 * * *", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	runs := s.NextN(mustTime(t, "2026-11-01T00:00:00Z"), 2)
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	if runs[1].Sub(runs[0]) < 23*time.Hour {
		t.Fatalf("ran twice on the fall-back day: %v", runs)
	}
}

func TestIntervalJobRunsThroughRepeatedHour(t *testing.T) {
	s, err := Parse("*/30 * * * *", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 00:50 EDT on fall-back day. 01:00-01:59 happens twice (EDT, then EST).
	runs := s.NextN(mustTime(t, "2026-11-01T04:50:00Z"), 4)
	want := []string{"2026-11-01T05:00:00Z", "2026-11-01T05:30:00Z", "2026-11-01T06:00:00Z", "2026-11-01T06:30:00Z"}
	for i, w := range want {
		if !runs[i].Equal(mustTime(t, w)) {
			t.Fatalf("run %d: got %s want %s (all: %v)", i, runs[i], w, runs)
		}
	}
}

func TestNextN(t *testing.T) {
	s, err := Parse("@hourly", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	runs := s.NextN(mustTime(t, "2026-11-05T14:10:00Z"), 3)
	want := []string{"2026-11-05T15:00:00Z", "2026-11-05T16:00:00Z", "2026-11-05T17:00:00Z"}
	for i, w := range want {
		if !runs[i].Equal(mustTime(t, w)) {
			t.Fatalf("run %d: got %s want %s", i, runs[i], w)
		}
	}
}
