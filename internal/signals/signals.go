// Package signals scores assets for the altcoin-discovery endpoints. Every
// function is pure: callers hand it a snapshot quote, that asset's history
// samples and the sector index built once per snapshot, and get back a
// transparent composite score with the components and reasons behind it.
package signals

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/fozagtx/coinstack/internal/market"
	"github.com/fozagtx/coinstack/internal/model"
)

// Options tunes eligibility and scoring. Zero fields take the documented
// defaults.
type Options struct {
	// Now is the reference time; default time.Now().
	Now time.Time
	// MaxMarketCap excludes assets already too big to be "early";
	// default 50e6.
	MaxMarketCap float64
	// MinMarketCap excludes dust; default 1e6.
	MinMarketCap float64
	// MinVolume24h excludes dead markets; default 100e3.
	MinVolume24h float64
	// ListedWithinDays limits to new listings; 0 = any age.
	ListedWithinDays int
	// ExcludeTags drops entire categories; default stablecoins, wrapped
	// tokens, tokenized assets and ETFs.
	ExcludeTags []string
}

func (o *Options) defaults() {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.MaxMarketCap <= 0 {
		o.MaxMarketCap = 50e6
	}
	if o.MinMarketCap <= 0 {
		o.MinMarketCap = 1e6
	}
	if o.MinVolume24h <= 0 {
		o.MinVolume24h = 100e3
	}
	if o.ExcludeTags == nil {
		o.ExcludeTags = []string{
			"stablecoin", "asset-backed-stablecoin", "fiat-stablecoin",
			"wrapped-tokens", "tokenized-stock", "tokenized-gold", "etf",
		}
	}
}

// Component is one signal's contribution to the composite score.
type Component struct {
	Score  float64            `json:"score"`
	Detail map[string]float64 `json:"detail,omitempty"`
}

// Gem is one scored asset.
type Gem struct {
	Quote      *model.Quote
	Score      float64 // 0..100 composite
	Confidence string  // "low" | "medium" | "high"
	Signals    struct {
		Turnover   Component `json:"turnover"`
		NewListing Component `json:"new_listing"`
		RankClimb  Component `json:"rank_climb"`
		SectorHeat Component `json:"sector_heat"`
	}
	RiskFlags  []string
	Why        []string // ≤4 short plain-English reasons, best first
	HotSectors []string // the asset's tags among the hot sectors (≤3)
	Turnover   float64  // volume_24h / market_cap
}

const (
	wTurnover   = 0.30
	wNewListing = 0.20
	wRankClimb  = 0.30
	wSectorHeat = 0.20
)

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// Eligible reports whether q passes the eligibility filters.
func Eligible(q *model.Quote, o Options) bool {
	o.defaults()
	if q.MarketCap < o.MinMarketCap || q.MarketCap > o.MaxMarketCap {
		return false
	}
	if q.Volume24h < o.MinVolume24h {
		return false
	}
	if o.ListedWithinDays > 0 {
		if q.DateAdded.IsZero() || o.Now.Sub(q.DateAdded) > time.Duration(o.ListedWithinDays)*24*time.Hour {
			return false
		}
	}
	excluded := make(map[string]bool, len(o.ExcludeTags))
	for _, t := range o.ExcludeTags {
		excluded[strings.ToLower(t)] = true
	}
	for _, t := range q.Tags {
		if excluded[strings.ToLower(t)] {
			return false
		}
	}
	return true
}

// Score evaluates one asset. hist is the asset's samples oldest-first
// (may be empty), sec the sector index (may be nil), historyHours the age
// of the oldest sample in the market's history ring.
func Score(q *model.Quote, hist []model.Sample, sec *Sectors, historyHours int, o Options) Gem {
	o.defaults()
	g := Gem{Quote: q}
	if q.MarketCap > 0 {
		g.Turnover = q.Volume24h / q.MarketCap
	}
	g.Signals.Turnover = turnoverScore(g.Turnover, hist, o.Now)
	g.Signals.NewListing = newListingScore(q, o.Now)
	climb, ok := rankClimbScore(q, hist, o.Now)
	g.Signals.RankClimb = climb
	g.Signals.SectorHeat, g.HotSectors = sectorScore(q, sec)

	var flags []string
	total := wTurnover + wNewListing + wSectorHeat
	composite := wTurnover*g.Signals.Turnover.Score + wNewListing*g.Signals.NewListing.Score + wSectorHeat*g.Signals.SectorHeat.Score
	if ok {
		composite += wRankClimb * climb.Score
		total += wRankClimb
	} else {
		flags = append(flags, "insufficient_history")
	}
	g.Score = composite / total

	switch {
	case historyHours < 24:
		g.Confidence = "low"
	case historyHours < 72:
		g.Confidence = "medium"
	default:
		g.Confidence = "high"
	}

	if q.Change24hPct > 100 || q.Change7dPct > 300 {
		flags = append(flags, "already_pumped")
		g.Score *= 0.6
	}
	if q.Volume24h < 250e3 {
		flags = append(flags, "thin_volume")
	}
	if q.MarketCap < 5e6 {
		flags = append(flags, "micro_cap")
	}
	if !q.DateAdded.IsZero() && o.Now.Sub(q.DateAdded) < 7*24*time.Hour {
		flags = append(flags, "new_and_unproven")
	}
	if q.Rank == 0 {
		flags = append(flags, "unranked")
	}
	g.RiskFlags = flags
	g.Score = math.Round(g.Score*10) / 10
	g.Why = why(q, g)
	return g
}

// turnoverScore rates volume/mcap against a fixed 0.05..2 band, and when
// enough older samples exist blends in how far it runs above its own
// baseline.
func turnoverScore(t float64, hist []model.Sample, now time.Time) Component {
	c := Component{Detail: map[string]float64{"turnover": t}}
	if t <= 0 {
		return c
	}
	base := clamp01((math.Log10(t)-math.Log10(0.05))/(math.Log10(2)-math.Log10(0.05))) * 100
	if base < 1e-9 {
		base = 0
	}
	var older []float64
	for _, s := range hist {
		if now.Sub(s.At) >= 24*time.Hour && s.MarketCap > 0 {
			older = append(older, s.Volume24h/s.MarketCap)
		}
	}
	if len(older) < 12 {
		c.Score = base
		return c
	}
	baseline := median(older)
	var score float64
	if baseline <= 0 {
		c.Detail["baseline_turnover"] = 0
		c.Score = base
		return c
	}
	surge := t / baseline
	score = 0.5*base + 0.5*clamp01((surge-1)/4)*100
	c.Detail["baseline_turnover"] = baseline
	c.Detail["surge"] = surge
	c.Score = score
	return c
}

// newListingScore rewards recency linearly: listed today scores 100, 90
// days or older scores 0.
func newListingScore(q *model.Quote, now time.Time) Component {
	c := Component{}
	if q.DateAdded.IsZero() {
		return c
	}
	ageDays := now.Sub(q.DateAdded).Hours() / 24
	if ageDays < 0 {
		ageDays = 0
	}
	c.Score = clamp01(1-ageDays/90) * 100
	c.Detail = map[string]float64{"age_days": math.Round(ageDays*10) / 10}
	return c
}

// rankClimbScore measures rank gains over 24h and 7d. ok is false when no
// retained sample is close enough to either window.
func rankClimbScore(q *model.Quote, hist []model.Sample, now time.Time) (Component, bool) {
	c := Component{Detail: map[string]float64{}}
	s24, ok24 := nearestSample(hist, now, 24*time.Hour)
	s7d, ok7d := nearestSample(hist, now, 7*24*time.Hour)
	nowRank := q.Rank
	var pct24, pct7d float64
	if ok24 && s24.Rank > 0 && nowRank > 0 {
		pct24 = float64(s24.Rank-nowRank) / float64(s24.Rank)
		c.Detail["rank_24h_ago"] = float64(s24.Rank)
		c.Detail["rank_change_24h"] = float64(s24.Rank - nowRank)
	}
	if ok7d && s7d.Rank > 0 && nowRank > 0 {
		pct7d = float64(s7d.Rank-nowRank) / float64(s7d.Rank)
		c.Detail["rank_7d_ago"] = float64(s7d.Rank)
		c.Detail["rank_change_7d"] = float64(s7d.Rank - nowRank)
	}
	c.Detail["rank_now"] = float64(nowRank)
	switch {
	case ok24 && ok7d:
		c.Score = (0.6*clamp01(pct24/0.30) + 0.4*clamp01(pct7d/0.50)) * 100
	case ok24:
		c.Score = clamp01(pct24/0.30) * 100
	case ok7d:
		c.Score = clamp01(pct7d/0.50) * 100
	default:
		delete(c.Detail, "rank_now")
		return c, false
	}
	return c, true
}

// nearestSample returns the sample closest to now-ago when it lies within
// ±25% of ago (30-minute minimum tolerance).
func nearestSample(hist []model.Sample, now time.Time, ago time.Duration) (model.Sample, bool) {
	target := now.Add(-ago)
	best := model.Sample{}
	bestDist := time.Duration(math.MaxInt64)
	for _, s := range hist {
		d := s.At.Sub(target)
		if d < 0 {
			d = -d
		}
		if d < bestDist {
			bestDist, best = d, s
		}
	}
	tol := ago / 4
	if tol < 30*time.Minute {
		tol = 30 * time.Minute
	}
	if best.At.IsZero() || bestDist > tol {
		return model.Sample{}, false
	}
	return best, true
}

// sectorScore returns the hottest sector heat among the asset's tags, and
// the tags that qualify as hot (Heat ≥ 60), best first.
func sectorScore(q *model.Quote, sec *Sectors) (Component, []string) {
	c := Component{}
	if sec == nil {
		return c, nil
	}
	var hot []string
	best := 0.0
	bestMedian := 0.0
	for _, t := range q.Tags {
		s, ok := sec.byTag[t]
		if !ok {
			continue
		}
		if s.Heat > best {
			best = s.Heat
			bestMedian = s.MedianChange24hPct
		}
		if s.Heat >= 60 {
			hot = append(hot, t)
		}
	}
	slices.SortFunc(hot, func(a, b string) int {
		ha, hb := sec.byTag[a].Heat, sec.byTag[b].Heat
		switch {
		case ha != hb:
			return cmpFloat64(hb, ha)
		default:
			return strings.Compare(a, b)
		}
	})
	if len(hot) > 3 {
		hot = hot[:3]
	}
	c.Score = best
	c.Detail = map[string]float64{"sector_heat": best, "sector_median_24h": bestMedian}
	return c, hot
}

// why renders up to four plain-English reasons, strongest components
// first; a component only earns a reason when it scores at least 40.
func why(q *model.Quote, g Gem) []string {
	type reason struct {
		score float64
		text  string
	}
	var rs []reason
	if s := g.Signals.Turnover; s.Score >= 40 {
		if surge := s.Detail["surge"]; surge > 0 {
			rs = append(rs, reason{s.Score, fmt.Sprintf("Turnover %.2f is %.1fx its 7-day baseline", g.Turnover, surge)})
		} else {
			rs = append(rs, reason{s.Score, fmt.Sprintf("Turnover %.2f (volume vs market cap)", g.Turnover)})
		}
	}
	if s := g.Signals.RankClimb; s.Score >= 40 {
		if then, ok := s.Detail["rank_24h_ago"]; ok && q.Rank > 0 {
			pct := (then - float64(q.Rank)) / then * 100
			rs = append(rs, reason{s.Score, fmt.Sprintf("Climbed %.0f%% in rank over 24h (#%.0f -> #%d)", pct, then, q.Rank)})
		} else if then, ok := s.Detail["rank_7d_ago"]; ok && q.Rank > 0 {
			pct := (then - float64(q.Rank)) / then * 100
			rs = append(rs, reason{s.Score, fmt.Sprintf("Climbed %.0f%% in rank over 7d (#%.0f -> #%d)", pct, then, q.Rank)})
		}
	}
	if s := g.Signals.NewListing; s.Score >= 40 {
		rs = append(rs, reason{s.Score, fmt.Sprintf("Listed %.0f days ago", s.Detail["age_days"])})
	}
	if s := g.Signals.SectorHeat; s.Score >= 40 && len(g.HotSectors) > 0 {
		rs = append(rs, reason{s.Score, fmt.Sprintf("Sector %s is hot (median +%.0f%% 24h)", g.HotSectors[0], s.Detail["sector_median_24h"])})
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].score > rs[j].score })
	out := make([]string, 0, min(4, len(rs)))
	for _, r := range rs {
		out = append(out, r.text)
		if len(out) == 4 {
			break
		}
	}
	return out
}

// Rank scores every eligible asset in snap and returns the top limit,
// highest score first (ties break toward smaller market cap, then id).
func Rank(snap *market.Snapshot, hist func(id int64) []model.Sample, sec *Sectors, historyHours int, o Options, limit int) []Gem {
	if snap == nil {
		return nil
	}
	o.defaults()
	gems := make([]Gem, 0, snap.Len())
	for _, q := range snap.ByRank {
		if !Eligible(q, o) {
			continue
		}
		var h []model.Sample
		if hist != nil {
			h = hist(q.ID)
		}
		gems = append(gems, Score(q, h, sec, historyHours, o))
	}
	sort.SliceStable(gems, func(i, j int) bool {
		a, b := gems[i], gems[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Quote.MarketCap != b.Quote.MarketCap {
			return a.Quote.MarketCap < b.Quote.MarketCap
		}
		return a.Quote.ID < b.Quote.ID
	})
	if limit > 0 && len(gems) > limit {
		gems = gems[:limit]
	}
	return gems
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	ys := slices.Clone(xs)
	slices.Sort(ys)
	n := len(ys)
	if n%2 == 1 {
		return ys[n/2]
	}
	return (ys[n/2-1] + ys[n/2]) / 2
}

func cmpFloat64(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
