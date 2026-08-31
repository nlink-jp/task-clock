package engine

import (
	"testing"
	"time"

	"github.com/nlink-jp/task-clock/internal/config"
)

func jitterTask(name string, jitter time.Duration) config.TaskConfig {
	return task(name, "0 * * * *", func(c *config.TaskConfig) {
		c.Jitter = config.Duration(jitter)
	})
}

// The jitter offset is deterministic (same task + fire = same offset) and
// bounded by the configured maximum — next_fire stays a pure function.
func TestJitterDeterministicAndBounded(t *testing.T) {
	fire := at(t, "2026-08-31T10:00:00Z")
	stA := &taskState{cfg: jitterTask("a", 5*time.Minute)}
	stB := &taskState{cfg: jitterTask("b", 5*time.Minute)}

	dueA1, dueA2 := stA.effectiveDue(fire), stA.effectiveDue(fire)
	if !dueA1.Equal(dueA2) {
		t.Fatalf("jitter not deterministic: %v vs %v", dueA1, dueA2)
	}
	for _, st := range []*taskState{stA, stB} {
		due := st.effectiveDue(fire)
		off := due.Sub(fire)
		if off < 0 || off >= 5*time.Minute {
			t.Errorf("task %s: offset %v out of [0, 5m)", st.cfg.Name, off)
		}
	}
	// Different tasks on the same fire spread (overwhelmingly likely for
	// any usable hash; pinned here so a degenerate hash change gets caught).
	if stA.effectiveDue(fire).Equal(stB.effectiveDue(fire)) {
		t.Errorf("tasks a and b got identical offsets — hash degenerate?")
	}
}

func TestJitterZeroIsIdentity(t *testing.T) {
	fire := at(t, "2026-08-31T10:00:00Z")
	st := &taskState{cfg: task("a", "0 * * * *")}
	if !st.effectiveDue(fire).Equal(fire) {
		t.Error("no jitter configured must mean no offset")
	}
}

// The task fires at the jittered time, not the raw cron time, and the
// on-time window is measured from the jittered due.
func TestJitterDelaysFireAndStatus(t *testing.T) {
	start := at(t, "2026-08-31T09:00:01Z")
	cfg := jitterTask("a", 10*time.Minute)
	e, clk, runner, st := newTestEngine(t, start, cfg)

	// Compute the expected jittered due for the 10:00 fire.
	fire := at(t, "2026-08-31T10:00:00Z")
	due := (&taskState{cfg: cfg}).effectiveDue(fire)
	if due.Equal(fire) {
		t.Skip("degenerate zero offset for this name/fire; covered by determinism test")
	}

	// Status shows the jittered time.
	if s := e.Status()[0]; s.NextFire == nil || !s.NextFire.Equal(due) {
		t.Fatalf("status next_fire = %v, want jittered %v", s.NextFire, due)
	}

	// At the raw cron time: not yet due.
	clk.Set(fire.Add(5 * time.Second))
	e.Tick(clk.Now())
	if runner.count() != 0 {
		t.Fatal("fired at the raw cron time despite jitter")
	}
	// Just after the jittered due: fires, and counts as on_time.
	clk.Set(due.Add(5 * time.Second))
	e.Tick(clk.Now())
	if runner.count() != 1 {
		t.Fatal("did not fire after the jittered due")
	}
	runs := history(t, st, "a")
	if runs[0].Outcome != "on_time" {
		t.Errorf("outcome = %q, want on_time (lateness measured from jittered due)", runs[0].Outcome)
	}
	if !runs[0].ScheduledFor.Equal(fire) {
		t.Errorf("scheduled_for = %v, want the raw cron time %v", runs[0].ScheduledFor, fire)
	}
}
