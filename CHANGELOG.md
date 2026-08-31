# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) +
[Semantic Versioning](https://semver.org/).

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
