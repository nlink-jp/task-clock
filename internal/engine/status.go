package engine

import "time"

// NextExpected is the policy-applied execution outlook — deliberately a
// separate field from next_fire, because "next per the cron expression"
// and "when it will actually run" diverge under overlap/catch-up (RFP §2).
type NextExpected struct {
	// Kind: "at" (a concrete time), "after_current" (immediately after the
	// running task finishes — a state, not a predictable timestamp),
	// "after_success" (a running watermark task: the next due time is
	// recomputed from this run's outcome), or "none" (disabled).
	Kind string     `json:"kind"`
	At   *time.Time `json:"at,omitempty"`
}

// RunningStatus describes the live run of a task.
type RunningStatus struct {
	ScheduledFor   time.Time `json:"scheduled_for"`
	StartedAt      time.Time `json:"started_at"`
	ElapsedSeconds float64   `json:"elapsed_seconds"`
}

// TaskStatus is one task's row in the status listing.
type TaskStatus struct {
	Name            string         `json:"name"`
	Enabled         bool           `json:"enabled"`
	Paused          bool           `json:"paused,omitempty"`
	Cron            string         `json:"cron,omitempty"`
	Watermark       string         `json:"watermark,omitempty"`
	Overlap         string         `json:"overlap"`
	CatchUp         bool           `json:"catch_up"`
	NextFire        *time.Time     `json:"next_fire,omitempty"`
	NextExpectedRun NextExpected   `json:"next_expected_run"`
	Running         *RunningStatus `json:"running,omitempty"`
	// ReleasedUnmanaged: the run predates this daemon (adopted from the
	// released ledger) — alive and honoring policies, but its exit status
	// will be unknowable.
	ReleasedUnmanaged bool `json:"released_unmanaged,omitempty"`
	QueuedFor       *time.Time     `json:"queued_for,omitempty"`
	// OverrunSeconds is how far past a due fire the current run has pushed
	// the schedule (displayed as the "overrun by N" state, never as a
	// negative countdown — RFP §2).
	OverrunSeconds float64 `json:"overrun_seconds,omitempty"`
}

// Status returns the current view of every task, in definition order.
func (e *Engine) Status() []TaskStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()

	out := make([]TaskStatus, 0, len(e.order))
	for _, name := range e.order {
		st := e.tasks[name]
		ts := TaskStatus{
			Name:    name,
			Enabled: st.cfg.IsEnabled(),
			Cron:    st.cfg.Cron,
			Overlap: st.cfg.OverlapPolicy(),
			CatchUp: st.cfg.CatchUpEnabled(),
		}
		if st.cfg.IsWatermark() {
			ts.Watermark = st.cfg.Watermark.Value().String()
			ts.Overlap = ""
			ts.CatchUp = false
		}
		if r := st.running; r != nil {
			ts.Running = &RunningStatus{
				ScheduledFor:   r.scheduledFor,
				StartedAt:      r.startedAt,
				ElapsedSeconds: now.Sub(r.startedAt).Seconds(),
			}
		} else if rel := st.released; rel != nil {
			ts.Running = &RunningStatus{
				ScheduledFor:   rel.scheduledFor,
				StartedAt:      rel.startedAt,
				ElapsedSeconds: now.Sub(rel.startedAt).Seconds(),
			}
			ts.ReleasedUnmanaged = true
		}
		if !ts.Enabled {
			ts.NextExpectedRun = NextExpected{Kind: "none"}
			out = append(out, ts)
			continue
		}
		if st.paused {
			ts.Paused = true
			ts.NextExpectedRun = NextExpected{Kind: "none"}
			out = append(out, ts)
			continue
		}
		if st.cfg.IsWatermark() {
			if st.busy() {
				ts.NextExpectedRun = NextExpected{Kind: "after_success"}
			} else {
				due := st.watermarkDue()
				if due.IsZero() || due.Before(now) {
					due = now // due immediately (next tick)
				}
				ts.NextFire = &due
				ts.NextExpectedRun = NextExpected{Kind: "at", At: ts.NextFire}
			}
			out = append(out, ts)
			continue
		}
		if !st.nextFire.IsZero() {
			// Display the jittered start, not the raw cron time — the
			// status must show when the task will actually run.
			nf := st.effectiveDue(st.nextFire)
			ts.NextFire = &nf
		}
		if st.queued != nil {
			q := *st.queued
			ts.QueuedFor = &q
			ts.NextExpectedRun = NextExpected{Kind: "after_current"}
			if over := now.Sub(q); over > 0 {
				ts.OverrunSeconds = over.Seconds()
			}
		} else {
			ts.NextExpectedRun = NextExpected{Kind: "at", At: ts.NextFire}
		}
		out = append(out, ts)
	}
	return out
}
