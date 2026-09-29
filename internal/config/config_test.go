package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"CMC_API_KEY": "k", "COINSTACK_API_KEYS": "secret-key-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != "8080" || c.TopN != 500 || c.FastN != 500 || c.PageSize != 200 {
		t.Fatalf("bad defaults: %+v", c)
	}
	if c.PollInterval != time.Minute || c.StaleAfter != 3*time.Minute || c.MaxStale != 30*time.Minute {
		t.Fatalf("bad durations: %+v", c)
	}
	if !c.AuthEnabled || c.CMCBaseURL != DefaultCMCBaseURL {
		t.Fatalf("bad auth/base: %+v", c)
	}
	if strings.Join(c.Currencies, ",") != "EUR,GBP,JPY" {
		t.Fatalf("currencies = %v", c.Currencies)
	}
}

func TestParsesOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"CMC_BASE_URL":            "http://localhost:9090/",
		"COINSTACK_AUTH":          "off",
		"COINSTACK_TOP_N":         "300",
		"COINSTACK_FAST_N":        "900",
		"COINSTACK_POLL_INTERVAL": "30",
		"COINSTACK_CURRENCIES":    "eur, usd, eur ,chf",
		"COINSTACK_API_KEYS":      "abcdefgh:alice:120, ijklmnop",
		"LOG_LEVEL":               "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.CMCBaseURL != "http://localhost:9090" || c.AuthEnabled || c.TopN != 300 || c.FastN != 300 {
		t.Fatalf("got %+v", c)
	}
	if c.PollInterval != 30*time.Second {
		t.Fatalf("poll = %s", c.PollInterval)
	}
	if strings.Join(c.Currencies, ",") != "EUR,CHF" {
		t.Fatalf("currencies = %v", c.Currencies)
	}
	if len(c.StaticKeys) != 2 || c.StaticKeys[0].Owner != "alice" || c.StaticKeys[0].RateLimit != 120 || c.StaticKeys[1].Owner != "static" {
		t.Fatalf("keys = %+v", c.StaticKeys)
	}
	if c.LogLevel.String() != "DEBUG" {
		t.Fatalf("level = %s", c.LogLevel)
	}
}

func TestErrors(t *testing.T) {
	_, err := Load(env(map[string]string{
		"COINSTACK_TOP_N":       "zero",
		"COINSTACK_STALE_AFTER": "10m",
		"COINSTACK_MAX_STALE":   "1m",
		"COINSTACK_API_KEYS":    "short",
	}))
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"COINSTACK_TOP_N", "COINSTACK_MAX_STALE", "CMC_API_KEY", "at least 8"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestAuthNeedsKeys(t *testing.T) {
	_, err := Load(env(map[string]string{"CMC_API_KEY": "k"}))
	if err == nil || !strings.Contains(err.Error(), "no keys can exist") {
		t.Fatalf("err = %v", err)
	}
}
