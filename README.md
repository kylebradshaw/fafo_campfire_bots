# 🤖 campfire-bots

A collection of Go bots that post daily digests to a [Campfire](https://once.com/campfire) chat room via robot webhooks.

---

## Bots

### CHANGELOG

- 2026.03.17: Initial commit with `crypto_bot`, `market_bot`, and `disk_bot` source code and README.

### 🟠 crypto_bot

Posts a daily Bitcoin & BTC-equity digest to a Campfire room, covering price, momentum, volume, 52-week position, and a composite sentiment score.

**Sample output**

```
🟠 CRYPTO BOT — Bitcoin & BTC-Equity Digest
📅 Mon Mar 17, 2026  8:05 AM ET
─────────────────────────────────────

₿ BITCOIN (BTC/USD)
  Price      : $84,312.00  ▲ +1.24%
  5d Move    : ▲ +3.81%
  24h Volume : $48.20B
  Market Cap : $1.67T
  Fear/Greed : 62/100 (Greed)  →  Sentiment 64/100 📈 Bullish

📊 BTC-LINKED EQUITIES

  MSTR    Strategy (MSTR)
  Price   : $312.50      ▲ +$8.20  (+2.70%)
  5d Move : ▲ +9.41%
  52wk    : $126.79 ████░░░░░░ $543.00  (35%)
  Volume  : 8.2M shares
  Sentiment: 61/100 📈 Bullish

  IBIT    iShares Bitcoin Trust (IBIT)
  Price   : $52.14       ▲ +$0.91  (+1.78%)
  5d Move : ▲ +4.02%
  52wk    : $33.08 █████░░░░░ $58.76  (51%)
  Volume  : 59.6M shares
  Sentiment: 63/100 📈 Bullish
  ...

─────────────────────────────────────
Sources: CoinGecko · alternative.me · Yahoo Finance
```

**Tickers tracked**

| Ticker | Name |
|--------|------|
| BTC    | Bitcoin (via CoinGecko) |
| MSTR   | Strategy Inc. (formerly MicroStrategy) |
| STRC   | Strategy Variable Rate Series A Perpetual Stretch Preferred |
| STRK   | Strategy 8.00% Series A Perpetual Strike Preferred |
| STRF   | Strategy 10.00% Series A Perpetual Strife Preferred |
| STRD   | Strategy 10.00% Series A Perpetual Stride Preferred |
| IBIT   | iShares Bitcoin Trust ETF (BlackRock) |
| XXI    | Twenty One Capital (Tether/Bitfinex/SoftBank) |

**Data sources** — all free, no API keys required

| Data | Source |
|------|--------|
| BTC price, 24h change, volume, market cap | [CoinGecko](https://www.coingecko.com/en/api) public API |
| BTC 5-day move | Yahoo Finance (BTC-USD) |
| Crypto Fear & Greed Index | [alternative.me](https://alternative.me/crypto/fear-and-greed-index/) |
| Equity quotes, 5d history, 52wk high/low | Yahoo Finance v8 (unofficial, key-free) |

**Sentiment score (0–100)**

Composite score calculated per asset using three weighted inputs:

```
50%  Crypto Fear & Greed Index        (0–100, sourced directly)
30%  BTC 24h price momentum           (±10% range mapped to 0–100)
20%  Per-equity day change %          (±10% range mapped to 0–100)
```

Score bands: `0–29` 😱 Fear · `30–44` 📉 Bearish · `45–59` 😐 Neutral · `60–74` 📈 Bullish · `75–100` 🔥 Greed

**52-week range bar**

Each equity shows a 10-character bar representing where the current price sits within its 52-week high/low range:

```
$126.79 ████░░░░░░ $543.00  (35%)
         ↑filled   ↑empty
```

---

### 📈 market_bot

Posts a daily US market summary to a Campfire room — major index prices/moves (S&P 500, Dow, Nasdaq) plus top 10 gainers and losers from Yahoo Finance's screener. Uses the same investing room as `crypto_bot`.

---

### 🖥️ disk_bot

Posts a daily filesystem usage digest to a Campfire room. Auto-discovers all real mounted filesystems via `syscall.Statfs` (no shell commands). Flags mounts at 🟡 warn (default 75%) or 🔴 critical (default 90%). Reports both space and inode usage.

---

## Prerequisites

- [Go](https://go.dev/dl/) 1.21 or later
- A [Campfire](https://once.com/campfire) instance with a robot configured for your rooms

---

## Setup

### 1. Clone the repo

```bash
git clone https://github.com/your-username/campfire-bots.git
cd campfire-bots
```

### 2. Configure environment variables

Copy the sample env file into each bot's deployment directory and fill in your values:

```bash
cp env_sample .env.local
```

Edit `.env.local`:

```bash
# Your Campfire instance hostname (no https://, no trailing slash)
CAMPFIRE_DOMAIN=your-campfire-domain.com

# Bot token — Campfire → Account Settings → Bots → (your bot)
CAMPFIRE_BOT_TOKEN=your-bot-token

# Room IDs — the integer in your Campfire room URL
CAMPFIRE_INVESTING_ROOM_ID=#   # used by crypto_bot, market_bot
CAMPFIRE_SERVER_ROOM_ID=#      # used by disk_bot
```

`.env.local` is gitignored and will never be committed. See [Environment variables](#environment-variables) for load order details.

### 3. Compile

```bash
cd bots
go build -o crypto_bot crypto_main.go
go build -o disk_bot disk_main.go
go build -o market_bot market_main.go
```

For a fully self-contained binary with no runtime dependencies (useful for deploying to a Raspberry Pi or remote server):

```bash
# Linux/amd64
GOOS=linux GOARCH=amd64 go build -o crypto_bot crypto_main.go

# Linux/arm64 (Raspberry Pi 4+)
GOOS=linux GOARCH=arm64 go build -o crypto_bot crypto_main.go
```

### 4. Test without posting

Use `--dry-run` to print the message to stdout without hitting Campfire:

```bash
./crypto_bot --dry-run
./disk_bot --dry-run
```

### 5. Run

```bash
./crypto_bot
./disk_bot
./market_bot
```

---

## Environment variables

Each binary loads env files automatically at startup — no `source` or shell wrapper required.

**Load order** (later entries win):

| Source | Wins over |
|--------|-----------|
| Process environment (cron, systemd, shell export) | Everything |
| `.env.local` | `.env` |
| `.env` | Nothing (lowest priority) |

The binary looks for these files in two locations, in order:
1. The directory containing the binary
2. The working directory at the time of invocation

Already-set process environment variables are **never overwritten** — so values injected by cron or systemd always take priority.

The webhook URL is constructed at runtime: `https://CAMPFIRE_DOMAIN/rooms/ROOM_ID/CAMPFIRE_BOT_TOKEN/messages`

**Variables**

| Variable | Used by | Description |
|----------|---------|-------------|
| `CAMPFIRE_DOMAIN` | all | Hostname only, no `https://` or trailing slash |
| `CAMPFIRE_BOT_TOKEN` | all | Bot token from Campfire → Account Settings → Bots |
| `CAMPFIRE_INVESTING_ROOM_ID` | crypto_bot, market_bot | Integer room ID for the investing room |
| `CAMPFIRE_SERVER_ROOM_ID` | disk_bot | Integer room ID for the server room |
| `DISK_WARN_PCT` | disk_bot | Warn threshold, default `75` |
| `DISK_CRIT_PCT` | disk_bot | Critical threshold, default `90` |
| `DISK_MOUNTS` | disk_bot | Comma-separated mount points to check (default: all real mounts) |

---

## Scheduling with cron

Add to your crontab (`crontab -e`) to post every weekday at 8:05 AM ET:

```cron
CRON_TZ=America/New_York
5 8 * * 1-5 /home/$USER/campfire-bots/crypto_bot >> /home/$USER/campfire-bots/crypto_bot.log 2>&1
5 8 * * 1-5 /home/$USER/campfire-bots/market_bot >> /home/$USER/campfire-bots/market_bot.log 2>&1
5 8 * * 1-5 /home/$USER/campfire-bots/disk_bot   >> /home/$USER/campfire-bots/disk_bot.log 2>&1
```

> **Note:** cron does not source shell profiles or `.env` files. The bot's built-in env-file loader handles this automatically — as long as `.env.local` is next to the binary, it will be found and loaded.

To verify the crontab is set correctly:

```bash
crontab -l
```

---

## Repository layout

```
campfire-bots/
├── bots/
│   ├── crypto_main.go    # crypto_bot source
│   ├── disk_main.go      # disk_bot source
│   └── market_main.go    # market_bot source
├── env_sample            # safe template — copy to .env.local, never commit
├── .gitignore
└── README.md
```

Compiled binaries and `.env.local` files are gitignored.

---

## Licence

MIT
