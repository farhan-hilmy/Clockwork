package scheduler

import (
	"time"

	"github.com/farhan-hilmy/clockwork/internal/schedule"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

// plan is what one scheduler pass does for one due task.
type plan struct {
	// occurrences are the scheduled_for times to enqueue, oldest first.
	occurrences []time.Time
	// next is the new next_run_at, always after now. Zero means the schedule never fires again.
	next time.Time
	// missed reports that at least one occurrence was later than the grace period.
	missed bool
	// catchUp reports that occurrences holds a run_once catch-up run.
	catchUp bool
}

// planDue decides which occurrences of a due task to enqueue.
//
// Occurrences no older than grace are on time and are all enqueued. Older ones
// are misfires (the process was down, or the scheduler fell behind):
//   - skip:     they are dropped.
//   - run_once: if no on-time occurrence is enqueued in this pass, the earliest
//     missed occurrence is enqueued as a single catch-up run.
//
// Either way next_run_at moves to the first occurrence after now, so a long
// outage never replays more than one run. Each enqueued occurrence is a real
// cron time, so two schedulers computing the same pass agree on scheduled_for
// and the unique index on executions rejects the duplicate.
func planDue(spec schedule.Spec, nextRun, now time.Time, grace time.Duration, misfire string) plan {
	var p plan
	cutoff := now.Add(-grace)
	t := nextRun
	if nextRun.Before(cutoff) {
		p.missed = true
		// First occurrence at or after the cutoff. Next is strictly-after, and
		// cron times are whole minutes, so stepping back 1ns includes the cutoff itself.
		t = spec.Next(cutoff.Add(-time.Nanosecond))
	}
	for !t.IsZero() && !t.After(now) {
		p.occurrences = append(p.occurrences, t)
		t = spec.Next(t)
	}
	if p.missed && misfire == tasks.MisfireRunOnce && len(p.occurrences) == 0 {
		p.occurrences = []time.Time{nextRun}
		p.catchUp = true
	}
	p.next = t
	return p
}
