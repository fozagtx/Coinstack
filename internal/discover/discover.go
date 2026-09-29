// Package discover holds the altcoin-discovery computations shared by the
// HTTP API and the Telegram bot: gem scoring, screening, rank climbers,
// new listings, sector aggregates and single-asset detail. Each method
// takes a parameter struct and returns presentation-ready items plus the
// warnings the caller should surface.
package discover

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/fozagtx/coinstack/internal/market"
	"github.com/fozagtx/coinstack/internal/model"
	"github.com/fozagtx/coinstack/internal/signals"
)

// Market is the market-data side the engine reads from; *market.Market
// implements it.
type Market interface {
	Snapshot() *market.Snapshot
	Quotes(ctx context.Context, ids []int64) (map[int64]model.Quote, error)
	Info(ctx context.Context, ids []int64) (map[int64]model.Info, error)
	NewListings(ctx context.Context, days int) ([]model.Quote, error)
	History(id int64) []model.Sample
	RankAt(id int64, ago time.Duration) (model.Sample, bool)
	HistoryHours() int
	Status() model.MarketStatus
}

// Resolver turns agent input (symbol, name, slug or CMC id) into assets.
type Resolver interface {
	Resolve(query string) (model.Resolution, error)
	Search(query string, limit int) []model.Candidate
	Size() int
	BuiltAt() time.Time
}

const (
	// sectorMinMembers and sectorMinVolume configure the shared sector index.
	sectorMinMembers = 5
	sectorMinVolume  = 100000

	// historyWarnHours is the history depth below which rank-climb and
	// turnover-surge signals cannot be computed.
	historyWarnHours = 24
)

// Warning is one advisory a method attaches to its result.
type Warning struct {
	Code       string
	Message    string
	Query      string
	ChosenID   int64
	Candidates []model.Candidate
}

// Warning codes used by the engine; the API maps them onto its contract.
const (
	WarnOutsideTop          = "outside_top_n"
	WarnByRank              = "symbol_resolved_by_rank"
	WarnInsufficientHistory = "insufficient_history"
)

// sectorEntry is the memoized sector index for one snapshot.
type sectorEntry struct {
	snap       *market.Snapshot
	minMembers int
	sec        *signals.Sectors
}

// Engine answers discovery queries over a Market's snapshot and history.
type Engine struct {
	market   Market
	resolver Resolver
	now      func() time.Time
	secCache atomic.Pointer[sectorEntry]
}

// New returns an Engine reading from m. r is required only by Asset and
// Resolve and may be nil when they are never called. now defaults to
// time.Now; pass a stub in tests.
func New(m Market, r Resolver, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{market: m, resolver: r, now: now}
}

// Status exposes the market's health summary.
func (e *Engine) Status() model.MarketStatus { return e.market.Status() }

// HistoryHours returns the depth of retained history in whole hours.
func (e *Engine) HistoryHours() int { return e.market.HistoryHours() }

// sectorsFor returns the sector index for snap, rebuilding it only when
// the snapshot pointer or member threshold changed.
func (e *Engine) sectorsFor(snap *market.Snapshot, minMembers int) *signals.Sectors {
	if c := e.secCache.Load(); c != nil && c.snap == snap && c.minMembers == minMembers {
		return c.sec
	}
	sec := signals.BuildSectors(snap, minMembers, sectorMinVolume)
	e.secCache.Store(&sectorEntry{snap: snap, minMembers: minMembers, sec: sec})
	return sec
}

// shortHistoryWarning returns the warning discovery endpoints add when
// history is too shallow for the time-based signals.
func (e *Engine) shortHistoryWarning() []Warning {
	h := e.market.HistoryHours()
	if h >= historyWarnHours {
		return nil
	}
	return []Warning{{
		Code:    WarnInsufficientHistory,
		Message: fmt.Sprintf("Rank-climb and turnover-surge signals need at least %d h of history; the service has %d h so far.", historyWarnHours, h),
	}}
}

// Item is the base asset object returned by every list endpoint.
type Item struct {
	ID           int64    `json:"id"`
	Symbol       string   `json:"symbol"`
	Name         string   `json:"name"`
	Rank         int      `json:"rank,omitempty"`
	Price        float64  `json:"price"`
	MarketCap    float64  `json:"market_cap"`
	Volume24h    float64  `json:"volume_24h"`
	Turnover     float64  `json:"turnover"`
	Change1hPct  float64  `json:"change_1h_pct"`
	Change24hPct float64  `json:"change_24h_pct"`
	Change7dPct  float64  `json:"change_7d_pct"`
	DateAdded    *string  `json:"date_added"`
	Tags         []string `json:"tags"`
	LastUpdated  string   `json:"last_updated"`
}

// maxTags caps the tags returned per asset.
const maxTags = 10

// itemOf renders q as a base Item.
func itemOf(q *model.Quote) Item {
	tags := q.Tags
	if len(tags) > maxTags {
		tags = tags[:maxTags]
	}
	var turn float64
	if q.MarketCap > 0 {
		turn = math.Round(q.Volume24h/q.MarketCap*10000) / 10000
	}
	return Item{
		ID:           q.ID,
		Symbol:       q.Symbol,
		Name:         q.Name,
		Rank:         max(q.Rank, 0),
		Price:        finite(q.Price),
		MarketCap:    finite(q.MarketCap),
		Volume24h:    finite(q.Volume24h),
		Turnover:     turn,
		Change1hPct:  finite(q.Change1hPct),
		Change24hPct: finite(q.Change24hPct),
		Change7dPct:  finite(q.Change7dPct),
		DateAdded:    optionalTime(q.DateAdded),
		Tags:         append(make([]string, 0, len(tags)), tags...),
		LastUpdated:  formatTime(q.LastUpdated),
	}
}

func itemsOf(qs []*model.Quote) []Item {
	out := make([]Item, 0, len(qs))
	for _, q := range qs {
		out = append(out, itemOf(q))
	}
	return out
}

// finite replaces NaN and infinities, which JSON cannot carry, with 0.
func finite(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func optionalTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := formatTime(t)
	return &s
}

// turnover is volume_24h / market_cap, or 0 when the cap is unknown.
func turnover(q *model.Quote) float64 {
	if q.MarketCap <= 0 {
		return 0
	}
	return q.Volume24h / q.MarketCap
}
