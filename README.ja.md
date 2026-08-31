# task-clock

launchd のタイミングエンジンを信頼しない、macOS 用の常駐スケジューラー。

launchd は省電力のためにタイマーを coalesce するため、周期ジョブが遅延・
スキップされることがあります。しかもタイマー状態は不可視で、次にいつ発火
するかを照会できず、スキップされた発火は痕跡を残しません。task-clock は
cron 式を自前で評価してタスク（汎用シェルコマンド）を起動し、launchd が
記録しないすべてを記録します。

## 特長

- **cron 式スケジューリング**をプロセス内で評価 — launchd はデーモンの常駐
  維持（`KeepAlive`）にのみ使い、タイミングエンジンとしては使わない
- **照会可能な次回発火**: `next_fire` は式と時刻の純関数で、CLI / HTTP API が
  同じ値を表示する
- **予定 vs 実績の履歴**: 発火ごとに `scheduled_for`・`started_at`・結果
  （`on_time` / `queued` / `missed(reason)`）を記録し、スキップとドリフトを
  「謎」ではなく観測可能な事実にする
- **超過はポリシーで扱う**: タスク単位の overlap ポリシー（既定 `queue-one`、
  `skip`、`kill-and-restart`）と catch-up。超過は「overrun 12m」という明示的な
  状態として表示され、無言の停滞にはならない
- **タスク出力の収集**: 各 run の stdout+stderr 統合ログを run 単位で保存し、
  履歴から参照可能。保持期間で自動削除
- **HTTP トリガー API**（localhost 限定・静的キー Bearer 認証）で他ツール
  から発火可能
- **シェルの不意打ちなし**: コマンドは argv 配列 + task-clock 自身による
  `${VAR}` 展開。未定義変数はエラー、`/bin/sh -c` は明示的 opt-in

## インストール

ソースからビルド（macOS, arm64）:

```bash
make build
cp dist/task-clock /usr/local/bin/   # PATH の通った場所へ
```

設定を作成（下記）後、LaunchAgent を登録:

```bash
task-clock validate     # 設定検証 + 次回発火プレビュー
task-clock install      # ~/Library/LaunchAgents/jp.nlink.task-clock.plist を生成・登録
```

生成される plist は `KeepAlive`（常駐維持）と `ProcessType: Interactive`
（バックグラウンド絞り込み回避）のみで、launchd のタイマーキーは意図的に
使いません。

## 設定

config 探索順（`config.toml` を含む最初のディレクトリが有効）:

1. `$TASK_CLOCK_CONFIG_DIR`
2. `$XDG_CONFIG_HOME/task-clock`
3. `~/.config/task-clock` ← 正位置
4. `~/Library/Application Support/task-clock`

配置:

```
~/.config/task-clock/
├── config.toml      # [daemon] 設定 — api_key を含むため chmod 600
└── tasks.d/
    └── *.toml       # [[task]] 定義。名前はファイル横断で一意
```

雛形は [config.example.toml](config.example.toml) と
[task.example.toml](task.example.toml)。API キーの生成:

```bash
openssl rand -hex 32
```

`api_key` 未設定ではデーモンは起動しません。また `api_key` を含む設定
ファイルが group/other 可読の場合も起動を拒否します（`chmod 600`）。

### タスク定義フィールド

| フィールド | 意味 | 既定値 |
|---|---|---|
| `name` | 一意なタスク名（`[A-Za-z0-9][A-Za-z0-9._-]*`） | 必須 |
| `cron` | 5 フィールド cron 式、`@hourly` 等も可 | 必須 |
| `command` | argv 配列。文字列は `shell = true` 時のみ | 必須 |
| `shell` | `command` を `/bin/sh -c` で解釈 | `false` |
| `workdir` | 作業ディレクトリ（`${VAR}` 展開あり） | 継承 |
| `env` | 追加環境変数（`[task.env]`）。`${VAR}` の第一解決先 | — |
| `timeout` | 超過で kill（例 `"25m"`） | なし |
| `overlap` | `queue-one` \| `skip` \| `kill-and-restart` | `queue-one` |
| `catch_up` | 遅延した発火を気づいた時点で実行 | `true` |
| `enabled` | スケジュール対象にする | `true` |

## 使い方

```bash
task-clock serve                  # デーモンをフォアグラウンド実行
task-clock status                 # タスク状態・次回発火・超過
task-clock list                   # デーモンが見ているタスク定義
task-clock history analyze        # 予定 vs 実績の履歴（ログパス付き）
task-clock trigger analyze        # 今すぐ発火
task-clock reload                 # tasks.d 再読込（SIGHUP でも可）
task-clock validate               # 設定検証 + 次回発火プレビュー
```

照会系コマンドは `--json`（API 生レスポンス）と `-config <dir>`（探索
パスの明示上書き）を受け付けます。

`status` は **next fire**（cron 式上の予定）と **next run**（ポリシー適用後の
実行見込み）を区別します。予定を食い潰しているタスクは
`running 42m (overrun 12m)` + next run `after current run` のように、負の
カウントダウンではなく明示的な状態語で表示されます。

## HTTP API

localhost 限定。`/v1/healthz` 以外は `Authorization: Bearer <api_key>` 必須。

| エンドポイント | 意味 |
|---|---|
| `GET /v1/tasks` | 全タスクの状態 + 最終 run |
| `GET /v1/tasks/{name}` | 単一タスク |
| `POST /v1/tasks/{name}/trigger` | 発火（実行中なら 409） |
| `GET /v1/tasks/{name}/history?limit=N` | 履歴 |
| `POST /v1/reload` | tasks.d 再読込 |
| `GET /v1/healthz` | 死活（認証不要・静的応答） |

```bash
curl -H "Authorization: Bearer $KEY" http://127.0.0.1:17282/v1/tasks
```

## データとログ

履歴（SQLite）とタスク出力は `~/Library/Application Support/task-clock/`
（`data_dir`）配下:

```
task-clock.db          # 予定 vs 実績の履歴
logs/<task>/<ts>.log   # run ごとの stdout+stderr 統合ログ
```

いずれも `retention_days`（既定 30 日）で削除されます。スリープ中に
タスクは実行されません（仕様）。スリープ中に逃した発火は復帰時に 1 回だけ
実行され（`catch_up`）、スキップ分は `missed(coalesced)` として記録されます。

## ビルド

```bash
make build    # dist/task-clock に出力 + 署名（`go build` 直接実行は禁止）
make test
```

## ドキュメント

- [RFP / 設計文書](docs/ja/task-clock-rfp.ja.md)（[English](docs/en/task-clock-rfp.md)）
- [English README](README.md)

## ライセンス

[MIT](LICENSE)
