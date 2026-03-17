package main

// disk_bot.go — Filesystem size & usage monitor → Campfire
//
// Reads all mounted filesystems via syscall.Statfs (no external commands,
// no dependencies beyond the Go stdlib).
//
// Posts a daily digest to a Campfire room. Filesystems at or above the
// WARN threshold are flagged 🟡; at or above CRITICAL they're flagged 🔴.
// If everything is healthy the header is 🟢.
//
// Environment variables (loaded from .env / .env.local next to the binary):
//
//	CAMPFIRE_WEBHOOK_URL   required — Campfire robot webhook URL
//	DISK_WARN_PCT          optional — warn threshold  (default: 75)
//	DISK_CRIT_PCT          optional — critical threshold (default: 90)
//	DISK_MOUNTS            optional — comma-separated list of mount points
//	                                   to include (default: all real mounts)

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ─── Env-file loader ─────────────────────────────────────────────────────────
// Identical pattern to crypto_bot: loads .env then .env.local from dir,
// never clobbering variables already present in the process environment.

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

// ─── Config ──────────────────────────────────────────────────────────────────

type config struct {
	campfireURL string
	warnPct     float64
	critPct     float64
	mounts      []string // empty = all real mounts
}

func loadConfig() config {
	domain := os.Getenv("CAMPFIRE_DOMAIN")
	token := os.Getenv("CAMPFIRE_BOT_TOKEN")
	roomID := os.Getenv("CAMPFIRE_SERVER_ROOM_ID")
	if domain == "" || token == "" || roomID == "" {
		log.Fatal("CAMPFIRE_DOMAIN, CAMPFIRE_BOT_TOKEN, and CAMPFIRE_SERVER_ROOM_ID must all be set")
	}
	cfg := config{
		campfireURL: fmt.Sprintf("https://%s/rooms/%s/%s/messages", domain, roomID, token),
		warnPct:     75,
		critPct:     90,
	}
	if v := os.Getenv("DISK_WARN_PCT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.warnPct = f
		}
	}
	if v := os.Getenv("DISK_CRIT_PCT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.critPct = f
		}
	}
	if v := os.Getenv("DISK_MOUNTS"); v != "" {
		for _, m := range strings.Split(v, ",") {
			if m = strings.TrimSpace(m); m != "" {
				cfg.mounts = append(cfg.mounts, m)
			}
		}
	}
	return cfg
}

// ─── Filesystem data ─────────────────────────────────────────────────────────

type fsInfo struct {
	Mount     string
	Device    string
	TotalGB   float64
	UsedGB    float64
	FreeGB    float64
	UsedPct   float64
	Inode     inodeInfo
}

type inodeInfo struct {
	Total uint64
	Used  uint64
	Pct   float64
}

// realFSTypes are filesystem types we care about — skip proc/sys/dev/tmpfs noise.
var realFSTypes = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true,
	"xfs": true, "btrfs": true, "zfs": true,
	"ntfs": true, "vfat": true, "exfat": true,
	"apfs": true, "hfs": true,
	"nfs": true, "nfs4": true, "cifs": true, "smb2": true,
	"fuse": true, "fuseblk": true,
	"overlay": true, // Docker layers — useful to see
}

// parseMounts reads /proc/mounts (Linux) or /etc/mtab, returning
// a map of mountpoint → device + fstype.
func parseMounts() map[string][2]string {
	result := map[string][2]string{}
	for _, path := range []string{"/proc/mounts", "/etc/mtab"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 3 {
				continue
			}
			device, mount, fstype := fields[0], fields[1], fields[2]
			result[mount] = [2]string{device, fstype}
		}
		f.Close()
		break
	}
	return result
}

func gatherFS(cfg config) []fsInfo {
	mountMap := parseMounts()

	// Determine which mounts to stat
	var targets []string
	if len(cfg.mounts) > 0 {
		targets = cfg.mounts
	} else {
		// Auto-discover: only real filesystem types
		for mount, info := range mountMap {
			fstype := strings.ToLower(info[1])
			// Accept known real types, or anything that isn't a known virtual type
			if realFSTypes[fstype] {
				targets = append(targets, mount)
			}
		}
		// Fallback: if nothing matched (e.g. macOS without /proc/mounts), stat /
		if len(targets) == 0 {
			targets = []string{"/"}
		}
	}

	// Sort for deterministic output
	sortStrings(targets)

	seen := map[string]bool{} // deduplicate by mount point
	var infos []fsInfo

	for _, mount := range targets {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(mount, &stat); err != nil {
			log.Printf("⚠ statfs %s: %v", mount, err)
			continue
		}

		// Skip duplicates — same mount point listed more than once.
		if seen[mount] {
			continue
		}
		seen[mount] = true

		blockSize := uint64(stat.Bsize)
		total := stat.Blocks * blockSize
		free := stat.Bavail * blockSize
		used := total - stat.Bfree*blockSize

		if total == 0 {
			continue
		}

		usedPct := float64(used) / float64(total) * 100

		var inode inodeInfo
		if stat.Files > 0 {
			inode.Total = stat.Files
			inode.Used = stat.Files - stat.Ffree
			inode.Pct = float64(inode.Used) / float64(inode.Total) * 100
		}

		device := ""
		if info, ok := mountMap[mount]; ok {
			device = info[0]
		}

		infos = append(infos, fsInfo{
			Mount:   mount,
			Device:  device,
			TotalGB: bytesToGB(total),
			UsedGB:  bytesToGB(used),
			FreeGB:  bytesToGB(free),
			UsedPct: usedPct,
			Inode:   inode,
		})
	}
	return infos
}

// ─── Formatting helpers ───────────────────────────────────────────────────────

func bytesToGB(b uint64) float64 {
	return float64(b) / (1024 * 1024 * 1024)
}

func fmtGB(gb float64) string {
	if gb >= 1000 {
		return fmt.Sprintf("%.1fT", gb/1024)
	}
	if gb >= 1 {
		return fmt.Sprintf("%.1fG", gb)
	}
	return fmt.Sprintf("%.0fM", gb*1024)
}

// usageBar renders a 10-char █/░ bar, same style as crypto_bot's rangeBar.
func usageBar(pct float64) string {
	const width = 10
	filled := int(math.Round(pct / 100 * float64(width)))
	if filled > width {
		filled = width
	}
	bar := make([]rune, width)
	for i := range bar {
		if i < filled {
			bar[i] = '█'
		} else {
			bar[i] = '░'
		}
	}
	return string(bar)
}

func statusEmoji(pct, warn, crit float64) string {
	switch {
	case pct >= crit:
		return "🔴"
	case pct >= warn:
		return "🟡"
	default:
		return "🟢"
	}
}

// sortStrings is a simple insertion sort to avoid importing "sort" just for this.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ─── Build Campfire message ───────────────────────────────────────────────────

func buildMessage(infos []fsInfo, cfg config) string {
	now := time.Now().In(mustLoadLocation("America/New_York"))

	nl := "<br>\n"
	blank := "<br><br>\n"
	var b strings.Builder
	line := func(s string) { b.WriteString(s + nl) }
	gap := func() { b.WriteString(blank) }

	// Determine overall health for header emoji
	overall := "🟢"
	for _, fs := range infos {
		e := statusEmoji(fs.UsedPct, cfg.warnPct, cfg.critPct)
		if e == "🔴" {
			overall = "🔴"
			break
		}
		if e == "🟡" {
			overall = "🟡"
		}
		// Also check inode usage
		if fs.Inode.Total > 0 {
			ie := statusEmoji(fs.Inode.Pct, cfg.warnPct, cfg.critPct)
			if ie == "🔴" && overall != "🔴" {
				overall = "🔴"
			}
			if ie == "🟡" && overall == "🟢" {
				overall = "🟡"
			}
		}
	}

	// Header
	line(fmt.Sprintf("%s **DISK BOT** — Filesystem Usage Digest", overall))
	line(fmt.Sprintf("📅 %s ET", now.Format("Mon Jan 2, 2006  3:04 PM")))
	line(fmt.Sprintf("🖥️  %s", mustHostname()))
	line("─────────────────────────────────────")
	gap()

	// Thresholds legend
	line(fmt.Sprintf("Thresholds:  🟡 Warn ≥ %.0f%%  ·  🔴 Critical ≥ %.0f%%",
		cfg.warnPct, cfg.critPct))
	gap()

	if len(infos) == 0 {
		line("⚠️  No filesystems found.")
		return b.String()
	}

	// Per-filesystem blocks
	line("**💾 FILESYSTEMS**")
	for _, fs := range infos {
		e := statusEmoji(fs.UsedPct, cfg.warnPct, cfg.critPct)
		gap()
		// Mount + device
		if fs.Device != "" && fs.Device != fs.Mount {
			line(fmt.Sprintf("  %s  **%s**  (%s)", e, fs.Mount, fs.Device))
		} else {
			line(fmt.Sprintf("  %s  **%s**", e, fs.Mount))
		}
		// Space
		line(fmt.Sprintf("  Space  : %s %s %s  (%s used / %s free / %s total)",
			usageBar(fs.UsedPct),
			fmt.Sprintf("%.1f%%", fs.UsedPct),
			spaceWarnNote(fs.UsedPct, cfg.warnPct, cfg.critPct),
			fmtGB(fs.UsedGB), fmtGB(fs.FreeGB), fmtGB(fs.TotalGB)))
		// Inodes (only if available and non-trivial)
		if fs.Inode.Total > 0 && fs.Inode.Total < 1e15 {
			ie := statusEmoji(fs.Inode.Pct, cfg.warnPct, cfg.critPct)
			line(fmt.Sprintf("  Inodes : %s %s  (%s used / %s free)",
				usageBar(fs.Inode.Pct),
				fmt.Sprintf("%.1f%%  %s", fs.Inode.Pct, ie),
				fmtCount(fs.Inode.Used), fmtCount(fs.Inode.Total-fs.Inode.Used)))
		}
	}

	// Footer
	gap()
	line("─────────────────────────────────────")
	line(fmt.Sprintf("Set DISK_WARN_PCT / DISK_CRIT_PCT env vars to change thresholds (currently %.0f%% / %.0f%%)",
		cfg.warnPct, cfg.critPct))
	line("Add DISK_MOUNTS=/ ,/data,/mnt/backup to limit which mounts are checked")

	return b.String()
}

func spaceWarnNote(pct, warn, crit float64) string {
	switch {
	case pct >= crit:
		return "⚠️ CRITICAL"
	case pct >= warn:
		return "⚠️ WARNING"
	default:
		return ""
	}
}

func fmtCount(n uint64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func mustHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func mustLoadLocation(tz string) *time.Location {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}

// ─── Post to Campfire ─────────────────────────────────────────────────────────

func postToCampfire(campfireURL, msg string) error {
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

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	log.SetFlags(0)
	log.SetPrefix("[disk_bot] ")

	dryRun := len(os.Args) > 1 && os.Args[1] == "--dry-run"

	// Load .env / .env.local (binary dir first, then working dir)
	if execPath, err := os.Executable(); err == nil {
		loadEnvFiles(filepath.Dir(execPath))
	}
	if cwd, err := os.Getwd(); err == nil {
		loadEnvFiles(cwd)
	}

	cfg := loadConfig()

	log.Println("Gathering filesystem info…")
	infos := gatherFS(cfg)
	log.Printf("Found %d filesystem(s)", len(infos))

	msg := buildMessage(infos, cfg)

	if dryRun {
		fmt.Println("─── DRY RUN — message that would be posted ───")
		fmt.Println(msg)
		return
	}

	log.Println("Posting to Campfire…")
	if err := postToCampfire(cfg.campfireURL, msg); err != nil {
		log.Printf("Post failed: %v\nFalling back to stdout:\n%s", err, msg)
		os.Exit(1)
	}
	log.Println("✅ Posted successfully.")
}