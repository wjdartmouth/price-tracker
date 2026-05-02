# 💹 価格追跡サービス (Japanese E-Commerce Price Tracker)

Yahoo! Japan、楽天市場、Amazon.co.jp、メルカリの価格を自動監視するGoサービスです。価格が目標値を下回ったり、値下がり率が閾値を超えた際にアラートを生成します。

> A zero-dependency (except `golang.org/x/net` for HTML parsing and `golang.org/x/text` for Shift-JIS decoding) Go service that monitors product prices across major Japanese e-commerce platforms.

---

## 対応プラットフォーム / Supported Platforms

| プラットフォーム | ドメイン | 方式 | 文字コード |
|----------------|---------|------|---------|
| 楽天市場 | item.rakuten.co.jp | HTML scraping | UTF-8 |
| Yahoo!ショッピング | shopping.yahoo.co.jp | HTML scraping | UTF-8 |
| Amazon.co.jp | amazon.co.jp | HTML scraping | UTF-8 |
| メルカリ | jp.mercari.com | JSON (`__NEXT_DATA__`) | UTF-8 |
| au PAYマーケット | au-paymarket.jp | HTML scraping | UTF-8 / Shift-JIS |

---

## 機能 / Features

- ✅ **価格スクレイピング** — HTML解析 + JSONフォールバック（Next.js対応）
- ✅ **Shift-JIS / EUC-JP 自動変換** — 旧来の日本サイト対応（`golang.org/x/text`）
- ✅ **価格履歴** — タイムシリーズ保存（最大500ポイント / 商品）
- ✅ **アラートエンジン** — 目標価格・値下がり率・過去最安値・在庫復活
- ✅ **スケジューラー** — 設定間隔での自動ポーリング
- ✅ **丁寧なクロール** — User-Agentローテーション・ランダムディレイ・429対応
- ✅ **REST API** — JSON
- ✅ **CLIモード** — `price-tracker scrape -platform rakuten <URL>`
- ✅ **インタラクティブダッシュボード** — `dashboard.html`

---

## クイックスタート

```bash
git clone https://github.com/yourname/price-tracker
cd price-tracker

# 依存関係のダウンロード
go mod tidy

# APIサーバー起動（ポート8085）
go run main.go

# ダッシュボードを開く
open dashboard.html
```

### CLIモード

```bash
# 楽天市場の商品価格を取得
go run main.go scrape -platform rakuten https://item.rakuten.co.jp/edion/4548736132276/

# 複数URLをCSV出力
go run main.go scrape -platform amazon_jp -format csv \
  https://www.amazon.co.jp/dp/B09XS7JWHH \
  https://www.amazon.co.jp/dp/B098RL6SBJ

# JSON出力
go run main.go scrape -platform mercari -format json https://jp.mercari.com/item/m12345678
```

---

## REST API

### 商品管理

| Method | Path | 説明 |
|--------|------|------|
| GET | `/api/v1/products` | 監視商品一覧 |
| POST | `/api/v1/products` | 商品追加 |
| GET | `/api/v1/products/:id/history` | 価格履歴（?platform=&days=） |
| GET | `/api/v1/products/:id/compare` | プラットフォーム間比較 |
| POST | `/api/v1/products/:id/scrape` | 手動スクレイプ |

### アラート

| Method | Path | 説明 |
|--------|------|------|
| GET | `/api/v1/alerts` | アラート一覧（?unread=true） |
| POST | `/api/v1/alerts/:id/read` | 既読にする |

### 統計

| Method | Path | 説明 |
|--------|------|------|
| GET | `/api/v1/stats` | 統計情報 |
| GET | `/health` | ヘルスチェック |

---

### 商品追加リクエスト例

```bash
curl -X POST http://localhost:8085/api/v1/products \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Sony WH-1000XM5",
    "name_ja": "ソニー WH-1000XM5",
    "category": "ヘッドホン",
    "target_price": 35000,
    "alert_pct": 5.0,
    "tags": ["sony", "ヘッドホン"],
    "listings": [
      {"platform": "rakuten",  "url": "https://item.rakuten.co.jp/..."},
      {"platform": "yahoo",    "url": "https://shopping.yahoo.co.jp/..."},
      {"platform": "amazon_jp","url": "https://www.amazon.co.jp/dp/..."}
    ]
  }'
```

### 価格比較レスポンス例

```json
{
  "product_id": "prod-001",
  "product_name": "Sony WH-1000XM5",
  "best_price": {
    "platform": "rakuten",
    "platform_name": "楽天市場",
    "current_price": 36800,
    "all_time_low": 36800,
    "trend_dir": "down"
  },
  "comparisons": [
    {"platform": "rakuten", "platform_name": "楽天市場", "current_price": 36800, "trend_dir": "down"},
    {"platform": "yahoo",   "platform_name": "Yahoo!ショッピング", "current_price": 37100},
    {"platform": "amazon_jp", "platform_name": "Amazon.co.jp", "current_price": 37500}
  ]
}
```

---

## アーキテクチャ / Architecture

```
price-tracker/
├── main.go          # スクレイパー・API・CLI・スケジューラー・アラートエンジン
├── dashboard.html   # インタラクティブダッシュボード
├── go.mod           # 依存: golang.org/x/net, golang.org/x/text
└── Dockerfile       # Alpine ベース（ca-certificates, tzdata含む）
```

### スクレイピング戦略

```
HTMLフェッチ
   ↓
文字コード検出 (Shift-JIS / EUC-JP / UTF-8)
   ↓
プラットフォーム別パーサー
   ├── 楽天: CSSセレクター → フォールバック正規表現
   ├── Yahoo: セレクター → PayPayボーナス抽出
   ├── Amazon: IDベース選択子 → 在庫確認
   └── メルカリ: __NEXT_DATA__ JSON → 正規表現フォールバック
   ↓
価格正規化（¥/円/カンマ/税込ラベル除去）
   ↓
PricePoint保存 → アラートチェック
```

### 価格パーサー

```go
// "¥1,234", "1,234円", "1,234 円（税込）", "￥1,234" をすべて処理
func parsePrice(s string) (int, bool) {
    clean := regexp.MustCompile(`[¥￥,\s円（税込）（税抜）税込税抜]`).ReplaceAllString(s, "")
    clean = regexp.MustCompile(`[^\d]`).ReplaceAllString(clean, "")
    n, err := strconv.Atoi(clean)
    if err != nil || n <= 0 || n > 50_000_000 { return 0, false }
    return n, true
}
```

---

## アラートの種類

| タイプ | 説明 | 例 |
|--------|------|---|
| `target_hit` | 目標価格に到達 | ¥42,800 → ¥34,800（目標: ¥35,000） |
| `drop_pct` | 設定%以上の値下がり | 8.5%値下がり検出 |
| `new_low` | 過去最安値更新 | ¥36,800 → 過去最安 |
| `back_in_stock` | 在庫復活 | 売り切れ → 在庫あり |

---

## 環境変数

| 変数 | デフォルト | 説明 |
|------|-----------|------|
| `PORT` | `8085` | APIサーバーポート |
| `CHECK_INTERVAL` | `30m` | スクレイプ間隔（例: `15m`, `1h`） |
| `AUTO_SCRAPE` | `false` | 自動スクレイプ有効化（`true`で有効） |
| `TZ` | `Asia/Tokyo` | タイムゾーン |

---

## 注意事項 / Legal & Ethical Notes

- 各サービスの利用規約（ToS）を遵守してください
- クロール間隔は最低2秒以上（デフォルト: 2–3秒のランダムディレイ）
- `robots.txt`は事前に確認してください
- 個人・非商用目的での使用を想定しています
- 過度なアクセスは控えてください

---

## ライセンス / License

MIT License © 2025

---

[Portfolio](https://yourname.dev) · [GitHub](https://github.com/yourname) · [Zenn](https://zenn.dev/yourname)
