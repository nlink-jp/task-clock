package engine

import (
	"sync"
	"testing"
	"time"

	"github.com/nlink-jp/task-clock/internal/config"
	"github.com/nlink-jp/task-clock/internal/store"
)

type eventSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *eventSink) add(ev Event) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
}

func (s *eventSink) byType(t string) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, ev := range s.events {
		if ev.Type == t {
			out = append(out, ev)
		}
	}
	return out
}

func newEventEngine(t *testing.T, start time.Time, threshold int, tasks ...config.TaskConfig) (*Engine, *fakeClock, *fakeRunner, *eventSink) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &fakeClock{t: start}
	runner := &fakeRunner{}
	sink := &eventSink{}
	e := New(clk, st, runner, Options{
		LookupEnv:              func(string) (string, bool) { return "", false },
		BaseEnv:                func() []string { return nil },
		Notify:                 sink.add,
		OverrunStreakThreshold: threshold,
	})
	if err := e.Configure(tasks); err != nil {
		t.Fatal(err)
	}
	return e, clk, runner, sink
}

func TestFailureEventEmitted(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, _, runner, sink := newEventEngine(t, start, 3, task("a", "0 0 1 1 *"))

	if err := e.Trigger("a"); err != nil {
		t.Fatal(err)
	}
	runner.handle(0).finish(Result{ExitCode: 7})
	waitFor(t, "failure event", func() bool { return len(sink.byType(EventFailure)) == 1 })
	ev := sink.byType(EventFailure)[0]
	if ev.Task != "a" || ev.ExitCode != 7 {
		t.Errorf("failure event = %+v", ev)
	}
}

func TestNoFailureEventOnSuccess(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, _, runner, sink := newEventEngine(t, start, 3, task("a", "0 0 1 1 *"))
	if err := e.Trigger("a"); err != nil {
		t.Fatal(err)
	}
	runner.handle(0).finish(Result{ExitCode: 0})
	waitFor(t, "finish", func() bool { return e.Status()[0].Running == nil })
	time.Sleep(20 * time.Millisecond) // give a wrong event time to surface
	if n := len(sink.byType(EventFailure)); n != 0 {
		t.Errorf("failure events on success = %d", n)
	}
}

func TestMissedEventForOverlapNotCoalesced(t *testing.T) {
	start := at(t, "2026-08-31T09:59:59Z")
	e, clk, _, sink := newEventEngine(t, start, 99,
		task("a", "0 * * * *", func(c *config.TaskConfig) { c.Overlap = config.OverlapSkip }))

	clk.Set(at(t, "2026-08-31T10:00:05Z"))
	e.Tick(clk.Now()) // starts run
	clk.Set(at(t, "2026-08-31T11:00:05Z"))
	e.Tick(clk.Now()) // overlap: missed
	waitFor(t, "missed event", func() bool { return len(sink.byType(EventMissed)) == 1 })
	ev := sink.byType(EventMissed)[0]
	if ev.Reason != store.MissedOverlap || !ev.ScheduledFor.Equal(at(t, "2026-08-31T11:00:00Z")) {
		t.Errorf("missed event = %+v", ev)
	}
}

func TestCoalescedBacklogEmitsNoMissedEvent(t *testing.T) {
	start := at(t, "2026-08-31T10:00:01Z")
	e, clk, _, sink := newEventEngine(t, start, 99, task("a", "*/30 * * * *"))

	clk.Set(at(t, "2026-08-31T11:31:30Z")) // slept through 2 fires + late catch-up
	e.Tick(clk.Now())
	time.Sleep(20 * time.Millisecond)
	if n := len(sink.byType(EventMissed)); n != 0 {
		t.Errorf("coalesced backlog produced %d missed events, want 0 (sleep is by design)", n)
	}
}

// SetNotifier swaps the hook wiring at runtime (config reload): events
// after the swap reach only the new callback, and a new threshold applies.
func TestSetNotifierSwapsAtRuntime(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, _, runner, oldSink := newEventEngine(t, start, 3, task("a", "0 0 1 1 *"))

	newSink := &eventSink{}
	e.SetNotifier(newSink.add, 5)

	if err := e.Trigger("a"); err != nil {
		t.Fatal(err)
	}
	runner.handle(0).finish(Result{ExitCode: 7})
	waitFor(t, "failure event on new sink", func() bool {
		return len(newSink.byType(EventFailure)) == 1
	})
	if len(oldSink.byType(EventFailure)) != 0 {
		t.Error("event leaked to the replaced notifier")
	}
}

// The streak fires exactly at the threshold, once, and re-arms after an
// on-time fire.
func TestOverrunStreakThresholdAndRearm(t *testing.T) {
	start := at(t, "2026-08-31T09:59:59Z")
	e, clk, runner, sink := newEventEngine(t, start, 2,
		task("a", "0 * * * *", func(c *config.TaskConfig) { c.Overlap = config.OverlapSkip }))

	// Fire 1: on time (streak 0).
	clk.Set(at(t, "2026-08-31T10:00:05Z"))
	e.Tick(clk.Now())
	// Fires 2..4 land while the first run is still going: streak 1, 2, 3.
	for _, ts := range []string{"11:00:05", "12:00:05", "13:00:05"} {
		clk.Set(at(t, "2026-08-31T"+ts+"Z"))
		e.Tick(clk.Now())
	}
	waitFor(t, "streak event", func() bool { return len(sink.byType(EventOverrunStreak)) >= 1 })
	events := sink.byType(EventOverrunStreak)
	if len(events) != 1 || events[0].Streak != 2 {
		t.Fatalf("streak events = %+v, want exactly one at streak 2", events)
	}

	// Finish; next on-time fire resets, then two more overruns re-fire.
	runner.handle(0).finish(Result{ExitCode: 0})
	waitFor(t, "finish", func() bool { return e.Status()[0].Running == nil })
	clk.Set(at(t, "2026-08-31T14:00:05Z"))
	e.Tick(clk.Now()) // on time: reset
	for _, ts := range []string{"15:00:05", "16:00:05"} {
		clk.Set(at(t, "2026-08-31T"+ts+"Z"))
		e.Tick(clk.Now())
	}
	waitFor(t, "second streak event", func() bool { return len(sink.byType(EventOverrunStreak)) == 2 })
}
