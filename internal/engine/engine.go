// Package engine is the scheduler core: a Tick-driven policy state machine.
//
// The daemon loop calls Tick on a short interval (~10s). Tick never sleeps
// until a target time — it asks "which fires have elapsed by now?" — so a
// delayed wake (App Nap, timer coalescing, machine sleep) bounds the worst
// case to one tick interval and every elapsed fire is individually
// accounted for in the history (RFP §3). All time flows in through
// clock.Clock and Tick's now parameter; nothing here calls time.Now().
package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nlink-jp/task-clock/internal/clock"
	"github.com/nlink-jp/task-clock/internal/config"
	"github.com/nlink-jp/task-clock/internal/schedule"
	"github.com/nlink-jp/task-clock/internal/store"
)

// ErrAlreadyRunning is returned by Trigger when the task has a live run.
var ErrAlreadyRunning = errors.New("task is already running")

// ErrUnknownTask is returned for a name with no definition.
var ErrUnknownTask = errors.New("unknown task")

// ErrDisabled is returned by Trigger for a disabled task.
var ErrDisabled = errors.New("task is disabled")

// DefaultOnTimeSlack separates "started at its fire" from "ran late":
// a fire handled within this window records on_time, beyond it queued.
// It must comfortably exceed the tick interval.
const DefaultOnTimeSlack = 60 * time.Second

// coalescedDetailLimit caps how many individual missed(coalesced) rows one
// delay produces. Beyond it (think: per-minute cron over a week of sleep)
// the backlog collapses into a single summary row — the cap itself is
// recorded in that row, never applied silently.
const coalescedDetailLimit = 50

// RunSpec describes one process launch.
type RunSpec struct {
	Task    string
	Argv    []string
	Workdir string
	Env     []string // full child environment
	LogPath string
}

// Result is how a finished process reports back.
type Result struct {
	ExitCode int
	Err      string // failure description beyond the exit code, if any
}

// Handle controls a started process.
type Handle interface {
	Kill()               // best-effort terminate (SIGTERM, then SIGKILL)
	Done() <-chan Result // closed-over channel receiving exactly one Result
	Pid() int            // process id (== its process group id)
}

// Runner launches processes. Injected so tests never spawn real ones.
type Runner interface {
	Start(spec RunSpec) (Handle, error)
}

// Event types delivered to Options.Notify.
const (
	EventMissed        = "missed"         // a fire was dropped (actionable reasons only)
	EventFailure       = "failure"        // a run exited non-zero or was killed
	EventOverrunStreak = "overrun_streak" // N consecutive fires did not start on time
)

// Event is a notification-worthy occurrence. The engine only emits these;
// what to do with them (hook commands) is the caller's business.
type Event struct {
	Type         string
	Task         string
	ScheduledFor time.Time
	Reason       string // missed reason (EventMissed)
	ExitCode     int    // EventFailure
	Error        string // EventFailure detail
	Streak       int    // EventOverrunStreak
}

// Options are the injectable dependencies beyond the required three.
type Options struct {
	LookupEnv   func(string) (string, bool) // defaults to os.LookupEnv
	BaseEnv     func() []string             // defaults to os.Environ
	OnTimeSlack time.Duration               // defaults to DefaultOnTimeSlack

	// Notify receives events (on their own goroutines; ordering between
	// events is not guaranteed). Nil disables emission.
	Notify func(Event)
	// OverrunStreakThreshold fires EventOverrunStreak when a task reaches
	// exactly this many consecutive not-on-time fires; it re-arms after an
	// on-time fire resets the streak.
	OverrunStreakThreshold int

	// Released-run adoption hooks (injected for tests; real defaults talk
	// to the OS). PidAlive: is the process alive. VerifyPid: does the
	// process still look like the run we released (identity guard against
	// pid reuse). KillPgid: signal a released run's process group.
	PidAlive  func(pid int) bool
	VerifyPid func(pid int, argv0 string) bool
	KillPgid  func(pid int)
}

// Engine owns the per-task scheduling state.
type Engine struct {
	mu     sync.Mutex
	clock  clock.Clock
	store  *store.Store
	runner Runner
	opts   Options

	tasks map[string]*taskState
	order []string
}

type taskState struct {
	cfg      config.TaskConfig
	spec     schedule.Spec // zero for watermark tasks
	nextFire time.Time     // cron tasks only
	running  *runningRun
	queued   *time.Time // scheduled_for of the one queued fire (cron only)

	// Watermark state: the trigger condition is "enough time elapsed since
	// the last success (and since the last attempt, so a failing task
	// retries at the same cadence instead of crash-looping)".
	lastSuccess      time.Time // finish time of the last exit-0 run
	lastAttemptStart time.Time // start time of the last run, any outcome

	// notOnTimeStreak counts consecutive fires that did not start on time
	// (queued or missed) — the early warning for the positive-feedback
	// loop that motivated task-clock.
	notOnTimeStreak int

	// paused is runtime-only intentional silence (API pause/resume): fires
	// are neither run nor recorded as missed, and the flag does not
	// survive a daemon restart.
	paused bool

	// released is a run left alive by a previous daemon (never killed —
	// field incident 2026-09-02) and re-adopted on startup: it counts as
	// running for overlap/timeout/log-cap purposes, its death is detected
	// by liveness polling, and its exit status is unknowable (not our
	// child).
	released *releasedRun
}

type releasedRun struct {
	id           int64
	pid          int
	scheduledFor time.Time
	startedAt    time.Time
	logPath      string
	timedOut     bool
	logCapKilled bool
}

// busy reports whether the task has a live run — owned or adopted.
func (st *taskState) busy() bool { return st.running != nil || st.released != nil }

// verifyPidCommand guards adoption against pid reuse: the process must
// still be running the command we released. Weak-but-cheap identity — a
// same-argv0 impostor at the same pid within the restart window is
// vanishingly unlikely.
func verifyPidCommand(pid int, argv0 string) bool {
	out, err := exec.Command("ps", "-p", fmt.Sprint(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), argv0)
}

// effectiveDue is when a cron fire actually starts: the fire time plus the
// task's jitter offset. The offset is a hash of the task name and the fire
// time — deterministic, so next_fire stays a queryable pure function while
// different tasks sharing a cron expression spread out.
func (st *taskState) effectiveDue(fire time.Time) time.Time {
	j := st.cfg.Jitter.Value()
	if j <= 0 {
		return fire
	}
	h := fnv.New64a()
	h.Write([]byte(st.cfg.Name))
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(fire.Unix()))
	h.Write(buf[:])
	return fire.Add(time.Duration(h.Sum64() % uint64(j)))
}

// watermarkDue returns when a watermark task is next due — a pure function
// of the recorded history and the interval, so it stays queryable. Zero
// means "never ran: due immediately".
func (st *taskState) watermarkDue() time.Time {
	base := st.lastSuccess
	if st.lastAttemptStart.After(base) {
		base = st.lastAttemptStart
	}
	if base.IsZero() {
		return time.Time{}
	}
	return base.Add(st.cfg.Watermark.Value())
}

type runningRun struct {
	id               int64
	scheduledFor     time.Time
	startedAt        time.Time
	handle           Handle
	logPath          string
	argv0            string
	timedOut         bool
	logCapKilled     bool
	killedForRestart bool
}

// New builds an Engine. Call Configure before the first Tick.
func New(clk clock.Clock, st *store.Store, runner Runner, opts Options) *Engine {
	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}
	if opts.BaseEnv == nil {
		opts.BaseEnv = os.Environ
	}
	if opts.OnTimeSlack == 0 {
		opts.OnTimeSlack = DefaultOnTimeSlack
	}
	if opts.OverrunStreakThreshold == 0 {
		opts.OverrunStreakThreshold = config.DefaultOverrunStreakThreshold
	}
	if opts.PidAlive == nil {
		opts.PidAlive = func(pid int) bool { return syscall.Kill(pid, 0) == nil }
	}
	if opts.VerifyPid == nil {
		opts.VerifyPid = verifyPidCommand
	}
	if opts.KillPgid == nil {
		opts.KillPgid = func(pid int) { syscall.Kill(-pid, syscall.SIGTERM) }
	}
	return &Engine{
		clock:  clk,
		store:  st,
		runner: runner,
		opts:   opts,
		tasks:  map[string]*taskState{},
	}
}

// Configure installs (or replaces, on reload) the task definitions. Tasks
// must already have passed config.Validate. Runtime state (running run,
// queued fire) survives for tasks that keep their name; nextFire survives
// only while the cron expression is unchanged.
func (e *Engine) Configure(tasks []config.TaskConfig) error {
	specs := make(map[string]schedule.Spec, len(tasks))
	for _, t := range tasks {
		if _, dup := specs[t.Name]; dup {
			return fmt.Errorf("task %q: duplicate name", t.Name)
		}
		if t.IsWatermark() {
			specs[t.Name] = schedule.Spec{}
			continue
		}
		spec, err := schedule.Parse(t.Cron)
		if err != nil {
			return fmt.Errorf("task %q: %w", t.Name, err)
		}
		specs[t.Name] = spec
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	// Pause persists across restarts (the operational off switch); a task
	// this process has not seen yet recovers its paused state here.
	persistedPaused, _ := e.store.PausedTasks()
	next := make(map[string]*taskState, len(tasks))
	order := make([]string, 0, len(tasks))
	for _, t := range tasks {
		st := &taskState{cfg: t, spec: specs[t.Name]}
		if prev, ok := e.tasks[t.Name]; ok {
			st.running = prev.running
			st.released = prev.released
			st.queued = prev.queued
			st.lastSuccess = prev.lastSuccess
			st.lastAttemptStart = prev.lastAttemptStart
			st.paused = prev.paused
			if prev.cfg.Cron == t.Cron {
				st.nextFire = prev.nextFire
			}
		} else {
			st.paused = persistedPaused[t.Name]
			if t.IsWatermark() {
				// A fresh watermark task recovers its state from the
				// history, so a daemon restart does not reset the
				// elapsed-since-success clock.
				if run, err := e.store.LastSuccess(t.Name); err == nil && run != nil && run.FinishedAt != nil {
					st.lastSuccess = *run.FinishedAt
				}
				if run, err := e.store.LastRun(t.Name); err == nil && run != nil && run.StartedAt != nil {
					st.lastAttemptStart = *run.StartedAt
				}
			}
		}
		if !t.IsWatermark() && st.nextFire.IsZero() && t.IsEnabled() {
			st.nextFire = st.spec.Next(now)
		}
		next[t.Name] = st
		order = append(order, t.Name)
	}
	// Adopt runs a previous daemon released (never killed — field incident
	// 2026-09-02): a live, identity-verified pid counts as running again,
	// so overlap policies hold and the task cannot double-run. A dead or
	// unverifiable entry is finalized with an unknowable exit.
	if ledger, err := e.store.ReleasedRuns(); err == nil {
		for _, rel := range ledger {
			st, ok := next[rel.Task]
			if ok && st.busy() {
				continue // already tracked (reload path carried it over)
			}
			alive := e.opts.PidAlive(rel.Pid) && e.opts.VerifyPid(rel.Pid, rel.Argv0)
			if ok && alive {
				st.released = &releasedRun{
					id:           rel.RunID,
					pid:          rel.Pid,
					scheduledFor: rel.ScheduledFor,
					startedAt:    rel.StartedAt,
					logPath:      rel.LogPath,
				}
				continue
			}
			if !alive {
				e.store.FinishRun(rel.RunID, -1,
					"released run ended while unmanaged (exit status unknown)", now)
				e.store.ClearReleased(rel.RunID)
			}
			// alive but its task is no longer defined: leave the ledger
			// entry — a later reload may re-define the task and adopt it.
		}
	}

	// Runs of removed tasks keep executing; their finish is recorded via the
	// run id even though the state entry is gone.
	e.tasks = next
	e.order = order
	return nil
}

// Tick advances every task to now: kills timed-out runs, accounts for every
// elapsed fire, and starts what the policies allow.
func (e *Engine) Tick(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, name := range e.order {
		e.tickTask(e.tasks[name], now)
	}
}

func (e *Engine) tickTask(st *taskState, now time.Time) {
	cfg := &st.cfg

	if r := st.running; r != nil && cfg.Timeout.Value() > 0 && !r.timedOut &&
		now.Sub(r.startedAt) >= cfg.Timeout.Value() {
		r.timedOut = true
		r.handle.Kill()
	}

	// Log size cap, tick-checked like the timeout: a runaway task that
	// floods its log gets killed with the reason recorded. (Capping the
	// stream in-flight would need pipe pumping, which hangs Wait when a
	// grandchild inherits the fd — the periodic stat avoids that trap.)
	if r := st.running; r != nil && cfg.LogMaxMB > 0 && !r.timedOut && !r.logCapKilled {
		if info, err := os.Stat(r.logPath); err == nil && info.Size() > int64(cfg.LogMaxMB)<<20 {
			r.logCapKilled = true
			r.handle.Kill()
		}
	}

	// Adopted (released) runs get the same per-task protections — the only
	// difference is the kill channel (pgid signal) and that death is
	// detected by liveness rather than a watch goroutine.
	if rel := st.released; rel != nil {
		if cfg.Timeout.Value() > 0 && !rel.timedOut && now.Sub(rel.startedAt) >= cfg.Timeout.Value() {
			rel.timedOut = true
			e.opts.KillPgid(rel.pid)
		}
		if cfg.LogMaxMB > 0 && !rel.timedOut && !rel.logCapKilled {
			if info, err := os.Stat(rel.logPath); err == nil && info.Size() > int64(cfg.LogMaxMB)<<20 {
				rel.logCapKilled = true
				e.opts.KillPgid(rel.pid)
			}
		}
		if !e.opts.PidAlive(rel.pid) {
			msg := "released run ended (exit status unknown)"
			switch {
			case rel.timedOut:
				msg = "released run killed: timeout exceeded (exit status unknown)"
			case rel.logCapKilled:
				msg = "released run killed: log size cap exceeded (exit status unknown)"
			}
			e.store.FinishRun(rel.id, -1, msg, now)
			e.store.ClearReleased(rel.id)
			st.released = nil
			if st.queued != nil {
				fire := *st.queued
				st.queued = nil
				e.startRunLocked(st, fire, store.OutcomeQueued, now)
			}
		}
	}

	if !cfg.IsEnabled() || st.paused {
		return
	}

	if cfg.IsWatermark() {
		// A watermark re-evaluates only when idle: the current run will
		// move the watermark itself, so there is nothing to queue or miss.
		if st.busy() {
			return
		}
		due := st.watermarkDue()
		if due.IsZero() {
			// Never ran: the backlog is maximally stale — run now.
			e.startRunLocked(st, now, store.OutcomeOnTime, now)
			return
		}
		if now.Before(due) {
			return
		}
		outcome := store.OutcomeOnTime
		if now.Sub(due) > e.opts.OnTimeSlack {
			outcome = store.OutcomeQueued // ran, but later than due
		}
		e.noteFireOutcome(st, outcome == store.OutcomeOnTime)
		e.startRunLocked(st, due, outcome, now)
		return
	}

	if st.nextFire.IsZero() || now.Before(st.effectiveDue(st.nextFire)) {
		return
	}

	// Enumerate the elapsed fires: all but the latest are backlog. The
	// enumeration is capped to bound memory on extreme delays (per-minute
	// cron over a week of sleep); a capped batch simply leaves nextFire in
	// the past, so the following Tick processes the remainder.
	fires := []time.Time{st.nextFire}
	fires = append(fires, schedule.FiresBetween(st.spec, st.nextFire, now, 10000)...)
	latest := fires[len(fires)-1]
	backlog := fires[:len(fires)-1]

	if n := len(backlog); n > coalescedDetailLimit {
		summary := fmt.Sprintf("%d fires between %s and %s coalesced into this row (detail cap %d exceeded)",
			n, backlog[0].UTC().Format(time.RFC3339), backlog[n-1].UTC().Format(time.RFC3339), coalescedDetailLimit)
		e.recordMissed(cfg.Name, backlog[0], store.MissedCoalesced, summary)
	} else {
		for _, f := range backlog {
			e.recordMissed(cfg.Name, f, store.MissedCoalesced, "")
		}
	}

	e.handleFire(st, latest, now)
	st.nextFire = st.spec.Next(latest)
}

// handleFire applies the overlap / catch-up policies to one due fire.
func (e *Engine) handleFire(st *taskState, fire, now time.Time) {
	cfg := &st.cfg
	late := now.Sub(st.effectiveDue(fire)) > e.opts.OnTimeSlack

	if !st.busy() {
		if late && !cfg.CatchUpEnabled() {
			e.noteFireOutcome(st, false)
			e.recordMissed(cfg.Name, fire, store.MissedCatchUpDisabled, "")
			return
		}
		outcome := store.OutcomeOnTime
		if late {
			outcome = store.OutcomeQueued // ran, but later than scheduled
		}
		e.noteFireOutcome(st, outcome == store.OutcomeOnTime)
		e.startRunLocked(st, fire, outcome, now)
		return
	}

	e.noteFireOutcome(st, false) // a fire during a run never starts on time
	switch cfg.OverlapPolicy() {
	case config.OverlapSkip:
		e.recordMissed(cfg.Name, fire, store.MissedOverlap, "")
	case config.OverlapQueueOne:
		if st.queued == nil {
			f := fire
			st.queued = &f
		} else {
			// The queue holds one fire; the earliest keeps its place (it has
			// waited longest), later fires are dropped as overlap misses.
			e.recordMissed(cfg.Name, fire, store.MissedOverlap, "")
		}
	case config.OverlapKillAndRestart:
		if r := st.running; r != nil && !r.killedForRestart {
			r.killedForRestart = true
			r.handle.Kill()
		}
		// An adopted run is killed over its pgid; its death (detected by
		// liveness) then releases the queued fire.
		if rel := st.released; rel != nil && !rel.timedOut && !rel.logCapKilled {
			e.opts.KillPgid(rel.pid)
		}
		if st.queued != nil {
			e.recordMissed(cfg.Name, *st.queued, store.MissedOverlap, "")
		}
		f := fire
		st.queued = &f
	}
}

// noteFireOutcome tracks the consecutive not-on-time streak and emits
// EventOverrunStreak exactly when the threshold is reached; an on-time
// fire re-arms it.
func (e *Engine) noteFireOutcome(st *taskState, onTime bool) {
	if onTime {
		st.notOnTimeStreak = 0
		return
	}
	st.notOnTimeStreak++
	if st.notOnTimeStreak == e.opts.OverrunStreakThreshold {
		e.notify(Event{Type: EventOverrunStreak, Task: st.cfg.Name, Streak: st.notOnTimeStreak})
	}
}

// notify dispatches an event on its own goroutine (callers hold e.mu).
func (e *Engine) notify(ev Event) {
	if e.opts.Notify != nil {
		go e.opts.Notify(ev)
	}
}

// SetNotifier swaps the event callback and streak threshold at runtime —
// hook config is reloadable because nothing about it is structural, unlike
// listen/api_key whose live swap would tear down the very connection
// delivering the reload.
func (e *Engine) SetNotifier(notify func(Event), overrunStreakThreshold int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.opts.Notify = notify
	if overrunStreakThreshold > 0 {
		e.opts.OverrunStreakThreshold = overrunStreakThreshold
	}
}

func (e *Engine) recordMissed(task string, fire time.Time, reason, detail string) {
	id, err := e.store.RecordMissed(task, fire, reason)
	if err != nil {
		return // history write failure must not stop the scheduler
	}
	if detail != "" {
		e.store.FinishRun(id, -1, detail, fire)
	}
	// Coalesced backlog is the by-design result of sleep — not notified.
	if reason == store.MissedOverlap || reason == store.MissedCatchUpDisabled {
		e.notify(Event{Type: EventMissed, Task: task, ScheduledFor: fire, Reason: reason})
	}
}

// startRunLocked launches a run. Caller holds e.mu.
func (e *Engine) startRunLocked(st *taskState, scheduledFor time.Time, outcome string, now time.Time) {
	cfg := &st.cfg
	argv, err := cfg.ResolvedArgv(e.opts.LookupEnv)
	if err == nil && len(argv) == 0 {
		err = errors.New("empty command")
	}
	var workdir string
	if err == nil {
		workdir, err = cfg.ResolvedWorkdir(e.opts.LookupEnv)
	}

	logPath := filepath.Join(e.store.LogDir(), cfg.Name,
		now.UTC().Format("20060102T150405.000000000Z")+".log")

	id, serr := e.store.StartRun(cfg.Name, scheduledFor, outcome, logPath, now)
	if serr != nil {
		return
	}
	if err != nil {
		e.store.FinishRun(id, -1, "launch failed: "+err.Error(), now)
		return
	}

	env := e.opts.BaseEnv()
	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+cfg.Env[k])
	}

	handle, err := e.runner.Start(RunSpec{
		Task:    cfg.Name,
		Argv:    argv,
		Workdir: workdir,
		Env:     env,
		LogPath: logPath,
	})
	if err != nil {
		e.store.FinishRun(id, -1, "launch failed: "+err.Error(), now)
		return
	}
	st.running = &runningRun{
		id:           id,
		scheduledFor: scheduledFor,
		startedAt:    now,
		handle:       handle,
		logPath:      logPath,
		argv0:        argv[0],
	}
	st.lastAttemptStart = now
	go e.watch(cfg.Name, id, handle)
}

// watch waits for a run to finish and feeds the result back in.
func (e *Engine) watch(task string, id int64, handle Handle) {
	res := <-handle.Done()
	e.finishRun(task, id, res)
}

func (e *Engine) finishRun(task string, id int64, res Result) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()

	errMsg := res.Err
	st, ok := e.tasks[task]
	if ok && st.running != nil && st.running.id == id {
		switch {
		case st.running.timedOut:
			errMsg = "killed: timeout exceeded"
		case st.running.logCapKilled:
			errMsg = "killed: log size cap exceeded (log_max_mb)"
		case st.running.killedForRestart:
			errMsg = "killed: overlap kill-and-restart"
		}
	}
	e.store.FinishRun(id, res.ExitCode, errMsg, now)

	if res.ExitCode != 0 || errMsg != "" {
		ev := Event{Type: EventFailure, Task: task, ExitCode: res.ExitCode, Error: errMsg}
		if ok && st.running != nil && st.running.id == id {
			ev.ScheduledFor = st.running.scheduledFor
		}
		e.notify(ev)
	}

	if !ok || st.running == nil || st.running.id != id {
		return // task removed by reload, or a stale watcher
	}
	st.running = nil
	if res.ExitCode == 0 && errMsg == "" {
		st.lastSuccess = now
	}
	if st.queued != nil {
		fire := *st.queued
		st.queued = nil
		e.startRunLocked(st, fire, store.OutcomeQueued, now)
	}
}

// Trigger starts a task immediately (the HTTP trigger endpoint / CLI).
func (e *Engine) Trigger(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.tasks[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTask, name)
	}
	if !st.cfg.IsEnabled() {
		return fmt.Errorf("%w: %s", ErrDisabled, name)
	}
	if st.busy() {
		return fmt.Errorf("%w: %s", ErrAlreadyRunning, name)
	}
	now := e.clock.Now()
	e.startRunLocked(st, now, store.OutcomeManual, now)
	if st.running == nil {
		return fmt.Errorf("task %s failed to launch (see history)", name)
	}
	return nil
}

// Pause suspends a task's scheduling until Resume — persisted, so it
// survives daemon restarts (the operational per-task off switch, distinct
// from the config's declarative `enabled`). Fires during a pause are
// intentionally neither run nor recorded.
func (e *Engine) Pause(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.tasks[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTask, name)
	}
	st.paused = true
	e.store.SetPaused(name, true)
	return nil
}

// Resume lifts a pause. A cron task restarts from the next future fire —
// fires skipped during the pause do not dump in as backlog (the pause was
// deliberate). A stale watermark task fires immediately, which is exactly
// its elapsed-since-success semantic.
func (e *Engine) Resume(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.tasks[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTask, name)
	}
	if st.paused && !st.cfg.IsWatermark() && !st.spec.IsZero() {
		st.nextFire = st.spec.Next(e.clock.Now())
	}
	st.paused = false
	e.store.SetPaused(name, false)
	return nil
}

// ReleaseAll hands every running run over to the released ledger —
// WITHOUT killing. A stopping daemon must never destroy the user's
// running work (field incident 2026-09-02: a daemon stop killed an
// in-flight task and the work was lost). The processes run in their own
// process groups and keep going; their history rows stay open, and the
// ledger (pid + identity) lets the next daemon adopt them so overlap
// policies hold across the restart and the task cannot double-run.
// Kills remain only where explicitly configured per task: timeout,
// log_max_mb, and the kill-and-restart overlap policy.
func (e *Engine) ReleaseAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, name := range e.order {
		st := e.tasks[name]
		if r := st.running; r != nil {
			e.store.MarkReleased(store.ReleasedRun{
				RunID:        r.id,
				Task:         name,
				Pid:          r.handle.Pid(),
				Argv0:        r.argv0,
				ScheduledFor: r.scheduledFor,
				StartedAt:    r.startedAt,
				LogPath:      r.logPath,
			})
			st.running = nil
		}
		// A queued fire cannot survive the restart in memory; record the
		// drop so it stays observable.
		if st.queued != nil {
			e.recordMissed(name, *st.queued, store.MissedDaemonStop, "")
			st.queued = nil
		}
	}
}
