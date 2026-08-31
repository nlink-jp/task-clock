package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/nlink-jp/task-clock/internal/config"
	"github.com/nlink-jp/task-clock/internal/store"
)

// A paused task neither runs nor records missed rows — the silence is
// deliberate, unlike an incident.
func TestPauseSuppressesFiresSilently(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, clk, runner, st := newTestEngine(t, start, task("a", "* * * * *"))

	if err := e.Pause("a"); err != nil {
		t.Fatal(err)
	}
	clk.Set(at(t, "2026-08-31T10:10:00Z"))
	e.Tick(clk.Now())
	if runner.count() != 0 {
		t.Fatal("paused task fired")
	}
	if runs := history(t, st, "a"); len(runs) != 0 {
		t.Fatalf("paused fires recorded: %+v", runs)
	}
	s := e.Status()[0]
	if !s.Paused || s.NextExpectedRun.Kind != "none" {
		t.Errorf("status = %+v, want paused/none", s)
	}
}

// Resume restarts from the next future fire — the fires skipped during the
// pause do not dump in as backlog.
func TestResumeDoesNotDumpBacklog(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, clk, runner, st := newTestEngine(t, start, task("a", "* * * * *"))

	if err := e.Pause("a"); err != nil {
		t.Fatal(err)
	}
	clk.Set(at(t, "2026-08-31T10:30:00Z"))
	if err := e.Resume("a"); err != nil {
		t.Fatal(err)
	}
	e.Tick(clk.Now())
	if runner.count() != 0 {
		t.Fatal("resume dumped paused-period fires")
	}
	if runs := history(t, st, "a"); len(runs) != 0 {
		t.Fatalf("resume recorded backlog: %+v", runs)
	}
	s := e.Status()[0]
	if s.Paused || s.NextFire == nil || !s.NextFire.Equal(at(t, "2026-08-31T10:31:00Z")) {
		t.Errorf("after resume: %+v, want next fire 10:31", s)
	}

	clk.Set(at(t, "2026-08-31T10:31:05Z"))
	e.Tick(clk.Now())
	if runner.count() != 1 {
		t.Fatal("resumed task did not fire at the next future fire")
	}
}

func TestPauseUnknownTask(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, _, _, _ := newTestEngine(t, start, task("a", "* * * * *"))
	if err := e.Pause("nope"); !errors.Is(err, ErrUnknownTask) {
		t.Errorf("Pause(nope) = %v", err)
	}
	if err := e.Resume("nope"); !errors.Is(err, ErrUnknownTask) {
		t.Errorf("Resume(nope) = %v", err)
	}
}

// Pause survives a reload; manual trigger still works while paused
// (explicit human intent overrides the pause).
func TestPauseSurvivesReconfigureAndAllowsTrigger(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, _, runner, _ := newTestEngine(t, start, task("a", "* * * * *"))

	if err := e.Pause("a"); err != nil {
		t.Fatal(err)
	}
	if err := e.Configure([]config.TaskConfig{task("a", "* * * * *")}); err != nil {
		t.Fatal(err)
	}
	if !e.Status()[0].Paused {
		t.Fatal("pause lost across reconfigure")
	}
	if err := e.Trigger("a"); err != nil {
		t.Fatalf("manual trigger while paused: %v", err)
	}
	if runner.count() != 1 {
		t.Fatal("trigger did not start")
	}
}

// Pause is the operational off switch: it must survive a daemon restart
// via the store, and resume must clear it just as durably.
func TestPauseSurvivesRestart(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	start := at(t, "2026-08-31T10:00:00Z")

	newEngine := func() (*Engine, *fakeRunner) {
		clk := &fakeClock{t: start}
		runner := &fakeRunner{}
		e := New(clk, st, runner, Options{
			LookupEnv: func(string) (string, bool) { return "", false },
			BaseEnv:   func() []string { return nil },
		})
		if err := e.Configure([]config.TaskConfig{task("a", "* * * * *")}); err != nil {
			t.Fatal(err)
		}
		return e, runner
	}

	e1, _ := newEngine()
	if err := e1.Pause("a"); err != nil {
		t.Fatal(err)
	}

	// "Restart": a fresh engine over the same store.
	e2, runner2 := newEngine()
	if !e2.Status()[0].Paused {
		t.Fatal("pause lost across restart")
	}
	e2.Tick(at(t, "2026-08-31T12:00:00Z"))
	if runner2.count() != 0 {
		t.Fatal("paused task fired after restart")
	}

	if err := e2.Resume("a"); err != nil {
		t.Fatal(err)
	}
	e3, _ := newEngine()
	if e3.Status()[0].Paused {
		t.Fatal("resume not persisted")
	}
}

// A paused watermark task fires immediately on resume when stale — that is
// the elapsed-since-success semantic, not backlog dumping.
func TestResumeStaleWatermarkFires(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, clk, runner, _ := newTestEngine(t, start, wmTask("w", 30*time.Minute))

	clk.Set(at(t, "2026-08-31T10:00:10Z"))
	e.Tick(clk.Now()) // bootstrap
	clk.Set(at(t, "2026-08-31T10:05:00Z"))
	runner.handle(0).finish(Result{ExitCode: 0})
	waitFor(t, "finish", func() bool { return e.Status()[0].Running == nil })

	e.Pause("w")
	clk.Set(at(t, "2026-08-31T12:00:00Z")) // long past due (10:35)
	e.Tick(clk.Now())
	if runner.count() != 1 {
		t.Fatal("paused watermark fired")
	}
	e.Resume("w")
	e.Tick(clk.Now())
	if runner.count() != 2 {
		t.Fatal("stale watermark did not fire on resume")
	}
}
