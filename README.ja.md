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
- **照会可能な次回発火**: `next_fire` は式と時刻の純関数で、CLI / HTTP API /
  GUI が同じ値を表示する
- **予定 vs 実績の履歴**: 発火ごとに `scheduled_for`・`started_at`・結果
  （`on_time` / `queued` / `missed(reason)`）を記録し、スキップとドリフトを
  「謎」ではなく観測可能な事実にする
- **超過はポリシーで扱う**: タスク単位の overlap ポリシー（既定 `queue-one`、
  `skip`、`kill-and-restart`）と catch-up。超過は「超過 N 分」という明示的な
  状態として表示され、無言の停滞にはならない
- **ウォーターマーク型トリガー**: 前回*成功*から N 分で発火 — AI エージェント
  起動のような実行時間が変動するバッチ向け
- **HTTP トリガー API**（localhost 限定・token 認証）で他ツールから発火可能
- **通知 hook**: `on_missed` / `on_overrun_streak` / `on_failure` に任意
  コマンドを登録
- **シェルの不意打ちなし**: コマンドは argv 配列 + task-clock 自身による
  `${VAR}` 展開。未定義変数はエラー、`/bin/sh -c` は明示的 opt-in

## ビルド

```bash
make build    # dist/task-clock に出力（`go build` 直接実行は禁止）
make test
```

## ドキュメント

- [RFP / 設計文書](docs/ja/task-clock-rfp.ja.md)（[English](docs/en/task-clock-rfp.md)）
- [English README](README.md)

## ライセンス

[MIT](LICENSE)
