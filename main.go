// 価格追跡サービス — Yahoo! Japan / 楽天 / Amazon Japan Price Tracker
// Monitors product prices across major Japanese e-commerce platforms.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"
)

// ─────────────────────────────────────────────
// Platform definitions
// ─────────────────────────────────────────────

// Platform represents a supported e-commerce platform
type Platform string

const (
	PlatformRakuten  Platform = "rakuten"
	PlatformYahoo    Platform = "yahoo"
	PlatformAmazonJP Platform = "amazon_jp"
	PlatformMercari  Platform = "mercari"
	PlatformAuPay    Platform = "aupay"
)

var platformMeta = map[Platform]PlatformInfo{
	PlatformRakuten: {
		Name:     "楽天市場",
		NameEn:   "Rakuten Ichiba",
		Color:    "#BF0000",
		BaseURL:  "https://item.rakuten.co.jp",
		IconChar: "R",
	},
	PlatformYahoo: {
		Name:     "Yahoo!ショッピング",
		NameEn:   "Yahoo! Shopping",
		Color:    "#FF0033",
		BaseURL:  "https://shopping.yahoo.co.jp",
		IconChar: "Y",
	},
	PlatformAmazonJP: {
		Name:     "Amazon.co.jp",
		NameEn:   "Amazon Japan",
		Color:    "#FF9900",
		BaseURL:  "https://www.amazon.co.jp",
		IconChar: "A",
	},
	PlatformMercari: {
		Name:     "メルカリ",
		NameEn:   "Mercari",
		Color:    "#FF2D55",
		BaseURL:  "https://jp.mercari.com",
		IconChar: "M",
	},
	PlatformAuPay: {
		Name:     "au PAYマーケット",
		NameEn:   "au PAY Market",
		Color:    "#FF6600",
		BaseURL:  "https://www.au-paymarket.jp",
		IconChar: "P",
	},
}

// PlatformInfo holds metadata about a platform
type PlatformInfo struct {
	Name     string
	NameEn   string
	Color    string
	BaseURL  string
	IconChar string
}

// ─────────────────────────────────────────────
// Data Models
// ─────────────────────────────────────────────

// TrackedProduct is a product being monitored
type TrackedProduct struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	NameJa       string    `json:"name_ja,omitempty"`
	Category     string    `json:"category"`
	ImageURL     string    `json:"image_url,omitempty"`
	TargetPrice  int       `json:"target_price,omitempty"` // Alert if price drops below this (円)
	AlertPct     float64   `json:"alert_pct,omitempty"`    // Alert if drops by this % from peak
	Tags         []string  `json:"tags,omitempty"`
	IsActive     bool      `json:"is_active"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`

	// Per-platform listings
	Listings []ProductListing `json:"listings"`
}

// ProductListing tracks a product on a specific platform
type ProductListing struct {
	ProductID    string    `json:"product_id"`
	Platform     Platform  `json:"platform"`
	URL          string    `json:"url"`
	ShopName     string    `json:"shop_name,omitempty"`
	SKU          string    `json:"sku,omitempty"`
	IsActive     bool      `json:"is_active"`
	LastChecked  time.Time `json:"last_checked"`
	LastError    string    `json:"last_error,omitempty"`
	CheckCount   int       `json:"check_count"`
}

// PricePoint is a single price observation
type PricePoint struct {
	ID          string    `json:"id"`
	ProductID   string    `json:"product_id"`
	Platform    Platform  `json:"platform"`
	Price       int       `json:"price"`        // 円 (tax-included)
	PriceTaxEx  int       `json:"price_tax_ex"` // 税抜価格
	Points      int       `json:"points,omitempty"` // 楽天ポイント etc.
	IsAvailable bool      `json:"is_available"`
	IsSale      bool      `json:"is_sale"`
	Shipping    int       `json:"shipping,omitempty"` // 送料 (0=free)
	Seller      string    `json:"seller,omitempty"`
	Condition   string    `json:"condition,omitempty"` // "新品", "中古", etc.
	Stock       string    `json:"stock,omitempty"`     // "在庫あり", "残り3点", etc.
	CapturedAt  time.Time `json:"captured_at"`
}

// PriceAlert triggered when conditions are met
type PriceAlert struct {
	ID          string    `json:"id"`
	ProductID   string    `json:"product_id"`
	ProductName string    `json:"product_name"`
	Platform    Platform  `json:"platform"`
	AlertType   string    `json:"alert_type"` // "target_hit", "drop_pct", "new_low", "back_in_stock"
	Message     string    `json:"message"`
	OldPrice    int       `json:"old_price"`
	NewPrice    int       `json:"new_price"`
	DropPct     float64   `json:"drop_pct"`
	URL         string    `json:"url"`
	IsRead      bool      `json:"is_read"`
	CreatedAt   time.Time `json:"created_at"`
}

// ScrapeStat tracks scraping performance
type ScrapeStat struct {
	Platform    Platform  `json:"platform"`
	Success     int       `json:"success"`
	Errors      int       `json:"errors"`
	LastRun     time.Time `json:"last_run"`
	AvgDuration int       `json:"avg_duration_ms"`
}

// ─────────────────────────────────────────────
// Price History & Summary
// ─────────────────────────────────────────────

// PriceSummary aggregates price history for a product/platform
type PriceSummary struct {
	ProductID    string   `json:"product_id"`
	Platform     Platform `json:"platform"`
	CurrentPrice int      `json:"current_price"`
	AllTimeHigh  int      `json:"all_time_high"`
	AllTimeLow   int      `json:"all_time_low"`
	AvgPrice     int      `json:"avg_price"`
	PriceHistory []PricePoint `json:"price_history"`
	Change24h    int      `json:"change_24h"`
	ChangePct24h float64  `json:"change_pct_24h"`
	Change7d     int      `json:"change_7d"`
	ChangePct7d  float64  `json:"change_pct_7d"`
	TrendDir     string   `json:"trend_dir"` // "up", "down", "flat"
	DataPoints   int      `json:"data_points"`
}

// ─────────────────────────────────────────────
// In-Memory Store
// ─────────────────────────────────────────────

type PriceStore struct {
	mu       sync.RWMutex
	products map[string]*TrackedProduct
	history  map[string][]PricePoint // key: productID:platform
	alerts   []*PriceAlert
	stats    map[Platform]*ScrapeStat
}

var store = &PriceStore{
	products: make(map[string]*TrackedProduct),
	history:  make(map[string][]PricePoint),
	alerts:   []*PriceAlert{},
	stats:    make(map[Platform]*ScrapeStat),
}

func histKey(productID string, platform Platform) string {
	return productID + ":" + string(platform)
}

// ─────────────────────────────────────────────
// Platform-Specific Scrapers
// ─────────────────────────────────────────────

// Scraper handles the actual HTTP fetch + price extraction
type Scraper struct {
	client    *http.Client
	userAgents []string
	delay     time.Duration
}

func newScraper() *Scraper {
	return &Scraper{
		client: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
		// Rotate among realistic Japanese browser user agents
		userAgents: []string{
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
			"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_4_1) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4.1 Safari/605.1.15",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:125.0) Gecko/20100101 Firefox/125.0",
			"Mozilla/5.0 (iPhone; CPU iPhone OS 17_4_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4.1 Mobile/15E148 Safari/604.1",
		},
		delay: 2 * time.Second, // Polite crawl delay
	}
}

func (s *Scraper) fetch(targetURL string) ([]byte, error) {
	// Polite delay to avoid rate limiting
	jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
	time.Sleep(s.delay + jitter)

	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return nil, err
	}

	// Rotate user agent
	ua := s.userAgents[rand.Intn(len(s.userAgents))]
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "ja-JP,ja;q=0.9,en-US;q=0.8,en;q=0.7")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Referer", "https://www.google.co.jp/")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("rate limited (429) — backing off")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, targetURL)
	}

	// Detect charset and handle Shift-JIS (common on older Japanese sites)
	contentType := resp.Header.Get("Content-Type")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("body read error: %w", err)
	}

	if strings.Contains(strings.ToLower(contentType), "shift_jis") ||
		strings.Contains(strings.ToLower(contentType), "sjis") {
		decoded, _, err := transform.Bytes(japanese.ShiftJIS.NewDecoder(), body)
		if err == nil {
			body = decoded
		}
	} else if strings.Contains(strings.ToLower(contentType), "euc-jp") {
		decoded, _, err := transform.Bytes(japanese.EUCJP.NewDecoder(), body)
		if err == nil {
			body = decoded
		}
	}

	return body, nil
}

// parsePrice cleans and parses a Japanese price string to int
// Handles: "¥1,234", "1,234円", "1,234 円（税込）", "￥1,234"
func parsePrice(s string) (int, bool) {
	// Remove currency symbols, commas, whitespace, tax labels
	clean := regexp.MustCompile(`[¥￥,\s円（税込）（税抜）税込税抜]`).ReplaceAllString(s, "")
	clean = regexp.MustCompile(`[^\d]`).ReplaceAllString(clean, "")
	if clean == "" {
		return 0, false
	}
	n, err := strconv.Atoi(clean)
	if err != nil || n <= 0 || n > 50_000_000 {
		return 0, false
	}
	return n, true
}

// extractTextFromNode recursively gets text content
func extractTextFromNode(n *html.Node) string {
	if n.Type == html.TextNode {
		return strings.TrimSpace(n.Data)
	}
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		sb.WriteString(extractTextFromNode(c))
	}
	return strings.TrimSpace(sb.String())
}

// findNodeByAttr finds first node matching tag + attribute condition
func findNodeByAttr(n *html.Node, tag, attrKey, attrVal string) *html.Node {
	if n.Type == html.ElementNode {
		if tag == "" || n.Data == tag {
			for _, a := range n.Attr {
				if a.Key == attrKey && (attrVal == "" || strings.Contains(a.Val, attrVal)) {
					return n
				}
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findNodeByAttr(c, tag, attrKey, attrVal); found != nil {
			return found
		}
	}
	return nil
}

// findAllNodesByAttr returns all matching nodes
func findAllNodesByAttr(n *html.Node, tag, attrKey, attrVal string) []*html.Node {
	var result []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if tag == "" || n.Data == tag {
				for _, a := range n.Attr {
					if a.Key == attrKey && (attrVal == "" || strings.Contains(a.Val, attrVal)) {
						result = append(result, n)
						break
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return result
}

// ScrapeRakuten scrapes a Rakuten product page
// Handles the typical 楽天市場 item page structure
func (s *Scraper) ScrapeRakuten(productURL string) (*PricePoint, error) {
	body, err := s.fetch(productURL)
	if err != nil {
		return nil, err
	}

	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("HTML parse error: %w", err)
	}

	pp := &PricePoint{
		Platform:    PlatformRakuten,
		CapturedAt:  time.Now(),
		IsAvailable: false,
		Condition:   "新品",
	}

	// Try multiple selectors for price (Rakuten layout varies by shop)
	priceSelectors := []struct{ tag, attr, val string }{
		{"span", "class", "price--OHBQi"},
		{"span", "class", "price"},
		{"span", "itemprop", "price"},
		{"div", "class", "price"},
	}

	for _, sel := range priceSelectors {
		node := findNodeByAttr(doc, sel.tag, sel.attr, sel.val)
		if node != nil {
			text := extractTextFromNode(node)
			if p, ok := parsePrice(text); ok {
				pp.Price = p
				pp.IsAvailable = true
				break
			}
		}
	}

	// Fallback: scan all spans/divs with price-like content
	if pp.Price == 0 {
		priceRegex := regexp.MustCompile(`[¥￥][\d,]+|[\d,]+円`)
		var scanNode func(*html.Node)
		scanNode = func(n *html.Node) {
			if n.Type == html.TextNode {
				matches := priceRegex.FindAllString(n.Data, -1)
				for _, m := range matches {
					if p, ok := parsePrice(m); ok && p > 100 {
						pp.Price = p
						pp.IsAvailable = true
						return
					}
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if pp.Price == 0 {
					scanNode(c)
				}
			}
		}
		scanNode(doc)
	}

	// Extract points (楽天ポイント)
	pointsNode := findNodeByAttr(doc, "span", "class", "point")
	if pointsNode != nil {
		text := extractTextFromNode(pointsNode)
		pointsRegex := regexp.MustCompile(`([\d,]+)\s*ポイント`)
		if m := pointsRegex.FindStringSubmatch(text); len(m) > 1 {
			clean := strings.ReplaceAll(m[1], ",", "")
			pp.Points, _ = strconv.Atoi(clean)
		}
	}

	// Extract shop name
	shopNode := findNodeByAttr(doc, "", "itemprop", "seller")
	if shopNode == nil {
		shopNode = findNodeByAttr(doc, "span", "class", "shop-name")
	}
	if shopNode != nil {
		pp.Seller = extractTextFromNode(shopNode)
	}

	// Stock status
	stockNode := findNodeByAttr(doc, "", "class", "stock")
	if stockNode != nil {
		stockText := extractTextFromNode(stockNode)
		if strings.Contains(stockText, "在庫あり") || strings.Contains(stockText, "在庫") {
			pp.Stock = "在庫あり"
		} else if strings.Contains(stockText, "売り切れ") || strings.Contains(stockText, "在庫なし") {
			pp.Stock = "在庫なし"
			pp.IsAvailable = false
		}
	}

	// Shipping
	shippingNode := findNodeByAttr(doc, "", "class", "shipping")
	if shippingNode != nil {
		shippingText := extractTextFromNode(shippingNode)
		if strings.Contains(shippingText, "送料無料") || strings.Contains(shippingText, "無料") {
			pp.Shipping = 0
		} else if p, ok := parsePrice(shippingText); ok {
			pp.Shipping = p
		}
	}

	// Tax-excluded price (税抜価格)
	if pp.Price > 0 {
		pp.PriceTaxEx = int(math.Round(float64(pp.Price) / 1.1))
	}

	return pp, nil
}

// ScrapeYahooShopping scrapes Yahoo! Shopping product page
func (s *Scraper) ScrapeYahooShopping(productURL string) (*PricePoint, error) {
	body, err := s.fetch(productURL)
	if err != nil {
		return nil, err
	}

	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("HTML parse error: %w", err)
	}

	pp := &PricePoint{
		Platform:    PlatformYahoo,
		CapturedAt:  time.Now(),
		IsAvailable: false,
		Condition:   "新品",
	}

	// Yahoo Shopping price selectors
	priceSelectors := []struct{ tag, attr, val string }{
		{"span", "class", "sale_price"},
		{"span", "class", "normal_price"},
		{"span", "itemprop", "price"},
		{"p", "class", "price"},
		{"span", "class", "elPrice"},
	}

	for _, sel := range priceSelectors {
		node := findNodeByAttr(doc, sel.tag, sel.attr, sel.val)
		if node != nil {
			text := extractTextFromNode(node)
			if p, ok := parsePrice(text); ok {
				pp.Price = p
				pp.IsAvailable = true
				break
			}
		}
	}

	// Fallback regex scan
	if pp.Price == 0 {
		priceRegex := regexp.MustCompile(`[¥￥][\d,]+|[\d,]+円`)
		bodyStr := string(body)
		matches := priceRegex.FindAllString(bodyStr, 5)
		for _, m := range matches {
			if p, ok := parsePrice(m); ok && p > 100 {
				pp.Price = p
				pp.IsAvailable = true
				break
			}
		}
	}

	// Check for PayPay bonus (ヤフー独自)
	paypayNode := findNodeByAttr(doc, "span", "class", "paypay")
	if paypayNode == nil {
		paypayNode = findNodeByAttr(doc, "", "class", "bonus")
	}
	if paypayNode != nil {
		bonusText := extractTextFromNode(paypayNode)
		bonusRegex := regexp.MustCompile(`([\d.]+)%`)
		if m := bonusRegex.FindStringSubmatch(bonusText); len(m) > 1 {
			if pct, err := strconv.ParseFloat(m[1], 64); err == nil && pp.Price > 0 {
				pp.Points = int(float64(pp.Price) * pct / 100)
			}
		}
	}

	if pp.Price > 0 {
		pp.PriceTaxEx = int(math.Round(float64(pp.Price) / 1.1))
	}

	// Seller name
	sellerNode := findNodeByAttr(doc, "span", "class", "seller")
	if sellerNode == nil {
		sellerNode = findNodeByAttr(doc, "a", "class", "store")
	}
	if sellerNode != nil {
		pp.Seller = extractTextFromNode(sellerNode)
	}

	return pp, nil
}

// ScrapeAmazonJP scrapes Amazon Japan product page
func (s *Scraper) ScrapeAmazonJP(productURL string) (*PricePoint, error) {
	body, err := s.fetch(productURL)
	if err != nil {
		return nil, err
	}

	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("HTML parse error: %w", err)
	}

	pp := &PricePoint{
		Platform:    PlatformAmazonJP,
		CapturedAt:  time.Now(),
		IsAvailable: false,
		Condition:   "新品",
		Seller:      "Amazon.co.jp",
	}

	// Amazon price selectors (ID-based)
	priceIDs := []struct{ tag, attr, val string }{
		{"span", "id", "priceblock_ourprice"},
		{"span", "id", "priceblock_dealprice"},
		{"span", "id", "priceblock_saleprice"},
		{"span", "class", "a-price-whole"},
		{"span", "class", "priceToPay"},
	}

	for _, sel := range priceIDs {
		node := findNodeByAttr(doc, sel.tag, sel.attr, sel.val)
		if node != nil {
			text := extractTextFromNode(node)
			if p, ok := parsePrice(text); ok {
				pp.Price = p
				pp.IsAvailable = true
				break
			}
		}
	}

	// Check availability
	availNode := findNodeByAttr(doc, "", "id", "availability")
	if availNode != nil {
		availText := extractTextFromNode(availNode)
		if strings.Contains(availText, "在庫あり") || strings.Contains(availText, "通常") {
			pp.IsAvailable = true
			pp.Stock = "在庫あり"
		} else if strings.Contains(availText, "在庫なし") || strings.Contains(availText, "一時的に") {
			pp.IsAvailable = false
			pp.Stock = "在庫なし"
		}
	}

	// Prime shipping
	primeNode := findNodeByAttr(doc, "", "id", "primeIcon")
	if primeNode != nil {
		pp.Shipping = 0 // Prime = free shipping
	}

	if pp.Price > 0 {
		pp.PriceTaxEx = int(math.Round(float64(pp.Price) / 1.1))
	}

	return pp, nil
}

// ScrapeMercari scrapes Mercari Japan (フリマアプリ)
func (s *Scraper) ScrapeMercari(productURL string) (*PricePoint, error) {
	body, err := s.fetch(productURL)
	if err != nil {
		return nil, err
	}

	pp := &PricePoint{
		Platform:   PlatformMercari,
		CapturedAt: time.Now(),
		Condition:  "中古",
	}

	// Mercari uses React/Next.js with JSON in script tags
	// Extract __NEXT_DATA__ JSON
	nextDataRegex := regexp.MustCompile(`<script[^>]*id="__NEXT_DATA__"[^>]*>([\s\S]*?)<\/script>`)
	m := nextDataRegex.FindSubmatch(body)
	if len(m) > 1 {
		var nextData map[string]interface{}
		if err := json.Unmarshal(m[1], &nextData); err == nil {
			// Navigate props.pageProps.item
			if props, ok := nextData["props"].(map[string]interface{}); ok {
				if pageProps, ok := props["pageProps"].(map[string]interface{}); ok {
					if item, ok := pageProps["item"].(map[string]interface{}); ok {
						if price, ok := item["price"].(float64); ok {
							pp.Price = int(price)
							pp.IsAvailable = true
						}
						if status, ok := item["status"].(string); ok {
							if status == "on_sale" {
								pp.IsAvailable = true
								pp.Stock = "出品中"
							} else {
								pp.IsAvailable = false
								pp.Stock = "売り切れ"
							}
						}
						if condition, ok := item["item_condition"].(map[string]interface{}); ok {
							if name, ok := condition["name"].(string); ok {
								pp.Condition = name
							}
						}
					}
				}
			}
		}
	}

	// Fallback regex for Mercari
	if pp.Price == 0 {
		priceRegex := regexp.MustCompile(`"price"\s*:\s*(\d+)`)
		if pm := priceRegex.FindSubmatch(body); len(pm) > 1 {
			if p, err := strconv.Atoi(string(pm[1])); err == nil {
				pp.Price = p
				pp.IsAvailable = true
			}
		}
	}

	pp.PriceTaxEx = pp.Price // Mercari prices shown tax-included for consumers
	pp.Shipping = 0          // Mercari usually includes shipping or shows separately

	return pp, nil
}

// ScrapeProduct dispatches to the right scraper based on URL
func (s *Scraper) ScrapeProduct(listing ProductListing) (*PricePoint, error) {
	switch listing.Platform {
	case PlatformRakuten:
		return s.ScrapeRakuten(listing.URL)
	case PlatformYahoo:
		return s.ScrapeYahooShopping(listing.URL)
	case PlatformAmazonJP:
		return s.ScrapeAmazonJP(listing.URL)
	case PlatformMercari:
		return s.ScrapeMercari(listing.URL)
	default:
		return nil, fmt.Errorf("unsupported platform: %s", listing.Platform)
	}
}

// ─────────────────────────────────────────────
// Alert Engine
// ─────────────────────────────────────────────

// checkAlerts evaluates whether a new price point triggers any alerts
func checkAlerts(product *TrackedProduct, pp *PricePoint) *PriceAlert {
	key := histKey(product.ID, pp.Platform)

	store.mu.RLock()
	history := store.history[key]
	store.mu.RUnlock()

	if len(history) == 0 {
		return nil // No history to compare against
	}

	// Find previous price
	prev := history[len(history)-1]
	oldPrice := prev.Price

	if pp.Price <= 0 || oldPrice <= 0 {
		return nil
	}

	// Target price alert
	if product.TargetPrice > 0 && pp.Price <= product.TargetPrice && oldPrice > product.TargetPrice {
		dropPct := (float64(oldPrice-pp.Price) / float64(oldPrice)) * 100
		return &PriceAlert{
			ID:          fmt.Sprintf("alert-%d", time.Now().UnixNano()),
			ProductID:   product.ID,
			ProductName: product.Name,
			Platform:    pp.Platform,
			AlertType:   "target_hit",
			Message:     fmt.Sprintf("🎯 目標価格に到達！ ¥%s → ¥%s", formatPrice(oldPrice), formatPrice(pp.Price)),
			OldPrice:    oldPrice,
			NewPrice:    pp.Price,
			DropPct:     dropPct,
			CreatedAt:   time.Now(),
		}
	}

	// All-time low alert
	allTimeLow := oldPrice
	for _, h := range history {
		if h.Price > 0 && h.Price < allTimeLow {
			allTimeLow = h.Price
		}
	}
	if pp.Price < allTimeLow {
		dropPct := (float64(allTimeLow-pp.Price) / float64(allTimeLow)) * 100
		return &PriceAlert{
			ID:          fmt.Sprintf("alert-%d", time.Now().UnixNano()),
			ProductID:   product.ID,
			ProductName: product.Name,
			Platform:    pp.Platform,
			AlertType:   "new_low",
			Message:     fmt.Sprintf("📉 過去最安値更新！ ¥%s（前最安値: ¥%s）", formatPrice(pp.Price), formatPrice(allTimeLow)),
			OldPrice:    allTimeLow,
			NewPrice:    pp.Price,
			DropPct:     dropPct,
			CreatedAt:   time.Now(),
		}
	}

	// Percentage drop alert
	if product.AlertPct > 0 && oldPrice > 0 {
		dropPct := (float64(oldPrice-pp.Price) / float64(oldPrice)) * 100
		if dropPct >= product.AlertPct {
			return &PriceAlert{
				ID:          fmt.Sprintf("alert-%d", time.Now().UnixNano()),
				ProductID:   product.ID,
				ProductName: product.Name,
				Platform:    pp.Platform,
				AlertType:   "drop_pct",
				Message:     fmt.Sprintf("📉 %.1f%%値下がり！ ¥%s → ¥%s", dropPct, formatPrice(oldPrice), formatPrice(pp.Price)),
				OldPrice:    oldPrice,
				NewPrice:    pp.Price,
				DropPct:     dropPct,
				CreatedAt:   time.Now(),
			}
		}
	}

	// Back in stock alert
	if !prev.IsAvailable && pp.IsAvailable {
		return &PriceAlert{
			ID:          fmt.Sprintf("alert-%d", time.Now().UnixNano()),
			ProductID:   product.ID,
			ProductName: product.Name,
			Platform:    pp.Platform,
			AlertType:   "back_in_stock",
			Message:     fmt.Sprintf("✅ 在庫が復活しました！ 現在価格: ¥%s", formatPrice(pp.Price)),
			OldPrice:    0,
			NewPrice:    pp.Price,
			CreatedAt:   time.Now(),
		}
	}

	return nil
}

// ─────────────────────────────────────────────
// Price Summary Builder
// ─────────────────────────────────────────────

func buildSummary(productID string, platform Platform) PriceSummary {
	key := histKey(productID, platform)

	store.mu.RLock()
	history := store.history[key]
	store.mu.RUnlock()

	summary := PriceSummary{
		ProductID:    productID,
		Platform:     platform,
		PriceHistory: history,
		DataPoints:   len(history),
	}

	if len(history) == 0 {
		return summary
	}

	// Latest price
	summary.CurrentPrice = history[len(history)-1].Price

	// All-time stats
	high, low, total := 0, math.MaxInt32, 0
	count := 0
	for _, p := range history {
		if p.Price <= 0 {
			continue
		}
		count++
		total += p.Price
		if p.Price > high {
			high = p.Price
		}
		if p.Price < low {
			low = p.Price
		}
	}
	if count > 0 {
		summary.AllTimeHigh = high
		summary.AllTimeLow = low
		summary.AvgPrice = total / count
	}

	now := time.Now()

	// 24h change
	for i := len(history) - 1; i >= 0; i-- {
		if now.Sub(history[i].CapturedAt) >= 24*time.Hour {
			summary.Change24h = summary.CurrentPrice - history[i].Price
			if history[i].Price > 0 {
				summary.ChangePct24h = float64(summary.Change24h) / float64(history[i].Price) * 100
			}
			break
		}
	}

	// 7d change
	for i := len(history) - 1; i >= 0; i-- {
		if now.Sub(history[i].CapturedAt) >= 7*24*time.Hour {
			summary.Change7d = summary.CurrentPrice - history[i].Price
			if history[i].Price > 0 {
				summary.ChangePct7d = float64(summary.Change7d) / float64(history[i].Price) * 100
			}
			break
		}
	}

	// Trend direction (based on last 5 data points)
	if len(history) >= 2 {
		recent := history[max(0, len(history)-5):]
		firstP := recent[0].Price
		lastP := recent[len(recent)-1].Price
		if lastP < firstP {
			summary.TrendDir = "down"
		} else if lastP > firstP {
			summary.TrendDir = "up"
		} else {
			summary.TrendDir = "flat"
		}
	}

	return summary
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ─────────────────────────────────────────────
// Scheduler
// ─────────────────────────────────────────────

// Scheduler runs periodic price checks
type Scheduler struct {
	scraper   *Scraper
	interval  time.Duration
	stopCh    chan struct{}
	running   bool
	mu        sync.Mutex
}

func newScheduler(scraper *Scraper, interval time.Duration) *Scheduler {
	return &Scheduler{
		scraper:  scraper,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

func (sc *Scheduler) Start() {
	sc.mu.Lock()
	if sc.running {
		sc.mu.Unlock()
		return
	}
	sc.running = true
	sc.mu.Unlock()

	go func() {
		log.Printf("スケジューラー開始: チェック間隔 %v", sc.interval)
		// Initial run
		sc.runChecks()

		ticker := time.NewTicker(sc.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				sc.runChecks()
			case <-sc.stopCh:
				log.Println("スケジューラー停止")
				return
			}
		}
	}()
}

func (sc *Scheduler) Stop() {
	close(sc.stopCh)
}

func (sc *Scheduler) runChecks() {
	store.mu.RLock()
	products := make([]*TrackedProduct, 0, len(store.products))
	for _, p := range store.products {
		if p.IsActive {
			products = append(products, p)
		}
	}
	store.mu.RUnlock()

	for _, product := range products {
		for _, listing := range product.Listings {
			if !listing.IsActive {
				continue
			}

			start := time.Now()
			pp, err := sc.scraper.ScrapeProduct(listing)
			duration := int(time.Since(start).Milliseconds())

			store.mu.Lock()

			// Update stats
			if store.stats[listing.Platform] == nil {
				store.stats[listing.Platform] = &ScrapeStat{Platform: listing.Platform}
			}
			stat := store.stats[listing.Platform]
			stat.LastRun = time.Now()
			if stat.AvgDuration == 0 {
				stat.AvgDuration = duration
			} else {
				stat.AvgDuration = (stat.AvgDuration + duration) / 2
			}

			if err != nil {
				stat.Errors++
				log.Printf("スクレイプエラー [%s] %s: %v", listing.Platform, product.Name, err)

				// Update listing error
				for i := range product.Listings {
					if product.Listings[i].Platform == listing.Platform {
						product.Listings[i].LastError = err.Error()
						product.Listings[i].LastChecked = time.Now()
						product.Listings[i].CheckCount++
					}
				}
				store.mu.Unlock()
				continue
			}

			stat.Success++

			// Set IDs
			pp.ID = fmt.Sprintf("pp-%d", time.Now().UnixNano())
			pp.ProductID = product.ID

			// Append to history (keep last 90 days / 500 points)
			key := histKey(product.ID, listing.Platform)
			store.history[key] = append(store.history[key], *pp)
			if len(store.history[key]) > 500 {
				store.history[key] = store.history[key][len(store.history[key])-500:]
			}

			// Update listing
			for i := range product.Listings {
				if product.Listings[i].Platform == listing.Platform {
					product.Listings[i].LastChecked = time.Now()
					product.Listings[i].LastError = ""
					product.Listings[i].CheckCount++
				}
			}
			product.UpdatedAt = time.Now()

			store.mu.Unlock()

			// Check alerts (outside lock)
			if alert := checkAlerts(product, pp); alert != nil {
				alert.URL = listing.URL
				store.mu.Lock()
				store.alerts = append(store.alerts, alert)
				store.mu.Unlock()
				log.Printf("🔔 アラート: %s — %s", product.Name, alert.Message)
			}

			log.Printf("✓ [%s] %s: ¥%s (%dms)", listing.Platform, product.Name, formatPrice(pp.Price), duration)
		}
	}
}

// ─────────────────────────────────────────────
// Seed Data
// ─────────────────────────────────────────────

func seedData() {
	now := time.Now()
	jst, _ := time.LoadLocation("Asia/Tokyo")

	products := []*TrackedProduct{
		{
			ID: "prod-001", Name: "Sony WH-1000XM5", NameJa: "ソニー WH-1000XM5 ワイヤレスノイキャンヘッドホン",
			Category: "ヘッドホン・イヤホン", AlertPct: 5, TargetPrice: 35000,
			Tags: []string{"sony", "ヘッドホン", "ノイキャン", "ワイヤレス"},
			IsActive: true, CreatedAt: now, UpdatedAt: now,
			Listings: []ProductListing{
				{ProductID: "prod-001", Platform: PlatformRakuten, URL: "https://item.rakuten.co.jp/edion/4548736132276/", IsActive: true},
				{ProductID: "prod-001", Platform: PlatformYahoo, URL: "https://shopping.yahoo.co.jp/product/detail/4548736132276/", IsActive: true},
				{ProductID: "prod-001", Platform: PlatformAmazonJP, URL: "https://www.amazon.co.jp/dp/B09XS7JWHH", IsActive: true},
			},
		},
		{
			ID: "prod-002", Name: "Nintendo Switch 本体", NameJa: "Nintendo Switch（有機ELモデル）",
			Category: "ゲーム機・本体", AlertPct: 3, TargetPrice: 30000,
			Tags: []string{"nintendo", "switch", "ゲーム機"},
			IsActive: true, CreatedAt: now, UpdatedAt: now,
			Listings: []ProductListing{
				{ProductID: "prod-002", Platform: PlatformRakuten, URL: "https://item.rakuten.co.jp/yodobashi/4902370548495/", IsActive: true},
				{ProductID: "prod-002", Platform: PlatformYahoo, URL: "https://shopping.yahoo.co.jp/product/detail/4902370548495/", IsActive: true},
				{ProductID: "prod-002", Platform: PlatformAmazonJP, URL: "https://www.amazon.co.jp/dp/B098RL6SBJ", IsActive: true},
				{ProductID: "prod-002", Platform: PlatformMercari, URL: "https://jp.mercari.com/search?keyword=Nintendo+Switch+有機EL", IsActive: true},
			},
		},
		{
			ID: "prod-003", Name: "Apple iPhone 16 Pro", NameJa: "Apple iPhone 16 Pro 256GB",
			Category: "スマートフォン", AlertPct: 5, TargetPrice: 140000,
			Tags: []string{"apple", "iphone", "スマホ"},
			IsActive: true, CreatedAt: now, UpdatedAt: now,
			Listings: []ProductListing{
				{ProductID: "prod-003", Platform: PlatformRakuten, URL: "https://item.rakuten.co.jp/biccamera/0045400000000/", IsActive: true},
				{ProductID: "prod-003", Platform: PlatformAmazonJP, URL: "https://www.amazon.co.jp/dp/B0CX23V2ZK", IsActive: true},
			},
		},
		{
			ID: "prod-004", Name: "Dyson V15 Detect", NameJa: "ダイソン コードレス掃除機 V15 Detect",
			Category: "家電・掃除機", AlertPct: 8, TargetPrice: 60000,
			Tags: []string{"dyson", "掃除機", "コードレス"},
			IsActive: true, CreatedAt: now, UpdatedAt: now,
			Listings: []ProductListing{
				{ProductID: "prod-004", Platform: PlatformRakuten, URL: "https://item.rakuten.co.jp/dyson-jp/sv22ff/", IsActive: true},
				{ProductID: "prod-004", Platform: PlatformYahoo, URL: "https://shopping.yahoo.co.jp/product/detail/dysonv15/", IsActive: true},
				{ProductID: "prod-004", Platform: PlatformAmazonJP, URL: "https://www.amazon.co.jp/dp/B08LNKM6JX", IsActive: true},
			},
		},
		{
			ID: "prod-005", Name: "LEGO テクニック ブガッティ", NameJa: "LEGO テクニック ブガッティ シロン 42083",
			Category: "おもちゃ・ホビー", AlertPct: 10, TargetPrice: 35000,
			Tags: []string{"lego", "テクニック"},
			IsActive: true, CreatedAt: now, UpdatedAt: now,
			Listings: []ProductListing{
				{ProductID: "prod-005", Platform: PlatformRakuten, URL: "https://item.rakuten.co.jp/toysrus/5702016112603/", IsActive: true},
				{ProductID: "prod-005", Platform: PlatformAmazonJP, URL: "https://www.amazon.co.jp/dp/B078967NP3", IsActive: true},
				{ProductID: "prod-005", Platform: PlatformMercari, URL: "https://jp.mercari.com/search?keyword=LEGO+42083", IsActive: true},
			},
		},
	}

	// Seed realistic price history (simulated, going back 30 days)
	priceSeeds := map[string]map[Platform][]int{
		"prod-001": {
			PlatformRakuten:  {42800, 42800, 41500, 41500, 40200, 40200, 39800, 39800, 38900, 38900, 37500, 37500, 37500, 36800, 36800},
			PlatformYahoo:    {43200, 43200, 42100, 42100, 40800, 40800, 40100, 39500, 39500, 38200, 38200, 37800, 37800, 37100, 37100},
			PlatformAmazonJP: {44000, 43500, 43500, 42000, 42000, 41000, 40500, 40500, 39200, 39200, 38500, 38500, 38000, 37500, 37500},
		},
		"prod-002": {
			PlatformRakuten:  {37980, 37980, 37980, 36500, 36500, 35980, 35980, 35980, 34500, 34500, 34500, 33980, 33980, 33500, 33500},
			PlatformYahoo:    {38500, 38500, 37200, 37200, 36800, 36800, 35500, 35500, 34800, 34800, 34200, 34200, 33700, 33700, 33200},
			PlatformAmazonJP: {37980, 37980, 37980, 37980, 36980, 36980, 35980, 35980, 35980, 34980, 34980, 34980, 33980, 33980, 33980},
			PlatformMercari:  {28000, 29000, 27500, 30000, 28000, 27000, 29500, 28000, 26000, 29000, 27000, 28500, 26500, 27000, 25000},
		},
		"prod-003": {
			PlatformRakuten:  {178000, 178000, 175000, 175000, 172000, 172000, 169800, 169800, 165000, 165000, 162000, 162000, 159800, 159800, 158000},
			PlatformAmazonJP: {180000, 179000, 179000, 176000, 176000, 173000, 170000, 170000, 167000, 167000, 163000, 163000, 160000, 160000, 158000},
		},
		"prod-004": {
			PlatformRakuten:  {84800, 84800, 82000, 82000, 79800, 79800, 77500, 77500, 74900, 74900, 72000, 72000, 69800, 69800, 68000},
			PlatformYahoo:    {85500, 83000, 83000, 80500, 80500, 78000, 78000, 75500, 75500, 73000, 73000, 70500, 70500, 68500, 68500},
			PlatformAmazonJP: {86800, 85000, 85000, 82000, 82000, 80000, 78000, 78000, 75000, 75000, 72000, 72000, 70000, 70000, 68500},
		},
		"prod-005": {
			PlatformRakuten:  {58000, 58000, 55000, 52000, 52000, 50000, 48000, 48000, 46000, 44000, 44000, 42000, 40000, 40000, 38000},
			PlatformAmazonJP: {60000, 58000, 55000, 53000, 51000, 49000, 47000, 46000, 45000, 43000, 42000, 41000, 39800, 38000, 37000},
			PlatformMercari:  {35000, 34000, 33000, 36000, 32000, 31000, 30000, 33000, 29000, 31000, 28000, 30000, 27000, 28000, 26000},
		},
	}

	now2 := time.Now()
	for _, product := range products {
		store.products[product.ID] = product
		seeds := priceSeeds[product.ID]

		for platform, prices := range seeds {
			key := histKey(product.ID, platform)
			for i, price := range prices {
				daysAgo := len(prices) - 1 - i
				hoursOffset := rand.Intn(4)
				capturedAt := now2.In(jst).AddDate(0, 0, -daysAgo).Add(-time.Duration(hoursOffset) * time.Hour)

				pp := PricePoint{
					ID:          fmt.Sprintf("pp-%s-%s-%d", product.ID, platform, i),
					ProductID:   product.ID,
					Platform:    platform,
					Price:       price,
					PriceTaxEx:  int(math.Round(float64(price) / 1.1)),
					IsAvailable: true,
					Condition:   "新品",
					CapturedAt:  capturedAt,
				}

				// Mercari = used, occasional unavailable
				if platform == PlatformMercari {
					pp.Condition = "中古"
					if rand.Float32() < 0.1 {
						pp.IsAvailable = false
						pp.Stock = "売り切れ"
					} else {
						pp.Stock = "出品中"
					}
				} else {
					pp.Stock = "在庫あり"
					// Simulate some shipping costs
					if rand.Float32() < 0.3 {
						pp.Shipping = 550
					}
				}

				// Rakuten points
				if platform == PlatformRakuten {
					pp.Points = int(float64(price) * 0.01) // 1% points
				}

				store.history[key] = append(store.history[key], pp)
			}
		}
	}

	// Seed some sample alerts
	store.alerts = []*PriceAlert{
		{
			ID: "alert-001", ProductID: "prod-001", ProductName: "Sony WH-1000XM5",
			Platform: PlatformRakuten, AlertType: "new_low",
			Message: "📉 過去最安値更新！ ¥36,800（前最安値: ¥37,500）",
			OldPrice: 37500, NewPrice: 36800, DropPct: 1.87,
			URL: "https://item.rakuten.co.jp/edion/4548736132276/",
			IsRead: false, CreatedAt: now.Add(-2 * time.Hour),
		},
		{
			ID: "alert-002", ProductID: "prod-002", ProductName: "Nintendo Switch 本体",
			Platform: PlatformMercari, AlertType: "drop_pct",
			Message: "📉 9.6%値下がり！ ¥27,000 → ¥24,400",
			OldPrice: 27000, NewPrice: 24400, DropPct: 9.6,
			URL: "https://jp.mercari.com/search?keyword=Nintendo+Switch+有機EL",
			IsRead: false, CreatedAt: now.Add(-5 * time.Hour),
		},
		{
			ID: "alert-003", ProductID: "prod-004", ProductName: "Dyson V15 Detect",
			Platform: PlatformYahoo, AlertType: "target_hit",
			Message: "🎯 目標価格に到達！ ¥70,500 → ¥68,500",
			OldPrice: 70500, NewPrice: 68500, DropPct: 2.84,
			URL: "https://shopping.yahoo.co.jp/product/detail/dysonv15/",
			IsRead: true, CreatedAt: now.Add(-24 * time.Hour),
		},
	}

	log.Printf("シードデータ読み込み完了: 商品 %d 件", len(store.products))
}

// ─────────────────────────────────────────────
// HTTP API Handlers
// ─────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

func formatPrice(n int) string {
	s := strconv.Itoa(n)
	var result strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result.WriteRune(',')
		}
		result.WriteRune(c)
	}
	return result.String()
}

// GET /api/v1/products
func handleProducts(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions { w.WriteHeader(204); return }

	store.mu.RLock()
	var list []*TrackedProduct
	for _, p := range store.products {
		list = append(list, p)
	}
	store.mu.RUnlock()

	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	writeJSON(w, 200, map[string]interface{}{"count": len(list), "products": list})
}

// POST /api/v1/products
func handleCreateProduct(w http.ResponseWriter, r *http.Request) {
	var p TrackedProduct
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, 400, "invalid_json", "JSONの形式が正しくありません")
		return
	}
	if p.Name == "" {
		writeError(w, 400, "missing_name", "商品名は必須です")
		return
	}

	store.mu.Lock()
	p.ID = fmt.Sprintf("prod-%03d", len(store.products)+1)
	p.IsActive = true
	p.CreatedAt = time.Now()
	p.UpdatedAt = time.Now()
	store.products[p.ID] = &p
	store.mu.Unlock()

	writeJSON(w, 201, p)
}

// GET /api/v1/products/:id/history
func handlePriceHistory(w http.ResponseWriter, r *http.Request, productID string) {
	platform := Platform(r.URL.Query().Get("platform"))
	daysStr := r.URL.Query().Get("days")
	days := 30
	if daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d > 0 {
			days = d
		}
	}

	cutoff := time.Now().AddDate(0, 0, -days)

	store.mu.RLock()
	_, exists := store.products[productID]
	if !exists {
		store.mu.RUnlock()
		writeError(w, 404, "not_found", "商品が見つかりません")
		return
	}

	var summaries []PriceSummary
	if platform != "" {
		key := histKey(productID, platform)
		history := store.history[key]
		store.mu.RUnlock()

		// Filter by date range
		var filtered []PricePoint
		for _, pp := range history {
			if pp.CapturedAt.After(cutoff) {
				filtered = append(filtered, pp)
			}
		}
		s := buildSummary(productID, platform)
		s.PriceHistory = filtered
		summaries = []PriceSummary{s}
	} else {
		// All platforms
		allKeys := []string{}
		for k := range store.history {
			if strings.HasPrefix(k, productID+":") {
				allKeys = append(allKeys, k)
			}
		}
		store.mu.RUnlock()

		for _, k := range allKeys {
			parts := strings.SplitN(k, ":", 2)
			if len(parts) == 2 {
				plat := Platform(parts[1])
				s := buildSummary(productID, plat)
				// Filter history
				var filtered []PricePoint
				for _, pp := range s.PriceHistory {
					if pp.CapturedAt.After(cutoff) {
						filtered = append(filtered, pp)
					}
				}
				s.PriceHistory = filtered
				summaries = append(summaries, s)
			}
		}
	}

	writeJSON(w, 200, map[string]interface{}{
		"product_id": productID,
		"days":       days,
		"summaries":  summaries,
	})
}

// GET /api/v1/products/:id/compare
func handleCompare(w http.ResponseWriter, r *http.Request, productID string) {
	store.mu.RLock()
	product, exists := store.products[productID]
	if !exists {
		store.mu.RUnlock()
		writeError(w, 404, "not_found", "商品が見つかりません")
		return
	}

	type PlatformComparison struct {
		Platform     Platform    `json:"platform"`
		PlatformName string      `json:"platform_name"`
		CurrentPrice int         `json:"current_price"`
		AllTimeLow   int         `json:"all_time_low"`
		AllTimeHigh  int         `json:"all_time_high"`
		TrendDir     string      `json:"trend_dir"`
		Points       int         `json:"points,omitempty"`
		URL          string      `json:"url"`
		IsAvailable  bool        `json:"is_available"`
		LastChecked  time.Time   `json:"last_checked"`
	}

	var comparisons []PlatformComparison
	for _, listing := range product.Listings {
		if !listing.IsActive {
			continue
		}
		s := buildSummary(productID, listing.Platform)
		meta := platformMeta[listing.Platform]

		latest := PricePoint{}
		key := histKey(productID, listing.Platform)
		if h := store.history[key]; len(h) > 0 {
			latest = h[len(h)-1]
		}

		comparisons = append(comparisons, PlatformComparison{
			Platform:     listing.Platform,
			PlatformName: meta.Name,
			CurrentPrice: s.CurrentPrice,
			AllTimeLow:   s.AllTimeLow,
			AllTimeHigh:  s.AllTimeHigh,
			TrendDir:     s.TrendDir,
			Points:       latest.Points,
			URL:          listing.URL,
			IsAvailable:  latest.IsAvailable,
			LastChecked:  listing.LastChecked,
		})
	}
	store.mu.RUnlock()

	// Sort by current price
	sort.Slice(comparisons, func(i, j int) bool {
		return comparisons[i].CurrentPrice < comparisons[j].CurrentPrice
	})

	writeJSON(w, 200, map[string]interface{}{
		"product_id":   productID,
		"product_name": product.Name,
		"comparisons":  comparisons,
		"best_price": func() interface{} {
			if len(comparisons) == 0 {
				return nil
			}
			return comparisons[0]
		}(),
	})
}

// GET /api/v1/alerts
func handleAlerts(w http.ResponseWriter, r *http.Request) {
	unreadOnly := r.URL.Query().Get("unread") == "true"

	store.mu.RLock()
	var alerts []*PriceAlert
	for _, a := range store.alerts {
		if unreadOnly && a.IsRead {
			continue
		}
		alerts = append(alerts, a)
	}
	store.mu.RUnlock()

	// Sort newest first
	sort.Slice(alerts, func(i, j int) bool {
		return alerts[i].CreatedAt.After(alerts[j].CreatedAt)
	})

	unread := 0
	for _, a := range store.alerts {
		if !a.IsRead {
			unread++
		}
	}

	writeJSON(w, 200, map[string]interface{}{
		"count":  len(alerts),
		"unread": unread,
		"alerts": alerts,
	})
}

// POST /api/v1/alerts/:id/read
func handleMarkRead(w http.ResponseWriter, r *http.Request, id string) {
	store.mu.Lock()
	defer store.mu.Unlock()

	for _, a := range store.alerts {
		if a.ID == id {
			a.IsRead = true
			writeJSON(w, 200, a)
			return
		}
	}
	writeError(w, 404, "not_found", "アラートが見つかりません")
}

// GET /api/v1/stats
func handleStats(w http.ResponseWriter, r *http.Request) {
	store.mu.RLock()
	defer store.mu.RUnlock()

	totalProducts := len(store.products)
	totalListings := 0
	for _, p := range store.products {
		totalListings += len(p.Listings)
	}

	totalPricePoints := 0
	for _, h := range store.history {
		totalPricePoints += len(h)
	}

	unreadAlerts := 0
	for _, a := range store.alerts {
		if !a.IsRead {
			unreadAlerts++
		}
	}

	writeJSON(w, 200, map[string]interface{}{
		"total_products":     totalProducts,
		"total_listings":     totalListings,
		"total_price_points": totalPricePoints,
		"total_alerts":       len(store.alerts),
		"unread_alerts":      unreadAlerts,
		"platforms":          store.stats,
		"supported_platforms": []string{
			string(PlatformRakuten),
			string(PlatformYahoo),
			string(PlatformAmazonJP),
			string(PlatformMercari),
			string(PlatformAuPay),
		},
	})
}

// POST /api/v1/scrape/:productID — manual scrape trigger
func handleManualScrape(w http.ResponseWriter, r *http.Request, productID string, sc *Scheduler) {
	store.mu.RLock()
	product, ok := store.products[productID]
	store.mu.RUnlock()

	if !ok {
		writeError(w, 404, "not_found", "商品が見つかりません")
		return
	}

	// Run in background
	go func() {
		for _, listing := range product.Listings {
			if !listing.IsActive {
				continue
			}
			pp, err := sc.scraper.ScrapeProduct(listing)
			if err != nil {
				log.Printf("手動スクレイプエラー [%s]: %v", listing.Platform, err)
				continue
			}
			pp.ID = fmt.Sprintf("pp-%d", time.Now().UnixNano())
			pp.ProductID = product.ID

			store.mu.Lock()
			key := histKey(product.ID, listing.Platform)
			store.history[key] = append(store.history[key], *pp)
			store.mu.Unlock()

			log.Printf("手動スクレイプ完了 [%s] %s: ¥%s", listing.Platform, product.Name, formatPrice(pp.Price))
		}
	}()

	writeJSON(w, 202, map[string]string{
		"message":    "スクレイプを開始しました",
		"product_id": productID,
	})
}

// ─────────────────────────────────────────────
// CLI Mode
// ─────────────────────────────────────────────

func runCLI(args []string) {
	fs := flag.NewFlagSet("price-tracker", flag.ExitOnError)
	platform := fs.String("platform", "rakuten", "プラットフォーム (rakuten|yahoo|amazon_jp|mercari)")
	outputFmt := fs.String("format", "table", "出力形式 (table|json|csv)")
	fs.Parse(args)

	urls := fs.Args()
	if len(urls) == 0 {
		fmt.Fprintln(os.Stderr, "使い方: price-tracker [options] <URL> [URL...]")
		fmt.Fprintln(os.Stderr, "例: price-tracker -platform rakuten https://item.rakuten.co.jp/...")
		fs.Usage()
		os.Exit(1)
	}

	scraper := newScraper()
	var results []struct {
		URL   string
		Price *PricePoint
		Error error
	}

	for _, u := range urls {
		listing := ProductListing{
			Platform: Platform(*platform),
			URL:      u,
		}

		fmt.Fprintf(os.Stderr, "スクレイプ中: %s\n", u)
		pp, err := scraper.ScrapeProduct(listing)
		results = append(results, struct {
			URL   string
			Price *PricePoint
			Error error
		}{u, pp, err})
	}

	switch *outputFmt {
	case "json":
		json.NewEncoder(os.Stdout).Encode(results)

	case "csv":
		fmt.Println("URL,Platform,Price,TaxExPrice,Available,Points,Shipping,Seller,CapturedAt")
		for _, r := range results {
			if r.Error != nil || r.Price == nil {
				fmt.Printf("%s,%s,ERROR,,,,,,%s\n", r.URL, *platform, r.Error)
				continue
			}
			fmt.Printf("%s,%s,%d,%d,%v,%d,%d,%s,%s\n",
				r.URL, r.Price.Platform, r.Price.Price, r.Price.PriceTaxEx,
				r.Price.IsAvailable, r.Price.Points, r.Price.Shipping,
				r.Price.Seller, r.Price.CapturedAt.Format(time.RFC3339))
		}

	default: // table
		fmt.Printf("\n%-60s %-12s %-10s %-10s %-8s\n",
			"URL", "プラットフォーム", "価格(税込)", "税抜価格", "在庫")
		fmt.Println(strings.Repeat("─", 100))
		for _, r := range results {
			u := r.URL
			if len(u) > 58 {
				u = u[:55] + "..."
			}
			if r.Error != nil {
				fmt.Printf("%-60s %-12s ERROR: %v\n", u, *platform, r.Error)
				continue
			}
			avail := "あり"
			if !r.Price.IsAvailable {
				avail = "なし"
			}
			fmt.Printf("%-60s %-12s ¥%-9s ¥%-9s %-8s\n",
				u, r.Price.Platform,
				formatPrice(r.Price.Price),
				formatPrice(r.Price.PriceTaxEx),
				avail)
		}
		fmt.Println()
	}
}

// ─────────────────────────────────────────────
// Main / Router
// ─────────────────────────────────────────────

func main() {
	rand.Seed(time.Now().UnixNano())

	// CLI mode: if -url flag or non-flag args present
	if len(os.Args) > 1 && os.Args[1] == "scrape" {
		runCLI(os.Args[2:])
		return
	}

	seedData()

	scraper := newScraper()
	intervalStr := os.Getenv("CHECK_INTERVAL")
	interval := 30 * time.Minute
	if intervalStr != "" {
		if d, err := time.ParseDuration(intervalStr); err == nil {
			interval = d
		}
	}

	scheduler := newScheduler(scraper, interval)

	// Only start scheduler in production (not during demo)
	if os.Getenv("AUTO_SCRAPE") == "true" {
		scheduler.Start()
	}

	mux := http.NewServeMux()

	// CORS preflight
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(204)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/api/v1")
		parts := strings.Split(strings.Trim(path, "/"), "/")

		switch {
		case path == "/products" || path == "/products/":
			if r.Method == http.MethodGet {
				handleProducts(w, r)
			} else if r.Method == http.MethodPost {
				handleCreateProduct(w, r)
			}

		case len(parts) == 3 && parts[0] == "products" && parts[2] == "history":
			handlePriceHistory(w, r, parts[1])

		case len(parts) == 3 && parts[0] == "products" && parts[2] == "compare":
			handleCompare(w, r, parts[1])

		case len(parts) == 3 && parts[0] == "products" && parts[2] == "scrape":
			handleManualScrape(w, r, parts[1], scheduler)

		case path == "/alerts" || path == "/alerts/":
			handleAlerts(w, r)

		case len(parts) == 3 && parts[0] == "alerts" && parts[2] == "read":
			handleMarkRead(w, r, parts[1])

		case path == "/stats":
			handleStats(w, r)

		default:
			writeError(w, 404, "not_found", fmt.Sprintf("エンドポイントが見つかりません: %s", path))
		}
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]interface{}{
			"status":   "ok",
			"service":  "価格追跡サービス",
			"version":  "1.0.0",
			"products": len(store.products),
		})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		endpoints := `価格追跡サービス v1.0.0
━━━━━━━━━━━━━━━━━━━━━━━━━━━

対応プラットフォーム:
  楽天市場 / Yahoo!ショッピング / Amazon.co.jp / メルカリ / au PAYマーケット

REST API:
  GET  /api/v1/products                       監視商品一覧
  POST /api/v1/products                       商品追加
  GET  /api/v1/products/:id/history           価格履歴 (?platform=&days=)
  GET  /api/v1/products/:id/compare           プラットフォーム間価格比較
  POST /api/v1/products/:id/scrape            手動スクレイプ
  GET  /api/v1/alerts                         アラート一覧 (?unread=true)
  POST /api/v1/alerts/:id/read                既読にする
  GET  /api/v1/stats                          統計情報
  GET  /health                                ヘルスチェック

CLIモード:
  price-tracker scrape -platform rakuten <URL>
  price-tracker scrape -platform yahoo -format json <URL> [URL...]
  price-tracker scrape -platform amazon_jp -format csv <URL>

環境変数:
  CHECK_INTERVAL=30m   価格チェック間隔 (default: 30m)
  AUTO_SCRAPE=true     自動スクレイプ有効化
  PORT=8085            ポート番号`
		fmt.Fprintln(w, endpoints)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8085"
	}

	log.Printf("価格追跡サービス 起動中: http://localhost:%s", port)
	log.Printf("対応プラットフォーム: 楽天市場, Yahoo!ショッピング, Amazon.co.jp, メルカリ")
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// urlDomain extracts domain from URL string
func urlDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Host
}
