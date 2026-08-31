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
- **ウォーターマーク型トリガー**: 前回*成功*から N 分で発火 — AI エージェント
  起動のような実行時間が変動するバッチ向け。どの経路で発火を失っても
  「成功からの経過時間」条件が再武装する
- **通知 hook**: `on_missed` / `on_failure` / `on_overrun_streak`（本ツールの
  発端になった正帰還ループの早期警報）に任意コマンドを登録。イベント詳細は
  `TASK_CLOCK_*` 環境変数で渡る
- **HTTP トリガー API**（localhost 限定・静的キー Bearer 認証）で他ツール
  から発火可能。実行時の `pause` / `resume` も API/CLI から
- **決定的 jitter**: 同じ cron 式を共有するタスクの起動を分散。タスク名×
  発火時刻のハッシュで決まるため `next_fire` の照会可能性は保たれる
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

雛形はデーモン用の [config.example.toml](config.example.toml) と、
**パターンごとに 1 ファイル**のタスクサンプル
[examples/tasks.d/](examples/tasks.d/) — 該当するものを自分の `tasks.d/`
にコピーして編集してください:

| サンプル | パターン |
|---|---|
| [cron-basic.toml](examples/tasks.d/cron-basic.toml) | 最小の cron + argv タスク |
| [cron-all-options.toml](examples/tasks.d/cron-all-options.toml) | 全オプション注釈付き（リファレンス） |
| [watermark.toml](examples/tasks.d/watermark.toml) | 前回成功から N 分で発火（AI バッチ型） |
| [shell-pipeline.toml](examples/tasks.d/shell-pipeline.toml) | `shell = true` + パイプ |
| [slack-status-report.toml](examples/tasks.d/slack-status-report.toml) | レシピ: scli で定期 Slack 投稿（launchd の PATH に注意） |

全サンプルはテストスイートが validate するため、腐りません。
API キーの生成:

```bash
openssl rand -hex 32
```

`api_key` 未設定ではデーモンは起動しません。また `api_key` を含む設定
ファイルが group/other 可読の場合も起動を拒否します（`chmod 600`）。

### タスク定義フィールド

| フィールド | 意味 | 既定値 |
|---|---|---|
| `name` | 一意なタスク名（`[A-Za-z0-9][A-Za-z0-9._-]*`） | 必須 |
| `cron` | 5 フィールド cron 式、`@hourly` 等も可 | cron / watermark どちらか必須 |
| `watermark` | 前回成功からこの時間で発火（例 `"30m"`）。`cron` と排他、`overlap` / `catch_up` / `jitter` は適用外 | cron / watermark どちらか必須 |
| `command` | argv 配列。文字列は `shell = true` 時のみ | 必須 |
| `shell` | `command` を `/bin/sh -c` で解釈 | `false` |
| `workdir` | 作業ディレクトリ（`${VAR}` 展開あり） | 継承 |
| `env` | 追加環境変数（`[task.env]`）。`${VAR}` の第一解決先 | — |
| `timeout` | 超過で kill（例 `"25m"`） | なし |
| `log_max_mb` | 収集出力がこのサイズ(MB)を超えたら kill | 0（無制限） |
| `overlap` | `queue-one` \| `skip` \| `kill-and-restart` | `queue-one` |
| `catch_up` | 遅延した発火を気づいた時点で実行 | `true` |
| `jitter` | 起動を 0〜jitter の決定的オフセットで分散 | なし |
| `enabled` | スケジュール対象にする | `true` |

### 通知 hook

`config.toml` の `[hooks]` テーブル（変更はデーモン再起動が必要）:

```toml
[hooks]
on_failure        = ["swrite", "-c", "alerts"]   # 非ゼロ exit / kill
on_missed         = ["swrite", "-c", "alerts"]   # 発火の喪失 (overlap / catch_up off)
on_overrun_streak = ["swrite", "-c", "alerts"]   # N 回連続で定刻に開始できず
overrun_streak_threshold = 3
```

hook には `TASK_CLOCK_EVENT` / `TASK_CLOCK_TASK` / `TASK_CLOCK_SCHEDULED_FOR` /
`TASK_CLOCK_REASON` / `TASK_CLOCK_EXIT_CODE` / `TASK_CLOCK_ERROR` /
`TASK_CLOCK_STREAK` が環境変数で渡ります。スリープ復帰時の coalesced
バックログは仕様どおりの挙動なので**意図的に通知しません**。streak hook は
定刻発火で再武装します。

## 使い方

```bash
task-clock serve                  # デーモンをフォアグラウンド実行
task-clock status                 # タスク状態・次回発火・超過
task-clock list                   # デーモンが見ているタスク定義
task-clock history analyze        # 予定 vs 実績の履歴（ログパス付き）
task-clock trigger analyze        # 今すぐ発火
task-clock pause analyze          # スケジュール停止（resume まで; 再起動を跨いで持続）
task-clock resume analyze         # 再開（停止中の発火はバックログ投入しない）
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
| `POST /v1/tasks/{name}/pause` / `.../resume` | スケジュール停止 / 再開 |
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
daemon.log[.1..3]      # デーモン自身の運用ログ（10 MB × 3 世代でサイズローテーション）
daemon.err             # launchd が捕捉するクラッシュトレース
```

履歴とタスクログは `retention_days`（既定 30 日）で削除、デーモン自身の
ログはサイズでローテーションされます。スリープ中に
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
