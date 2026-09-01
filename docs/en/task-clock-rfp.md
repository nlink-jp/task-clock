# RFP: task-clock

> Generated: 2026-08-31
> Status: Draft

## 1. Problem Statement

Periodic execution via macOS launchd suffers from structural flaws: timer coalescing for power efficiency can delay or skip firings, and the internal timer state is invisible — there is no way to query the next fire time, and a skipped firing cannot be observed even after the fact. In an actual incident, an AI-agent batch analysis launched on a 30-minute cycle had one run skipped, causing unprocessed data to accumulate, turn execution time to stretch, and token consumption to balloon in a positive-feedback loop.

task-clock is a resident scheduler that does not trust launchd's timing engine: it evaluates cron expressions itself and launches tasks (generic shell commands). It provides, as first-class features, complete "scheduled vs. actual" records, queryable next-fire times, visualization and notification of overruns and skips, and a watermark-style trigger (elapsed time since last success) for tasks whose run time varies.

Target users are individuals and small-scale operators who want to run periodic batch jobs reliably on macOS — especially jobs with variable run time such as AI-agent launches. Tasks not running during sleep is accepted as designed behavior.

## 2. Functional Specification

### Commands / API Surface

Single binary with subcommands (org standard):

```
task-clock serve                        # resident daemon (launched via launchd KeepAlive)
task-clock status  [--json]             # per-task state: next_fire / next_expected_run / overrun / running
task-clock list    [--json]             # task definitions
task-clock history <task> [--json] [--limit N]   # scheduled-vs-actual history
task-clock trigger <task>               # manual trigger
task-clock reload                       # reload config / tasks.d
task-clock validate                     # config validation + next-fire preview per task
task-clock install / uninstall          # generate/register the launchd plist (KeepAlive only)
```

HTTP API (localhost only, token auth) — the concrete form of the API-call trigger requirement:

```
GET  /v1/tasks                     list + state
GET  /v1/tasks/{name}              state detail (next_fire, next_expected_run, overrun, last_run)
POST /v1/tasks/{name}/trigger      external trigger
GET  /v1/tasks/{name}/history      history
POST /v1/reload                    reload configuration
GET  /v1/healthz                   liveness
```

CLI status / history / trigger / reload all go through the HTTP API (single channel). The daemon is the single source of truth for state, keeping the SQLite schema an internal implementation detail. When the daemon is down, the CLI says so explicitly.

**API authentication** — a static API key. Deliberately a simple "set a key in the config file" implementation; no OAuth or other elaborate schemes:

- The key is set as `[daemon] api_key` in `config.toml`. **`serve` refuses to start when it is unset** (fail-closed; `validate` also catches it). The README shows a one-liner such as `openssl rand -hex 32` for generating one
- Every endpoint requires `Authorization: Bearer <key>`; the only exception is `/v1/healthz` (a static response carrying no information)
- Comparison is **constant-time** (crypto/subtle). The key value is never logged
- File permissions are checked on config load: if a file containing `api_key` is group/other-readable, startup is refused (with `chmod 600` guidance) — per the org rule that configs holding credentials must check permissions
- Binds to 127.0.0.1 only (never 0.0.0.0). No CORS headers are served, denying cross-origin browser calls by default. Error messages are static (no input echoed); JSON responses go through json.Marshal
- Rate limiting is **deliberately omitted** (excessive for a localhost + key-required threat model; revisit only if the no-remote-exposure premise ever changes)

### Input / Output

**Status model** — "next" is split into two fields:

- `next_fire` — the next scheduled firing, a pure function of the cron expression and the current time
- `next_expected_run` — the expected execution after applying overlap / catch-up policies. While overrunning under queue-one it is the state word "immediately after current run finishes"; under skip it is the next fire time

Overruns are displayed not as a negative countdown but as an **explicit state word plus a positive value ("overrun by N min")**; the sign stays in the data model.

**Execution history (SQLite)** — recorded per firing:

- `scheduled_for` / `started_at` / `finished_at` / exit code
- firing outcome: `on_time` / `queued` / `missed(reason)` — reconciling scheduled vs. actual makes skips and drift observable after the fact

**Run logs** — stdout/stderr of each run saved as files under the data directory; retention period is configurable.

### Configuration

```
~/.config/task-clock/config.toml      # [daemon] listen / token / retention / hooks
~/.config/task-clock/tasks.d/*.toml   # multiple [[task]]; task names unique across files
data: ~/Library/Application Support/task-clock/   # SQLite / run logs
```

Task definition fields: cron expression, command, workdir, env, timeout, overlap policy (`skip` / `queue-one` / `kill-and-restart`, default `queue-one`), catch-up on/off (default: on), `enabled`.

**Command form** — argv array by default, no shell involved:

```toml
command = ["/path/to/bin", "--out", "${HOME}/data"]
```

- `${VAR}` is expanded by task-clock itself (mcp.json style). Resolution order: the task's `env` table, then the daemon's process environment
- `$${` escapes a literal `${`
- **Undefined variables are errors** (recorded in history as a launch failure; `validate` catches them ahead of time). No silent expansion to empty strings
- Only tasks that need pipes etc. opt in with `shell = true`, interpreted via `/bin/sh -c` (command is then a string)

**Notification hooks** — arbitrary commands registered on `on_missed` / `on_overrun_streak` / `on_failure` (e.g. Slack notification via swrite). The daemon itself carries no notification implementation.

### External Dependencies

No external services. A cron-expression parser library (robfig/cron assumed). launchd (KeepAlive) is depended on only for keeping the daemon resident.

## 3. Design Decisions

- **Language**: daemon + CLI in Go (org standard, pure Go, no CGO). The GUI is a Swift/SwiftUI menu-bar resident app developed later as a **separate project, task-clock-gui** (same shape as active-lens / active-lens-gui: a thin frontend bundling the signed CLI and calling it with `--json`)
- **Timing-engine design (App Nap / timer coalescing countermeasures)**:
  - No "sleep on one long timer until the next fire". Instead, a short-interval (~10 s) ticker polling loop checks whether any task is due, bounding worst-case delay to the ticker interval even if individual wakes are delayed
  - The plist generated by `install` includes `ProcessType: Interactive` to avoid background resource throttling
  - Since scheduled vs. actual is always recorded, drift is self-measured; only if measurements prove insufficient does an NSProcessInfo activity assertion (cgo) become a Phase 2 contingency
- **Next-fire visibility**: with cron expressions, the next fire is a pure function of the expression and the current time, so API / CLI / GUI all display the same computed value (the essential differentiation from launchd's hidden timers)
- **Defensive policy defaults**: post-hoc investigation of the originating incident (by an AI) could not determine the root cause (coalescing vs. skip-while-running unknown). Defaults are therefore overlap = `queue-one` and catch-up = enabled, so that either path of losing a firing (timer delay or skip-while-running) recovers with "run once as soon as noticed". Overridable per task
- **Complementing existing tools**: notification hooks connect naturally to swrite (chatops-series) for Slack. If remote triggering is ever needed, webhook-relay can relay to the localhost API (out of initial scope)
- **Explicitly out of scope**: sleep control (pmset wake etc.), multi-machine distribution, task-dependency DAGs, cron `@reboot`-style directives, full launchd replacement (launchd is still used for residency), Linux / Windows support (macOS only)

## 4. Development Plan

### Phase 1: Core (minimal form releasable on its own)

- Daemon: cron evaluation (designed as pure functions with an injected clock for testability), argv + `${VAR}` expansion execution, timeout kill, all three overlap policies (kill machinery shared with timeout)
- SQLite history (`scheduled_for` / `started_at` / firing outcome), run-log storage + retention
- HTTP API (token auth) + all CLI subcommands, config + tasks.d loading + reload, install / uninstall (generating the `ProcessType: Interactive` plist)
- Tests: centered on next-fire computation, variable expansion, and the policy state machine

### Phase 2: Features (completing the incident countermeasures)

- Watermark trigger (fire N minutes after last success)
- Notification hooks (`on_missed` / `on_overrun_streak` / `on_failure`)
- Catch-up policy refinement, pause / resume via API, jitter
- App Nap contingency: NSProcessInfo activity assertion if measured drift proves the polling loop insufficient

### Phase 3: Release

- README.md / README.ja.md / CHANGELOG
- make build-all + Developer ID signing + notarization (org standard pipeline)
- Umbrella submodule integration, homebrew-tap consideration, check-org.sh

Each phase is independently reviewable. At the end of Phase 1 the core value as a launchd alternative (accurate firing + full observability) already stands. The GUI (task-clock-gui) starts as a separate project after task-clock v0.1 ships, providing a next-fire countdown, overrun display, and a history view.

## 5. Required API Scopes / Permissions

**None** — no external service integration. The HTTP API is localhost-only with its own token auth.

## 6. Series Placement

Series: **util-series**

Reason: the resident-daemon + CLI + separate-GUI-project shape matches active-lens / active-lens-gui and the util-series pattern. It is not a client for an external service (not cli-series) and not Slack automation (not chatops-series); it is a general-purpose local tool.

## 7. External Platform Constraints

- **launchd**: depended on only for residency (KeepAlive); not trusted as a timing engine (the reason this tool exists). Restart-on-crash behavior follows launchd's KeepAlive semantics
- **App Nap / timer coalescing**: macOS may coalesce and throttle timers of background processes. Addressed by the polling loop + `ProcessType: Interactive` per §3, with drift monitored via self-measurement
- **Signing / distribution**: Developer ID signing + notarization required for macOS distribution (org standard pipeline)
- **Sleep**: tasks do not run during sleep; accepted as designed (stated in the Problem Statement)

---

## Discussion Log

- **Origin**: on another machine, a 30-minute launchd job ran an hour late (one firing skipped); for an AI-agent batch analysis this produced a positive-feedback incident of data accumulation → longer turns → token blowup. Discussed the need for a self-built scheduler
- **Upstream check first**: investigated launchd-side fixes before building. Confirmed timer coalescing is the default (OS X 10.9+) and that `LegacyTimers: true` in the plist switches to precise firing. Also, launchd drops (does not queue) a firing while the same-label job is still running, so "previous run exceeded 30 minutes → firing skipped" is a candidate root cause. Recommended verifying the root cause on the affected machine; either way, launchd's structural flaw — non-execution is unobservable — remains, so building our own was judged appropriate
- **Observability as a requirement**: launchd cannot be queried for next fire time and skips are unobservable. With cron expressions the next fire is derivable as a pure function, and recording both scheduled and actual enables drift measurement, missed detection, and watchdog self-checks — made first-class requirements
- **Overrun display**: considered a negative countdown vs. "expected next run after current finishes"; decided they are different pieces of information (overrun magnitude belongs to the current run; the next-run projection is the policy-applied `next_expected_run`) and both are kept. Display uses the state word "overrun by N min", not a signed value
- **Notification**: "notify after N consecutive overruns" adopted as the early warning for the positive-feedback failure. Implemented as hook commands (the daemon delegates to e.g. swrite, carrying no notification code itself)
- **Watermark trigger**: for AI-agent workloads the token-cost driver is the backlog since the last success, so a "N minutes since last success" trigger is in scope alongside cron expressions (implementation in Phase 2)
- **Naming**: considered cron-beat / task-clock / task-conductor; settled on **task-clock** (GUI: task-clock-gui)
- **Config**: chose tasks.d/ split (anticipating dynamic add/remove via GUI/API). CLI↔daemon communication unified over the HTTP API
- **Command form**: argv array with `${VAR}` expansion performed by task-clock itself (mcp.json style, proposed by the user). Shell is opt-in. Undefined variables are errors
- **Platform**: macOS only (the problem domain is launchd-specific)
- **App Nap**: explicitly required per user feedback. Short-interval ticker polling + `ProcessType: Interactive` + drift self-measurement; a cgo assertion is a Phase 2 contingency
- **Phase 2 implementation decisions (2026-08-31)**: (1) watermark is **mutually exclusive** with cron; the fire condition is "max(last success, last attempt start) + interval" — structurally ruling out crash-loops on failure, with state recovered from the history. It never re-evaluates while running, so overlap/catch_up/jitter do not apply (validate rejects them). (2) Hook event details travel as TASK_CLOCK_* environment variables (no argv templating). Coalesced (sleep-borne) misses are deliberately not notified; the streak fires exactly at the threshold and re-arms on an on-time fire. (3) pause is runtime-only and paused fires are not even recorded (deliberate silence is not an incident); resume does not dump backlog. (4) jitter is a deterministic hash(task, fire-time) offset — randomness would break next_fire's purity, the tool's core differentiation
- **API authentication settled (2026-08-31)**: per user request, codified as a simple "static key in the config file" scheme. Only the essentials are mandated within that simplicity — fail-closed (no key, no start), Bearer header, constant-time comparison, config permission check (0600), never logging the key, 127.0.0.1-only bind, no CORS. Rate limiting deliberately omitted as excessive for the localhost threat model
- **pause made persistent (2026-08-31)**: originally runtime-only (cleared on restart); changed on user request for a GUI enable/disable control. Having the GUI rewrite the config's `enabled` would trespass on the out-of-scope tasks.d editing, so pause is instead persisted in SQLite (an internal detail) and promoted to the daemon-owned operational off switch. Two layers with distinct owners: `enabled` = declarative baseline (config), `paused` = operational state (API/GUI)
- **Follow-up on the incident machine (2026-08-31)**: an AI-driven investigation could not identify the root cause (past events are also hard to trace given unified-log retention limits). This itself corroborates the Problem Statement — launchd records neither why nor even that a run did not happen. Designing under an unknown root cause, the policy defaults were set defensively (overlap = queue-one, catch-up = enabled)
- **Field incident: daemon stop killed tasks (2026-09-02)**: quitting the GUI app took the daemon down with it, and a running task was killed — work lost. Two root causes: (1) the bundled CLI's `install` wrote `os.Executable()` (an .app-interior path) into the plist, chaining the daemon to the app's lifecycle. Fix: `install` self-copies the binary to `data_dir/bin/` and the plist points only there. (2) shutdown killed running tasks. The first fix proposal ("release without tracking") was rejected by the user — a recovering daemon, ignorant of the orphan, would start the next fire and **double-run** the task. Settled design: ReleaseAll leaves the run row open and records it in a released_runs ledger (pid/argv0/timestamps); the next daemon verifies liveness + identity (ps match) and adopts the orphan, holding the overlap policy against it. timeout / log_max_mb / kill-and-restart still apply to an adopted run via pgid signals, but its exit code is fundamentally unknowable (the row closes as "exit status unknown"). A queued fire that cannot survive the stop is recorded as `missed(daemon_stop)`. Promoted to an invariant: the daemon kills a task only where explicitly configured
- **start/stop added — run state separated from setup (2026-09-02)**: incident follow-up, raised by the user — the GUI's power switch conflated launch-agent registration with the run state, so stopping meant uninstalling. `start`/`stop` subcommands separate the layers. stop is `launchctl disable` + `bootout` (without the disable, RunAtLoad would revive a deliberately stopped daemon at the next login — the switch-holds-durable-intent principle); start and install `enable` to clear it. stop rides the release semantics, so running tasks are never killed
- **Pre-release verification pass hardened the design (2026-09-02)**: an independent verification (org norm) confirmed four defects in the adoption design, revised as follows — (1) identity moved from an argv0 substring match to the **process start time** (eliminating both the false negative — an in-run exec → double-run — and the false positive — pid reuse → killing an unrelated process; re-verified before every signal). (2) The ledger moved from stop-time writes to a **spawn-time live-run registry** — adoption now also works after a crash (KeepAlive restarts within seconds), and the shutdown-ordering races vanish structurally. (3) Kills of adopted runs escalate TERM→KILL (a TERM-ignoring process cannot hold the task busy forever). (4) "exit unknown" is recorded as a NULL exit_code (unknown ≠ failure) + FinishRun gained an open-row guard (a real exit recorded during the stop window can no longer be overwritten by a later finalization)
