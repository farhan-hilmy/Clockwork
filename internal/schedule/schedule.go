// Package schedule parses cron expressions and computes timezone-aware run times.
package schedule

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Standard 5-field cron plus descriptors such as @hourly and @daily.
// Seconds are intentionally not supported: Clockwork is minute-granular.
var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Spec is a parsed, validated schedule.
type Spec struct {
	expr      string
	loc       *time.Location
	s         cron.Schedule
	fixedHour bool // hour field names specific hours, e.g. "30 1 * * *" but not "*/5 * * * *"
}

// Parse validates a cron expression and IANA timezone name.
func Parse(expr, timezone string) (Spec, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return Spec{}, fmt.Errorf("cron expression is empty")
	}
	if strings.HasPrefix(expr, "@every") {
		return Spec{}, fmt.Errorf("@every is not supported; use a cron expression")
	}
	if strings.Contains(expr, "TZ=") {
		return Spec{}, fmt.Errorf("set the timezone field instead of embedding TZ= in the expression")
	}
	loc, err := LoadLocation(timezone)
	if err != nil {
		return Spec{}, err
	}
	s, err := parser.Parse(expr)
	if err != nil {
		return Spec{}, fmt.Errorf("invalid cron expression: %w", err)
	}
	return Spec{expr: expr, loc: loc, s: s, fixedHour: hasFixedHour(expr)}, nil
}

func hasFixedHour(expr string) bool {
	if strings.HasPrefix(expr, "@") {
		return expr != "@hourly"
	}
	f := strings.Fields(expr)
	return len(f) >= 2 && !strings.Contains(f[1], "*")
}

// LoadLocation resolves an IANA timezone name. An empty name means UTC.
func LoadLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	if strings.EqualFold(name, "local") {
		return nil, fmt.Errorf("timezone %q is ambiguous; use an IANA name such as Asia/Jakarta", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q", name)
	}
	return loc, nil
}

// Next returns the first run time strictly after t, in UTC.
// The cron fields are evaluated in the schedule's timezone, so "0 9 * * *" in
// Asia/Jakarta means 09:00 Jakarta time.
//
// DST behaviour: a wall-clock time that does not exist (spring forward) is skipped.
// When clocks fall back, a job pinned to specific hours runs only at the first
// occurrence of the repeated wall-clock time; interval jobs such as "*/5 * * * *"
// keep running through the repeated hour, since that time really elapses.
func (s Spec) Next(after time.Time) time.Time {
	n := s.s.Next(after.In(s.loc))
	if s.fixedHour {
		for i := 0; i < 2 && !n.IsZero() && repeatedWallClock(n); i++ {
			n = s.s.Next(n)
		}
	}
	return n.UTC()
}

// repeatedWallClock reports whether t's local wall-clock time already happened
// earlier because of a fall-back DST transition.
func repeatedWallClock(t time.Time) bool {
	_, offNow := t.Zone()
	_, offBefore := t.Add(-3 * time.Hour).Zone()
	if offBefore <= offNow {
		return false
	}
	earlier := t.Add(-time.Duration(offBefore-offNow) * time.Second)
	return earlier.Format("2006-01-02 15:04") == t.Format("2006-01-02 15:04")
}

// NextN returns the next n run times after t.
func (s Spec) NextN(after time.Time, n int) []time.Time {
	out := make([]time.Time, 0, n)
	t := after
	for range n {
		t = s.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out
}

func (s Spec) String() string { return s.expr }
