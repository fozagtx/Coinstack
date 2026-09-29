package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fozagtx/coinstack/internal/market"
	"github.com/fozagtx/coinstack/internal/model"
)

var testNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

type fakeMarket struct {
	snap    *market.Snapshot
	hist    map[int64][]model.Sample
	newList []model.Quote
	quotes  map[int64]model.Quote
	hours   int
}

func (f *fakeMarket) Snapshot() *market.Snapshot { return f.snap }
func (f *fakeMarket) Quotes(context.Context, []int64) (map[int64]model.Quote, error) {
	return f.quotes, nil
}
func (f *fakeMarket) Info(context.Context, []int64) (map[int64]model.Info, error) {
	return nil, nil
}
func (f *fakeMarket) NewListings(context.Context, int) ([]model.Quote, error) { return f.newList, nil }
func (f *fakeMarket) History(id int64) []model.Sample                         { return f.hist[id] }
func (f *fakeMarket) RankAt(id int64, ago time.Duration) (model.Sample, bool) {
	ring := f.hist[id]
	if len(ring) == 0 {
		return model.Sample{}, false
	}
	return ring[0], true
}
func (f *fakeMarket) HistoryHours() int { return f.hours }
func (f *fakeMarket) Status() model.MarketStatus {
	return model.MarketStatus{
		LastPollAt: testNow, LastSuccessAt: testNow, CacheSize: f.snap.Len(),
		TopN: 3000, PollInterval: 2 * time.Minute, HistoryAssets: len(f.hist), HistoryHours: f.hours,
		ProjectedCreditsPerDay: 14090,
	}
}

type fakeResolver struct{ entries []model.MapEntry }

func (r *fakeResolver) Resolve(query string) (model.Resolution, error) {
	for _, e := range r.entries {
		if e.Symbol == query {
			return model.Resolution{Asset: e, MatchedBy: "symbol"}, nil
		}
	}
	return model.Resolution{}, &model.NotFoundError{Query: query}
}
func (r *fakeResolver) Search(query string, limit int) []model.Candidate {
	out := make([]model.Candidate, 0, limit)
	for _, e := range r.entries {
		out = append(out, model.CandidateOf(e, "symbol"))
	}
	return out
}
func (r *fakeResolver) Size() int          { return len(r.entries) }
func (r *fakeResolver) BuiltAt() time.Time { return testNow.Add(-time.Hour) }

type fakeKeys struct{ known map[string]bool }

func (k fakeKeys) Lookup(_ context.Context, raw string) (model.APIKey, error) {
	if k.known[raw] {
		return model.APIKey{ID: 1, Active: true}, nil
	}
	return model.APIKey{}, model.ErrKeyNotFound
}

func quote(id int64, sym string, rank int, mcap, vol, c24 float64, tags ...string) model.Quote {
	return model.Quote{
		ID: id, Symbol: sym, Name: sym, Slug: sym,
		Rank: rank, Price: 2, MarketCap: mcap, Volume24h: vol,
		Change24hPct: c24, DateAdded: testNow.Add(-10 * 24 * time.Hour),
		Tags: tags, LastUpdated: testNow.Add(-time.Minute), FetchedAt: testNow,
	}
}

func testServer(t *testing.T, auth bool) (*httptest.Server, *fakeMarket) {
	t.Helper()
	m := &fakeMarket{
		snap: market.NewSnapshot([]model.Quote{
			quote(1, "AAA", 500, 20e6, 4e6, 10, "depin"),
			quote(2, "BBB", 501, 10e6, 500e3, 5, "memes"),
			quote(3, "STBL", 502, 30e6, 9e6, 0.1, "stablecoin"),
		}, testNow),
		quotes: map[int64]model.Quote{1: quote(1, "AAA", 500, 20e6, 4e6, 10, "depin")},
		newList: []model.Quote{
			quote(9, "NEW", 900, 5e6, 1e6, 3, "depin"),
		},
		hours: 48,
	}
	r := &fakeResolver{entries: []model.MapEntry{{ID: 1, Symbol: "AAA", Name: "AAA", Slug: "aaa", Rank: 500}}}
	var keys Keys
	if auth {
		keys = fakeKeys{known: map[string]bool{"good-key-123": true}}
	}
	srv := New(Config{AuthDisabled: !auth, Now: func() time.Time { return testNow }}, m, r, keys, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, m
}

func get(t *testing.T, ts *httptest.Server, path string, hdrs map[string]string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("%s: bad json: %v\n%s", path, err, body)
	}
	return resp.StatusCode, m
}

func dataArray(m map[string]any) []any {
	d, _ := m["data"].([]any)
	return d
}

func TestGems(t *testing.T) {
	ts, _ := testServer(t, false)
	code, m := get(t, ts, "/v1/gems?limit=3", nil)
	if code != 200 {
		t.Fatalf("status %d: %v", code, m)
	}
	if m["note"] == "" || m["history_hours"] != 48.0 {
		t.Fatalf("note/history_hours missing: %v", m)
	}
	items := dataArray(m)
	if len(items) != 2 { // stablecoin excluded by eligibility
		t.Fatalf("want 2 gems, got %v", items)
	}
	g := items[0].(map[string]any)
	if g["symbol"] != "AAA" || g["score"].(float64) <= 0 || g["risk_flags"] == nil || g["signals"] == nil {
		t.Fatalf("bad gem item: %v", g)
	}
	if _, ok := g["turnover"]; !ok {
		t.Fatal("turnover missing")
	}
	// sector filter
	_, m = get(t, ts, "/v1/gems?sector=depin", nil)
	if len(dataArray(m)) != 1 {
		t.Fatalf("sector filter: %v", m["data"])
	}
}

func TestScreen(t *testing.T) {
	ts, _ := testServer(t, false)
	code, m := get(t, ts, "/v1/screen?sort=turnover&limit=3", nil)
	if code != 200 {
		t.Fatalf("status %d: %v", code, m)
	}
	items := dataArray(m)
	if len(items) != 2 || items[0].(map[string]any)["symbol"] != "AAA" {
		t.Fatalf("screen: %v", items)
	}
	code, m = get(t, ts, "/v1/screen?bogus=1", nil)
	if code != 400 {
		t.Fatalf("unknown param should 400, got %d", code)
	}
	errObj := m["error"].(map[string]any)
	if errObj["code"] != "invalid_parameter" {
		t.Fatalf("error: %v", errObj)
	}
}

func TestClimbersEmptyWithWarning(t *testing.T) {
	ts, m := testServer(t, false)
	m.hours = 0
	code, body := get(t, ts, "/v1/climbers?limit=2", nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if len(dataArray(body)) != 0 {
		t.Fatalf("want empty data, got %v", body["data"])
	}
	warns, _ := body["warnings"].([]any)
	if len(warns) == 0 || warns[0].(map[string]any)["code"] != "insufficient_history" {
		t.Fatalf("want insufficient_history warning: %v", body["warnings"])
	}
}

func TestClimbersWithHistory(t *testing.T) {
	ts, m := testServer(t, false)
	m.hist = map[int64][]model.Sample{1: {{At: testNow.Add(-24 * time.Hour), Rank: 900}}}
	code, body := get(t, ts, "/v1/climbers?limit=2", nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	items := dataArray(body)
	if len(items) != 1 || items[0].(map[string]any)["rank_change"].(float64) != 400 {
		t.Fatalf("climber: %v", items)
	}
}

func TestNewListings(t *testing.T) {
	ts, _ := testServer(t, false)
	code, body := get(t, ts, "/v1/new-listings?days=14&limit=2", nil)
	if code != 200 {
		t.Fatalf("status %d: %v", code, body)
	}
	items := dataArray(body)
	if len(items) != 1 || items[0].(map[string]any)["symbol"] != "NEW" {
		t.Fatalf("new-listings: %v", items)
	}
}

func TestSectors(t *testing.T) {
	ts, m := testServer(t, false)
	// need >=5 members for a sector; add more depin assets
	qs := []model.Quote{}
	for i := 0; i < 5; i++ {
		qs = append(qs, quote(int64(10+i), "D"+string(rune('a'+i)), 600+i, 20e6, 4e6, float64(10-i), "depin"))
	}
	m.snap = market.NewSnapshot(qs, testNow)
	code, body := get(t, ts, "/v1/sectors?limit=3", nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	items := dataArray(body)
	if len(items) != 1 || items[0].(map[string]any)["tag"] != "depin" {
		t.Fatalf("sectors: %v", items)
	}
	code, body = get(t, ts, "/v1/sectors?sector=nope", nil)
	if code != 404 || body["error"].(map[string]any)["code"] != "sector_not_found" {
		t.Fatalf("sector 404: %d %v", code, body)
	}
}

func TestAsset(t *testing.T) {
	ts, _ := testServer(t, false)
	code, body := get(t, ts, "/v1/asset?asset=AAA", nil)
	if code != 200 {
		t.Fatalf("status %d: %v", code, body)
	}
	d := body["data"].(map[string]any)
	if d["symbol"] != "AAA" || d["signals"] == nil || d["eligible_for_gems"] != true {
		t.Fatalf("asset: %v", d)
	}
}

func TestHealth(t *testing.T) {
	ts, _ := testServer(t, false)
	code, body := get(t, ts, "/v1/health", nil)
	if code != 200 || body["status"] != "ok" {
		t.Fatalf("health: %d %v", code, body)
	}
	for _, k := range []string{"history_assets", "history_hours", "projected_credits_per_day", "telegram"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("health missing %s", k)
		}
	}
	if _, ok := body["currencies"]; ok {
		t.Fatal("currencies must be gone")
	}
}

func TestOpenAPI(t *testing.T) {
	ts, _ := testServer(t, false)
	code, body := get(t, ts, "/v1/openapi.json", nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	paths := body["paths"].(map[string]any)
	for _, p := range []string{"/v1/gems", "/v1/screen", "/v1/climbers", "/v1/new-listings", "/v1/sectors", "/v1/asset", "/v1/resolve", "/v1/health"} {
		if _, ok := paths[p]; !ok {
			t.Fatalf("openapi missing %s", p)
		}
	}
}

func TestAuth(t *testing.T) {
	ts, _ := testServer(t, true)
	code, _ := get(t, ts, "/v1/gems", nil)
	if code != 401 {
		t.Fatalf("want 401 without key, got %d", code)
	}
	code, _ = get(t, ts, "/v1/gems", map[string]string{"Authorization": "Bearer good-key-123"})
	if code != 200 {
		t.Fatalf("want 200 with key, got %d", code)
	}
	// health is public
	code, _ = get(t, ts, "/v1/health", nil)
	if code != 200 {
		t.Fatalf("health public: %d", code)
	}
}

func TestCurrencyGone(t *testing.T) {
	ts, _ := testServer(t, false)
	code, body := get(t, ts, "/v1/gems?currency=EUR", nil)
	if code != 400 {
		t.Fatalf("currency param should 400, got %d: %v", code, body)
	}
}
