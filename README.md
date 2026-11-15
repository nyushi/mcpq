# mcpq

MCP の読取り結果を取得し、必須の形の表明と jq で絞る段階1の CLI。実機適合・効果・運用受入れは未確認。接続テストはローカル HTTP/SSE モックだけを使う。

## インターフェース

```
mcpq call SERVER TOOL --expect EXPR [--args JSON] [--jq EXPR] [--max-bytes N] [--allow-partial]
mcpq query RAW_ID --server SERVER --tool TOOL --expect EXPR [--jq EXPR] [--max-age DURATION] [--max-bytes N] [--allow-partial]
mcpq tools SERVER
mcpq compare SERVER TOOL --raw-id RAW_ID --baseline-id BASELINE_ID
```

これはインターフェース定義であり、実機の接続・確認手順ではない。実接続定義・実許可一覧・実ハッシュは含まない。`config.example.json` は架空かつ無効な例。固定設定パスは `types.go` の `defaultConfigPath` だけで定義し、CLI・環境から変更できない。設定パスと shadow 基準の採用はユーザー判断待ち。

## 結果の判断

stdout は JSON オブジェクト1個。`meta` と、表示可能な場合の `data`（jq 出力ストリームを配列化）を返す。空ストリームは `data:[]`、null 出力は2。expect は同じ入力への評価が厳密に true 1件の場合のみ合格。structuredContent を優先し、無ければ text の JSON、複数なら配列、JSON でない text が混じれば文字列配列を入力とする。

|コード|意味|
|---|---|
|0|成功|
|1|引数・式の構文・max-age 上限違反|
|2|取得・形・保存・比較の失敗|
|3|HTTP 401/403・認証値不足|
|4|不完全（false/unknown）|
|5|許可・設定・スキーマ・無効化|
|6|保存結果の鮮度・時計不整合|

優先順位は `1 > 5 > 3 > 2 > 6 > 4`。非0は data を出さず、4と `--allow-partial` の組合せだけ表示を許す（4は維持）。`meta.complete` は JSON の true/false または文字列 `"unknown"`。否定の結論には matched と complete を添えるか、既存の認可済み経路で確認する。`MCPQ_DISABLE=1` は全コマンドを拒否する。5を認可の迂回に使わない。パイプを使う側は `set -o pipefail` を設定し、`|| true` や `| head` で失敗を捨てない。

許可は引数値の完全一致列挙、未知キーは拒否。必須引数を宣言できる。ページ用カーソルは初回入力を拒否し、設定内の規則でのみ生成する。ページの items/next はドット区切りオブジェクトパス。終端到達後も最終件数が limit に達する場合は保守的に4。shape の最上位キー基準は初回に記録し、変更は2、基準を自動更新しない。初回基準が正しいかは expect と管理者の責任。

## 設定と保存

設定ファイルは symlink・group/other 書込みを拒否。自分または管理者が所有する通常ファイルのみ。設定中の endpoint_env/token_env は環境変数の**名前**であり、値を保存しない。許可ツールの `{name,inputSchema}` を JSON キー順で正規化した SHA256 を固定する。初期登録や変更承認を CLI が自動化する機能はない。readOnlyHint を認可には使わない。

保存ルートは自分所有・0700、経路の symlink を拒否する。保存は0600の一時ファイルを完成させて排他的に公開する。保存形式は `meta` と元の `pages` と `integrity_sha256`。raw_sha256 は pages の正規化 JSON の SHA256、integrity_sha256 は自身を空文字にした保存文書全体の正規化 JSON の SHA256。保存時刻と mtime が一致しない・age が負なら6。期限の既定は15分、設定上限は最大60分、query 指定でその上限を超えれば1。query は保存 server/tool と現在の許可・schema を照合し、元の args と完全性を表示する。args の意味的な一致は利用者が判断する。認証先の失効はオフラインでは検出できない。

保存ルートに管理対象名以外のファイルが混在したら削除せず2で停止する。24時間経過した保存物と shadow ログを、次のコマンド開始時に削除する。実行停止中に時刻どおりの削除はしない。常駐の削除ジョブは含まない。shape 基準は本文を含まないキー集合で、24時間削除の対象外。同じユーザーによる悪意ある改ざん・競合を防ぐセキュリティ境界ではない。出力・引数・raw は機微な外部データを含み得る。バックアップ・同期・出力先の扱いは運用側が決める。保存先外へ raw を複製する機能はない。

## shadow と測定

compare は同形式で保存された2つの raw を入力とする。任意の外部ファイルの直接インポートはしない。対象・args・schema・鮮度・完全性を検査し、既定は順序・重複・数値・null/欠落を保った完全一致。設定 `shadow_jq` で両側に適用する比較式を変更できるが、既定 `.` の変更はユーザーの基準確定後に行う。異なる取得時点の一貫性は保証しない。

`meta.diff_log` の保存先 `<raw_dir>/shadow-<乱数>.json` に、時刻・比較値のハッシュ・一致判定・基準のハッシュのみを0600で保存する。本文や引数の差分は保存しない。24時間で削除対象になる。`meta.tool_calls`（tools/call 数）、`http_requests`（補助 POST 込み）、`response_bytes`（受信本文バイト）を別々に合計する。query/compare は今回の通信数・受信量0、元の回数・受信量は source_* に引き継ぐ。CLI 出力のバイト数は呼出し側で別計測する。実際の業務シナリオ・差分の採否・測定・撤回はユーザーの範囲。

## 対応範囲と開発

Streamable HTTP の2025-06-18版、POST の JSON/SSE、セッションヘッダーに対応。自動再接続・再試行・認証復旧・stdio・batch は含まない。HTTP は loopback に限り、それ以外は HTTPS 必須。リダイレクトは拒否。全体期限は既定30秒（設定で最大5分）、進捗受信で延長しない。OS のブロックするファイル操作を強制中断する機能はなく、前後に期限を検査する。

受信上限は各応答8 MiB、ページ上限は設定で最大100、保存読取り上限は800 MiB、jq ストリーム上限100000件、結果予算は最大1 MiB。max-bytes は完全な jq 結果単位で省略し4にする。メタ情報の長さは結果予算に含まない。単一巨大 jq 値のメモリ上限は強制していない。整数は任意精度で保持し、小数・指数は浮動小数点。Linux のファイル権限・所有者・O_NOFOLLOW を前提とする。

Go と gojq を使用。`go test ./...`、`go vet ./...`、goimports、gofmt で検証する。gojq/timefmt-go は MIT ライセンスの依存物。コアの公開ライセンスはユーザー未決定であり、この作業では選定しない。
