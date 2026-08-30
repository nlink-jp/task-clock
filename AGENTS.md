# AGENTS.md — task-clock

## Summary

macOS 用常駐スケジューラー。launchd の timer coalescing による周期タスクの
遅延・スキップ（および launchd のタイマー状態の不可視性）への対策として、
cron 式を自前評価して汎用シェルコマンドを起動し、「予定 vs 実績」を完全
記録する。launchd には KeepAlive による常駐維持のみ依存。設計の正は
`docs/ja/task-clock-rfp.ja.md`。

- Module path: `github.com/nlink-jp/task-clock`
- Language: Go (pure Go, CGO 不要), macOS 専用 (darwin/arm64)
- 依存方針: 標準ライブラリ + cron 式パーサ（robfig/cron 想定）のみ

## Build / test commands

```bash
make build     # dist/task-clock に出力 + Developer ID 署名（go build 直接禁止）
make test      # go test ./...
make vet       # go vet ./...（darwin 専用のためクロス OS matrix なし）
make build-all # darwin/arm64 のみ（macOS 専用ツール）
make package   # zip + notarize
```

## Structure

```
main.go            # package main — cmd.Run() を呼ぶだけ。version は ldflags 注入
cmd/               # CLI dispatch（stdlib flag、cobra 不使用 — org 実運用に合わせる）
docs/ja, docs/en   # RFP / 設計文書
scripts/           # codesign-darwin.sh / notarize-darwin.sh（.github/templates から vendored）
```

Phase 3 で追加予定（RFP §4）: `internal/schedule`（cron 評価・純関数）、
`internal/task`（実行・ポリシー状態機械）、`internal/store`（SQLite 履歴）、
`internal/api`（localhost HTTP）、`internal/config`（TOML + tasks.d + ${VAR} 展開）。

## Gotchas

- **タイミングループは短周期 ticker のポーリング**。長い timer 一本で眠る
  実装は App Nap / timer coalescing で壊れる（RFP §3）。
- 時刻は必ず注入クロック経由。`time.Now()` をロジックに直書きしない。
- SQLite スキーマは内部実装。外部契約は HTTP API（CLI も API 経由で一本化）。
- `${VAR}` 展開で未定義変数は**エラー**。空文字に黙って展開しない。
- `version` サブコマンドと `--version` は同一出力（cmd/cmd_test.go が固定。
  brew test が `--version` を叩くため）。
- config は `~/.config/task-clock/` + `tasks.d/`、データは
  `~/Library/Application Support/task-clock/`。
