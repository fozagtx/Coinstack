package cmc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fixture for one asset in listings/quotes payloads.
func assetJSON(id int64, sym, name string, rank int, price, mcap, vol float64, extra string) string {
	return `{
		"id": ` + itoa(id) + `,
		"name": "` + name + `",
		"symbol": "` + sym + `",
		"slug": "` + name + `-slug",
		"cmc_rank": ` + itoa(int64(rank)) + `,
		"circulating_supply": 1000,
		"total_supply": 2000,
		"max_supply": null,
		"date_added": "2024-05-01T00:00:00.000Z",
		"tags": ["memes", "solana-ecosystem"],
		"quote": {"USD": {
			"price": ` + ftoa(price) + `,
			"volume_24h": ` + ftoa(vol) + `,
			"market_cap": ` + ftoa(mcap) + `,
			"percent_change_1h": 1.5,
			"percent_change_24h": -2.5,
			"percent_change_7d": 12.0,
			"last_updated": "2026-09-29T11:00:00.000Z"
		}}` + extra + `
	}`
}

func itoa(n int64) string { return jsonNumber(n) }
func ftoa(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}
func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func status(credits int) string {
	return `{"status": {"timestamp": "2026-09-29T12:00:00.000Z", "error_code": 0, "error_message": null, "elapsed": 12, "credit_count": ` + itoa(int64(credits)) + `}}`
}

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Options{BaseURL: srv.URL, APIKey: "test-key", RequestsPerMinute: 60000})
}

func TestListingsLatest(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/cryptocurrency/listings/latest" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("X-CMC_PRO_API_KEY") != "test-key" {
			t.Errorf("missing api key header")
		}
		q := r.URL.Query()
		if q.Get("start") != "1" || q.Get("limit") != "2" || q.Get("convert") != "USD" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"data": [` +
			assetJSON(1, "BTC", "Bitcoin", 1, 112000, 2.2e12, 30e9, "") + `,` +
			assetJSON(1027, "ETH", "Ethereum", 2, 4150, 500e9, 20e9, "") +
			`],` + status(1)[1:]))
	})
	qs, meta, err := c.ListingsLatest(context.Background(), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 2 {
		t.Fatalf("got %d quotes", len(qs))
	}
	btc := qs[0]
	if btc.ID != 1 || btc.Symbol != "BTC" || btc.Rank != 1 || btc.Price != 112000 {
		t.Fatalf("bad quote %+v", btc)
	}
	if btc.MaxSupply != nil {
		t.Fatalf("max supply should be nil, got %v", *btc.MaxSupply)
	}
	if btc.CirculatingSupply != 1000 || len(btc.Tags) != 2 || btc.Tags[0] != "memes" {
		t.Fatalf("bad supplies/tags %+v", btc)
	}
	if btc.Change24hPct != -2.5 || btc.Change7dPct != 12 {
		t.Fatalf("bad changes %+v", btc)
	}
	if meta.CreditCount != 1 || meta.HTTPStatus != 200 || meta.Elapsed != 12*time.Millisecond {
		t.Fatalf("bad meta %+v", meta)
	}
}

func TestQuotesLatestMapKeyed(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/cryptocurrency/quotes/latest" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("id"); got != "1,1027" {
			t.Errorf("ids = %q", got)
		}
		w.Write([]byte(`{"data": {"1": ` + assetJSON(1, "BTC", "Bitcoin", 1, 112000, 2.2e12, 30e9, "") +
			`, "1027": ` + assetJSON(1027, "ETH", "Ethereum", 2, 4150, 500e9, 20e9, "") +
			`},` + status(1)[1:]))
	})
	got, _, err := c.QuotesLatest(context.Background(), []int64{1, 1027})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Symbol != "BTC" || got[1027].Price != 4150 {
		t.Fatalf("got %+v", got)
	}
}

func TestMap(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/cryptocurrency/map" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"data": [
			{"id": 1, "name": "Bitcoin", "symbol": "BTC", "slug": "bitcoin", "rank": 1, "is_active": 1,
			 "first_historical_data": "2013-04-28T00:00:00.000Z", "platform": null},
			{"id": 24478, "name": "Pepe", "symbol": "PEPE", "slug": "pepe", "rank": 38, "is_active": 1,
			 "first_historical_data": "2023-04-17T00:00:00.000Z",
			 "platform": {"id": 1027, "name": "Ethereum", "symbol": "ETH", "slug": "ethereum"}}
		],` + status(1)[1:]))
	})
	entries, _, err := c.Map(context.Background(), 1, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].ID != 1 || entries[0].Platform != "" || !entries[0].IsActive {
		t.Fatalf("bad entry %+v", entries[0])
	}
	if entries[1].Platform != "Ethereum" || entries[1].FirstHistoricalData.Year() != 2023 {
		t.Fatalf("bad entry %+v", entries[1])
	}
}

func TestErrorStatusCode1008(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"status": {"timestamp": "2026-09-29T12:00:00.000Z", "error_code": 1008,
			"error_message": "Your API Key's rate limit exceeded.", "elapsed": 0, "credit_count": 0}}`))
	})
	_, _, err := c.ListingsLatest(context.Background(), 1, 10)
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v", err)
	}
	if ae.Code != 1008 || ae.HTTPStatus != 400 {
		t.Fatalf("apiErr = %+v", ae)
	}
}

func Test429RetryAfter(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(429)
		w.Write([]byte(`{"status": {"error_code": 1008, "error_message": "rate limit", "elapsed": 0, "credit_count": 0}}`))
	})
	_, _, err := c.QuotesLatest(context.Background(), []int64{1})
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v", err)
	}
	if ae.HTTPStatus != 429 || ae.RetryAfter != 12*time.Second {
		t.Fatalf("apiErr = %+v", ae)
	}
	if !ae.Temporary() {
		t.Fatal("429 should be temporary")
	}
}
