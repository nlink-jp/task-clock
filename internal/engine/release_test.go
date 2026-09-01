package engine

import (
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/nlink-jp/task-clock/internal/config"
	"github.com/nlink-jp/task-clock/internal/store"
)

// Test double for the adoption hooks: liveness and process identity fully
// under test control, kills recorded as "pgid:SIGNAME".
type fakeOS struct {
	alive    map[int]bool
	identity map[int]string // what ProcessIdentity answers NOW
	killed   []string
}

func newFakeOS() *fakeOS {
	return &fakeOS{alive: map[int]bool{}, identity: map[int]string{}}
}

func (f *fakeOS) options(base Options) Options {
	base.PidAlive = func(pid int) bool { return f.alive[pid] }
	base.ProcessIdentity = func(pid int) string { return f.identity[pid] }
	base.KillPgid = func(pgid int, sig syscall.Signal) {
		f.killed = append(f.killed, fmt.Sprintf("%d:%v", pgid, sig))
	}
	return base
}

// fakeRunnerFirstPid is the pid fakeRunner.Start assigns to its first
// handle (10_000 + index).
const fakeRunnerFirstPid = 10_000

// spawned marks pid as a live process with a stable identity, as the OS
// would after a real spawn — ProcessIdentity is consulted at spawn time to
// record the identity, so this must be set BEFORE the run starts.
func (f *fakeOS) spawned(pid int) {
	f.alive[pid] = true
	f.identity[pid] = fmt.Sprintf("lstart-%d", pid)
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
	osHooks := newFakeOS()
	osHooks.spawned(fakeRunnerFirstPid)
	start := at(t, "2026-09-02T09:59:59Z")

	// Daemon #1: start the 10:00 run — registered at spawn — then stop.
	e1, clk1, runner1 := newReleaseEngine(t, st, start, osHooks, task("a", "0 * * * *"))
	clk1.Set(at(t, "2026-09-02T10:00:05Z"))
	e1.Tick(clk1.Now())
	if runner1.count() != 1 {
		t.Fatal("run did not start")
	}
	pid := runner1.handle(0).Pid()
	registry, _ := st.LiveRuns()
	if len(registry) != 1 || registry[0].Pid != pid || registry[0].Identity == "" {
		t.Fatalf("spawn must register the live run: %+v", registry)
	}
	e1.ReleaseAll()
	if runner1.handle(0).wasKilled() {
		t.Fatal("ReleaseAll must never kill")
	}
	run, _ := st.LastRun("a")
	if run.FinishedAt != nil {
		t.Fatal("released run's row must stay open")
	}

	// Daemon #2: the orphan is alive with its recorded identity — adopt.
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

	// The orphan dies → row finalized (exit unknowable = NULL, not a
	// failure code), queued fire runs.
	osHooks.alive[pid] = false
	clk2.Set(at(t, "2026-09-02T11:05:00Z"))
	e2.Tick(clk2.Now())
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
	if releasedRow.ExitCode != nil {
		t.Fatalf("unknowable exit must stay NULL, got %d", *releasedRow.ExitCode)
	}
	if runner2.count() != 1 {
		t.Fatal("queued fire did not start after the orphan ended")
	}
}

// A crash leaves no ReleaseAll behind — the registry is written at spawn,
// so the next daemon (KeepAlive restarts a crashed daemon within seconds)
// adopts the orphan all the same and the no-double-run promise holds.
func TestCrashedDaemonRunsAreAdopted(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	osHooks := newFakeOS()
	osHooks.spawned(fakeRunnerFirstPid)

	e1, clk1, runner1 := newReleaseEngine(t, st, at(t, "2026-09-02T09:59:59Z"), osHooks, task("a", "0 * * * *"))
	clk1.Set(at(t, "2026-09-02T10:00:05Z"))
	e1.Tick(clk1.Now())
	pid := runner1.handle(0).Pid()
	// Crash: no ReleaseAll — the engine simply ceases to exist.

	e2, clk2, runner2 := newReleaseEngine(t, st, at(t, "2026-09-02T10:00:30Z"), osHooks, task("a", "0 * * * *"))
	if s := e2.Status()[0]; s.Running == nil || !s.ReleasedUnmanaged {
		t.Fatalf("crash orphan not adopted: %+v", s)
	}
	clk2.Set(at(t, "2026-09-02T11:00:05Z"))
	e2.Tick(clk2.Now())
	if runner2.count() != 0 {
		t.Fatal("double-run after crash: fire started alongside the orphan")
	}
	_ = pid
}

// A dead pid — or a live one whose identity no longer matches (pid reuse;
// including a shell task that exec'd its real binary being IMITATED by a
// stranger) — is finalized at adoption time, and scheduling proceeds.
func TestAdoptionRejectsDeadOrImpostorPid(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	osHooks := newFakeOS()
	osHooks.spawned(fakeRunnerFirstPid)

	e1, clk1, runner1 := newReleaseEngine(t, st, at(t, "2026-09-02T09:59:59Z"), osHooks, task("a", "0 * * * *"))
	clk1.Set(at(t, "2026-09-02T10:00:05Z"))
	e1.Tick(clk1.Now())
	pid := runner1.handle(0).Pid()
	e1.ReleaseAll()

	// Same pid alive but a DIFFERENT process (new start time): pid reuse —
	// do not adopt, and above all do not kill it later.
	osHooks.identity[pid] = "lstart-of-a-stranger"
	e2, clk2, runner2 := newReleaseEngine(t, st, at(t, "2026-09-02T10:30:00Z"), osHooks, task("a", "0 * * * *"))
	if s := e2.Status()[0]; s.Running != nil {
		t.Fatalf("impostor pid adopted: %+v", s)
	}
	run, _ := st.LastRun("a")
	if run.FinishedAt == nil {
		t.Fatal("unadoptable released row must be finalized")
	}
	if len(osHooks.killed) != 0 {
		t.Fatalf("a stranger's pid was signalled: %v", osHooks.killed)
	}

	// Scheduling proceeds: the 11:00 fire starts normally.
	clk2.Set(at(t, "2026-09-02T11:00:05Z"))
	e2.Tick(clk2.Now())
	if runner2.count() != 1 {
		t.Fatal("scheduling did not resume after rejecting the registry entry")
	}
}

// An exec inside the run (shell task execs its real binary) does NOT
// change the recorded identity — the process start time survives exec —
// so the orphan is still adopted. This pins the reason ProcessIdentity
// replaced a command-line match.
func TestAdoptionSurvivesExecInsideTheRun(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	osHooks := newFakeOS()
	osHooks.spawned(fakeRunnerFirstPid)

	e1, clk1, runner1 := newReleaseEngine(t, st, at(t, "2026-09-02T09:59:59Z"), osHooks, task("a", "0 * * * *"))
	clk1.Set(at(t, "2026-09-02T10:00:05Z"))
	e1.Tick(clk1.Now())
	pid := runner1.handle(0).Pid()
	e1.ReleaseAll()
	// exec changes the image, not the identity: same pid, same start time.

	e2, _, _ := newReleaseEngine(t, st, at(t, "2026-09-02T10:30:00Z"), osHooks, task("a", "0 * * * *"))
	if s := e2.Status()[0]; s.Running == nil || !s.ReleasedUnmanaged {
		t.Fatalf("exec'd orphan not adopted: %+v", s)
	}
	_ = pid
}

// Timeout applies to an adopted run too — SIGTERM over the pgid, then
// SIGKILL after the grace when the process ignores it — and the finalized
// row names the reason.
func TestAdoptedRunHonorsTimeoutWithEscalation(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	osHooks := newFakeOS()
	osHooks.spawned(fakeRunnerFirstPid)

	timeoutTask := task("a", "0 * * * *", func(c *config.TaskConfig) {
		c.Timeout = config.Duration(5 * time.Minute)
	})
	e1, clk1, runner1 := newReleaseEngine(t, st, at(t, "2026-09-02T09:59:59Z"), osHooks, timeoutTask)
	clk1.Set(at(t, "2026-09-02T10:00:05Z"))
	e1.Tick(clk1.Now())
	pid := runner1.handle(0).Pid()
	e1.ReleaseAll()

	e2, clk2, _ := newReleaseEngine(t, st, at(t, "2026-09-02T10:02:00Z"), osHooks, timeoutTask)
	// Past the timeout (started 10:00:05): SIGTERM first.
	clk2.Set(at(t, "2026-09-02T10:06:00Z"))
	e2.Tick(clk2.Now())
	if want := []string{fmt.Sprintf("%d:terminated", pid)}; len(osHooks.killed) != 1 || osHooks.killed[0] != want[0] {
		t.Fatalf("kills = %v, want %v", osHooks.killed, want)
	}
	// The process ignores SIGTERM: the next tick past the grace escalates
	// to SIGKILL — the task must not stay busy forever.
	clk2.Set(at(t, "2026-09-02T10:06:10Z"))
	e2.Tick(clk2.Now())
	if len(osHooks.killed) != 2 || osHooks.killed[1] != fmt.Sprintf("%d:killed", pid) {
		t.Fatalf("no SIGKILL escalation: %v", osHooks.killed)
	}
	osHooks.alive[pid] = false
	clk2.Set(at(t, "2026-09-02T10:06:30Z"))
	e2.Tick(clk2.Now())
	run, _ := st.LastRun("a")
	if run.Error != "released run killed: timeout exceeded (exit status unknown)" {
		t.Errorf("finalized reason = %q", run.Error)
	}
	if run.ExitCode != nil {
		t.Errorf("unknowable exit must stay NULL, got %d", *run.ExitCode)
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
	osHooks := newFakeOS()
	osHooks.spawned(fakeRunnerFirstPid)
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

// A run that finished during the shutdown window (its real exit recorded
// by the still-alive watch goroutine) must KEEP that result: the next
// daemon's finalization of the stale registry entry is a guarded no-op.
func TestFinalizationNeverOverwritesARealExit(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	osHooks := newFakeOS()
	osHooks.spawned(fakeRunnerFirstPid)

	e1, clk1, runner1 := newReleaseEngine(t, st, at(t, "2026-09-02T09:59:59Z"), osHooks, task("a", "0 * * * *"))
	clk1.Set(at(t, "2026-09-02T10:00:05Z"))
	e1.Tick(clk1.Now())
	pid := runner1.handle(0).Pid()
	e1.ReleaseAll()
	// The run finishes in the shutdown window: the watch goroutine records
	// the real exit. (Delivered synchronously here via the handle.)
	runner1.handle(0).finish(Result{ExitCode: 0})
	waitFinished(t, st, "a")
	osHooks.alive[pid] = false

	// Daemon #2 sees a stale registry entry for a dead pid — its
	// finalization must not clobber the recorded success.
	newReleaseEngine(t, st, at(t, "2026-09-02T10:30:00Z"), osHooks, task("a", "0 * * * *"))
	run, _ := st.LastRun("a")
	if run.ExitCode == nil || *run.ExitCode != 0 || run.Error != "" {
		t.Fatalf("real exit overwritten: %+v", run)
	}
	if registry, _ := st.LiveRuns(); len(registry) != 0 {
		t.Fatalf("stale registry entry not cleared: %+v", registry)
	}
}

// waitFinished waits for the watch goroutine to record the run's finish.
func waitFinished(t *testing.T, st *store.Store, taskName string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if run, _ := st.LastRun(taskName); run != nil && run.FinishedAt != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("run never finished")
}
