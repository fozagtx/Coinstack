package discover

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/fozagtx/coinstack/internal/market"
	"github.com/fozagtx/coinstack/internal/model"
	"github.com/fozagtx/coinstack/internal/signals"
)

// SignalSet is the four signal components of a scored asset.
type SignalSet struct {
	Turnover   signals.Component `json:"turnover"`
	NewListing signals.Component `json:"new_listing"`
	RankClimb  signals.Component `json:"rank_climb"`
	SectorHeat signals.Component `json:"sector_heat"`
}

// GemItem is one /v1/gems row.
type GemItem struct {
	Item
	Score      float64   `json:"score"`
	Confidence string    `json:"confidence"`
	Signals    SignalSet `json:"signals"`
	RiskFlags  []string  `json:"risk_flags"`
	Why        []string  `json:"why"`
	HotSectors []string  `json:"hot_sectors"`
}

// GemsParams selects which assets are scored for /v1/gems.
type GemsParams struct {
	MaxMarketCap     float64 // default 50e6
	MinMarketCap     float64 // default 1e6
	MinVolume        float64 // default 100e3
	ListedWithinDays int     // 0 = any age
	Sector           string  // when set, only assets carrying this tag
	IncludePumped    bool    // keep gems flagged already_pumped
	Limit            int     // 0 = all
}

// GemsResult is the /v1/gems payload plus response metadata.
type GemsResult struct {
	Items        []GemItem
	AsOf         time.Time
	HistoryHours int
	Warnings     []Warning
}

// Gems scores every eligible asset in the snapshot and returns the best,
// highest composite score first.
func (e *Engine) Gems(_ context.Context, p GemsParams) (*GemsResult, error) {
	snap := e.market.Snapshot()
	sec := e.sectorsFor(snap, sectorMinMembers)
	hours := e.market.HistoryHours()
	opts := signals.Options{
		Now:              e.now(),
		MaxMarketCap:     p.MaxMarketCap,
		MinMarketCap:     p.MinMarketCap,
		MinVolume24h:     p.MinVolume,
		ListedWithinDays: p.ListedWithinDays,
	}
	gems := signals.Rank(snap, e.market.History, sec, hours, opts, 0)
	items := make([]GemItem, 0, len(gems))
	for _, g := range gems {
		if p.Sector != "" && !hasTag(g.Quote, p.Sector) {
			continue
		}
		if !p.IncludePumped && hasFlag(g.RiskFlags, "already_pumped") {
			continue
		}
		items = append(items, GemItem{
			Item:       itemOf(g.Quote),
			Score:      g.Score,
			Confidence: g.Confidence,
			Signals: SignalSet{
				Turnover:   g.Signals.Turnover,
				NewListing: g.Signals.NewListing,
				RankClimb:  g.Signals.RankClimb,
				SectorHeat: g.Signals.SectorHeat,
			},
			RiskFlags:  nonNil(g.RiskFlags),
			Why:        nonNil(g.Why),
			HotSectors: nonNil(g.HotSectors),
		})
		if p.Limit > 0 && len(items) >= p.Limit {
			break
		}
	}
	return &GemsResult{
		Items:        items,
		AsOf:         snap.Oldest(),
		HistoryHours: hours,
		Warnings:     e.shortHistoryWarning(),
	}, nil
}

// ScreenParams filters the snapshot for /v1/screen.
type ScreenParams struct {
	MinMarketCap, MaxMarketCap float64
	MinVolume, MaxVolume       float64
	MinTurnover                float64
	MinChange1h, MaxChange1h   float64
	MinChange24h, MaxChange24h float64
	MinChange7d, MaxChange7d   float64
	HasMin1h, HasMax1h         bool
	HasMin24h, HasMax24h       bool
	HasMin7d, HasMax7d         bool
	Tags                       []string // match ANY
	ListedWithinDays           int
	ExcludeStablecoins         bool
	Sort                       string // change_1h_pct|change_24h_pct|change_7d_pct|volume_24h|market_cap|turnover|rank
	Order                      string // asc|desc
	Limit                      int
}

// ScreenResult is the /v1/screen payload plus response metadata.
type ScreenResult struct {
	Items []Item
	AsOf  time.Time
}

var stableTags = map[string]bool{
	"stablecoin": true, "asset-backed-stablecoin": true,
	"fiat-stablecoin": true, "wrapped-tokens": true,
}

// Screen filters the snapshot without scoring.
func (e *Engine) Screen(_ context.Context, p ScreenParams) (*ScreenResult, error) {
	snap := e.market.Snapshot()
	cutoff := e.now().Add(-time.Duration(p.ListedWithinDays) * 24 * time.Hour)
	var out []Item
	for _, q := range snap.ByRank {
		if q.MarketCap < p.MinMarketCap || (p.MaxMarketCap > 0 && q.MarketCap > p.MaxMarketCap) {
			continue
		}
		if q.Volume24h < p.MinVolume || (p.MaxVolume > 0 && q.Volume24h > p.MaxVolume) {
			continue
		}
		if turnover(q) < p.MinTurnover {
			continue
		}
		if p.HasMin1h && q.Change1hPct < p.MinChange1h || p.HasMax1h && q.Change1hPct > p.MaxChange1h {
			continue
		}
		if p.HasMin24h && q.Change24hPct < p.MinChange24h || p.HasMax24h && q.Change24hPct > p.MaxChange24h {
			continue
		}
		if p.HasMin7d && q.Change7dPct < p.MinChange7d || p.HasMax7d && q.Change7dPct > p.MaxChange7d {
			continue
		}
		if len(p.Tags) > 0 && !hasAnyTag(q, p.Tags) {
			continue
		}
		if p.ListedWithinDays > 0 && (q.DateAdded.IsZero() || q.DateAdded.Before(cutoff)) {
			continue
		}
		if p.ExcludeStablecoins && hasAnyTagMap(q, stableTags) {
			continue
		}
		out = append(out, itemOf(q))
	}
	key := screenKey(p.Sort)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := key(&out[i]), key(&out[j])
		if a != b {
			if p.Order == "asc" {
				return a < b
			}
			return a > b
		}
		return out[i].ID < out[j].ID
	})
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return &ScreenResult{Items: out, AsOf: snap.Oldest()}, nil
}

func screenKey(sort string) func(*Item) float64 {
	switch sort {
	case "change_1h_pct":
		return func(i *Item) float64 { return i.Change1hPct }
	case "change_7d_pct":
		return func(i *Item) float64 { return i.Change7dPct }
	case "volume_24h":
		return func(i *Item) float64 { return i.Volume24h }
	case "market_cap":
		return func(i *Item) float64 { return i.MarketCap }
	case "turnover":
		return func(i *Item) float64 { return i.Turnover }
	case "rank":
		return func(i *Item) float64 {
			if i.Rank == 0 {
				return math.MaxFloat64
			}
			return float64(i.Rank)
		}
	default: // change_24h_pct
		return func(i *Item) float64 { return i.Change24hPct }
	}
}

// ClimberItem is one /v1/climbers row.
type ClimberItem struct {
	Item
	RankThen      int     `json:"rank_then"`
	RankChange    int     `json:"rank_change"`
	RankChangePct float64 `json:"rank_change_pct"`
}

// ClimbersParams selects the window and direction for /v1/climbers.
type ClimbersParams struct {
	Window       time.Duration // 24h or 7*24h
	Down         bool          // largest drops instead of climbs
	MinVolume    float64
	MaxMarketCap float64 // 0 = none
	Limit        int
}

// ClimbersResult is the /v1/climbers payload plus response metadata.
type ClimbersResult struct {
	Items        []ClimberItem
	AsOf         time.Time
	HistoryHours int
	Warnings     []Warning
}

// Climbers ranks assets by rank change over the window using retained
// history samples.
func (e *Engine) Climbers(_ context.Context, p ClimbersParams) (*ClimbersResult, error) {
	snap := e.market.Snapshot()
	var out []ClimberItem
	for _, q := range snap.ByRank {
		if q.Rank <= 0 || q.Volume24h < p.MinVolume {
			continue
		}
		if p.MaxMarketCap > 0 && q.MarketCap > p.MaxMarketCap {
			continue
		}
		then, ok := e.market.RankAt(q.ID, p.Window)
		if !ok || then.Rank == 0 {
			continue
		}
		change := then.Rank - q.Rank
		pct := float64(change) / float64(then.Rank)
		out = append(out, ClimberItem{
			Item:          itemOf(q),
			RankThen:      then.Rank,
			RankChange:    change,
			RankChangePct: math.Round(pct*10000) / 100,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.RankChangePct != b.RankChangePct {
			if p.Down {
				return a.RankChangePct < b.RankChangePct
			}
			return a.RankChangePct > b.RankChangePct
		}
		if a.RankChange != b.RankChange {
			if p.Down {
				return a.RankChange < b.RankChange
			}
			return a.RankChange > b.RankChange
		}
		return a.ID < b.ID
	})
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return &ClimbersResult{
		Items:        out,
		AsOf:         snap.Oldest(),
		HistoryHours: e.market.HistoryHours(),
		Warnings:     e.shortHistoryWarning(),
	}, nil
}

// NewListingItem is one /v1/new-listings row.
type NewListingItem struct {
	Item
	DaysListed float64 `json:"days_listed"`
	// RankChangeSinceFirstSeen is the rank gained since the oldest retained
	// history sample (positive = climbed); null without history.
	RankChangeSinceFirstSeen *int `json:"rank_change_since_first_seen"`
}

// NewListingsParams selects the freshness window for /v1/new-listings.
type NewListingsParams struct {
	Days      int
	MinVolume float64
	Limit     int
}

// NewListingsResult is the /v1/new-listings payload plus metadata.
type NewListingsResult struct {
	Items        []NewListingItem
	AsOf         time.Time
	HistoryHours int
	Warnings     []Warning
}

// NewListings returns assets first listed within p.Days days, newest
// first. Upstream errors from the market layer propagate unchanged.
func (e *Engine) NewListings(ctx context.Context, p NewListingsParams) (*NewListingsResult, error) {
	qs, err := e.market.NewListings(ctx, p.Days)
	if err != nil {
		return nil, err
	}
	now := e.now()
	items := make([]NewListingItem, 0, len(qs))
	for i := range qs {
		q := &qs[i]
		if q.Volume24h < p.MinVolume {
			continue
		}
		it := NewListingItem{Item: itemOf(q)}
		if !q.DateAdded.IsZero() {
			d := now.Sub(q.DateAdded).Hours() / 24
			if d < 0 {
				d = 0
			}
			it.DaysListed = math.Round(d*10) / 10
		}
		if ring := e.market.History(q.ID); len(ring) > 0 && ring[0].Rank > 0 && q.Rank > 0 {
			change := ring[0].Rank - q.Rank
			it.RankChangeSinceFirstSeen = &change
		}
		items = append(items, it)
		if p.Limit > 0 && len(items) >= p.Limit {
			break
		}
	}
	return &NewListingsResult{
		Items:        items,
		AsOf:         oldest(qs),
		HistoryHours: e.market.HistoryHours(),
		Warnings:     e.shortHistoryWarning(),
	}, nil
}

// Leader is a sector's top member by 24h change.
type Leader struct {
	ID           int64   `json:"id"`
	Symbol       string  `json:"symbol"`
	Name         string  `json:"name"`
	Change24hPct float64 `json:"change_24h_pct"`
}

// SectorOut is one sector row for /v1/sectors.
type SectorOut struct {
	Tag                string   `json:"tag"`
	Members            int      `json:"members"`
	MedianChange24hPct float64  `json:"median_change_24h_pct"`
	MedianChange7dPct  float64  `json:"median_change_7d_pct"`
	TotalVolume24h     float64  `json:"total_volume_24h"`
	TotalMarketCap     float64  `json:"total_market_cap"`
	Heat               float64  `json:"heat"`
	Leaders            []Leader `json:"leaders"`
}

// SectorNotFoundError is returned by Sectors when the requested tag is not
// tracked. Hottest lists the current five hottest tags for guidance.
type SectorNotFoundError struct {
	Tag     string
	Hottest []string
}

func (e *SectorNotFoundError) Error() string { return fmt.Sprintf("sector %q not found", e.Tag) }

// SectorsParams selects /v1/sectors output.
type SectorsParams struct {
	Sort       string // heat|change_24h_pct|change_7d_pct|volume_24h|market_cap
	MinMembers int    // default 5
	Sector     string // when set, return that tag's detail
	Limit      int
}

// SectorsResult is the /v1/sectors payload. Detail is set instead of
// Sectors when a single sector was requested.
type SectorsResult struct {
	Sectors []SectorOut // sector list view
	Detail  *SectorDetail
	AsOf    time.Time
}

// SectorDetail is one sector plus its member items.
type SectorDetail struct {
	Sector  SectorOut `json:"sector"`
	Members []Item    `json:"members"`
}

// Sectors aggregates the snapshot by tag, or returns one sector's detail.
func (e *Engine) Sectors(_ context.Context, p SectorsParams) (*SectorsResult, error) {
	if p.MinMembers <= 0 {
		p.MinMembers = sectorMinMembers
	}
	snap := e.market.Snapshot()
	sec := e.sectorsFor(snap, p.MinMembers)
	if p.Sector != "" {
		s, ok := sec.Sector(p.Sector)
		if !ok {
			return nil, &SectorNotFoundError{Tag: p.Sector, Hottest: e.hottest(sec, 5)}
		}
		var members []*model.Quote
		for _, q := range snap.ByRank {
			if hasTag(q, p.Sector) {
				members = append(members, q)
			}
		}
		sort.SliceStable(members, func(i, j int) bool {
			if members[i].Change24hPct != members[j].Change24hPct {
				return members[i].Change24hPct > members[j].Change24hPct
			}
			return members[i].ID < members[j].ID
		})
		if p.Limit > 0 && len(members) > p.Limit {
			members = members[:p.Limit]
		}
		return &SectorsResult{
			Detail: &SectorDetail{Sector: e.sectorOut(snap, s), Members: itemsOf(members)},
			AsOf:   snap.Oldest(),
		}, nil
	}
	sorted := sec.Sorted(p.Sort)
	if p.Limit > 0 && len(sorted) > p.Limit {
		sorted = sorted[:p.Limit]
	}
	out := make([]SectorOut, 0, len(sorted))
	for _, s := range sorted {
		out = append(out, e.sectorOut(snap, s))
	}
	return &SectorsResult{Sectors: out, AsOf: snap.Oldest()}, nil
}

func (e *Engine) sectorOut(snap *market.Snapshot, s signals.Sector) SectorOut {
	leaders := make([]Leader, 0, len(s.Leaders))
	for _, id := range s.Leaders {
		if q, ok := snap.Get(id); ok {
			leaders = append(leaders, Leader{ID: q.ID, Symbol: q.Symbol, Name: q.Name, Change24hPct: finite(q.Change24hPct)})
		}
	}
	return SectorOut{
		Tag:                s.Tag,
		Members:            s.Members,
		MedianChange24hPct: finite(s.MedianChange24hPct),
		MedianChange7dPct:  finite(s.MedianChange7dPct),
		TotalVolume24h:     finite(s.TotalVolume24h),
		TotalMarketCap:     finite(s.TotalMarketCap),
		Heat:               finite(s.Heat),
		Leaders:            leaders,
	}
}

func (e *Engine) hottest(sec *signals.Sectors, n int) []string {
	sorted := sec.Sorted("heat")
	out := make([]string, 0, n)
	for _, s := range sorted {
		out = append(out, s.Tag)
		if len(out) == n {
			break
		}
	}
	return out
}

// AssetItem is the /v1/asset detail object.
type AssetItem struct {
	Item
	Slug              string    `json:"slug"`
	CirculatingSupply float64   `json:"circulating_supply"`
	TotalSupply       float64   `json:"total_supply"`
	MaxSupply         *float64  `json:"max_supply"`
	Category          string    `json:"category,omitempty"`
	Platform          string    `json:"platform,omitempty"`
	Website           string    `json:"website,omitempty"`
	Score             float64   `json:"score"`
	Confidence        string    `json:"confidence"`
	EligibleForGems   bool      `json:"eligible_for_gems"`
	Signals           SignalSet `json:"signals"`
	RiskFlags         []string  `json:"risk_flags"`
	Why               []string  `json:"why"`
	HotSectors        []string  `json:"hot_sectors"`
}

// NoQuoteError is returned by Asset when CMC has no current quote for a
// resolved asset.
type NoQuoteError struct {
	Name string
	ID   int64
}

func (e *NoQuoteError) Error() string {
	return fmt.Sprintf("no current market quote for %s (id %d)", e.Name, e.ID)
}

// AssetResult is the /v1/asset payload plus response metadata.
type AssetResult struct {
	Item     AssetItem
	AsOf     time.Time
	Warnings []Warning
}

// Asset resolves query, fetches its quote and metadata, and scores it.
// Resolver errors (*model.AmbiguousError, *model.NotFoundError) and market
// errors propagate to the caller unchanged.
func (e *Engine) Asset(ctx context.Context, query string) (*AssetResult, error) {
	res, err := e.resolver.Resolve(query)
	if err != nil {
		return nil, err
	}
	var warns []Warning
	if len(res.Alternatives) > 0 {
		a := res.Asset
		rank := ""
		if a.Rank > 0 {
			rank = fmt.Sprintf(", rank %d", a.Rank)
		}
		warns = append(warns, Warning{
			Code: WarnByRank,
			Message: fmt.Sprintf("%q matches %d assets; returned %s (id %d%s), the dominant one. Pass an id from candidates to get another.",
				query, len(res.Alternatives)+1, a.Name, a.ID, rank),
			Query:      query,
			ChosenID:   a.ID,
			Candidates: capCandidates(res.Alternatives),
		})
	}
	got, err := e.market.Quotes(ctx, []int64{res.Asset.ID})
	q, ok := got[res.Asset.ID]
	if !ok {
		if err != nil {
			return nil, err
		}
		return nil, &NoQuoteError{Name: res.Asset.Name, ID: res.Asset.ID}
	}
	snap := e.market.Snapshot()
	if _, ok := snap.Get(q.ID); !ok {
		top := "top-N"
		if n := e.market.Status().TopN; n > 0 {
			top = fmt.Sprintf("top %d", n)
		}
		warns = append(warns, Warning{
			Code:    WarnOutsideTop,
			Message: fmt.Sprintf("%s is outside the %s cache and was fetched on demand.", q.Symbol, top),
		})
	}
	item := AssetItem{
		Item:              itemOf(&q),
		Slug:              q.Slug,
		CirculatingSupply: finite(q.CirculatingSupply),
		TotalSupply:       finite(q.TotalSupply),
	}
	if q.MaxSupply != nil && !math.IsNaN(*q.MaxSupply) && !math.IsInf(*q.MaxSupply, 0) {
		v := *q.MaxSupply
		item.MaxSupply = &v
	}
	if infos, ierr := e.market.Info(ctx, []int64{q.ID}); ierr == nil {
		if in, ok := infos[q.ID]; ok {
			item.Category = in.Category
			item.Platform = in.Platform
			item.Website = in.Website
		}
	}
	hours := e.market.HistoryHours()
	g := signals.Score(&q, e.market.History(q.ID), e.sectorsFor(snap, sectorMinMembers), hours, signals.Options{Now: e.now()})
	item.Score = g.Score
	item.Confidence = g.Confidence
	item.Signals = SignalSet{
		Turnover:   g.Signals.Turnover,
		NewListing: g.Signals.NewListing,
		RankClimb:  g.Signals.RankClimb,
		SectorHeat: g.Signals.SectorHeat,
	}
	item.RiskFlags = nonNil(g.RiskFlags)
	item.Why = nonNil(g.Why)
	item.HotSectors = nonNil(g.HotSectors)
	item.EligibleForGems = signals.Eligible(&q, signals.Options{Now: e.now()})
	return &AssetResult{Item: item, AsOf: q.LastUpdated, Warnings: warns}, nil
}

// ResolveResult is the /v1/resolve payload.
type ResolveResult struct {
	Candidates []model.Candidate `json:"candidates"`
	ResolvedID *int64            `json:"resolved_id"`
}

// Resolve searches the resolver index and reports what asset=<query>
// would resolve to.
func (e *Engine) Resolve(_ context.Context, query string, limit int) (*ResolveResult, time.Time, error) {
	cands := e.resolver.Search(query, limit)
	res := &ResolveResult{Candidates: append(make([]model.Candidate, 0, len(cands)), cands...)}
	if r, err := e.resolver.Resolve(query); err == nil {
		id := r.Asset.ID
		res.ResolvedID = &id
	}
	return res, e.resolver.BuiltAt(), nil
}

// maxCandidates caps the candidates listed in a warning.
const maxCandidates = 10

func capCandidates(c []model.Candidate) []model.Candidate {
	if len(c) > maxCandidates {
		c = c[:maxCandidates]
	}
	return append(make([]model.Candidate, 0, len(c)), c...)
}

func hasTag(q *model.Quote, tag string) bool {
	for _, t := range q.Tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

func hasAnyTag(q *model.Quote, tags []string) bool {
	for _, want := range tags {
		if hasTag(q, want) {
			return true
		}
	}
	return false
}

func hasAnyTagMap(q *model.Quote, tags map[string]bool) bool {
	for _, t := range q.Tags {
		if tags[strings.ToLower(t)] {
			return true
		}
	}
	return false
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// oldest returns the earliest LastUpdated among qs, or the zero time.
func oldest(qs []model.Quote) time.Time {
	var t time.Time
	for i := range qs {
		if t.IsZero() || qs[i].LastUpdated.Before(t) {
			t = qs[i].LastUpdated
		}
	}
	return t
}
