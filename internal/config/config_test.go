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
	if c.Preset != "startup" || c.TopN != 3000 || c.FastN != 200 || c.PageSize != 1000 {
		t.Fatalf("bad defaults: %+v", c)
	}
	if c.PollInterval != 2*time.Minute || c.SlowInterval != 15*time.Minute || c.StaleAfter != 3*time.Minute {
		t.Fatalf("bad durations: %+v", c)
	}
	if !c.AuthEnabled || c.CMCBaseURL != DefaultCMCBaseURL {
		t.Fatalf("bad auth/base: %+v", c)
	}
}

func TestPresets(t *testing.T) {
	base := map[string]string{"CMC_API_KEY": "k", "COINSTACK_API_KEYS": "secret-key-1"}
	cases := []struct {
		preset      string
		topN, fastN int
		poll, slow  time.Duration
	}{
		{"free", 1000, 200, 10 * time.Minute, 30 * time.Minute},
		{"startup", 3000, 200, 2 * time.Minute, 15 * time.Minute},
		{"standard", 5000, 500, time.Minute, 10 * time.Minute},
	}
	for _, tc := range cases {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		m["COINSTACK_PRESET"] = tc.preset
		c, err := Load(env(m))
		if err != nil {
			t.Fatalf("%s: %v", tc.preset, err)
		}
		if c.TopN != tc.topN || c.FastN != tc.fastN || c.PollInterval != tc.poll || c.SlowInterval != tc.slow {
			t.Fatalf("%s: got %+v", tc.preset, c)
		}
	}
	// An explicit env var overrides its preset default.
	m := map[string]string{}
	for k, v := range base {
		m[k] = v
	}
	m["COINSTACK_PRESET"] = "free"
	m["COINSTACK_TOP_N"] = "2500"
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.TopN != 2500 || c.PollInterval != 10*time.Minute {
		t.Fatalf("override got %+v", c)
	}
	// Unknown presets are rejected.
	m["COINSTACK_PRESET"] = "whale"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "COINSTACK_PRESET") {
		t.Fatalf("want preset error, got %v", err)
	}
}

func TestProjectedCreditsPerDay(t *testing.T) {
	c := Config{TopN: 3000, FastN: 200, PollInterval: 2 * time.Minute, SlowInterval: 15 * time.Minute}
	want := 1*(86400/120) + 14*(86400/900) + 50 // 720 + 1344 + 50
	if got := c.ProjectedCreditsPerDay(); got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
}

func TestParsesOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"CMC_BASE_URL":            "http://localhost:9090/",
		"COINSTACK_AUTH":          "off",
		"COINSTACK_TOP_N":         "300",
		"COINSTACK_FAST_N":        "900",
		"COINSTACK_POLL_INTERVAL": "30",
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
