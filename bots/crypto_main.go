package main

// crypto_bot.go — Bitcoin & BTC-equity daily digest → Campfire
//
// Data sources (all free, no API keys required):
//   BTC price/vol  : CoinGecko public API
//   Fear & Greed   : alternative.me
//   Stocks         : Yahoo Finance v8 (unofficial, key-free)
//
// Tickers tracked:
//   MSTR  – Strategy Inc. (Bitcoin treasury company, formerly MicroStrategy)
//   STRC  – Strategy Variable Rate Series A Perpetual Stretch Preferred
//   STRK  – Strategy 8.00% Series A Perpetual Strike Preferred
//   STRF  – Strategy 10.00% Series A Perpetual Strife Preferred
//   STRD  – Strategy 10.00% Series A Perpetual Stride Preferred
//   IBIT  – iShares Bitcoin Trust ETF (BlackRock)
//   XXI   – Twenty One Capital (Tether/Bitfinex/SoftBank BTC treasury)
//
// Sentiment score (0–100):
//   50% weight  → Crypto Fear & Greed Index (alternative.me)
//   30% weight  → BTC 24 h price momentum  (scaled −10%→+10% range)
//   20% weight  → Per-equity price momentum (day change %)

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ─── Env-file loader ─────────────────────────────────────────────────────────
//
// loadEnvFiles loads .env and then .env.local from dir, in that order.
// Rules:
//   - .env.local values override .env values
//   - Variables already present in the process environment are never clobbered
//     (so values injected by cron/systemd/CI always win)
//   - Blank lines and lines starting with # are ignored
//   - Inline comments (value # comment) are NOT stripped — keep values clean

func loadEnvFiles(dir string) {
	for _, name := range []string{".env", ".env.local"} {
		path := filepath.Join(dir, name)
		f, err := os.Open(path)
		if err != nil {
			continue // file simply absent — that's fine
		}
		log.Printf("Loading %s", path)
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, val, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			val = strings.TrimSpace(val)
			// Strip optional surrounding quotes
			if len(val) >= 2 {
				if (val[0] == '"' && val[len(val)-1] == '"') ||
					(val[0] == '\'' && val[len(val)-1] == '\'') {
					val = val[1 : len(val)-1]
				}
			}
			if key == "" {
				continue
			}
			// Never overwrite a value already set in the environment
			if os.Getenv(key) == "" {
				os.Setenv(key, val)
			}
		}
		f.Close()
	}
}

// ─── Config ──────────────────────────────────────────────────────────────────

const (
	coinGeckoURL  = "https://api.coingecko.com/api/v3/simple/price?ids=bitcoin&vs_currencies=usd&include_24hr_change=true&include_24hr_vol=true&include_market_cap=true"
	fearGreedURL  = "https://api.alternative.me/fng/?limit=1"
	yahooQuoteURL = "https://query1.finance.yahoo.com/v8/finance/chart/%s?interval=1d&range=5d"
)

// campfireURL is loaded from the CAMPFIRE_WEBHOOK_URL environment variable at startup.
var campfireURL string

// Equity entries in display order
var equities = []struct {
	Symbol string
	Label  string
}{
	{"MSTR", "Strategy (MSTR)"},
	{"STRC", "Strategy STRC (Stretch Pref)"},
	{"STRK", "Strategy STRK (Strike Pref)"},
	{"STRF", "Strategy STRF (Strife Pref)"},
	{"STRD", "Strategy STRD (Stride Pref)"},
	{"IBIT", "iShares Bitcoin Trust (IBIT)"},
	{"XXI", "Twenty One Capital (XXI)"},
}

// gFinanceURL returns the Google Finance URL for a given ticker symbol.
// Crypto uses the bare SYMBOL-USD form; equities use SYMBOL:EXCHANGE.
var gFinanceURLs = map[string]string{
	"BTC":  "https://www.google.com/finance/quote/BTC-USD",
	"MSTR": "https://www.google.com/finance/quote/MSTR:NASDAQ",
	"STRC": "https://www.google.com/finance/quote/STRC:NASDAQ",
	"STRK": "https://www.google.com/finance/quote/STRK:NASDAQ",
	"STRF": "https://www.google.com/finance/quote/STRF:NASDAQ",
	"STRD": "https://www.google.com/finance/quote/STRD:NASDAQ",
	"IBIT": "https://www.google.com/finance/quote/IBIT:NASDAQ",
	"XXI":  "https://www.google.com/finance/quote/XXI:NYSE",
}

// tickerLink wraps a ticker symbol in an HTML anchor tag pointing to Google Finance.
// Falls back to plain text if no URL is registered for the symbol.
func tickerLink(symbol string) string {
	if url, ok := gFinanceURLs[symbol]; ok {
		return fmt.Sprintf(`<a href="%s">%s</a>`, url, symbol)
	}
	return symbol
}

// ─── Data types ──────────────────────────────────────────────────────────────

type BTCData struct {
	Price     float64
	Change24h float64
	Volume24h float64
	MarketCap float64
	Move5d    float64 // % change over last 5 trading days (NaN if unavailable)
	Week52Low  float64
	Week52High float64
}

type FearGreed struct {
	Value     int
	Label     string
	Timestamp string
}

type EquityData struct {
	Symbol    string
	Label     string
	Price     float64
	Change    float64 // absolute $
	ChangePct float64 // %
	Volume    int64
	PrevClose float64
	Move5d    float64 // % change over last 5 trading days (NaN if unavailable)
	Week52Low  float64
	Week52High float64
}

// ─── Fetch: BTC via CoinGecko ─────────────────────────────────────────────────

func fetchBTC() (BTCData, error) {
	resp, err := httpGet(coinGeckoURL)
	if err != nil {
		return BTCData{}, err
	}
	defer resp.Body.Close()

	var raw map[string]map[string]float64
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return BTCData{}, err
	}
	btc := raw["bitcoin"]
	return BTCData{
		Price:     btc["usd"],
		Change24h: btc["usd_24h_change"],
		Volume24h: btc["usd_24h_vol"],
		MarketCap: btc["usd_market_cap"],
	}, nil
}

// ─── Fetch: BTC 5-day move via Yahoo (BTC-USD) ───────────────────────────────

func fetchBTC5d() float64 {
	url := fmt.Sprintf(yahooQuoteURL, "BTC-USD")
	resp, err := httpGet(url)
	if err != nil {
		return math.NaN()
	}
	defer resp.Body.Close()

	var raw struct {
		Chart struct {
			Result []struct {
				Meta struct {
					RegularMarketPrice float64 `json:"regularMarketPrice"`
				} `json:"meta"`
				Indicators struct {
					Quote []struct {
						Close []float64 `json:"close"`
					} `json:"quote"`
				} `json:"indicators"`
			} `json:"result"`
		} `json:"chart"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return math.NaN()
	}
	if len(raw.Chart.Result) == 0 {
		return math.NaN()
	}
	r := raw.Chart.Result[0]
	if len(r.Indicators.Quote) == 0 {
		return math.NaN()
	}
	closes := r.Indicators.Quote[0].Close
	for _, c := range closes {
		if c > 0 {
			return (r.Meta.RegularMarketPrice - c) / c * 100
		}
	}
	return math.NaN()
}

// ─── Fetch: Fear & Greed ──────────────────────────────────────────────────────

func fetchFearGreed() (FearGreed, error) {
	resp, err := httpGet(fearGreedURL)
	if err != nil {
		return FearGreed{}, err
	}
	defer resp.Body.Close()

	var raw struct {
		Data []struct {
			Value               string `json:"value"`
			ValueClassification string `json:"value_classification"`
			Timestamp           string `json:"timestamp"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return FearGreed{}, err
	}
	if len(raw.Data) == 0 {
		return FearGreed{}, fmt.Errorf("no fear/greed data")
	}
	d := raw.Data[0]
	var val int
	fmt.Sscanf(d.Value, "%d", &val)
	return FearGreed{Value: val, Label: d.ValueClassification, Timestamp: d.Timestamp}, nil
}

// ─── Fetch: Yahoo Finance equity quote ───────────────────────────────────────

func fetchEquity(symbol string) (EquityData, error) {
	url := fmt.Sprintf(yahooQuoteURL, symbol)
	resp, err := httpGet(url)
	if err != nil {
		return EquityData{}, err
	}
	defer resp.Body.Close()

	var raw struct {
		Chart struct {
			Result []struct {
				Meta struct {
					RegularMarketPrice         float64 `json:"regularMarketPrice"`
					PreviousClose              float64 `json:"previousClose"`
					RegularMarketVolume        int64   `json:"regularMarketVolume"`
					RegularMarketChangePercent float64 `json:"regularMarketChangePercent"`
					FiftyTwoWeekLow            float64 `json:"fiftyTwoWeekLow"`
					FiftyTwoWeekHigh           float64 `json:"fiftyTwoWeekHigh"`
				} `json:"meta"`
				Indicators struct {
					Quote []struct {
						Close []float64 `json:"close"`
					} `json:"quote"`
				} `json:"indicators"`
			} `json:"result"`
			Error interface{} `json:"error"`
		} `json:"chart"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return EquityData{}, err
	}
	if len(raw.Chart.Result) == 0 {
		return EquityData{}, fmt.Errorf("no data for %s", symbol)
	}
	r := raw.Chart.Result[0]
	m := r.Meta
	change := m.RegularMarketPrice - m.PreviousClose

	// 5-day move: first valid close in the window vs current price
	move5d := math.NaN()
	if len(r.Indicators.Quote) > 0 {
		closes := r.Indicators.Quote[0].Close
		for _, c := range closes {
			if c > 0 {
				move5d = (m.RegularMarketPrice - c) / c * 100
				break
			}
		}
	}

	return EquityData{
		Symbol:     symbol,
		Price:      m.RegularMarketPrice,
		Change:     change,
		ChangePct:  m.RegularMarketChangePercent,
		Volume:     m.RegularMarketVolume,
		PrevClose:  m.PreviousClose,
		Move5d:     move5d,
		Week52Low:  m.FiftyTwoWeekLow,
		Week52High: m.FiftyTwoWeekHigh,
	}, nil
}

// ─── Sentiment scorer ─────────────────────────────────────────────────────────
//
// Returns 0–100.
// fg      : Fear & Greed index (0–100)             → 50% weight
// btcChg  : BTC 24 h % change (clamped ±10 %)      → 30% weight
// eqChg   : equity day % change (clamped ±10 %)    → 20% weight

func sentiment(fg int, btcChg, eqChg float64) int {
	fgScore := float64(fg) // already 0–100

	// Map percent change → 0–100  (0% change → 50)
	pctToScore := func(pct float64) float64 {
		clamped := math.Max(-10, math.Min(10, pct))
		return (clamped + 10) / 20 * 100
	}

	score := fgScore*0.50 + pctToScore(btcChg)*0.30 + pctToScore(eqChg)*0.20
	return int(math.Round(score))
}

func sentimentEmoji(s int) string {
	switch {
	case s >= 75:
		return "🔥 Greed"
	case s >= 60:
		return "📈 Bullish"
	case s >= 45:
		return "😐 Neutral"
	case s >= 30:
		return "📉 Bearish"
	default:
		return "😱 Fear"
	}
}

// ─── Formatting helpers ───────────────────────────────────────────────────────

func arrow(v float64) string {
	if v > 0 {
		return "▲"
	}
	if v < 0 {
		return "▼"
	}
	return "■"
}

func sign(v float64) string {
	if v > 0 {
		return "+"
	}
	return ""
}

func fmtVol(v float64) string {
	switch {
	case v >= 1e12:
		return fmt.Sprintf("$%.2fT", v/1e12)
	case v >= 1e9:
		return fmt.Sprintf("$%.2fB", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("$%.2fM", v/1e6)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}

func fmtShareVol(v int64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(v)/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%.1fK", float64(v)/1_000)
	default:
		return fmt.Sprintf("%d", v)
	}
}

// ─── 52-week range bar ───────────────────────────────────────────────────────
//
// Renders a 10-char bar like:  LOW ┃████●─────┃ HIGH  (@ 62%)
// Uses █ for the filled portion left of current price, ─ for empty right side,
// ● as the position marker. Falls back to "N/A" if data missing.

func rangeBar(price, lo, hi float64) string {
	if lo <= 0 || hi <= lo || price <= 0 {
		return "N/A"
	}
	const width = 10
	pct := (price - lo) / (hi - lo)
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	filled := int(math.Round(pct * float64(width)))
	bar := make([]rune, width)
	for i := range bar {
		if i < filled {
			bar[i] = '█'
		} else {
			bar[i] = '░'
		}
	}
	return fmt.Sprintf("$%.2f %s $%.2f  (%d%%)",
		lo, string(bar), hi, int(math.Round(pct*100)))
}

// ─── Build Campfire message ───────────────────────────────────────────────────
//
// Campfire uses the Trix rich-text editor which ignores bare \n — line breaks
// must be <br> tags. Every line ends with <br>\n (the \n is just for readability
// in --dry-run / log output).

func buildMessage(btc BTCData, fg FearGreed, stocks []EquityData) string {
	now := time.Now().In(mustLoadLocation("America/New_York"))
	var b strings.Builder

	nl := "<br>\n"
	blank := "<br><br>\n"
	line := func(s string) { b.WriteString(s + nl) }
	gap := func() { b.WriteString(blank) }

	// ── Header ──
	line("🟠 **CRYPTO BOT** — Bitcoin & BTC-Equity Digest")
	line(fmt.Sprintf("📅 %s ET", now.Format("Mon Jan 2, 2006  3:04 PM")))
	line("─────────────────────────────────────")
	gap()

	// ── BTC ──
	line(fmt.Sprintf("**₿ %s (BTC/USD)**", tickerLink("BTC")))
	line(fmt.Sprintf("  Price      : $%s  %s %s%.2f%%",
		commaFmt(btc.Price), arrow(btc.Change24h), sign(btc.Change24h), btc.Change24h))
	if !math.IsNaN(btc.Move5d) {
		line(fmt.Sprintf("  5d Move    : %s%s%.2f%%", arrow(btc.Move5d), sign(btc.Move5d), btc.Move5d))
	}
	line(fmt.Sprintf("  24h Volume : %s", fmtVol(btc.Volume24h)))
	line(fmt.Sprintf("  Market Cap : %s", fmtVol(btc.MarketCap)))

	// ── Fear & Greed ──
	btcSentiment := sentiment(fg.Value, btc.Change24h, btc.Change24h)
	line(fmt.Sprintf("  Fear/Greed : %d/100 (%s)  →  Sentiment %d/100 %s",
		fg.Value, fg.Label, btcSentiment, sentimentEmoji(btcSentiment)))
	gap()

	// ── Equities ──
	line("**📊 BTC-LINKED EQUITIES**")

	for _, eq := range stocks {
		s := sentiment(fg.Value, btc.Change24h, eq.ChangePct)
		b.WriteString(blank)
		line(fmt.Sprintf("  %s  %s", tickerLink(eq.Symbol), eq.Label))
		line(fmt.Sprintf("  Price   : $%-10s  %s %s$%.2f  (%s%.2f%%)",
			fmt.Sprintf("%.2f", eq.Price),
			arrow(eq.Change),
			sign(eq.Change), math.Abs(eq.Change),
			sign(eq.ChangePct), eq.ChangePct))
		if !math.IsNaN(eq.Move5d) {
			line(fmt.Sprintf("  5d Move : %s%s%.2f%%", arrow(eq.Move5d), sign(eq.Move5d), eq.Move5d))
		}
		line(fmt.Sprintf("  52wk    : %s", rangeBar(eq.Price, eq.Week52Low, eq.Week52High)))
		line(fmt.Sprintf("  Volume  : %s shares", fmtShareVol(eq.Volume)))
		line(fmt.Sprintf("  Sentiment: %d/100 %s", s, sentimentEmoji(s)))
	}

	// ── Footer ──
	gap()
	line("─────────────────────────────────────")
	line("Sources: CoinGecko · alternative.me · Yahoo Finance")
	line("STRC/STRK/STRF/STRD = Strategy preferred shares  |  XXI = Twenty One Capital")

	return b.String()
}

// ─── Post to Campfire ─────────────────────────────────────────────────────────
//
// Campfire robot webhooks expect the raw message text as the POST body,
// exactly equivalent to: curl -d 'Hello!' <url>
// Content-Type: text/plain, no JSON wrapper, no form encoding.

func postToCampfire(msg string) error {
	req, err := http.NewRequest("POST", campfireURL, strings.NewReader(msg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("campfire returned %d: %s", resp.StatusCode, body)
	}
	return nil
}

// ─── HTTP helper ─────────────────────────────────────────────────────────────

func httpGet(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	// Yahoo Finance needs a realistic UA or it 429s
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; crypto_bot/1.0)")
	client := &http.Client{Timeout: 20 * time.Second}
	return client.Do(req)
}

// ─── Misc helpers ─────────────────────────────────────────────────────────────

func mustLoadLocation(tz string) *time.Location {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}

// commaFmt formats large numbers with comma separators
func commaFmt(f float64) string {
	s := fmt.Sprintf("%.2f", f)
	parts := strings.Split(s, ".")
	intPart := parts[0]
	var result []byte
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	if len(parts) > 1 {
		return string(result) + "." + parts[1]
	}
	return string(result)
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	log.SetFlags(0)
	log.SetPrefix("[crypto_bot] ")

	dryRun := len(os.Args) > 1 && os.Args[1] == "--dry-run"

	// ── Load .env / .env.local (binary dir, then working dir) ──
	execPath, err := os.Executable()
	if err == nil {
		loadEnvFiles(filepath.Dir(execPath))
	}
	if cwd, err := os.Getwd(); err == nil {
		loadEnvFiles(cwd)
	}

	// ── Build Campfire webhook URL from parts ──
	domain := os.Getenv("CAMPFIRE_DOMAIN")
	token := os.Getenv("CAMPFIRE_BOT_TOKEN")
	roomID := os.Getenv("CAMPFIRE_INVESTING_ROOM_ID")
	if domain == "" || token == "" || roomID == "" {
		log.Fatal("CAMPFIRE_DOMAIN, CAMPFIRE_BOT_TOKEN, and CAMPFIRE_INVESTING_ROOM_ID must all be set")
	}
	campfireURL = fmt.Sprintf("https://%s/rooms/%s/%s/messages", domain, roomID, token)

	// ── Fetch BTC ──
	log.Println("Fetching BTC price…")
	btc, err := fetchBTC()
	if err != nil {
		log.Fatalf("BTC fetch failed: %v", err)
	}
	log.Println("Fetching BTC 5d move…")
	btc.Move5d = fetchBTC5d()

	// ── Fetch Fear & Greed ──
	log.Println("Fetching Fear & Greed index…")
	fg, err := fetchFearGreed()
	if err != nil {
		log.Printf("Fear/Greed fetch failed (using neutral 50): %v", err)
		fg = FearGreed{Value: 50, Label: "Neutral"}
	}

	// ── Fetch equities ──
	var stocks []EquityData
	for _, e := range equities {
		log.Printf("Fetching %s…", e.Symbol)
		eq, err := fetchEquity(e.Symbol)
		if err != nil {
			log.Printf("  ⚠ skipped %s: %v", e.Symbol, err)
			continue
		}
		eq.Label = e.Label
		stocks = append(stocks, eq)
		time.Sleep(300 * time.Millisecond) // be polite to Yahoo
	}

	// ── Build message ──
	msg := buildMessage(btc, fg, stocks)

	if dryRun {
		fmt.Println("─── DRY RUN — message that would be posted ───")
		fmt.Println(msg)
		return
	}

	// ── Post ──
	log.Println("Posting to Campfire…")
	if err := postToCampfire(msg); err != nil {
		log.Printf("Post failed: %v\nFalling back to stdout:\n%s", err, msg)
		os.Exit(1)
	}
	log.Println("✅ Posted successfully.")
}