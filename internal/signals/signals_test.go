package signals

import (
	"math"
	"testing"
	"time"

	"github.com/fozagtx/coinstack/internal/market"
	"github.com/fozagtx/coinstack/internal/model"
)

var t0 = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func quote(id int64, rank int, mcap, vol float64) *model.Quote {
	return &model.Quote{
		ID: id, Symbol: "T", Name: "Test", Rank: rank,
		Price: 1, MarketCap: mcap, Volume24h: vol,
		DateAdded:   t0.Add(-40 * 24 * time.Hour),
		LastUpdated: t0,
	}
}

func opts() Options {
	return Options{Now: t0}
}

func TestTurnoverBase(t *testing.T) {
	cases := []struct {
		t    float64
		want float64
	}{
		{0, 0}, {0.05, 0}, {2, 100}, {3, 100},
	}
	for _, tc := range cases {
		c := turnoverScore(tc.t, nil, t0)
		if c.Score != tc.want {
			t.Errorf("t=%v: got %v want %v", tc.t, c.Score, tc.want)
		}
	}
	// t=0.5: (log10(0.5)-log10(0.05))/(log10(2)-log10(0.05)) = 1/1.602.
	if got := turnoverScore(0.5, nil, t0).Score; got < 62 || got > 63 {
		t.Errorf("t=0.5: got %v", got)
	}
}

func TestTurnoverSurge(t *testing.T) {
	// 24 hourly samples older than 24h at turnover 0.1.
	var hist []model.Sample
	for i := 0; i < 24; i++ {
		hist = append(hist, model.Sample{
			At:        t0.Add(-time.Duration(25+i) * time.Hour),
			MarketCap: 1e6, Volume24h: 1e5,
		})
	}
	// Current turnover 0.4 = 4x baseline → surge term clamp01(3/4)=0.75.
	c := turnoverScore(0.4, hist, t0)
	if c.Detail["baseline_turnover"] != 0.1 {
		t.Fatalf("baseline = %v", c.Detail["baseline_turnover"])
	}
	base := clamp01((log10(0.4)-log10(0.05))/(log10(2)-log10(0.05))) * 100
	want := 0.5*base + 0.5*clamp01(3.0/4)*100
	if c.Score != want {
		t.Fatalf("got %v want %v", c.Score, want)
	}
	// Fewer than 12 old samples: base only.
	c2 := turnoverScore(0.4, hist[:11], t0)
	if c2.Score != base {
		t.Fatalf("short history: got %v want %v", c2.Score, base)
	}
}

func TestNewListing(t *testing.T) {
	q := quote(1, 500, 1e7, 1e6)
	for _, tc := range []struct {
		age  time.Duration
		want float64
	}{
		{0, 100}, {45 * 24 * time.Hour, 50}, {90 * 24 * time.Hour, 0}, {200 * 24 * time.Hour, 0},
	} {
		q.DateAdded = t0.Add(-tc.age)
		c := newListingScore(q, t0)
		if c.Score != tc.want {
			t.Errorf("age %v: got %v want %v", tc.age, c.Score, tc.want)
		}
	}
	q.DateAdded = time.Time{}
	if c := newListingScore(q, t0); c.Score != 0 {
		t.Errorf("zero date: got %v", c.Score)
	}
}

func histRank(ago time.Duration, rank int) model.Sample {
	return model.Sample{At: t0.Add(-ago), Rank: rank}
}

func TestRankClimb24hOnly(t *testing.T) {
	// 1000 -> 700 over 24h = +30%; only the 24h window available: full weight.
	hist := []model.Sample{histRank(24*time.Hour, 1000)}
	c, ok := rankClimbScore(&model.Quote{Rank: 700}, hist, t0)
	if !ok {
		t.Fatal("want climb available")
	}
	if c.Score != 100 {
		t.Fatalf("got %v want 100", c.Score)
	}
	if c.Detail["rank_change_24h"] != 300 {
		t.Fatalf("detail %+v", c.Detail)
	}
}

func TestRankClimbBoth(t *testing.T) {
	// 24h: +30% -> clamp01(1)=1; 7d: +25% -> clamp01(0.5)=0.5.
	hist := []model.Sample{histRank(24*time.Hour, 1000), histRank(7*24*time.Hour, 1000)}
	c, ok := rankClimbScore(&model.Quote{Rank: 700}, hist, t0)
	if !ok {
		t.Fatal("want climb")
	}
	// pct24 = 300/1000 = 0.3 -> 1; pct7d = 300/1000 = 0.3 -> 0.6.
	want := (0.6*1 + 0.4*0.6) * 100
	if c.Score != want {
		t.Fatalf("got %v want %v", c.Score, want)
	}
}

func TestRankClimbNone(t *testing.T) {
	_, ok := rankClimbScore(&model.Quote{Rank: 700}, nil, t0)
	if ok {
		t.Fatal("want unavailable")
	}
	// A sample too far from the window also fails (tolerance).
	hist := []model.Sample{histRank(48*time.Hour, 1000)}
	if _, ok := rankClimbScore(&model.Quote{Rank: 700}, hist, t0); ok {
		t.Fatal("48h sample should not satisfy the 24h or 7d windows")
	}
}

func TestScoreNoHistoryRenormalizes(t *testing.T) {
	q := quote(1, 500, 1e7, 5e6) // turnover 0.5 -> base ~49.7
	g := Score(q, nil, nil, 0, opts())
	sum := wTurnover*g.Signals.Turnover.Score + wNewListing*g.Signals.NewListing.Score
	// wTurnover+wNewListing+wSectorHeat = 0.7 renormalized to sum 1.
	want := round1(sum / 0.7)
	if g.Score != want {
		t.Fatalf("got %v want %v", g.Score, want)
	}
	if g.Confidence != "low" {
		t.Fatalf("confidence = %s", g.Confidence)
	}
	if !contains(g.RiskFlags, "insufficient_history") {
		t.Fatalf("flags = %v", g.RiskFlags)
	}
}

func TestAlreadyPumpedPenalty(t *testing.T) {
	q := quote(1, 500, 1e7, 5e6)
	base := Score(q, nil, nil, 0, opts()).Score
	q.Change24hPct = 150
	pumped := Score(q, nil, nil, 0, opts())
	if !contains(pumped.RiskFlags, "already_pumped") {
		t.Fatalf("flags = %v", pumped.RiskFlags)
	}
	if pumped.Score != round1(base*0.6) {
		t.Fatalf("got %v want %v", pumped.Score, base*0.6)
	}
}

func TestEligible(t *testing.T) {
	if !Eligible(quote(1, 500, 1e7, 5e5), opts()) {
		t.Fatal("want eligible")
	}
	for name, q := range map[string]*model.Quote{
		"too_big":    quote(2, 10, 6e7, 5e5),
		"too_small":  quote(3, 4000, 5e5, 5e5),
		"no_volume":  quote(4, 500, 1e7, 1e3),
		"stablecoin": {ID: 5, Rank: 500, MarketCap: 1e7, Volume24h: 5e5, Tags: []string{"stablecoin"}},
	} {
		if Eligible(q, opts()) {
			t.Errorf("%s should be ineligible", name)
		}
	}
	// ListedWithinDays filter.
	old := quote(6, 500, 1e7, 5e5)
	old.DateAdded = t0.Add(-100 * 24 * time.Hour)
	o := opts()
	o.ListedWithinDays = 30
	if Eligible(old, o) {
		t.Error("old listing should be ineligible under ListedWithinDays")
	}
}

func TestBuildSectors(t *testing.T) {
	mk := func(id int64, tag string, c24, c7, vol, mcap float64) model.Quote {
		return model.Quote{ID: id, Symbol: "S", Rank: int(id), MarketCap: mcap, Volume24h: vol,
			Change24hPct: c24, Change7dPct: c7, Tags: []string{tag}, LastUpdated: t0}
	}
	snap := market.NewSnapshot([]model.Quote{
		mk(1, "ai-big-data", 20, 30, 1e6, 1e7),
		mk(2, "ai-big-data", 10, 20, 2e6, 2e7),
		mk(3, "ai-big-data", 30, 10, 3e6, 3e7),
		mk(4, "memes", -5, 5, 1e6, 1e7),
		mk(5, "memes", -5, 5, 1e6, 1e7),
		mk(6, "memes", -5, 5, 1e6, 1e7),
		mk(7, "mineable", 50, 50, 9e6, 9e7), // excluded tag
		mk(8, "pow", 50, 50, 9e6, 9e7),      // excluded tag
		mk(9, "rare", 50, 50, 9e6, 9e7),     // below minMembers
	}, t0)
	sec := BuildSectors(snap, 3, 0)
	ai, ok := sec.Sector("ai-big-data")
	if !ok {
		t.Fatal("ai-big-data missing")
	}
	if ai.Members != 3 || ai.MedianChange24hPct != 20 || ai.MedianChange7dPct != 20 {
		t.Fatalf("ai sector %+v", ai)
	}
	if ai.Heat != 100 { // 20/15 clamped
		t.Fatalf("heat = %v", ai.Heat)
	}
	if len(ai.Leaders) != 3 || ai.Leaders[0] != 3 {
		t.Fatalf("leaders = %v", ai.Leaders)
	}
	m, _ := sec.Sector("memes")
	if m.Heat != 0 { // -5% median -> clamped 0
		t.Fatalf("memes heat = %v", m.Heat)
	}
	if _, ok := sec.Sector("mineable"); ok {
		t.Fatal("mineable should be excluded")
	}
	if _, ok := sec.Sector("pow"); ok {
		t.Fatal("pow should be excluded")
	}
	if _, ok := sec.Sector("rare"); ok {
		t.Fatal("rare below minMembers should be excluded")
	}
	sorted := sec.Sorted("heat")
	if len(sorted) != 2 || sorted[0].Tag != "ai-big-data" {
		t.Fatalf("sorted = %+v", sorted)
	}
	// Leaders respect minVolume.
	sec2 := BuildSectors(snap, 3, 2e6)
	ai2, _ := sec2.Sector("ai-big-data")
	if len(ai2.Leaders) != 2 {
		t.Fatalf("leaders with minVolume = %v", ai2.Leaders)
	}
}

func TestRankDeterministic(t *testing.T) {
	snap := market.NewSnapshot([]model.Quote{
		*quote(3, 100, 1e7, 5e6),
		*quote(1, 200, 1e7, 5e6),
		*quote(2, 300, 1e7, 5e6),
	}, t0)
	gems := Rank(snap, nil, nil, 0, opts(), 10)
	if len(gems) != 3 {
		t.Fatalf("got %d gems", len(gems))
	}
	for i := 1; i < len(gems); i++ {
		if gems[i-1].Score < gems[i].Score {
			t.Fatal("not sorted by score")
		}
	}
	// Identical inputs produce identical ordering.
	gems2 := Rank(snap, nil, nil, 0, opts(), 10)
	for i := range gems {
		if gems[i].Quote.ID != gems2[i].Quote.ID {
			t.Fatal("nondeterministic order")
		}
	}
	// Limit respected.
	if got := Rank(snap, nil, nil, 0, opts(), 2); len(got) != 2 {
		t.Fatalf("limit: got %d", len(got))
	}
}

func TestConfidenceTiers(t *testing.T) {
	q := quote(1, 500, 1e7, 5e6)
	if g := Score(q, nil, nil, 24, opts()); g.Confidence != "medium" {
		t.Fatalf("got %s", g.Confidence)
	}
	if g := Score(q, nil, nil, 72, opts()); g.Confidence != "high" {
		t.Fatalf("got %s", g.Confidence)
	}
}

func log10(x float64) float64 { return math.Log10(x) }

func round1(x float64) float64 { return math.Round(x*10) / 10 }

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
