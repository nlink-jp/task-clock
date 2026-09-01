package engine

import (
	"testing"
	"time"

	"github.com/nlink-jp/task-clock/internal/config"
	"github.com/nlink-jp/task-clock/internal/store"
)

// Test double for the adoption hooks: liveness and identity fully under
// test control, kills recorded.
type fakeOS struct {
	alive    map[int]bool
	identity map[int]bool
	killed   []int
}

func (f *fakeOS) options(base Options) Options {
	base.PidAlive = func(pid int) bool { return f.alive[pid] }
	base.VerifyPid = func(pid int, argv0 string) bool { return f.identity[pid] }
	base.KillPgid = func(pid int) { f.killed = append(f.killed, pid) }
	return base
}

func newReleaseEngine(t *testing.T, st *store.Store, start time.Time, osHooks *fakeOS, tasks ...config.TaskConfig) (*Engine, *fakeClock, *fakeRunner) {
	t.Helper()
	clk := &fakeClock{t: start}
	runner := &fakeRunner{}
	e := New(clk, st, runner, osHooks.options(Options{
		LookupEnv: func(string) (string, bool) { return "", false },
		BaseEnv:   func() []string { return nil },
	}))
	if err := e.Configure(tasks); err != nil {
		t.Fatal(err)
	}
	return e, clk, runner
}

// The full incident scenario: daemon stops mid-run (release, not kill),
// restarts, adopts the live orphan — and the overlap policy holds, so the
// task cannot double-run while the orphan finishes.
func TestReleaseAdoptAndNoDoubleRun(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	osHooks := &fakeOS{alive: map[int]bool{}, identity: map[int]bool{}}
	start := at(t, "2026-09-02T09:59:59Z")

	// Daemon #1: start the 10:00 run, then stop (release).
	e1, clk1, runner1 := newReleaseEngine(t, st, start, osHooks, task("a", "0 * * * *"))
	clk1.Set(at(t, "2026-09-02T10:00:05Z"))
	e1.Tick(clk1.Now())
	if runner1.count() != 1 {
		t.Fatal("run did not start")
	}
	pid := runner1.handle(0).Pid()
	e1.ReleaseAll()
	if runner1.handle(0).wasKilled() {
		t.Fatal("ReleaseAll must never kill")
	}
	ledger, _ := st.ReleasedRuns()
	if len(ledger) != 1 || ledger[0].Pid != pid {
		t.Fatalf("ledger = %+v", ledger)
	}
	run, _ := st.LastRun("a")
	if run.FinishedAt != nil {
		t.Fatal("released run's row must stay open")
	}

	// Daemon #2: the orphan is alive and verified — adopt it.
	osHooks.alive[pid] = true
	osHooks.identity[pid] = true
	e2, clk2, runner2 := newReleaseEngine(t, st, at(t, "2026-09-02T10:30:00Z"), osHooks, task("a", "0 * * * *"))
	status := e2.Status()[0]
	if status.Running == nil || !status.ReleasedUnmanaged {
		t.Fatalf("adopted run not shown as running unmanaged: %+v", status)
	}

	// The 11:00 fire lands while the orphan still runs: queue-one must
	// queue it, NOT start a duplicate.
	clk2.Set(at(t, "2026-09-02T11:00:05Z"))
	e2.Tick(clk2.Now())
	if runner2.count() != 0 {
		t.Fatal("double-run: a fire started alongside the adopted orphan")
	}
	if s := e2.Status()[0]; s.QueuedFor == nil {
		t.Fatal("fire was not queued behind the adopted run")
	}

	// The orphan dies → row finalized (exit unknowable), queued fire runs.
	osHooks.alive[pid] = false
	clk2.Set(at(t, "2026-09-02T11:05:00Z"))
	e2.Tick(clk2.Now())
	run, _ = st.LastRun("a")
	// Newest row is now the queued run; the released row is finalized.
	runs, _ := st.History("a", 10)
	var releasedRow *store.Run
	for i := range runs {
		if runs[i].Error != "" {
			releasedRow = &runs[i]
		}
	}
	if releasedRow == nil || releasedRow.FinishedAt == nil ||
		releasedRow.Error != "released run ended (exit status unknown)" {
		t.Fatalf("released row not finalized properly: %+v", runs)
	}
	if runner2.count() != 1 {
		t.Fatal("queued fire did not start after the orphan ended")
	}
	if ledger, _ := st.ReleasedRuns(); len(ledger) != 0 {
		t.Fatalf("ledger not cleared: %+v", ledger)
	}
	_ = run
}

// A dead or identity-mismatched ledger entry is finalized at adoption
// time, and scheduling proceeds normally (no phantom busy state).
func TestAdoptionRejectsDeadOrImpostorPid(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	osHooks := &fakeOS{alive: map[int]bool{}, identity: map[int]bool{}}
	start := at(t, "2026-09-02T09:59:59Z")

	e1, clk1, runner1 := newReleaseEngine(t, st, start, osHooks, task("a", "0 * * * *"))
	clk1.Set(at(t, "2026-09-02T10:00:05Z"))
	e1.Tick(clk1.Now())
	pid := runner1.handle(0).Pid()
	e1.ReleaseAll()

	// Same pid alive but running something else: pid reuse — do not adopt.
	osHooks.alive[pid] = true
	osHooks.identity[pid] = false
	e2, clk2, runner2 := newReleaseEngine(t, st, at(t, "2026-09-02T10:30:00Z"), osHooks, task("a", "0 * * * *"))
	if s := e2.Status()[0]; s.Running != nil {
		t.Fatalf("impostor pid adopted: %+v", s)
	}
	run, _ := st.LastRun("a")
	if run.FinishedAt == nil {
		t.Fatal("unadoptable released row must be finalized")
	}

	// Scheduling proceeds: the 11:00 fire starts normally.
	clk2.Set(at(t, "2026-09-02T11:00:05Z"))
	e2.Tick(clk2.Now())
	if runner2.count() != 1 {
		t.Fatal("scheduling did not resume after rejecting the ledger entry")
	}
}

// Timeout applies to an adopted run too, over the pgid kill channel, and
// the finalized row names the reason.
func TestAdoptedRunHonorsTimeout(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	osHooks := &fakeOS{alive: map[int]bool{}, identity: map[int]bool{}}
	start := at(t, "2026-09-02T09:59:59Z")

	timeoutTask := task("a", "0 * * * *", func(c *config.TaskConfig) {
		c.Timeout = config.Duration(5 * time.Minute)
	})
	e1, clk1, runner1 := newReleaseEngine(t, st, start, osHooks, timeoutTask)
	clk1.Set(at(t, "2026-09-02T10:00:05Z"))
	e1.Tick(clk1.Now())
	pid := runner1.handle(0).Pid()
	e1.ReleaseAll()

	osHooks.alive[pid] = true
	osHooks.identity[pid] = true
	e2, clk2, _ := newReleaseEngine(t, st, at(t, "2026-09-02T10:02:00Z"), osHooks, timeoutTask)
	// Past the timeout (started 10:00:05): the adopted run gets the pgid kill.
	clk2.Set(at(t, "2026-09-02T10:06:00Z"))
	e2.Tick(clk2.Now())
	if len(osHooks.killed) != 1 || osHooks.killed[0] != pid {
		t.Fatalf("adopted run not killed on timeout: %+v", osHooks.killed)
	}
	osHooks.alive[pid] = false
	clk2.Set(at(t, "2026-09-02T10:06:30Z"))
	e2.Tick(clk2.Now())
	run, _ := st.LastRun("a")
	if run.Error != "released run killed: timeout exceeded (exit status unknown)" {
		t.Errorf("finalized reason = %q", run.Error)
	}
}

// A queued fire that cannot survive the stop is recorded, not silently
// dropped.
func TestReleaseRecordsDroppedQueuedFire(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	osHooks := &fakeOS{alive: map[int]bool{}, identity: map[int]bool{}}
	e, clk, _ := newReleaseEngine(t, st, at(t, "2026-09-02T09:59:59Z"), osHooks, task("a", "0 * * * *"))
	clk.Set(at(t, "2026-09-02T10:00:05Z"))
	e.Tick(clk.Now()) // run starts
	clk.Set(at(t, "2026-09-02T11:00:05Z"))
	e.Tick(clk.Now()) // 11:00 queued
	e.ReleaseAll()
	runs, _ := st.History("a", 10)
	found := false
	for _, r := range runs {
		if r.Outcome == store.OutcomeMissed && r.MissedReason == store.MissedDaemonStop {
			found = true
		}
	}
	if !found {
		t.Fatalf("dropped queued fire not recorded: %+v", runs)
	}
}
