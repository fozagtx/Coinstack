// Package config reads CoinStack's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// StaticKey is an agent API key configured through COINSTACK_API_KEYS.
type StaticKey struct {
	Key       string
	Owner     string
	RateLimit int
}

// Config is the full runtime configuration.
type Config struct {
	Port        string
	LogLevel    slog.Level
	CMCAPIKey   string
	CMCBaseURL  string
	DatabaseURL string

	AuthEnabled      bool
	StaticKeys       []StaticKey
	DefaultRateLimit int

	Preset       string
	TopN         int
	FastN        int
	PageSize     int
	PollInterval time.Duration
	SlowInterval time.Duration
	CMCRPM       int

	OnDemandPerMin int
	OnDemandPerDay int

	StaleAfter    time.Duration
	MaxStale      time.Duration
	SnapshotEvery time.Duration

	TelegramToken      string
	TelegramChatIDs    []int64
	TelegramDigestHour int
	TelegramAlerts     []string
}

// DefaultCMCBaseURL is the production CoinMarketCap Pro API.
const DefaultCMCBaseURL = "https://pro-api.coinmarketcap.com"

// FromEnv builds a Config from the process environment.
func FromEnv() (Config, error) { return Load(os.Getenv) }

// preset is a named polling schedule, selected with COINSTACK_PRESET. The
// preset supplies the defaults; each individual env var still wins when
// set explicitly.
type preset struct {
	topN, fastN int
	poll, slow  time.Duration
}

var presets = map[string]preset{
	"free":     {topN: 1000, fastN: 200, poll: 10 * time.Minute, slow: 30 * time.Minute},
	"startup":  {topN: 3000, fastN: 200, poll: 2 * time.Minute, slow: 15 * time.Minute},
	"standard": {topN: 5000, fastN: 500, poll: time.Minute, slow: 10 * time.Minute},
}

// ProjectedCreditsPerDay estimates the configured schedule's daily CMC
// credit burn: fast-tier pages every PollInterval, slow-tier pages every
// SlowInterval (one credit per 200 assets per call), plus a flat overhead
// for map, new-listings and key-info calls.
func (c Config) ProjectedCreditsPerDay() int {
	ceilDiv := func(n, d int) int { return (n + d - 1) / d }
	fast := ceilDiv(c.FastN, 200) * int((24*time.Hour)/c.PollInterval)
	slow := ceilDiv(c.TopN-c.FastN, 200) * int((24*time.Hour)/c.SlowInterval)
	return fast + slow + 50
}

// Load builds a Config using getenv to read variables.
func Load(getenv func(string) string) (Config, error) {
	p := parser{getenv: getenv}
	presetName := p.str("COINSTACK_PRESET", "startup")
	pr, ok := presets[presetName]
	if !ok {
		p.fail("COINSTACK_PRESET", presetName, "want free, startup or standard")
		pr = presets["startup"]
	}
	c := Config{
		Port:             p.str("PORT", "8080"),
		CMCAPIKey:        strings.TrimSpace(getenv("CMC_API_KEY")),
		CMCBaseURL:       strings.TrimRight(p.str("CMC_BASE_URL", DefaultCMCBaseURL), "/"),
		DatabaseURL:      strings.TrimSpace(getenv("DATABASE_URL")),
		Preset:           presetName,
		DefaultRateLimit: p.int("COINSTACK_DEFAULT_RATE_LIMIT", 60, 1, 100000),
		TopN:             p.int("COINSTACK_TOP_N", pr.topN, 1, 10000),
		PageSize:         p.int("COINSTACK_PAGE_SIZE", 1000, 1, 5000),
		PollInterval:     p.dur("COINSTACK_POLL_INTERVAL", pr.poll, 10*time.Second),
		SlowInterval:     p.dur("COINSTACK_SLOW_INTERVAL", pr.slow, 10*time.Second),
		CMCRPM:           p.int("COINSTACK_CMC_RPM", 25, 1, 10000),
		OnDemandPerMin:   p.int("COINSTACK_ONDEMAND_PER_MIN", 5, 0, 10000),
		OnDemandPerDay:   p.int("COINSTACK_ONDEMAND_PER_DAY", 1000, 0, 1000000),
		StaleAfter:       p.dur("COINSTACK_STALE_AFTER", 180*time.Second, time.Second),
		MaxStale:         p.dur("COINSTACK_MAX_STALE", 30*time.Minute, time.Second),
		SnapshotEvery:    p.dur("COINSTACK_SNAPSHOT_EVERY", 5*time.Minute, time.Second),
	}
	c.FastN = p.int("COINSTACK_FAST_N", pr.fastN, 1, 10000)
	if c.FastN > c.TopN {
		c.FastN = c.TopN
	}
	c.LogLevel = p.level("LOG_LEVEL", slog.LevelInfo)

	switch v := strings.ToLower(p.str("COINSTACK_AUTH", "on")); v {
	case "on", "true", "1":
		c.AuthEnabled = true
	case "off", "false", "0":
		c.AuthEnabled = false
	default:
		p.fail("COINSTACK_AUTH", v, "use on or off")
	}
	c.StaticKeys = p.keys("COINSTACK_API_KEYS")

	c.TelegramToken = strings.TrimSpace(getenv("TELEGRAM_BOT_TOKEN"))
	c.TelegramChatIDs = p.int64s("TELEGRAM_CHAT_IDS")
	c.TelegramDigestHour = p.int("TELEGRAM_DIGEST_HOUR", 9, 0, 23)
	if v := strings.TrimSpace(getenv("TELEGRAM_ALERTS")); v != "" {
		for _, a := range strings.Split(v, ",") {
			a = strings.ToLower(strings.TrimSpace(a))
			switch a {
			case "":
				continue
			case "gems", "climbers", "listings", "digest":
				c.TelegramAlerts = append(c.TelegramAlerts, a)
			default:
				p.fail("TELEGRAM_ALERTS", a, "want a comma list of gems, climbers, listings, digest")
			}
		}
	}
	if c.TelegramAlerts == nil {
		c.TelegramAlerts = []string{"gems", "climbers", "listings", "digest"}
	}

	if c.MaxStale < c.StaleAfter {
		p.errs = append(p.errs, errors.New("COINSTACK_MAX_STALE must be >= COINSTACK_STALE_AFTER"))
	}
	if c.CMCAPIKey == "" && c.CMCBaseURL == DefaultCMCBaseURL {
		p.errs = append(p.errs, errors.New("CMC_API_KEY is required (or point CMC_BASE_URL at cmd/fakecmc)"))
	}
	if c.AuthEnabled && c.DatabaseURL == "" && len(c.StaticKeys) == 0 {
		p.errs = append(p.errs, errors.New("auth is on but no keys can exist: set DATABASE_URL or COINSTACK_API_KEYS, or COINSTACK_AUTH=off for local development"))
	}
	return c, errors.Join(p.errs...)
}

type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) fail(name, val, why string) {
	p.errs = append(p.errs, fmt.Errorf("%s=%q: %s", name, val, why))
}

func (p *parser) str(name, def string) string {
	if v := strings.TrimSpace(p.getenv(name)); v != "" {
		return v
	}
	return def
}

func (p *parser) int(name string, def, lo, hi int) int {
	v := strings.TrimSpace(p.getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		p.fail(name, v, fmt.Sprintf("want an integer in [%d, %d]", lo, hi))
		return def
	}
	return n
}

func (p *parser) dur(name string, def, min time.Duration) time.Duration {
	v := strings.TrimSpace(p.getenv(name))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		// Bare integers are seconds.
		n, nerr := strconv.Atoi(v)
		if nerr != nil {
			p.fail(name, v, "want a duration like 60s or 5m")
			return def
		}
		d = time.Duration(n) * time.Second
	}
	if d < min {
		p.fail(name, v, fmt.Sprintf("must be at least %s", min))
		return def
	}
	return d
}

func (p *parser) level(name string, def slog.Level) slog.Level {
	v := strings.TrimSpace(p.getenv(name))
	if v == "" {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.fail(name, v, "use debug, info, warn or error")
		return def
	}
	return l
}

// int64s parses a comma-separated list of integers.
func (p *parser) int64s(name string) []int64 {
	raw := strings.TrimSpace(p.getenv(name))
	if raw == "" {
		return nil
	}
	var out []int64
	for _, part := range strings.Split(raw, ",") {
		n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil {
			p.fail(name, part, "want a comma-separated list of integers")
			continue
		}
		out = append(out, n)
	}
	return out
}

// keys parses "key[:owner[:rpm]]" entries separated by commas.
func (p *parser) keys(name string) []StaticKey {
	raw := strings.TrimSpace(p.getenv(name))
	if raw == "" {
		return nil
	}
	var out []StaticKey
	for i, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, ":", 3)
		k := StaticKey{Key: strings.TrimSpace(parts[0]), Owner: "static"}
		if len(k.Key) < 8 {
			p.errs = append(p.errs, fmt.Errorf("%s entry %d: key must be at least 8 characters", name, i+1))
			continue
		}
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
			k.Owner = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			n, err := strconv.Atoi(strings.TrimSpace(parts[2]))
			if err != nil || n < 1 {
				p.errs = append(p.errs, fmt.Errorf("%s entry %d: rate limit must be a positive integer", name, i+1))
				continue
			}
			k.RateLimit = n
		}
		out = append(out, k)
	}
	return out
}
