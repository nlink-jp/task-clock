package engine

import (
	"testing"
	"time"

	"github.com/nlink-jp/task-clock/internal/config"
	"github.com/nlink-jp/task-clock/internal/store"
)

func wmTask(name string, interval time.Duration, mut ...func(*config.TaskConfig)) config.TaskConfig {
	t := config.TaskConfig{
		Name:      name,
		Watermark: config.Duration(interval),
		Command:   config.CommandValue{Argv: []string{"/bin/echo", "hi"}},
	}
	for _, m := range mut {
		m(&t)
	}
	return t
}

// A watermark task that never ran is maximally stale: it fires on the
// first tick.
func TestWatermarkFirstRunFiresImmediately(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, clk, runner, st := newTestEngine(t, start, wmTask("w", 30*time.Minute))

	clk.Set(at(t, "2026-08-31T10:00:10Z"))
	e.Tick(clk.Now())
	if runner.count() != 1 {
		t.Fatalf("first tick should launch, count = %d", runner.count())
	}
	runs := history(t, st, "w")
	if runs[0].Outcome != store.OutcomeOnTime {
		t.Errorf("bootstrap run outcome = %q", runs[0].Outcome)
	}
}

// After a success, the next due is success-finish + interval.
func TestWatermarkNextDueAfterSuccess(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, clk, runner, _ := newTestEngine(t, start, wmTask("w", 30*time.Minute))

	clk.Set(at(t, "2026-08-31T10:00:10Z"))
	e.Tick(clk.Now()) // bootstrap run
	if s := e.Status()[0]; s.NextExpectedRun.Kind != "after_success" {
		t.Errorf("running watermark task: kind = %q, want after_success", s.NextExpectedRun.Kind)
	}

	clk.Set(at(t, "2026-08-31T10:25:00Z"))
	runner.handle(0).finish(Result{ExitCode: 0})
	waitFor(t, "finish", func() bool { return e.Status()[0].Running == nil })

	s := e.Status()[0]
	want := at(t, "2026-08-31T10:55:00Z") // 10:25 success + 30m
	if s.NextFire == nil || !s.NextFire.Equal(want) {
		t.Fatalf("next due = %v, want %v", s.NextFire, want)
	}

	// Not yet due: no fire.
	clk.Set(at(t, "2026-08-31T10:54:00Z"))
	e.Tick(clk.Now())
	if runner.count() != 1 {
		t.Fatal("fired before the watermark elapsed")
	}
	// Due: fires with scheduled_for = the due time.
	clk.Set(at(t, "2026-08-31T10:55:05Z"))
	e.Tick(clk.Now())
	if runner.count() != 2 {
		t.Fatal("did not fire once due")
	}
}

// A failing task retries at the watermark cadence measured from its last
// attempt — never a crash-loop of immediate refires.
func TestWatermarkFailureDoesNotCrashLoop(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, clk, runner, _ := newTestEngine(t, start, wmTask("w", 30*time.Minute))

	clk.Set(at(t, "2026-08-31T10:00:10Z"))
	e.Tick(clk.Now())
	clk.Set(at(t, "2026-08-31T10:01:00Z"))
	runner.handle(0).finish(Result{ExitCode: 1})
	waitFor(t, "finish", func() bool { return e.Status()[0].Running == nil })

	// Immediately after the failure: must NOT refire.
	e.Tick(clk.Now())
	if runner.count() != 1 {
		t.Fatal("failure caused an immediate refire (crash-loop)")
	}
	// Next due is attempt-start (10:00:10) + 30m.
	s := e.Status()[0]
	want := at(t, "2026-08-31T10:30:10Z")
	if s.NextFire == nil || !s.NextFire.Equal(want) {
		t.Fatalf("next due after failure = %v, want %v", s.NextFire, want)
	}
	clk.Set(at(t, "2026-08-31T10:31:00Z"))
	e.Tick(clk.Now())
	if runner.count() != 2 {
		t.Fatal("retry did not fire after the interval")
	}
}

// A very late fire (e.g. after sleep) records queued, not on_time.
func TestWatermarkLateFireRecordsQueued(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, clk, runner, st := newTestEngine(t, start, wmTask("w", 30*time.Minute))

	clk.Set(at(t, "2026-08-31T10:00:10Z"))
	e.Tick(clk.Now())
	clk.Set(at(t, "2026-08-31T10:05:00Z"))
	runner.handle(0).finish(Result{ExitCode: 0})
	waitFor(t, "finish", func() bool { return e.Status()[0].Running == nil })

	// Slept through the due time (10:35); wake at 11:20.
	clk.Set(at(t, "2026-08-31T11:20:00Z"))
	e.Tick(clk.Now())
	runs := history(t, st, "w")
	if runs[0].Outcome != store.OutcomeQueued {
		t.Errorf("late watermark fire outcome = %q, want queued", runs[0].Outcome)
	}
	if !runs[0].ScheduledFor.Equal(at(t, "2026-08-31T10:35:00Z")) {
		t.Errorf("scheduled_for = %v, want the due time 10:35", runs[0].ScheduledFor)
	}
}

// The elapsed-since-success clock survives a daemon restart via the store.
func TestWatermarkStateRecoveredFromStore(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sched := at(t, "2026-08-31T09:00:00Z")
	id, _ := st.StartRun("w", sched, store.OutcomeOnTime, "", sched)
	st.FinishRun(id, 0, "", at(t, "2026-08-31T09:10:00Z"))

	clk := &fakeClock{t: at(t, "2026-08-31T09:20:00Z")}
	runner := &fakeRunner{}
	e := New(clk, st, runner, Options{
		LookupEnv: func(string) (string, bool) { return "", false },
		BaseEnv:   func() []string { return nil },
	})
	if err := e.Configure([]config.TaskConfig{wmTask("w", 30*time.Minute)}); err != nil {
		t.Fatal(err)
	}

	// Not a bootstrap fire: history says success at 09:10, due 09:40.
	e.Tick(clk.Now())
	if runner.count() != 0 {
		t.Fatal("restart reset the watermark (bootstrap fire happened)")
	}
	s := e.Status()[0]
	if s.NextFire == nil || !s.NextFire.Equal(at(t, "2026-08-31T09:40:00Z")) {
		t.Fatalf("recovered due = %v, want 09:40", s.NextFire)
	}
	clk.Set(at(t, "2026-08-31T09:40:10Z"))
	e.Tick(clk.Now())
	if runner.count() != 1 {
		t.Fatal("recovered watermark did not fire when due")
	}
}

// Reload keeps the in-memory watermark state for surviving tasks.
func TestWatermarkSurvivesReconfigure(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, clk, runner, _ := newTestEngine(t, start, wmTask("w", 30*time.Minute))

	clk.Set(at(t, "2026-08-31T10:00:10Z"))
	e.Tick(clk.Now())
	clk.Set(at(t, "2026-08-31T10:05:00Z"))
	runner.handle(0).finish(Result{ExitCode: 0})
	waitFor(t, "finish", func() bool { return e.Status()[0].Running == nil })

	if err := e.Configure([]config.TaskConfig{wmTask("w", 30*time.Minute)}); err != nil {
		t.Fatal(err)
	}
	s := e.Status()[0]
	if s.NextFire == nil || !s.NextFire.Equal(at(t, "2026-08-31T10:35:00Z")) {
		t.Fatalf("due after reconfigure = %v, want 10:35", s.NextFire)
	}
}
