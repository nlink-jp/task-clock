# task-clock

A resident scheduler for macOS that does not trust launchd's timing engine.

launchd coalesces timers for power efficiency, which can delay or skip
periodic jobs — and its timer state is invisible: you cannot ask when the
next fire will happen, and a skipped firing leaves no trace. task-clock
evaluates cron expressions itself, launches your tasks (plain shell
commands), and records everything launchd does not.

## Features

- **Cron-expression scheduling** evaluated in-process — launchd is used only
  to keep the daemon resident (`KeepAlive`), never as the timing engine
- **Queryable next fire**: `next_fire` is a pure function of the expression
  and the clock, shown identically by CLI and HTTP API
- **Scheduled-vs-actual history**: every firing records `scheduled_for`,
  `started_at`, and an outcome (`on_time` / `queued` / `missed(reason)`), so
  skips and drift are observable facts instead of mysteries
- **Overrun handling as policy**: per-task overlap policy (`queue-one` by
  default, `skip`, `kill-and-restart`) and catch-up, with overruns displayed
  as an explicit state ("overrun 12m"), never a silent stall
- **Task output captured**: each run's combined stdout+stderr is saved to a
  per-run log file, referenced from the history, and pruned by retention
- **Watermark trigger**: fire N minutes after the last *successful* run —
  built for variable-duration batch jobs such as AI-agent launches;
  however a fire was lost, the elapsed-since-success condition re-arms
- **Notification hooks**: run any command on `on_missed` / `on_failure` /
  `on_overrun_streak` (the early warning for the feedback loop that
  motivated this tool), with event details in `TASK_CLOCK_*` env vars
- **HTTP trigger API** (localhost, static-key Bearer auth) for firing tasks
  from other tools, plus runtime `pause` / `resume`
- **Deterministic jitter** to spread tasks sharing a cron expression —
  hashed from task and fire time, so `next_fire` stays queryable
- **No shell surprises**: commands are argv arrays with `${VAR}` expansion
  done by task-clock itself; undefined variables are errors, and
  `/bin/sh -c` is strictly opt-in

## Installation

Build from source (macOS, arm64):

```bash
make build
cp dist/task-clock /usr/local/bin/   # or anywhere on PATH
```

Create the config (see below), then register the LaunchAgent:

```bash
task-clock validate     # check config and preview next fires
task-clock install      # writes ~/Library/LaunchAgents/jp.nlink.task-clock.plist
```

The generated plist uses `KeepAlive` (residency) and
`ProcessType: Interactive` (no background throttling) — and deliberately no
launchd timer keys.

## Configuration

Config search order (first directory containing `config.toml` wins):

1. `$TASK_CLOCK_CONFIG_DIR`
2. `$XDG_CONFIG_HOME/task-clock`
3. `~/.config/task-clock` ← canonical location
4. `~/Library/Application Support/task-clock`

Layout:

```
~/.config/task-clock/
├── config.toml      # [daemon] settings — chmod 600, holds api_key
└── tasks.d/
    └── *.toml       # [[task]] definitions, names unique across files
```

Starter files: [config.example.toml](config.example.toml) and
[task.example.toml](task.example.toml). Generate the API key with:

```bash
openssl rand -hex 32
```

The daemon refuses to start without `api_key`, and refuses a
group/other-readable config file that holds one (`chmod 600`).

### Task fields

| Field | Meaning | Default |
|---|---|---|
| `name` | unique task name (`[A-Za-z0-9][A-Za-z0-9._-]*`) | required |
| `cron` | 5-field cron expression or `@hourly` etc. | one of cron / watermark |
| `watermark` | fire this long after the last success (e.g. `"30m"`); mutually exclusive with `cron`, and `overlap` / `catch_up` / `jitter` do not apply | one of cron / watermark |
| `command` | argv array; string only with `shell = true` | required |
| `shell` | interpret `command` via `/bin/sh -c` | `false` |
| `workdir` | working directory (`${VAR}` expanded) | inherit |
| `env` | extra environment (`[task.env]` table); also the first source for `${VAR}` | — |
| `timeout` | kill the run after this duration (e.g. `"25m"`) | none |
| `log_max_mb` | kill the run if its captured output exceeds this size | 0 (no cap) |
| `overlap` | `queue-one` \| `skip` \| `kill-and-restart` | `queue-one` |
| `catch_up` | run a delayed fire as soon as noticed | `true` |
| `jitter` | spread the start by a deterministic 0..jitter offset | none |
| `enabled` | schedule this task | `true` |

### Notification hooks

Optional `[hooks]` table in `config.toml` (changes need a daemon restart):

```toml
[hooks]
on_failure        = ["swrite", "-c", "alerts"]   # non-zero exit or killed
on_missed         = ["swrite", "-c", "alerts"]   # dropped fire (overlap / catch_up off)
on_overrun_streak = ["swrite", "-c", "alerts"]   # N consecutive not-on-time fires
overrun_streak_threshold = 3
```

Hooks receive `TASK_CLOCK_EVENT`, `TASK_CLOCK_TASK`,
`TASK_CLOCK_SCHEDULED_FOR`, `TASK_CLOCK_REASON`, `TASK_CLOCK_EXIT_CODE`,
`TASK_CLOCK_ERROR`, and `TASK_CLOCK_STREAK` in the environment. The
coalesced backlog after sleep is deliberately not notified — sleeping is by
design; the streak hook re-arms after an on-time fire.

## Usage

```bash
task-clock serve                  # run the daemon in the foreground
task-clock status                 # per-task state, next fire, overrun
task-clock list                   # task definitions as the daemon sees them
task-clock history analyze        # scheduled-vs-actual record (+ log paths)
task-clock trigger analyze        # fire a task now
task-clock pause analyze          # suspend scheduling until resume (survives restarts)
task-clock resume analyze         # lift the pause (no backlog dump)
task-clock reload                 # re-read tasks.d (also: SIGHUP)
task-clock validate               # config check + next-fire preview
```

Every query command accepts `--json` for the raw API response and
`-config <dir>` to bypass the search paths.

`status` distinguishes **next fire** (what the cron expression says) from
**next run** (what the policies will actually do): a task overrunning its
schedule shows `running 42m (overrun 12m)` with next run `after current
run` — an explicit state, never a negative countdown.

## HTTP API

Localhost only. Every endpoint except `/v1/healthz` requires
`Authorization: Bearer <api_key>`.

| Endpoint | Meaning |
|---|---|
| `GET /v1/tasks` | all tasks: status + last run |
| `GET /v1/tasks/{name}` | one task |
| `POST /v1/tasks/{name}/trigger` | fire a task (409 if already running) |
| `POST /v1/tasks/{name}/pause` / `.../resume` | suspend / restore scheduling |
| `GET /v1/tasks/{name}/history?limit=N` | run history |
| `POST /v1/reload` | re-read tasks.d |
| `GET /v1/healthz` | liveness (unauthenticated, static) |

```bash
curl -H "Authorization: Bearer $KEY" http://127.0.0.1:17282/v1/tasks
```

## Data and logs

Run history (SQLite) and per-run task output live under
`~/Library/Application Support/task-clock/` (`data_dir`):

```
task-clock.db          # scheduled-vs-actual history
logs/<task>/<ts>.log   # combined stdout+stderr per run
daemon.log[.1..3]      # the daemon's own operational log, size-rotated (10 MB x 3)
daemon.err             # crash traces captured by launchd
```

Run history and task logs are pruned after `retention_days` (default 30);
the daemon's own log rotates by size. Tasks do not run while
the machine sleeps — by design; a fire that was missed while asleep runs
once at wake (`catch_up`), and the skipped ones are recorded as
`missed(coalesced)`.

## Building

```bash
make build    # dist/task-clock, signed (never use `go build` directly)
make test
```

## Documentation

- [RFP / design document](docs/en/task-clock-rfp.md) ([日本語](docs/ja/task-clock-rfp.ja.md))
- [README in Japanese](README.ja.md)

## License

[MIT](LICENSE)
