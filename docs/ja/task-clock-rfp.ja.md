# RFP: task-clock

> Generated: 2026-08-31
> Status: Draft

## 1. Problem Statement

macOS の launchd による周期実行は、省電力のためのタイマー coalescing で発火が遅延・スキップされることがあり、さらに内部タイマー状態が不可視（次回発火時刻を照会できない・スキップは事後にも観測できない）という構造的欠陥を持つ。実際に、30分周期で起動していた AI エージェントのバッチ分析が1回分スキップされ、未処理データの蓄積 → ターン実行時間の伸張 → 消費トークン増大という正帰還の事故が発生した。

task-clock は launchd のタイミングエンジンを信頼せず、自前で cron 式を評価してタスク（汎用シェルコマンド）を起動する常駐スケジューラーである。「予定 vs 実績」の完全な記録、次回発火時刻の照会、超過・スキップの可視化と通知、そして実行時間が変動するタスクに対するウォーターマーク型トリガー（前回成功からの経過時間ベース）を一級の機能として提供する。

対象ユーザーは、macOS 上で周期バッチ（特に AI エージェント起動のような実行時間が変動するタスク）を安定運用したい個人〜小規模運用者。スリープ中にタスクが実行されないことは仕様として許容する。

## 2. Functional Specification

### Commands / API Surface

単一バイナリ + サブコマンド構成（org 定型）:

```
task-clock serve                        # 常駐デーモン本体（launchd KeepAlive から起動）
task-clock status  [--json]             # 全タスクの状態: next_fire / next_expected_run / 超過・実行中
task-clock list    [--json]             # タスク定義一覧
task-clock history <task> [--json] [--limit N]   # 予定 vs 実績の履歴
task-clock trigger <task>               # 手動トリガー
task-clock reload                       # config / tasks.d の再読込
task-clock validate                     # config 検証 + 各タスクの次回発火プレビュー
task-clock install / uninstall          # launchd plist（KeepAlive のみ）の生成・登録
```

HTTP API（localhost 限定、token 認証）— API コールによるトリガー要件の実体:

```
GET  /v1/tasks                     一覧 + 状態
GET  /v1/tasks/{name}              状態詳細（next_fire, next_expected_run, overrun, last_run）
POST /v1/tasks/{name}/trigger      外部トリガー
GET  /v1/tasks/{name}/history      履歴
POST /v1/reload                    設定再読込
GET  /v1/healthz                   死活
```

CLI の status / history / trigger / reload はすべて HTTP API 経由（一本化）。状態の正はデーモンに一元化し、SQLite スキーマは内部実装に保つ。デーモン停止中は CLI がその旨を明示する。

**API 認証** — 静的 API キー方式。config ファイルにキーを設定するだけの簡易実装とし、OAuth 等の高度な仕組みは採用しない:

- キーは `config.toml` の `[daemon] api_key` に設定。**未設定なら `serve` は起動を拒否**（fail-closed; `validate` も事前検出）。キー生成は `openssl rand -hex 32` 程度を README に例示
- 全エンドポイントで `Authorization: Bearer <key>` を要求。例外は `/v1/healthz` のみ（静的応答で情報を返さない）
- 照合は**定数時間比較**（crypto/subtle）。キー値はログに絶対に出力しない
- config 読込時にファイル権限を検査し、`api_key` を含むファイルが group/other 可読なら起動拒否（`chmod 600` を案内）— org 規約「credentials を含む config は権限チェック」
- bind は 127.0.0.1 固定（0.0.0.0 不可）。CORS ヘッダは一切返さず、ブラウザ経由のクロスオリジン呼び出しを既定拒否。エラーメッセージは静的（入力をエコーしない）、JSON 応答は json.Marshal 経由
- レートリミットは**意図的に非搭載**（localhost + キー必須の脅威モデルでは過剰。リモート公開しない前提が崩れる時に再検討）

### Input / Output

**ステータスモデル** — 「次回」を2フィールドに分離する:

- `next_fire` — cron 式と現在時刻から導出される純関数的な次回発火予定
- `next_expected_run` — overlap / catch-up ポリシー適用後の実行見込み。queue-one で超過中なら「終了後即時」という状態語、skip なら次の発火時刻

超過は負のカウントダウンではなく **「超過 N 分」という明示的な状態語 + 正の値** で表示する（符号はデータモデル側の表現に留める）。

**実行履歴（SQLite）** — 発火ごとに記録:

- `scheduled_for` / `started_at` / `finished_at` / exit code
- 発火結果: `on_time` / `queued` / `missed(reason)` — 予定と実績の突合せでスキップとドリフトを事後観測可能にする

**run ログ** — 各 run の stdout/stderr をデータディレクトリ配下にファイル保存。保持期間は config で指定。

### Configuration

```
~/.config/task-clock/config.toml      # [daemon] listen / token / 保持期間 / hook
~/.config/task-clock/tasks.d/*.toml   # [[task]] 複数可、タスク名はファイル横断で一意
データ: ~/Library/Application Support/task-clock/   # SQLite / run ログ
```

タスク定義フィールド: cron 式、command、workdir、env、timeout、overlap ポリシー（`skip` / `queue-one` / `kill-and-restart`、既定 `queue-one`）、catch-up 有無（既定: 有効）、`enabled`。

**コマンド指定形式** — argv 配列基本、シェル非経由:

```toml
command = ["/path/to/bin", "--out", "${HOME}/data"]
```

- `${VAR}` を task-clock 自身が展開（mcp.json 方式）。解決順はタスクの `env` テーブル → デーモンのプロセス環境
- `$${` で `${` のリテラル表現
- **未定義変数はエラー**（起動失敗として履歴に記録、`validate` が事前検出）。空文字への黙殺はしない
- パイプ等が必要なタスクのみ `shell = true` の opt-in で `/bin/sh -c` 解釈（command は文字列）

**通知 hook** — `on_missed` / `on_overrun_streak` / `on_failure` に任意コマンドを登録（swrite で Slack 通知等）。デーモン自体は通知実装を持たない。

### External Dependencies

外部サービスなし。ライブラリとして cron 式パーサ（robfig/cron 想定）。常駐維持のみ launchd（KeepAlive）に依存。

## 3. Design Decisions

- **言語**: デーモン + CLI は Go（org 標準、pure Go で CGO 不要）。GUI は Swift/SwiftUI メニューバー常駐アプリを **別プロジェクト task-clock-gui** として後続で開発（active-lens / active-lens-gui と同型: 署名済み CLI 同梱 + `--json` を叩く薄いフロントエンド）
- **タイミングエンジン設計（App Nap / timer coalescing 対策）**:
  - 「次回発火まで一本の長い timer で眠る」設計は採らない。短周期（10秒程度）の ticker で期限到来タスクの有無を判定するポーリングループとし、個々の wake が遅延しても最悪遅延を ticker 間隔に有界化する
  - `install` が生成する plist に `ProcessType: Interactive` を含め、Background 扱いのリソース絞りを回避する
  - 予定 vs 実績の常時記録によりドリフトは自己計測できる。実測で不足する場合のみ NSProcessInfo activity assertion（cgo）を Phase 2 contingency とする
- **次回発火の可視性**: cron 式ベースなら次回発火は式と現在時刻の純関数であり、API / CLI / GUI が同じ計算結果を表示できる（launchd の隠蔽タイマーとの本質的差別化）
- **ポリシー既定値は防御的に**: 発端事故は事後調査（AI による）でも真因を特定できなかった（coalescing か実行中スキップか不明）。既定を overlap = `queue-one`・catch-up = 有効とし、発火喪失のどちらの経路（タイマー遅延・実行中スキップ）でも「気づいた時点で1回だけ追い実行」で回復する。タスク単位で上書き可能
- **既存ツールとの補完**: 通知 hook から swrite（chatops-series）で Slack 通知。リモートトリガーが必要になれば webhook-relay → localhost API の中継で実現（初期スコープ外）
- **スコープ外（明示）**: スリープ制御（pmset wake 等）、マシン間分散、タスク依存 DAG、cron の `@reboot` 系ディレクティブ、launchd の完全置換（常駐維持には引き続き launchd を使う）、Linux / Windows 対応（macOS 専用）

## 4. Development Plan

### Phase 1: Core（単体でリリース可能な最小形）

- デーモン: cron 式評価（注入クロックで純関数テスト可能に設計）、argv + `${VAR}` 展開実行、timeout kill、overlap ポリシー3種（kill 機構は timeout と共用）
- SQLite 履歴（`scheduled_for` / `started_at` / 発火結果）、run ログ保存 + 保持期間
- HTTP API（token 認証）+ CLI 全サブコマンド、config + tasks.d 読込 + reload、install / uninstall（`ProcessType: Interactive` plist 生成）
- テスト: 次回発火計算・変数展開・ポリシー状態機械を中心に

### Phase 2: Features（事故対策の完成）

- ウォーターマーク型トリガー（前回成功から N 分経過で発火）
- 通知 hook（`on_missed` / `on_overrun_streak` / `on_failure`）
- catch-up ポリシーの精緻化、API 経由の一時 pause / resume、jitter
- App Nap contingency: ドリフト実測で不足が確認された場合の NSProcessInfo activity assertion

### Phase 3: Release

- README.md / README.ja.md / CHANGELOG 整備
- make build-all + Developer ID 署名 + notarize（org 標準パイプライン）
- umbrella submodule 統合、homebrew-tap 登録検討、check-org.sh

各 Phase は独立レビュー可能。Phase 1 完了時点で launchd 代替としての基本価値（正確な発火 + 完全な可観測性）が成立する。GUI（task-clock-gui）は task-clock v0.1 リリース後に別プロジェクトとして着手し、next_fire カウントダウン / 超過表示 / 履歴ビューを提供する。

## 5. Required API Scopes / Permissions

**None** — 外部サービス連携なし。HTTP API は localhost 限定 + 自前 token 認証。

## 6. Series Placement

Series: **util-series**

Reason: 常駐デーモン + CLI + 別プロジェクト GUI という構成は active-lens / active-lens-gui と同型で、util-series の定型に一致する。外部サービスのクライアントではなく（cli-series 非該当）、Slack 自動化でもなく（chatops-series 非該当）、汎用ローカルツールである。

## 7. External Platform Constraints

- **launchd**: 常駐維持（KeepAlive）のみに依存し、タイミングエンジンとしては信頼しない（本ツールの存在理由）。クラッシュ時の再起動挙動は launchd の KeepAlive 仕様に従う
- **App Nap / timer coalescing**: macOS はバックグラウンドプロセスのタイマーを coalesce・抑制し得る。§3 のポーリングループ + `ProcessType: Interactive` で対処し、ドリフトを自己計測で監視する
- **署名 / 配布**: Developer ID 署名 + notarization が macOS 配布要件（org 標準パイプライン）
- **スリープ**: スリープ中は実行されない。仕様として許容（Problem Statement に明記）

---

## Discussion Log

- **発端**: 別マシンで launchd の 30 分周期タスクが 1 時間遅れ（1回分スキップ）で実行され、AI エージェントのバッチ分析でデータ蓄積 → ターン伸張 → トークン増大の正帰還事故が発生。自前スケジューラーの必要性を議論
- **upstream 確認**: 自作の前に launchd 側の対処を調査。タイマー coalescing がデフォルト（OS X 10.9+）であり、plist の `LegacyTimers: true` で精密発火に切替可能なことを確認。また launchd は同一ラベルのジョブ実行中は次の発火を捨てる（キューしない）ため、「前回実行が 30 分超過 → 発火スキップ」も真因候補。当該マシンでの真因確認を推奨とした上で、いずれにせよ「実行されなかったことが観測できない」という launchd の構造的欠陥は残るため自作を妥当と判断
- **可観測性の要件化**: launchd は次回発火時刻を照会できず、スキップも事後観測できない。cron 式ベースなら次回発火は純関数として導出でき、「予定 vs 実績」の両記録でドリフト実測・missed 検出・watchdog 自己検査が可能になる — これを一級要件とした
- **超過時の表示**: 負のカウントダウン案と「終了後の想定次回時刻」案を検討し、両者は別情報（超過量はカレント実行の属性、次回見込みはポリシー適用後の `next_expected_run`）として両立させる設計に決定。表示は符号付きでなく「超過 N 分」の状態語とする
- **通知**: 「N 回連続超過で通知」を正帰還事故の早期警報として要件化。実装は hook コマンド方式（デーモンは通知実装を持たず、swrite 等に委譲）
- **ウォーターマーク型トリガー**: AI エージェント用途ではトークン増大のドライバが「前回成功からの未処理量」であるため、cron 式に加え「前回成功から N 分」トリガーをスコープに含める（実装は Phase 2）
- **命名**: cron-beat / task-clock / task-conductor を検討し **task-clock** に決定（GUI は task-clock-gui）
- **config**: tasks.d/ 分割を選択（GUI・API 経由の動的追加を見据える）。CLI↔デーモン通信は HTTP API に一本化
- **コマンド形式**: argv 配列 + task-clock 自身による `${VAR}` 展開（mcp.json 方式、ユーザー提案）。シェルは opt-in。未定義変数はエラー
- **プラットフォーム**: macOS 専用に決定（問題領域が launchd 固有）
- **App Nap 対策**: ユーザー指摘により明示要件化。短周期 ticker ポーリング + `ProcessType: Interactive` + ドリフト自己計測、cgo assertion は Phase 2 contingency
- **Phase 2 実装の設計決定 (2026-08-31)**: (1) watermark は cron と**排他**とし、発火条件は「max(前回成功, 前回試行開始) + interval」— 失敗時のクラッシュループを構造的に排除し、状態は履歴から復元。実行中は再評価しないため overlap/catch_up/jitter は適用外（設定すると validate エラー）。(2) 通知 hook のイベント詳細は TASK_CLOCK_* 環境変数渡し（argv テンプレート不採用）。coalesced（スリープ由来）は意図的に非通知。streak は閾値ちょうどで 1 回発火し on_time で再武装。(3) pause は runtime-only とし、pause 中の発火は記録もしない（意図的沈黙は事故ではない）。resume はバックログを投入しない。(4) jitter は hash(タスク名, 発火時刻) の決定的オフセット — next_fire の純関数性（本ツールの核心的差別化）を乱数で壊さないため
- **API 認証の確定 (2026-08-31)**: ユーザー要望により「config ファイルに静的キー」の簡易実装で明文化。簡易さの中で守る定石のみ義務化 — fail-closed（キー未設定は起動拒否）、Bearer ヘッダ、定数時間比較、config 権限検査（0600）、キー非ログ、127.0.0.1 固定、CORS 拒否。レートリミットは localhost 脅威モデルでは過剰として意図的に非搭載
- **事故マシンの追調査 (2026-08-31)**: AI による調査でも原因特定に至らず（unified log の保持期間の制約もあり過去事象の追跡は困難）。「実行されなかった理由すら残らない」という launchd の可観測性欠陥の実証例として Problem Statement を裏づける。真因不明のまま設計する前提とし、ポリシー既定値を防御的（overlap = queue-one、catch-up = 有効）に決定
