# quota-panel

Thermalright Trofeo Vision 9.16 インチ用の利用枠ダッシュボード。macOS と Linux で動く Go の単一実行ファイルです。左から Claude Code、Codex、Antigravity、OpenCode Go を表示します。取得は既定で 5 分ごとです。

## 実機で最初の 1 枚を表示

リポジトリをクローンした場合は、先に Go で実行ファイルをビルドします。[ビルド手順](#設定変更とビルド)

1. パネルを接続したコンピューターで trcc を起動します。下記の API と画像保持の動作は **trcc-linux v9.12.2** の実装で確認しました。macOS 用 DMG では `trcc` の代わりに `/Applications/TRCC.app/Contents/MacOS/TRCC` を使えます。[trcc の導入手順](https://github.com/Lexonight1/thermalright-trcc-linux/blob/v9.12.2/doc/GUIDE_INSTALL.md#macos-experimental)

   ```sh
   trcc api --port 9876
   ```

2. 実行ファイルのあるディレクトリで次のコマンドを実行します。`-doctor` は OS から見える USB ID、trcc が認識した機器、カタログ寸法、接続済み機器のハンドシェイク寸法、ログの場所、`agy --version` を報告します。初回接続前はハンドシェイク寸法が不明なことがあります。`-demo` は必要に応じて trcc に接続を依頼し、実寸大の静止画を 1 枚送ります。画面内の **DEMO** が点灯確認の目印です。

   ```sh
   ./trofeo-quota -doctor
   ./trofeo-quota -demo
   ```

3. 同梱の `trofeo.toml` を編集して常駐モードを起動します。終了は Ctrl+C です。

   ```sh
   ./trofeo-quota -config ./trofeo.toml
   ```

| 実行例 | 動作 |
| --- | --- |
| `./trofeo-quota -demo -preview demo.png` | ダミー値の PNG を保存 |
| `./trofeo-quota -preview quota.png` | 実データの PNG を保存。取得できない行も確認可能 |
| `./trofeo-quota -once` | 実データの画像を 1 回送信 |
| `./trofeo-quota` | カレントディレクトリの `trofeo.toml` で常駐。ファイルがなければ内蔵既定値を使用 |

## パネルの寸法と送信経路

trcc のレジストリでは `0416:5408` と `0416:5409` の両方が **1920×462** の LY 系です。`5408` の利用者は 462 行と 480 行でテストカードを比較し、462 行で四辺が正しく表示されることを確認しています。6.86 インチなどで使われる HID の `0416:5302` は選択対象外です。[デバイス表](https://github.com/Lexonight1/thermalright-trcc-linux/blob/v9.12.2/doc/REFERENCE_DEVICES.md) · [レジストリ](https://github.com/Lexonight1/thermalright-trcc-linux/blob/v9.12.2/src/trcc/core/registry.py) · [実機での比較](https://github.com/Lexonight1/thermalright-trcc-linux/issues/248#issuecomment-6670753448)

送信時は `GET /devices` で対象を選び、`GET /devices/{key}/state` の接続状態と `resolution` を読みます。その幅と高さで文字とバーを直接描画し、`POST /devices/{key}/display/send-image` の multipart フィールド `image` に PNG を渡します。実寸が読めないときは接続を再確認します。プレビューでは実機、trcc のカタログ、内蔵カタログの順で寸法を選び、採用した根拠を標準エラーに出力します。[REST API](https://github.com/Lexonight1/thermalright-trcc-linux/blob/v9.12.2/doc/REFERENCE_API.md)

LY パネルには直前のフレームを定期的に再転送する必要があります。trcc がその USB 転送を担当するため、表示中は trcc を起動しておいてください。Go 側は PNG の SHA-256 と送信先を、trcc が成功を返した時点で記憶します。10 秒ごとの接続確認と、対応版では `/events` の着脱通知で復帰を検出し、その時点の画像を再送します。trcc の終了、機器の着脱、送信拒否、取得失敗は個別に扱います。[LY の keepalive](https://github.com/Lexonight1/thermalright-trcc-linux/blob/v9.12.2/src/trcc/adapters/device/ly_lcd.py) · [trcc の SendImage](https://github.com/Lexonight1/thermalright-trcc-linux/blob/v9.12.2/src/trcc/core/commands/device.py)

### macOS で REST 送信を切り分ける

この開発環境は USB パススルーのない Linux です。trcc 9.12.2 の配布版 REST ハンドラと仮想デバイスを組み合わせ、Go バイナリから送った 1920×462 PNG が trcc の `SendImage` に届くところまで確認しました。macOS と LY 実機での点灯確認は未実施です。trcc 自身も macOS 対応を experimental としています。REST が失敗する場合や、受理されても画面に出ない場合は、保存した PNG を trcc の CLI から送ると、問題が REST 経路に限られるかを確認できます。

```sh
./trofeo-quota -demo -preview demo.png
trcc display send-image 0416:5408 demo.png
```

`5409` または `@port` 付きの機器では `-doctor` が表示したキーを使います。CLI でも表示されない場合は `trcc report` でハンドシェイクと転送エラーを確認できます。[CLI リファレンス](https://github.com/Lexonight1/thermalright-trcc-linux/blob/v9.12.2/doc/REFERENCE_CLI.md)

## データの読み方

| 列 | 表示値と取得元 |
| --- | --- |
| Claude Code | ローカルログから計算した **推定使用率**。5 時間枠・週間枠のリセットまでの残り時間を表示 |
| Codex | セッションログに記録された **使用率**。primary と secondary の期間・リセット日時を表示 |
| Antigravity | Gemini と Claude/GPT のプールごとの **残量**。5 時間枠・週間枠を独立して表示 |
| OpenCode Go | 設定したモデルに帰属する **5 時間枠の残量**。共通枠用の行も選択可能 |

数値の欠落、期限切れ、コマンド失敗、読めないログ、曖昧なアカウントは該当行を **取得不能** にします。計測できた 0% は 0% と表示します。取得に失敗した理由は標準エラーに記録します。日時はプログラムを起動したコンピューターのローカル時刻です。

### Claude Code

`~/.claude/projects/**/*.jsonl` を再帰的に読み、assistant の入力・出力・キャッシュ作成・キャッシュ読み込みトークンを数えます。メッセージ ID とリクエスト ID を使って複製をまとめ、時刻順に ccusage 方式の 5 時間ブロックを組み立てます。開始は最初の利用時刻を UTC の正時に切り下げます。週は設定したリセット基準から 7 日単位で区切ります。[ccusage のブロックと上限](https://github.com/ryoppippi/ccusage/blob/main/docs/guide/blocks-reports.md)

セッションのトークン数だけでは Anthropic の契約上の正確な利用率を復元できません。`[claude]` の `five_hour_tokens` と `weekly_tokens` に自分のトークン予算を、`weekly_reset_anchor` に契約画面で確認した週リセットの日時を設定します。上限を未設定にした枠は「取得不能」です。他のマシンや削除済みログでの利用はこの推定に含まれません。入力例は `trofeo.toml` にあります。

### Codex

`~/.codex/sessions/` 配下の JSONL の `event_msg → token_count → rate_limits` を読み、イベント時刻が最新の通常 Codex 枠を採用します。`used_percent` は使用率、`resets_at` は Unix 秒です。別枠の `limit_id` は混在させません。期限を過ぎた窓は、新しい記録を取得するまで「取得不能」です。[Codex のデータ型](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/protocol.rs)

### Antigravity

毎回、同じ `agy` 実行ファイルに対して `--version` を先に実行します。**1.1.11 以上と確定した場合だけ** `agy -p /usage --output-format json` を呼びます。1.1.11 で追加された読み取り専用の print-mode 動作は Google のリリースノートに明記されています。JSON の `command.data.groups[].buckets[]` から、対象バケットの `remaining_fraction` と `reset_time` を読みます。[Google の v1.1.11 リリース](https://antigravity.google/releases?tab=cli&version=1.1.11) · [モデル枠の説明](https://antigravity.google/docs/cli/commands/usage)

旧版、未導入、バージョン不明の場合は、同じ OS ユーザーで起動中の Antigravity language server を探し、プロセス引数のポートと CSRF トークンでループバックへ Connect-RPC を送ります。`RetrieveUserQuotaSummary` を優先し、不足する場合は同じ接続先の `GetUserStatus` を照会します。期間を区別できない旧応答、ポートが引数に出ていない環境、停止中のサーバーは該当行が「取得不能」です。プロセス引数やトークンは表示・保存しません。[ローカル API の参考実装](https://github.com/steipete/CodexBar/blob/main/Sources/CodexBarCore/Providers/Antigravity/AntigravityStatusProbe.swift)

### OpenCode Go

`npx --yes @slkiser/opencode-quota@latest show --json` の公開 JSON v3 を読みます。通常は `--provider opencode-go` で絞り込みます。この CLI は OpenCode が最後に保存したクォータを返すため、OpenCode 側でプラグインのセットアップとログインが必要です。

調査した 6.3.0 の標準プロバイダーはアカウント共通の 5 時間・週・月の行を出力します。**標準出力だけから 4 モデルそれぞれの残量は計算できません。** モデル別の 5 時間行がない場合は「取得不能」です。`models = ["@shared"]` にすると実在する共通 5 時間枠を表示できます。モデル専用の 5 時間行をプラグインにカスタム登録済みの場合は `[opencode.model_rows]` で各モデルに異なる公開行 ID を割り当てられます。デモのモデル別数値は画面確認用です。[公開 JSON 形式](https://github.com/slkiser/opencode-quota/blob/main/docs/readme/external-integration.md) · [OpenCode Go プロバイダー](https://github.com/slkiser/opencode-quota/blob/main/src/providers/opencode-go.ts)

## 設定変更とビルド

`display_order` で列の選択と順番、各節の `enabled` で取得の有無、`items` または `models` で行の選択と順番を指定します。列ごとに最大 4 行を表示し、5 行以上は取得サイクルごとにページを切り替えます。`interval_seconds` と `trcc_url` も含めてサイクルごとに読み直します。編集中の TOML に誤りがある場合は最後に読めた設定で動作を続けます。ログのルートは TOML と環境変数 `CLAUDE_CONFIG_DIR`、`CODEX_HOME` に対応します。

ソースからのビルドには Go 1.23 以降を使います。日本語フォントはバイナリに埋め込んであります。実行にはパネル制御用の trcc と、有効にしたソースで利用する外部コマンドが必要です。

```sh
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go build -trimpath -o trofeo-quota .
```

テストはハンドシェイク寸法に一致する PNG、HTTP の受理・再送、設定の再読み込み、ログの重複・欠損、実測 0% と取得不能、旧版 agy の起動引数、ローカル RPC を検証します。

## ライセンス

quota-panel 本体は MIT License で公開しています。[LICENSE](LICENSE)

同梱のフォントと Go ライブラリには、それぞれのライセンスが適用されます。[第三者ライセンス一覧](THIRD_PARTY.txt) · [日本語フォントの SIL Open Font License](assets/OFL.txt)
