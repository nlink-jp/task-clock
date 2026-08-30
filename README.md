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
  and the clock, shown identically by CLI, HTTP API, and GUI
- **Scheduled-vs-actual history**: every firing records `scheduled_for`,
  `started_at`, and an outcome (`on_time` / `queued` / `missed(reason)`), so
  skips and drift are observable facts instead of mysteries
- **Overrun handling as policy**: per-task overlap policy (`queue-one` by
  default, `skip`, `kill-and-restart`) and catch-up, with overruns displayed
  as an explicit state ("overrun by N min"), never a silent stall
- **Watermark trigger**: fire N minutes after the last *successful* run —
  built for variable-duration batch jobs such as AI-agent launches
- **HTTP trigger API** (localhost, token-authenticated) for firing tasks
  from other tools
- **Notification hooks**: run any command on `on_missed` /
  `on_overrun_streak` / `on_failure`
- **No shell surprises**: commands are argv arrays with `${VAR}` expansion
  done by task-clock itself; undefined variables are errors, and
  `/bin/sh -c` is strictly opt-in

## Building

```bash
make build    # outputs dist/task-clock (never use `go build` directly)
make test
```

## Documentation

- [RFP / design document](docs/en/task-clock-rfp.md) ([日本語](docs/ja/task-clock-rfp.ja.md))
- [README in Japanese](README.ja.md)

## License

[MIT](LICENSE)
