package discover

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/fozagtx/coinstack/internal/market"
	"github.com/fozagtx/coinstack/internal/model"
)

var testNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// fakeMarket is an in-memory discover.Market for tests.
type fakeMarket struct {
	snap    *market.Snapshot
	hist    map[int64][]model.Sample
	newList []model.Quote
	newErr  error
	quotes  map[int64]model.Quote
	infos   map[int64]model.Info
	hours   int
}

func (f *fakeMarket) Snapshot() *market.Snapshot { return f.snap }
func (f *fakeMarket) Quotes(context.Context, []int64) (map[int64]model.Quote, error) {
	return f.quotes, nil
}
func (f *fakeMarket) Info(context.Context, []int64) (map[int64]model.Info, error) {
	return f.infos, nil
}
func (f *fakeMarket) NewListings(context.Context, int) ([]model.Quote, error) {
	return f.newList, f.newErr
}
func (f *fakeMarket) History(id int64) []model.Sample { return f.hist[id] }
func (f *fakeMarket) RankAt(id int64, ago time.Duration) (model.Sample, bool) {
	ring := f.hist[id]
	if len(ring) == 0 {
		return model.Sample{}, false
	}
	target := testNow.Add(-ago)
	best := ring[0]
	bestDist := absDur(best.At.Sub(target))
	for _, s := range ring[1:] {
		if d := absDur(s.At.Sub(target)); d < bestDist {
			best, bestDist = s, d
		}
	}
	tol := ago / 4
	if tol < 30*time.Minute {
		tol = 30 * time.Minute
	}
	if bestDist > tol {
		return model.Sample{}, false
	}
	return best, true
}
func (f *fakeMarket) HistoryHours() int { return f.hours }
func (f *fakeMarket) Status() model.MarketStatus {
	return model.MarketStatus{TopN: 3000, CacheSize: f.snap.Len(), LastSuccessAt: testNow}
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// fakeResolver is a minimal Resolver for tests.
type fakeResolver struct {
	entries []model.MapEntry
}

func (r *fakeResolver) Resolve(query string) (model.Resolution, error) {
	for _, e := range r.entries {
		if e.Symbol == query || e.Slug == query || e.Name == query || query == itoa(e.ID) {
			return model.Resolution{Asset: e, MatchedBy: "symbol"}, nil
		}
	}
	return model.Resolution{}, &model.NotFoundError{Query: query}
}

func (r *fakeResolver) Search(query string, limit int) []model.Candidate {
	var out []model.Candidate
	for _, e := range r.entries {
		out = append(out, model.CandidateOf(e, "symbol"))
		if len(out) >= limit {
			break
		}
	}
	return out
}
func (r *fakeResolver) Size() int          { return len(r.entries) }
func (r *fakeResolver) BuiltAt() time.Time { return testNow.Add(-time.Hour) }

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

func quote(id int64, sym, name string, rank int, mcap, vol, c24 float64, tags ...string) model.Quote {
	return model.Quote{
		ID: id, Symbol: sym, Name: name, Slug: name,
		Rank: rank, Price: 1.5, MarketCap: mcap, Volume24h: vol,
		Change24hPct: c24, Change7dPct: 2 * c24,
		DateAdded:   testNow.Add(-30 * 24 * time.Hour),
		Tags:        tags,
		LastUpdated: testNow.Add(-time.Minute),
		FetchedAt:   testNow.Add(-time.Minute),
	}
}

func newEngine(t *testing.T, m *fakeMarket, r *fakeResolver) *Engine {
	t.Helper()
	if r == nil {
		r = &fakeResolver{}
	}
	return New(m, r, func() time.Time { return testNow })
}

func TestGemsSortedWithSignals(t *testing.T) {
	m := &fakeMarket{snap: market.NewSnapshot([]model.Quote{
		quote(1, "AAA", "Alpha", 500, 20e6, 4e6, 10, "depin"),
		quote(2, "BBB", "Beta", 501, 10e6, 0.5e6, 5, "memes"),
		quote(3, "STBL", "Stable", 502, 20e6, 5e6, 0.1, "stablecoin"),
	}, testNow), hours: 48}
	e := newEngine(t, m, nil)
	res, err := e.Gems(context.Background(), GemsParams{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("want 2 gems (stablecoin excluded), got %d", len(res.Items))
	}
	if res.Items[0].Symbol != "AAA" {
		t.Fatalf("AAA should outrank BBB on turnover, got %s", res.Items[0].Symbol)
	}
	if res.Items[0].RiskFlags == nil {
		t.Fatal("risk_flags must never be null")
	}
	if res.Items[0].Signals.Turnover.Score <= 0 {
		t.Fatal("turnover signal missing")
	}
}

func TestGemsWarningsAndFilters(t *testing.T) {
	m := &fakeMarket{snap: market.NewSnapshot([]model.Quote{
		quote(1, "AAA", "Alpha", 500, 20e6, 4e6, 10, "depin"),
		quote(2, "PMP", "Pumped", 501, 20e6, 4e6, 150, "depin"),
	}, testNow)}
	e := newEngine(t, m, nil)
	res, err := e.Gems(context.Background(), GemsParams{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.HistoryHours != 0 || len(res.Warnings) != 1 || res.Warnings[0].Code != WarnInsufficientHistory {
		t.Fatalf("want one insufficient_history warning, got %+v", res.Warnings)
	}
	// PMP is already_pumped and excluded by default.
	if len(res.Items) != 1 || res.Items[0].Symbol != "AAA" {
		t.Fatalf("pumped asset should be excluded, got %+v", res.Items)
	}
	res, _ = e.Gems(context.Background(), GemsParams{Limit: 10, IncludePumped: true})
	if len(res.Items) != 2 {
		t.Fatalf("include_pumped should keep both, got %d", len(res.Items))
	}
	res, _ = e.Gems(context.Background(), GemsParams{Limit: 10, Sector: "nope"})
	if len(res.Items) != 0 {
		t.Fatalf("sector filter should empty the list, got %d", len(res.Items))
	}
}

func TestScreenFiltersAndSort(t *testing.T) {
	m := &fakeMarket{snap: market.NewSnapshot([]model.Quote{
		quote(1, "AAA", "Alpha", 100, 20e6, 4e6, 10, "depin"),
		quote(2, "BBB", "Beta", 200, 40e6, 1e6, -5, "gaming"),
		quote(3, "STBL", "Stable", 300, 30e6, 9e6, 0.1, "stablecoin"),
	}, testNow)}
	e := newEngine(t, m, nil)

	res, err := e.Screen(context.Background(), ScreenParams{
		MinVolume: 2e6, Sort: "market_cap", Order: "desc", Limit: 10, ExcludeStablecoins: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Symbol != "AAA" {
		t.Fatalf("min_volume+stablecoin exclusion leaves AAA, got %+v", res.Items)
	}

	res, _ = e.Screen(context.Background(), ScreenParams{
		HasMin24h: true, MinChange24h: -10, Sort: "rank", Order: "asc", Limit: 10,
	})
	if len(res.Items) != 3 {
		t.Fatalf("no exclusion: want 3, got %d", len(res.Items))
	}
	if res.Items[0].Rank != 100 {
		t.Fatalf("rank asc expected rank 100 first, got %d", res.Items[0].Rank)
	}
}

func TestClimbersWithAndWithoutHistory(t *testing.T) {
	m := &fakeMarket{snap: market.NewSnapshot([]model.Quote{
		quote(1, "AAA", "Alpha", 700, 20e6, 4e6, 10, "depin"),
	}, testNow)}
	e := newEngine(t, m, nil)

	res, err := e.Climbers(context.Background(), ClimbersParams{Window: 24 * time.Hour, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 || len(res.Warnings) != 1 {
		t.Fatalf("no history: want empty + warning, got %+v", res.Items)
	}

	m.hist = map[int64][]model.Sample{
		1: {{At: testNow.Add(-24 * time.Hour), Rank: 1000}},
	}
	m.hours = 24
	res, err = e.Climbers(context.Background(), ClimbersParams{Window: 24 * time.Hour, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].RankChange != 300 || res.Items[0].RankThen != 1000 {
		t.Fatalf("want climb 1000→700, got %+v", res.Items)
	}
	if res.Items[0].RankChangePct != 30 {
		t.Fatalf("rank_change_pct want 30, got %v", res.Items[0].RankChangePct)
	}
}

func TestSectorsListAndDetail(t *testing.T) {
	mk := func(id int64, sym string, c24 float64) model.Quote {
		return quote(id, sym, sym, int(id)+100, 20e6, 4e6, c24, "depin")
	}
	qs := []model.Quote{mk(1, "A", 30), mk(2, "B", 20), mk(3, "C", 10), mk(4, "D", 0), mk(5, "E", -5)}
	m := &fakeMarket{snap: market.NewSnapshot(qs, testNow), hours: 100}
	e := newEngine(t, m, nil)

	res, err := e.Sectors(context.Background(), SectorsParams{Sort: "heat", MinMembers: 2, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sectors) != 1 || res.Sectors[0].Tag != "depin" || res.Sectors[0].Heat <= 60 {
		t.Fatalf("unexpected sectors %+v", res.Sectors)
	}
	if len(res.Sectors[0].Leaders) != 3 || res.Sectors[0].Leaders[0].Symbol != "A" {
		t.Fatalf("leaders wrong: %+v", res.Sectors[0].Leaders)
	}

	det, err := e.Sectors(context.Background(), SectorsParams{Sector: "depin", Limit: 2})
	if err != nil || det.Detail == nil {
		t.Fatalf("detail: %+v %v", det, err)
	}
	if len(det.Detail.Members) != 2 || det.Detail.Members[0].Symbol != "A" {
		t.Fatalf("members sorted by 24h desc: %+v", det.Detail.Members)
	}

	_, err = e.Sectors(context.Background(), SectorsParams{Sector: "nope"})
	var snf *SectorNotFoundError
	if !errors.As(err, &snf) || len(snf.Hottest) == 0 {
		t.Fatalf("want SectorNotFoundError with hottest tags, got %v", err)
	}
}

func TestAsset(t *testing.T) {
	m := &fakeMarket{
		snap: market.NewSnapshot([]model.Quote{
			quote(1, "SOL", "Solana", 5, 100e9, 3e9, 2, "layer-1"),
		}, testNow),
		quotes: map[int64]model.Quote{1: quote(1, "SOL", "Solana", 5, 100e9, 3e9, 2, "layer-1")},
		hours:  100,
	}
	r := &fakeResolver{entries: []model.MapEntry{{ID: 1, Symbol: "SOL", Name: "Solana", Slug: "solana", Rank: 5}}}
	e := newEngine(t, m, r)
	res, err := e.Asset(context.Background(), "SOL")
	if err != nil {
		t.Fatal(err)
	}
	if res.Item.Slug != "solana" && res.Item.Slug != "Solana" {
		t.Fatalf("slug missing: %+v", res.Item)
	}
	if res.Item.Confidence != "high" || res.Item.EligibleForGems {
		t.Fatalf("want high confidence, ineligible (mcap too big): %+v", res.Item)
	}
	_, err = e.Asset(context.Background(), "nope")
	var nf *model.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("want NotFoundError, got %v", err)
	}
}

func TestResolve(t *testing.T) {
	r := &fakeResolver{entries: []model.MapEntry{{ID: 7, Symbol: "UNI", Name: "Uniswap", Slug: "uniswap", Rank: 20}}}
	m := &fakeMarket{snap: market.NewSnapshot(nil, testNow)}
	e := newEngine(t, m, r)
	res, _, err := e.Resolve(context.Background(), "UNI", 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResolvedID == nil || *res.ResolvedID != 7 || len(res.Candidates) != 1 {
		t.Fatalf("resolve: %+v", res)
	}
}
