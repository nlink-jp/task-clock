# AGENTS.md — task-clock

## Summary

macOS 用常駐スケジューラー。launchd の timer coalescing による周期タスクの
遅延・スキップ（および launchd のタイマー状態の不可視性）への対策として、
cron 式を自前評価して汎用シェルコマンドを起動し、「予定 vs 実績」を完全
記録する。launchd には KeepAlive による常駐維持のみ依存。設計の正は
`docs/ja/task-clock-rfp.ja.md`。

- Module path: `github.com/nlink-jp/task-clock`
- Language: Go (pure Go, CGO 不要), macOS 専用 (darwin/arm64)
- 依存: robfig/cron (cron 式), BurntSushi/toml (strict decode),
  modernc.org/sqlite (pure-Go SQLite)。他は標準ライブラリ

## Build / test commands

```bash
make build     # dist/task-clock に出力 + Developer ID 署名（go build 直接禁止）
make test      # go test ./...
make vet       # go vet ./...（darwin 専用のためクロス OS matrix なし）
make build-all # darwin/arm64 のみ（macOS 専用ツール）
make package   # zip + notarize
```

テストは `-race` で回すこと（engine は並行プローブテストを含む）。

## Structure

```
main.go              # cmd.Run() を呼ぶだけ。version は ldflags 注入
cmd/                 # CLI dispatch (stdlib flag) + APIクライアント + serve ループ + install
  cmd.go             #   dispatch。Run(args, stdout, stderr, version) が唯一の入口
  client.go          #   config 解決 + HTTP クライアント（デーモン停止を明示診断）
  commands.go        #   status/list/history/trigger/reload の実装
  serve.go           #   デーモン本体: ticker ループ・reload・retention prune・shutdown drain
  hooks.go           #   [hooks] → engine.Notify 変換（TASK_CLOCK_* env で hook 実行）
  validate.go        #   設定検証 + 次回発火プレビュー（読んだ config パスを必ず表示）
  install.go         #   LaunchAgent plist 生成 + launchctl bootstrap/bootout
internal/clock/      # 注入クロック (Clock interface + Real)
internal/schedule/   # cron 式の純関数評価 (Parse/Next/FiresBetween)
internal/config/     # config.toml + tasks.d 読込・strict decode・${VAR} 展開・検証
internal/store/      # SQLite 履歴 (scheduled_for/started_at/outcome) + prune
internal/engine/     # ポリシー状態機械 (Tick 駆動) + ExecRunner (pgid + ログ収集)
internal/api/        # localhost HTTP API (Bearer 認証・定数時間比較)
```

## Gotchas

- **タイミングループは短周期 ticker のポーリング**（既定 10s）。長い timer
  一本で眠る実装は App Nap / timer coalescing で壊れる（RFP §3）。
- 時刻は必ず注入クロック経由。`time.Now()` をロジックに直書きしない
  （cmd/ の表示・serve ループのみ実時刻可）。engine のテストは fakeClock +
  fakeRunner で決定的に書く。
- SQLite スキーマは内部実装。外部契約は HTTP API（CLI も API 経由で一本化）。
  スキーマを変えても API の JSON 形を守ること。
- `${VAR}` 展開で未定義変数は**エラー**。空文字に黙って展開しない。
  展開は argv 配列と workdir のみ（shell 形式はシェル自身に任せる）。
- config は strict decode — 未知キーはエラー。フィールド追加時は
  config_test.go の typo テストを壊さないよう注意。
- `serve` は api_key 未設定・キー入りファイルが group/other 可読なら
  **起動拒否**（fail-closed）。テストがこの文言に依存している。
- plist には launchd タイマーキー（StartInterval 等）を**絶対に入れない**
  — install_test.go が禁止をピンしている。`ProcessType: Interactive` 必須。
- serve ループのテストは `serveTestStop` チャネル（テスト専用フック、
  本番は nil で永久ブロック）で停止させる。
- reload が反映するのは tasks.d + [hooks] + retention_days
  （hooks は engine.SetNotifier で live swap）。listen / api_key /
  data_dir / tick_interval の変更のみ再起動が必要（構造的 — reload を
  運んでいる接続自体を壊さないため）。
- 実行プロセスは自前の process group で起動（Setpgid）し、kill は
  `-pgid` へ SIGTERM → 5s 後 SIGKILL。stdout+stderr は run ごとの
  ログファイルへ統合保存。
- **デーモン自身のログは data_dir/daemon.log**（internal/logrotate、
  10MB×3 世代）。launchd 配下では stdout が消えるため serve が tee する。
  クラッシュトレースは plist の StandardErrorPath → daemon.err。
- **log_max_mb は Tick 駆動の stat 監視で kill**。ストリームを pipe で
  挟む実装にしないこと — 孫プロセスが fd を継承すると exec.Cmd.Wait が
  ハングする Go の既知の罠（コメント参照）。既定 0=無効は意図的
  （タスクを不意に殺す既定は驚きが大きい）。
- **watermark** は cron と排他（validate が強制）。発火条件は
  「max(前回成功, 前回試行開始) + interval」— 失敗はクラッシュループに
  ならず同ケイデンスで再試行。状態は履歴から復元（再起動で巻き戻らない）。
  実行中は再評価しない（queue/missed の概念なし）。
- **jitter** は hash(タスク名, 発火時刻) で決定的 — `time.Now()` や乱数を
  使ってはならない（next_fire の純関数性が壊れる）。scheduled_for は
  素の cron 時刻のまま、on_time 判定と表示は jitter 適用後の時刻。
- **通知イベント**は engine が emit するだけ（Options.Notify、goroutine
  dispatch）。coalesced は通知しない。streak は閾値ちょうどで 1 回だけ発火し
  on_time で再武装。hook 実行は cmd/hooks.go（60s timeout、キー非ログ）。
- **pause は永続**（SQLite の paused_tasks; 再起動・reload とも生存、resume で
  解除）— 「デーモン所有の運用上の OFF スイッチ」で、config の宣言的 `enabled`
  とは別層。pause 中の発火は記録もしない（意図的沈黙）。resume は cron を
  未来の次発火から再開（バックログ投入しない）。手動 trigger は pause 中も許可。
- テスト実行は `go test | tail` のようなパイプ禁止（失敗を飲む）。
  `set -o pipefail` するか素で実行。
