# CLAUDE.md — task-clock

Org rules: nlink-jp/.github `CONVENTIONS.md` を先に読むこと。
設計の正は `docs/ja/task-clock-rfp.ja.md`（英訳 `docs/en/task-clock-rfp.md`）。

## Project invariants (RFP で確定済み — 変更は RFP 改訂とセットで)

- **launchd はタイミングエンジンとして使わない。** 依存は KeepAlive による
  常駐維持のみ。`install` が生成する plist には `ProcessType: Interactive`。
- **タイミングループは短周期 ticker のポーリング**（~10s）。「次回発火まで
  一本の長い timer で眠る」実装は App Nap / timer coalescing で壊れるので禁止。
- **`next_fire` は純関数**（cron 式 × 注入クロック）。時刻は必ず注入し、
  `time.Now()` をロジック内で直接呼ばない（テスト可能性の要）。
- **`next_fire` と `next_expected_run` は別フィールド。** 後者は overlap /
  catch-up ポリシー適用後の値。超過表示は負値でなく状態語「超過 N 分」。
- **発火ごとに `scheduled_for` と `started_at` の両方を記録**し、結果を
  `on_time` / `queued` / `missed(reason)` で残す。SQLite スキーマは内部実装 —
  外部契約は HTTP API のみ（CLI も API 経由）。
- **command は argv 配列 + `${VAR}` 展開**（task-clock 自身が展開、mcp.json
  方式）。未定義変数はエラー（空文字への黙殺禁止）。`shell = true` は opt-in。
- **ポリシー既定値は防御的**: overlap = `queue-one`、catch-up = 有効。
- **HTTP API は localhost + token のみ。** リモート公開はスコープ外。
- **macOS 専用**（darwin/arm64）。クロスプラットフォーム化しない。

## Build

- `make build`（`go build` 直接実行は禁止 — dist/ に出力 + 署名）
- `make test` / `make vet` をコミット前に実行
