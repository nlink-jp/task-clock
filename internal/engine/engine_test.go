package engine

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nlink-jp/task-clock/internal/config"
	"github.com/nlink-jp/task-clock/internal/store"
)

// --- test doubles -----------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

type fakeHandle struct {
	done   chan Result
	pid    int
	mu     sync.Mutex
	killed bool
}

func (h *fakeHandle) Done() <-chan Result { return h.done }

func (h *fakeHandle) Kill() {
	h.mu.Lock()
	h.killed = true
	h.mu.Unlock()
}

func (h *fakeHandle) wasKilled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.killed
}

func (h *fakeHandle) finish(r Result) { h.done <- r }

func (h *fakeHandle) Pid() int { return h.pid }

type fakeRunner struct {
	mu       sync.Mutex
	specs    []RunSpec
	handles  []*fakeHandle
	startErr error
}

func (r *fakeRunner) Start(spec RunSpec) (Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return nil, r.startErr
	}
	h := &fakeHandle{done: make(chan Result, 1), pid: 10_000 + len(r.handles)}
	r.specs = append(r.specs, spec)
	r.handles = append(r.handles, h)
	return h, nil
}

func (r *fakeRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.handles)
}

func (r *fakeRunner) handle(i int) *fakeHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handles[i]
}

func (r *fakeRunner) spec(i int) RunSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.specs[i]
}

// --- helpers ----------------------------------------------------------------

func at(t *testing.T, v string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func newTestEngine(t *testing.T, start time.Time, tasks ...config.TaskConfig) (*Engine, *fakeClock, *fakeRunner, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &fakeClock{t: start}
	runner := &fakeRunner{}
	e := New(clk, st, runner, Options{
		LookupEnv: func(string) (string, bool) { return "", false },
		BaseEnv:   func() []string { return []string{"BASE=1"} },
	})
	if err := e.Configure(tasks); err != nil {
		t.Fatal(err)
	}
	return e, clk, runner, st
}

func task(name, cron string, mut ...func(*config.TaskConfig)) config.TaskConfig {
	t := config.TaskConfig{
		Name:    name,
		Cron:    cron,
		Command: config.CommandValue{Argv: []string{"/bin/echo", "hi"}},
	}
	for _, m := range mut {
		m(&t)
	}
	return t
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func history(t *testing.T, st *store.Store, name string) []store.Run {
	t.Helper()
	runs, err := st.History(name, 0)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// --- tests ------------------------------------------------------------------

func TestOnTimeFire(t *testing.T) {
	start := at(t, "2026-08-31T10:00:01Z")
	e, clk, runner, st := newTestEngine(t, start, task("a", "*/30 * * * *"))

	clk.Set(at(t, "2026-08-31T10:29:59Z"))
	e.Tick(clk.Now())
	if runner.count() != 0 {
		t.Fatal("fired before the scheduled time")
	}

	clk.Set(at(t, "2026-08-31T10:30:05Z"))
	e.Tick(clk.Now())
	if runner.count() != 1 {
		t.Fatalf("runner started %d times, want 1", runner.count())
	}
	runs := history(t, st, "a")
	if len(runs) != 1 || runs[0].Outcome != store.OutcomeOnTime {
		t.Fatalf("history = %+v, want one on_time row", runs)
	}
	if !runs[0].ScheduledFor.Equal(at(t, "2026-08-31T10:30:00Z")) {
		t.Errorf("scheduled_for = %v", runs[0].ScheduledFor)
	}

	// Same tick window must not double-fire.
	e.Tick(clk.Now())
	if runner.count() != 1 {
		t.Fatal("double-fired within one window")
	}

	clk.Set(at(t, "2026-08-31T10:33:00Z"))
	runner.handle(0).finish(Result{ExitCode: 0})
	waitFor(t, "finish recorded", func() bool {
		r := history(t, st, "a")
		return len(r) == 1 && r[0].FinishedAt != nil
	})
	runs = history(t, st, "a")
	if runs[0].ExitCode == nil || *runs[0].ExitCode != 0 {
		t.Errorf("exit code not recorded: %+v", runs[0])
	}
}

// The originating incident: a 30-minute schedule delayed by over an hour.
// Every elapsed fire must be accounted for — backlog as missed(coalesced),
// the latest as a late (queued) catch-up run.
func TestDelayCatchUp(t *testing.T) {
	start := at(t, "2026-08-31T10:00:01Z")
	e, clk, runner, st := newTestEngine(t, start, task("a", "*/30 * * * *"))

	clk.Set(at(t, "2026-08-31T11:31:30Z")) // slept through 10:30, 11:00, 11:30
	e.Tick(clk.Now())

	if runner.count() != 1 {
		t.Fatalf("runner started %d times, want 1 (coalesced catch-up)", runner.count())
	}
	runs := history(t, st, "a") // newest first
	if len(runs) != 3 {
		t.Fatalf("history = %d rows, want 3", len(runs))
	}
	if runs[0].Outcome != store.OutcomeQueued || !runs[0].ScheduledFor.Equal(at(t, "2026-08-31T11:30:00Z")) {
		t.Errorf("latest fire: %+v, want queued @11:30", runs[0])
	}
	for _, r := range runs[1:] {
		if r.Outcome != store.OutcomeMissed || r.MissedReason != store.MissedCoalesced {
			t.Errorf("backlog fire: %+v, want missed(coalesced)", r)
		}
	}
}

func TestDelayWithCatchUpDisabled(t *testing.T) {
	off := false
	start := at(t, "2026-08-31T10:00:01Z")
	e, clk, runner, st := newTestEngine(t, start,
		task("a", "*/30 * * * *", func(c *config.TaskConfig) { c.CatchUp = &off }))

	clk.Set(at(t, "2026-08-31T11:31:30Z"))
	e.Tick(clk.Now())

	if runner.count() != 0 {
		t.Fatal("catch_up=false must not launch a late run")
	}
	runs := history(t, st, "a")
	if len(runs) != 3 {
		t.Fatalf("history = %d rows, want 3", len(runs))
	}
	if runs[0].MissedReason != store.MissedCatchUpDisabled {
		t.Errorf("latest fire: %+v, want missed(catch_up_disabled)", runs[0])
	}
}

func TestQueueOneOverlap(t *testing.T) {
	start := at(t, "2026-08-31T09:59:59Z")
	e, clk, runner, st := newTestEngine(t, start, task("a", "0 * * * *"))

	clk.Set(at(t, "2026-08-31T10:00:05Z"))
	e.Tick(clk.Now()) // starts the 10:00 run
	clk.Set(at(t, "2026-08-31T11:00:05Z"))
	e.Tick(clk.Now()) // still running: 11:00 queued
	clk.Set(at(t, "2026-08-31T12:00:05Z"))
	e.Tick(clk.Now()) // queue full: 12:00 dropped

	if runner.count() != 1 {
		t.Fatalf("second run started while first still running")
	}
	status := e.Status()[0]
	if status.QueuedFor == nil || !status.QueuedFor.Equal(at(t, "2026-08-31T11:00:00Z")) {
		t.Errorf("queued_for = %v, want 11:00 (earliest keeps its place)", status.QueuedFor)
	}
	if status.NextExpectedRun.Kind != "after_current" {
		t.Errorf("next_expected_run = %+v, want after_current", status.NextExpectedRun)
	}
	if status.OverrunSeconds < 3600 {
		t.Errorf("overrun_seconds = %v, want >= 3600", status.OverrunSeconds)
	}

	runner.handle(0).finish(Result{ExitCode: 0})
	waitFor(t, "queued run start", func() bool { return runner.count() == 2 })
	runs := history(t, st, "a")
	// newest first: queued run (11:00), missed 12:00, first run (10:00)
	if runs[0].Outcome != store.OutcomeQueued || !runs[0].ScheduledFor.Equal(at(t, "2026-08-31T11:00:00Z")) {
		t.Errorf("queued run row: %+v", runs[0])
	}
	if runs[1].Outcome != store.OutcomeMissed || runs[1].MissedReason != store.MissedOverlap {
		t.Errorf("dropped fire row: %+v", runs[1])
	}
}

func TestSkipOverlap(t *testing.T) {
	start := at(t, "2026-08-31T09:59:59Z")
	e, clk, runner, st := newTestEngine(t, start,
		task("a", "0 * * * *", func(c *config.TaskConfig) { c.Overlap = config.OverlapSkip }))

	clk.Set(at(t, "2026-08-31T10:00:05Z"))
	e.Tick(clk.Now())
	clk.Set(at(t, "2026-08-31T11:00:05Z"))
	e.Tick(clk.Now())

	runs := history(t, st, "a")
	if len(runs) != 2 || runs[0].Outcome != store.OutcomeMissed || runs[0].MissedReason != store.MissedOverlap {
		t.Fatalf("history = %+v, want missed(overlap) on top", runs)
	}
	runner.handle(0).finish(Result{ExitCode: 0})
	waitFor(t, "finish recorded", func() bool {
		r := history(t, st, "a")
		return r[1].FinishedAt != nil
	})
	if runner.count() != 1 {
		t.Error("skip policy must not queue a follow-up run")
	}
}

func TestKillAndRestartOverlap(t *testing.T) {
	start := at(t, "2026-08-31T09:59:59Z")
	e, clk, runner, st := newTestEngine(t, start,
		task("a", "0 * * * *", func(c *config.TaskConfig) { c.Overlap = config.OverlapKillAndRestart }))

	clk.Set(at(t, "2026-08-31T10:00:05Z"))
	e.Tick(clk.Now())
	clk.Set(at(t, "2026-08-31T11:00:05Z"))
	e.Tick(clk.Now())

	first := runner.handle(0)
	if !first.wasKilled() {
		t.Fatal("running task not killed by kill-and-restart")
	}
	first.finish(Result{ExitCode: -1, Err: "signal: terminated"})
	waitFor(t, "restart", func() bool { return runner.count() == 2 })

	runs := history(t, st, "a")
	if runs[1].Error != "killed: overlap kill-and-restart" {
		t.Errorf("killed run error = %q", runs[1].Error)
	}
	if runs[0].Outcome != store.OutcomeQueued || !runs[0].ScheduledFor.Equal(at(t, "2026-08-31T11:00:00Z")) {
		t.Errorf("restarted run row: %+v", runs[0])
	}
}

func TestTimeoutKill(t *testing.T) {
	start := at(t, "2026-08-31T09:59:59Z")
	e, clk, runner, st := newTestEngine(t, start,
		task("a", "0 * * * *", func(c *config.TaskConfig) { c.Timeout = config.Duration(5 * time.Minute) }))

	clk.Set(at(t, "2026-08-31T10:00:05Z"))
	e.Tick(clk.Now())
	clk.Set(at(t, "2026-08-31T10:04:00Z"))
	e.Tick(clk.Now())
	if runner.handle(0).wasKilled() {
		t.Fatal("killed before timeout")
	}
	clk.Set(at(t, "2026-08-31T10:06:00Z"))
	e.Tick(clk.Now())
	if !runner.handle(0).wasKilled() {
		t.Fatal("not killed after timeout")
	}
	runner.handle(0).finish(Result{ExitCode: -1, Err: "signal: killed"})
	waitFor(t, "finish recorded", func() bool {
		r := history(t, st, "a")
		return len(r) == 1 && r[0].FinishedAt != nil
	})
	if got := history(t, st, "a")[0].Error; got != "killed: timeout exceeded" {
		t.Errorf("error = %q, want timeout message", got)
	}
}

func TestTrigger(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	off := false
	e, _, runner, st := newTestEngine(t, start,
		task("a", "0 0 1 1 *"),
		task("dis", "0 0 1 1 *", func(c *config.TaskConfig) { c.Enabled = &off }))

	if err := e.Trigger("nope"); !errors.Is(err, ErrUnknownTask) {
		t.Errorf("Trigger(nope) = %v, want ErrUnknownTask", err)
	}
	if err := e.Trigger("dis"); !errors.Is(err, ErrDisabled) {
		t.Errorf("Trigger(dis) = %v, want ErrDisabled", err)
	}
	if err := e.Trigger("a"); err != nil {
		t.Fatal(err)
	}
	if err := e.Trigger("a"); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second Trigger = %v, want ErrAlreadyRunning", err)
	}
	if runner.count() != 1 {
		t.Fatalf("runner count = %d", runner.count())
	}
	if runs := history(t, st, "a"); runs[0].Outcome != store.OutcomeManual {
		t.Errorf("outcome = %q, want manual", runs[0].Outcome)
	}
}

func TestLaunchFailureRecorded(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, _, runner, st := newTestEngine(t, start, task("a", "0 0 1 1 *"))
	runner.startErr = errors.New("exec format error")

	if err := e.Trigger("a"); err == nil {
		t.Fatal("Trigger should surface launch failure")
	}
	runs := history(t, st, "a")
	if len(runs) != 1 || runs[0].ExitCode == nil || *runs[0].ExitCode != -1 {
		t.Fatalf("history = %+v", runs)
	}
	if runs[0].Error == "" {
		t.Error("launch failure not described")
	}
}

func TestUndefinedVarFailsLaunch(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, _, runner, st := newTestEngine(t, start,
		task("a", "0 0 1 1 *", func(c *config.TaskConfig) {
			c.Command = config.CommandValue{Argv: []string{"/bin/echo", "${UNDEFINED_XYZ}"}}
		}))
	if err := e.Trigger("a"); err == nil {
		t.Fatal("undefined variable must fail the launch, not expand to empty")
	}
	if runner.count() != 0 {
		t.Error("process must not start")
	}
	runs := history(t, st, "a")
	if len(runs) != 1 || runs[0].Error == "" {
		t.Fatalf("failure not recorded: %+v", runs)
	}
}

func TestRunSpecEnvAndExpansion(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, _, runner, _ := newTestEngine(t, start,
		task("a", "0 0 1 1 *", func(c *config.TaskConfig) {
			c.Command = config.CommandValue{Argv: []string{"/bin/echo", "${FOO}"}}
			c.Env = map[string]string{"FOO": "bar"}
		}))
	if err := e.Trigger("a"); err != nil {
		t.Fatal(err)
	}
	spec := runner.spec(0)
	if spec.Argv[1] != "bar" {
		t.Errorf("argv = %v, task env not expanded", spec.Argv)
	}
	if len(spec.Env) != 2 || spec.Env[0] != "BASE=1" || spec.Env[1] != "FOO=bar" {
		t.Errorf("env = %v", spec.Env)
	}
	if spec.LogPath == "" {
		t.Error("log path not assigned")
	}
}

func TestDisabledTaskNeverFires(t *testing.T) {
	off := false
	start := at(t, "2026-08-31T10:00:00Z")
	e, clk, runner, _ := newTestEngine(t, start,
		task("a", "* * * * *", func(c *config.TaskConfig) { c.Enabled = &off }))
	clk.Set(at(t, "2026-08-31T12:00:00Z"))
	e.Tick(clk.Now())
	if runner.count() != 0 {
		t.Fatal("disabled task fired")
	}
	if s := e.Status()[0]; s.NextExpectedRun.Kind != "none" {
		t.Errorf("status kind = %q, want none", s.NextExpectedRun.Kind)
	}
}

func TestReconfigureKeepsNextFireForUnchangedCron(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, clk, _, _ := newTestEngine(t, start, task("a", "0 12 * * *"))
	before := *e.Status()[0].NextFire

	clk.Set(at(t, "2026-08-31T10:05:00Z"))
	if err := e.Configure([]config.TaskConfig{task("a", "0 12 * * *")}); err != nil {
		t.Fatal(err)
	}
	if after := *e.Status()[0].NextFire; !after.Equal(before) {
		t.Errorf("unchanged cron: next_fire moved %v -> %v", before, after)
	}

	if err := e.Configure([]config.TaskConfig{task("a", "0 13 * * *")}); err != nil {
		t.Fatal(err)
	}
	if after := *e.Status()[0].NextFire; !after.Equal(at(t, "2026-08-31T13:00:00Z")) {
		t.Errorf("changed cron: next_fire = %v", after)
	}
}

func TestStatusNormalShape(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, _, _, _ := newTestEngine(t, start, task("a", "*/30 * * * *"))
	s := e.Status()[0]
	if s.NextFire == nil || !s.NextFire.Equal(at(t, "2026-08-31T10:30:00Z")) {
		t.Errorf("next_fire = %v", s.NextFire)
	}
	if s.NextExpectedRun.Kind != "at" || s.NextExpectedRun.At == nil ||
		!s.NextExpectedRun.At.Equal(*s.NextFire) {
		t.Errorf("next_expected_run = %+v", s.NextExpectedRun)
	}
	if s.Overlap != config.OverlapQueueOne || !s.CatchUp {
		t.Errorf("defensive defaults not surfaced: %+v", s)
	}
}

// Concurrency probe: ticks, finishes, and triggers race under -race. The
// invariant is one live run per task and no panics (org lesson: prove
// concurrency with a concurrent test, not by reading the code).
func TestConcurrentTickFinishTrigger(t *testing.T) {
	start := at(t, "2026-08-31T10:00:00Z")
	e, clk, runner, _ := newTestEngine(t, start, task("a", "* * * * *"))

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Each handle must be finished exactly once; the shared cursor keeps
	// the mid-test finisher and the final drain from double-finishing.
	var finMu sync.Mutex
	finishedCount := 0
	finishNext := func() bool {
		finMu.Lock()
		defer finMu.Unlock()
		if finishedCount < runner.count() {
			runner.handle(finishedCount).finish(Result{ExitCode: 0})
			finishedCount++
			return true
		}
		return false
	}

	wg.Add(1)
	go func() { // advancing ticker
		defer wg.Done()
		now := start
		for range 200 {
			now = now.Add(30 * time.Second)
			clk.Set(now)
			e.Tick(now)
		}
		close(stop)
	}()

	wg.Add(1)
	go func() { // finisher: completes whatever is running
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if !finishNext() {
					time.Sleep(100 * time.Microsecond)
				}
			}
		}
	}()

	wg.Add(1)
	go func() { // trigger noise: most calls fail with ErrAlreadyRunning — fine
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				e.Trigger("a")
				time.Sleep(200 * time.Microsecond)
			}
		}
	}()

	wg.Wait()
	// Drain: a finish can start a queued run, so keep finishing until the
	// engine reports fully idle.
	waitFor(t, "all runs finished", func() bool {
		finishNext()
		s := e.Status()[0]
		return s.Running == nil && s.QueuedFor == nil
	})
}
