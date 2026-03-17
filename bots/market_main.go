package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ─── Env-file loader ─────────────────────────────────────────────────────────

func loadEnvFiles(dir string) {
	for _, name := range []string{".env", ".env.local"} {
		path := filepath.Join(dir, name)
		f, err := os.Open(path)
		if err != nil {
			continue
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
			if len(val) >= 2 {
				if (val[0] == '"' && val[len(val)-1] == '"') ||
					(val[0] == '\'' && val[len(val)-1] == '\'') {
					val = val[1 : len(val)-1]
				}
			}
			if key == "" {
				continue
			}
			if os.Getenv(key) == "" {
				os.Setenv(key, val)
			}
		}
		f.Close()
	}
}

// Structs to parse Yahoo's JSON responses
type ChartResponse struct {
	Chart struct {
		Result[]struct {
			Meta struct {
				RegularMarketPrice float64 `json:"regularMarketPrice"`
				PreviousClose      float64 `json:"previousClose"`
			} `json:"meta"`
		} `json:"result"`
	} `json:"chart"`
}

type ScreenerResponse struct {
	Finance struct {
		Result []struct {
			Quotes[]struct {
				Symbol                     string  `json:"symbol"`
				Exchange                   string  `json:"exchange"`
				RegularMarketPrice         float64 `json:"regularMarketPrice"`
				RegularMarketChangePercent float64 `json:"regularMarketChangePercent"`
			} `json:"quotes"`
		} `json:"result"`
	} `json:"finance"`
}

// ─── Google Finance links ─────────────────────────────────────────────────────
//
// Major indexes use canonical Google Finance URLs keyed by their Yahoo ticker.
// Individual stocks (screener results) use the generic /quote/SYMBOL form,
// which Google resolves for all standard US exchange tickers.

var gFinanceURLs = map[string]string{
	"^GSPC": "https://www.google.com/finance/quote/.INX:INDEXSP",
	"^DJI":  "https://www.google.com/finance/quote/.DJI:INDEXDJX",
	"^IXIC": "https://www.google.com/finance/quote/.IXIC:INDEXNASDAQ",
}

// namedLink wraps displayName in an anchor tag. Used for indexes where the
// display name ("S&P 500") differs from the Yahoo ticker ("^GSPC").
func namedLink(displayName, yahooTicker string) string {
	if url, ok := gFinanceURLs[yahooTicker]; ok {
		return fmt.Sprintf(`<a href="%s">%s</a>`, url, displayName)
	}
	return displayName
}

// yahooToGoogleExchange maps Yahoo Finance exchange codes to Google Finance exchange suffixes.
var yahooToGoogleExchange = map[string]string{
	"NMS": "NASDAQ",       // NASDAQ National Market System
	"NGM": "NASDAQ",       // NASDAQ Global Market
	"NCM": "NASDAQ",       // NASDAQ Capital Market
	"NYQ": "NYSE",         // New York Stock Exchange
	"ASE": "NYSEAMERICAN", // NYSE American (formerly AMEX)
	"PCX": "NYSEARCA",     // NYSE Arca (ETFs)
}

// tickerLink wraps a stock ticker in an anchor tag pointing to Google Finance.
// yahooExchange is the exchange code returned by the Yahoo Finance screener API.
func tickerLink(symbol, yahooExchange string) string {
	exchange, ok := yahooToGoogleExchange[yahooExchange]
	if !ok {
		exchange = yahooExchange // fall back to raw value
	}
	return fmt.Sprintf(`<a href="https://www.google.com/finance/quote/%s:%s">%s</a>`, symbol, exchange, symbol)
}

func doYahooRequest(url string) ([]byte, error) {
	req, _ := http.NewRequest("GET", url, nil)
	// Yahoo blocks requests without a standard User-Agent
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("[market_bot] ")

	// ── Load .env / .env.local (binary dir, then working dir) ──
	if execPath, err := os.Executable(); err == nil {
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
	webhookURL := fmt.Sprintf("https://%s/rooms/%s/%s/messages", domain, roomID, token)

	dateStr := time.Now().Format("Monday, Jan 02")

	// We use <br> tags because Campfire renders the webhook payload as HTML
	message := fmt.Sprintf("📈 **Market Summary for %s** 📉<br><br>\n", dateStr)

	// --- 1. Get Major Indexes ---
	message += "🏛️ **Major Indexes (Previous Close):**<br>\n"
	indexes := map[string]string{"S&P 500": "^GSPC", "Dow Jones": "^DJI", "Nasdaq": "^IXIC"}

	for name, ticker := range indexes {
		url := fmt.Sprintf("https://query1.finance.yahoo.com/v8/finance/chart/%s", ticker)
		body, err := doYahooRequest(url)
		if err == nil {
			var data ChartResponse
			json.Unmarshal(body, &data)
			if len(data.Chart.Result) > 0 {
				meta := data.Chart.Result[0].Meta
				changePct := ((meta.RegularMarketPrice - meta.PreviousClose) / meta.PreviousClose) * 100

				symbol := "🔴"
				if changePct >= 0 {
					symbol = "🟢"
				}
				message += fmt.Sprintf("%s %s: %.2f (%+.2f%%)<br>\n", symbol, namedLink(name, ticker), meta.RegularMarketPrice, changePct)
				continue
			}
		}
		message += fmt.Sprintf("⚠️ %s: Data unavailable<br>\n", name)
	}

	// --- 2. Get Top 10 Gainers ---
	message += "<br>\n🚀 **Top 10 Gainers:**<br>\n"
	gainersURL := "https://query1.finance.yahoo.com/v1/finance/screener/predefined/saved?formatted=false&lang=en-US&region=US&scrIds=day_gainers&count=10"
	body, err := doYahooRequest(gainersURL)
	if err == nil {
		var data ScreenerResponse
		json.Unmarshal(body, &data)
		if len(data.Finance.Result) > 0 {
			for _, q := range data.Finance.Result[0].Quotes {
				message += fmt.Sprintf("• %s : $%.2f (+%.2f%%)<br>\n", tickerLink(q.Symbol, q.Exchange), q.RegularMarketPrice, q.RegularMarketChangePercent)
			}
		}
	}

	// --- 3. Get Top 10 Losers ---
	message += "<br>\n🔻 **Top 10 Losers:**<br>\n"
	losersURL := "https://query1.finance.yahoo.com/v1/finance/screener/predefined/saved?formatted=false&lang=en-US&region=US&scrIds=day_losers&count=10"
	body, err = doYahooRequest(losersURL)
	if err == nil {
		var data ScreenerResponse
		json.Unmarshal(body, &data)
		if len(data.Finance.Result) > 0 {
			for _, q := range data.Finance.Result[0].Quotes {
				message += fmt.Sprintf("• %s : $%.2f (%.2f%%)<br>\n", tickerLink(q.Symbol, q.Exchange), q.RegularMarketPrice, q.RegularMarketChangePercent)
			}
		}
	}

	// --- 4. Send to Campfire ---
	fmt.Println("Sending to Campfire...")
	req, _ := http.NewRequest("POST", webhookURL, bytes.NewBufferString(message))
	// Setting text/html ensures the rich text editor parses the break tags properly
	req.Header.Set("Content-Type", "text/html; charset=utf-8")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)

	if err != nil {
		fmt.Printf("Failed to connect to webhook: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 || resp.StatusCode == 201 {
		fmt.Println("Successfully posted to Campfire!")
	} else {
		fmt.Printf("Campfire returned status: %d\n", resp.StatusCode)
	}
}