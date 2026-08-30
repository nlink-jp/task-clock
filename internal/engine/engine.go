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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
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
}

// Runner launches processes. Injected so tests never spawn real ones.
type Runner interface {
	Start(spec RunSpec) (Handle, error)
}

// Options are the injectable dependencies beyond the required three.
type Options struct {
	LookupEnv   func(string) (string, bool) // defaults to os.LookupEnv
	BaseEnv     func() []string             // defaults to os.Environ
	OnTimeSlack time.Duration               // defaults to DefaultOnTimeSlack
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
	spec     schedule.Spec
	nextFire time.Time
	running  *runningRun
	queued   *time.Time // scheduled_for of the one queued fire
}

type runningRun struct {
	id               int64
	scheduledFor     time.Time
	startedAt        time.Time
	handle           Handle
	timedOut         bool
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
		spec, err := schedule.Parse(t.Cron)
		if err != nil {
			return fmt.Errorf("task %q: %w", t.Name, err)
		}
		if _, dup := specs[t.Name]; dup {
			return fmt.Errorf("task %q: duplicate name", t.Name)
		}
		specs[t.Name] = spec
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	next := make(map[string]*taskState, len(tasks))
	order := make([]string, 0, len(tasks))
	for _, t := range tasks {
		st := &taskState{cfg: t, spec: specs[t.Name]}
		if prev, ok := e.tasks[t.Name]; ok {
			st.running = prev.running
			st.queued = prev.queued
			if prev.cfg.Cron == t.Cron {
				st.nextFire = prev.nextFire
			}
		}
		if st.nextFire.IsZero() && t.IsEnabled() {
			st.nextFire = st.spec.Next(now)
		}
		next[t.Name] = st
		order = append(order, t.Name)
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

	if !cfg.IsEnabled() || st.nextFire.IsZero() || now.Before(st.nextFire) {
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
	late := now.Sub(fire) > e.opts.OnTimeSlack

	if st.running == nil {
		if late && !cfg.CatchUpEnabled() {
			e.recordMissed(cfg.Name, fire, store.MissedCatchUpDisabled, "")
			return
		}
		outcome := store.OutcomeOnTime
		if late {
			outcome = store.OutcomeQueued // ran, but later than scheduled
		}
		e.startRunLocked(st, fire, outcome, now)
		return
	}

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
		if !st.running.killedForRestart {
			st.running.killedForRestart = true
			st.running.handle.Kill()
		}
		if st.queued != nil {
			e.recordMissed(cfg.Name, *st.queued, store.MissedOverlap, "")
		}
		f := fire
		st.queued = &f
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
	}
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
		case st.running.killedForRestart:
			errMsg = "killed: overlap kill-and-restart"
		}
	}
	e.store.FinishRun(id, res.ExitCode, errMsg, now)

	if !ok || st.running == nil || st.running.id != id {
		return // task removed by reload, or a stale watcher
	}
	st.running = nil
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
	if st.running != nil {
		return fmt.Errorf("%w: %s", ErrAlreadyRunning, name)
	}
	now := e.clock.Now()
	e.startRunLocked(st, now, store.OutcomeManual, now)
	if st.running == nil {
		return fmt.Errorf("task %s failed to launch (see history)", name)
	}
	return nil
}

// KillAll terminates every running task (daemon shutdown).
func (e *Engine) KillAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, name := range e.order {
		if r := e.tasks[name].running; r != nil {
			r.handle.Kill()
		}
	}
}
