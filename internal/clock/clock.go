// Package clock provides an injectable time source. All scheduling logic
// takes a Clock instead of calling time.Now() directly, so tests can drive
// the scheduler deterministically (see CLAUDE.md invariants).
package clock

import "time"

// Clock is the time source injected into every component that needs "now".
type Clock interface {
	Now() time.Time
}

// Real is the production Clock backed by the system time.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }
