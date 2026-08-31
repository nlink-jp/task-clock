# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) +
[Semantic Versioning](https://semver.org/).

## [0.3.0] - 2026-09-01

### Changed

- `reload` now also applies `[hooks]` and `retention_days` — nothing about
  them is structural, so requiring a restart was an arbitrary asymmetry
  (user feedback). Only `listen` / `api_key` / `data_dir` /
  `tick_interval` still need a restart, and the README now states the
  live-reload vs. restart boundary explicitly

### Docs

- README: full command reference (all twelve subcommands with flags and
  the flags-before-positional convention), the three overlap policies
  defined in place, the reload-vs-restart boundary as a lookup table, and
  the self-referential sample-validation note removed

## [0.2.1] - 2026-08-31

### Fixed

- `task-clock validate` panicked (nil pointer in `Spec.Next`) when any
  watermark task was defined: the next-fire preview assumed every task had
  a cron expression. Watermark tasks now preview their trigger semantics
  ("fires N after the last success"), and the zero `schedule.Spec` answers
  `Next` with the zero time instead of crashing. (Reported by a user —
  thanks for the precise root-cause analysis.)

## [0.2.0] - 2026-08-31

### Added

- The daemon's own operational log now survives launchd: serve tees its
  log lines into a size-rotated `data_dir/daemon.log` (10 MB × 3
  generations), and the launch agent captures crash traces to
  `daemon.err`
- Per-task `log_max_mb`: a run whose captured output exceeds the cap is
  killed with the reason recorded (tick-checked like `timeout`; default
  0 = no cap)

### Changed

- Task samples split into one file per pattern under `examples/tasks.d/`
  (cron-basic / cron-all-options / watermark / shell-pipeline /
  slack-status-report), each validated by the test suite; the monolithic
  `task.example.toml` is gone
- pause now persists across daemon restarts (stored alongside the run
  history): it is the operational per-task off switch, layered under the
  config's declarative `enabled` — added so GUI clients can offer a durable
  enable/disable control without editing the user's tasks.d files

## [0.1.0] - 2026-08-31

### Added

- Phase 2 (RFP §4): watermark trigger (`watermark = "30m"` — fire N after
  the last success; state survives restarts; failures retry at the same
  cadence), notification hooks (`[hooks]` `on_missed` / `on_failure` /
  `on_overrun_streak` with `TASK_CLOCK_*` env vars and a re-arming streak
  threshold), runtime `pause` / `resume` (API + CLI), and deterministic
  per-task cron `jitter`
- Shutdown now drains briefly so runs killed at daemon stop are recorded
  instead of staying "running" in the history forever

- Phase 1 core (RFP §4): resident scheduler daemon (`serve`) with
  tick-driven cron evaluation, overlap policies (`queue-one` / `skip` /
  `kill-and-restart`), catch-up for delayed fires, and per-task timeout kill
- Scheduled-vs-actual run history in SQLite: every fire recorded as
  `on_time` / `queued` / `missed(reason)`, with retention pruning
- Per-run task output capture (combined stdout+stderr) referenced from the
  history
- Localhost HTTP API with static-key Bearer authentication (fail-closed,
  constant-time comparison, config permission gate): tasks / status /
  trigger / history / reload
- CLI subcommands `status` / `list` / `history` / `trigger` / `reload`
  (all through the API, `--json` passthrough), `validate` with next-fire
  preview, `serve`, `install` / `uninstall` (KeepAlive-only
  `ProcessType: Interactive` LaunchAgent)
- Config loading with multi-path search, strict TOML decoding, `tasks.d/`
  task files, and `${VAR}` expansion (undefined variables are errors)
- RFP / design document (docs/ja, docs/en) and example config files
- Project scaffold: CLI dispatch skeleton with `version` / `--version`,
  Makefile (macOS-only build + signing pipeline), signing scripts, docs
