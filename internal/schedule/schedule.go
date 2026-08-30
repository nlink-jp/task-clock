// Package schedule evaluates cron expressions as pure functions of the
// expression and a supplied time. Nothing here reads the wall clock: the
// caller passes "now", which is what makes next-fire queryable and testable
// (the essential differentiation from launchd's hidden timers — RFP §3).
package schedule

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// standardParser accepts the standard 5-field cron syntax plus descriptors
// (@hourly, @daily, ..., @every 30m). @reboot is not supported (RFP §3,
// explicitly out of scope).
var standardParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// Spec is a parsed cron expression.
type Spec struct {
	inner cron.Schedule
	raw   string
}

// Parse parses a standard 5-field cron expression or descriptor.
func Parse(expr string) (Spec, error) {
	if expr == "" {
		return Spec{}, fmt.Errorf("empty cron expression")
	}
	sched, err := standardParser.Parse(expr)
	if err != nil {
		return Spec{}, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	return Spec{inner: sched, raw: expr}, nil
}

// Next returns the first fire time strictly after t.
func (s Spec) Next(t time.Time) time.Time { return s.inner.Next(t) }

// String returns the original expression text.
func (s Spec) String() string { return s.raw }

// IsZero reports whether the Spec is the zero value (not parsed).
func (s Spec) IsZero() bool { return s.inner == nil }

// FiresBetween returns every fire time in (after, until], oldest first,
// capped at max entries (0 or negative means no cap). The scheduler uses it
// to enumerate fires that elapsed while the process was delayed (sleep,
// timer coalescing), so each one can be individually accounted for in the
// history instead of vanishing.
func FiresBetween(s Spec, after, until time.Time, max int) []time.Time {
	var fires []time.Time
	t := after
	for {
		t = s.Next(t)
		if t.IsZero() || t.After(until) {
			return fires
		}
		fires = append(fires, t)
		if max > 0 && len(fires) >= max {
			return fires
		}
	}
}
