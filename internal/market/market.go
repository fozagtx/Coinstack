// Package market keeps CoinStack's market data fresh. A background poller
// refreshes the top-N snapshot from CoinMarketCap in tiers, while on-demand
// lookups fetch (and briefly cache) assets outside it, coalescing identical
// concurrent requests and spending a bounded credit budget. Smaller loops
// keep the resolver map, new listings, fiat rates and credit usage current.
package market

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/fozagtx/coinstack/internal/cache"
	"github.com/fozagtx/coinstack/internal/cmc"
	"github.com/fozagtx/coinstack/internal/model"
)

const (
	mapPageSize       = 5000             // assets per /cryptocurrency/map call
	maxMapPages       = 200              // guards against a map that never ends
	mapRetry          = 10 * time.Minute // retry delay after a failed map refresh
	newListingsLimit  = 200              // assets per /listings/new call
	fallbackMaxAssets = 100              // map entries quoted by the new-listings fallback
	fallbackWindow    = 30 * 24 * time.Hour
	maxBatch          = 100 // ids per quotes/info call
	onDemandMax       = 5000
	sweepInterval     = time.Minute
	staleWarnAfter    = 5 * time.Minute
	defaultRetryAfter = 30 * time.Second
	upstreamTimeout   = 30 * time.Second
)

// Config tunes a Market. Zero fields take the documented defaults.
type Config struct {
	// TopN is the number of assets the poller keeps fresh; default 500.
	TopN int
	// FastN splits the poll into tiers: ranks 1..FastN refresh every
	// PollInterval, ranks FastN+1..TopN every SlowInterval; default TopN.
	FastN int
	// PageSize is the number of assets per listings call; default 200
	// (CMC charges one credit per 200 assets returned).
	PageSize int
	// PollInterval is the fast-tier refresh interval; default 60s.
	PollInterval time.Duration
	// SlowInterval is the slow-tier refresh interval; default 5m.
	SlowInterval time.Duration
	// Workers is the number of listings pages fetched in parallel; default 3.
	Workers int
	// OnDemandTTL is how long an on-demand quote (or a "CMC does not know
	// this id" answer) is served without asking CMC again; default 60s.
	OnDemandTTL time.Duration
	// MaxOnDemandStale is how long after it was fetched an expired
	// on-demand quote may still be served, with its true age, when CMC
	// cannot be reached; default 30m.
	MaxOnDemandStale time.Duration
	// OnDemandBudgetPerMinute caps on-demand upstream calls (quotes and
	// info) in any rolling minute; default 5.
	OnDemandBudgetPerMinute int
	// OnDemandBudgetPerDay caps on-demand upstream calls per UTC day;
	// default 1000.
	OnDemandBudgetPerDay int
	// InfoTTL is how long asset metadata is cached; default 24h.
	InfoTTL time.Duration
	// MapRefresh is the resolver-map refresh interval; default 24h.
	MapRefresh time.Duration
	// LastMapFetch is when the resolver map was last fetched (for example
	// restored from the database). The first map refresh is due at
	// LastMapFetch+MapRefresh, or immediately when zero.
	LastMapFetch time.Time
	// SeedMap is a map restored from the database, used by the new-listings
	// fallback until a fresh map arrives.
	SeedMap []model.MapEntry
	// NewListingsRefresh is the new-listings refresh interval; default 10m.
	NewListingsRefresh time.Duration
	// FXRefresh is the fiat-rate refresh interval; default 15m.
	FXRefresh time.Duration
	// Currencies are the fiat currencies to convert to besides USD;
	// default EUR, GBP, JPY. A non-nil empty slice disables fiat rates.
	Currencies []string
	// KeyInfoRefresh is the interval for reading CMC's own credit usage;
	// default 5m.
	KeyInfoRefresh time.Duration
	// Now returns the current time; default time.Now. For tests.
	Now func() time.Time
	// Logger receives the market's logs; default slog.Default().
	Logger *slog.Logger

	// OnSnapshot is called with the full merged set after each published
	// poll result. It must not block for long or modify the quotes.
	OnSnapshot func(quotes []model.Quote)
	// OnPollRun is called after every upstream call (kinds: listings,
	// quotes, map, info, new, fx, keyinfo). It must not block for long.
	OnPollRun func(run model.PollRun)
	// OnMap is called after each successful full map refresh. It must not
	// block for long or modify the entries.
	OnMap func(entries []model.MapEntry, fetchedAt time.Time)
}

func (c *Config) setDefaults() {
	if c.TopN <= 0 {
		c.TopN = 500
	}
	if c.FastN <= 0 || c.FastN > c.TopN {
		c.FastN = c.TopN
	}
	if c.PageSize <= 0 {
		c.PageSize = 200
	}
	setDuration(&c.PollInterval, 60*time.Second)
	setDuration(&c.SlowInterval, 5*time.Minute)
	if c.Workers <= 0 {
		c.Workers = 3
	}
	setDuration(&c.OnDemandTTL, 60*time.Second)
	setDuration(&c.MaxOnDemandStale, 30*time.Minute)
	if c.OnDemandBudgetPerMinute <= 0 {
		c.OnDemandBudgetPerMinute = 5
	}
	if c.OnDemandBudgetPerDay <= 0 {
		c.OnDemandBudgetPerDay = 1000
	}
	setDuration(&c.InfoTTL, 24*time.Hour)
	setDuration(&c.MapRefresh, 24*time.Hour)
	setDuration(&c.NewListingsRefresh, 10*time.Minute)
	setDuration(&c.FXRefresh, 15*time.Minute)
	setDuration(&c.KeyInfoRefresh, 5*time.Minute)
	if c.Currencies == nil {
		c.Currencies = []string{"EUR", "GBP", "JPY"}
	}
	seen := map[string]bool{"USD": true, "": true}
	currencies := make([]string, 0, len(c.Currencies))
	for _, cur := range c.Currencies {
		cur = model.NormalizeCurrency(cur)
		if !seen[cur] {
			seen[cur] = true
			currencies = append(currencies, cur)
		}
	}
	c.Currencies = currencies
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

func setDuration(d *time.Duration, def time.Duration) {
	if *d <= 0 {
		*d = def
	}
}

// Market serves market data from memory and keeps it fresh from CMC. It
// implements api.Market. All methods are safe for concurrent use.
type Market struct {
	up    cmc.Upstream
	cfg   Config
	log   *slog.Logger
	now   func() time.Time
	store cache.Store

	pollMu sync.Mutex // serializes top-N polls; no reader ever waits on it

	// mu guards the fields below. It is only held briefly and never across
	// an upstream call.
	mu            sync.Mutex
	pages         []page
	orphans       map[int64]orphan
	published     bool // the poller has published at least once
	lastPollAt    time.Time
	lastSuccessAt time.Time
	lastError     string
	lastStaleWarn time.Time
	started       time.Time
	mapEntries    []model.MapEntry
	mapFetchedAt  time.Time
	newList       newListings

	running    atomic.Bool
	calls      atomic.Int64
	callErrors atomic.Int64
	credits    credits
	fx         atomic.Pointer[map[string]fxRate]

	quoteSrc *source[model.Quote]
	infoSrc  *source[model.Info]
	budget   *budget
	flights  singleflight.Group
	newKick  chan struct{}
}

// New returns a Market that reads from up. Zero Config fields take their
// defaults. Call Run to start the background refresh loops.
func New(up cmc.Upstream, cfg Config) *Market {
	cfg.setDefaults()
	m := &Market{
		up:           up,
		cfg:          cfg,
		log:          cfg.Logger.With("component", "market"),
		now:          cfg.Now,
		pages:        buildPages(cfg),
		orphans:      make(map[int64]orphan),
		mapEntries:   cfg.SeedMap,
		mapFetchedAt: cfg.LastMapFetch,
		budget:       newBudget(cfg.OnDemandBudgetPerMinute, cfg.OnDemandBudgetPerDay),
		newKick:      make(chan struct{}, 1),
	}
	m.started = m.now()
	m.quoteSrc = &source[model.Quote]{
		kind:  "quotes",
		cache: newLookupCache[model.Quote](cfg.OnDemandTTL, cfg.OnDemandTTL, cfg.MaxOnDemandStale),
		fetch: up.QuotesLatest,
		stamp: func(q model.Quote, now time.Time) model.Quote {
			if q.FetchedAt.IsZero() {
				q.FetchedAt = now
			}
			return q
		},
	}
	m.infoSrc = &source[model.Info]{
		kind:  "info",
		cache: newLookupCache[model.Info](cfg.InfoTTL, cfg.OnDemandTTL, 2*cfg.InfoTTL),
		fetch: up.Info,
		stamp: func(in model.Info, now time.Time) model.Info {
			if in.FetchedAt.IsZero() {
				in.FetchedAt = now
			}
			return in
		},
	}
	return m
}

// Run starts every refresh loop, beginning with an immediate top-N poll,
// and blocks until ctx is done. It returns nil once every loop has stopped.
func (m *Market) Run(ctx context.Context) error {
	if !m.running.CompareAndSwap(false, true) {
		return errors.New("market: Run is already running")
	}
	defer m.running.Store(false)

	var wg sync.WaitGroup
	start := func(loop func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop(ctx)
		}()
	}
	start(m.pollLoop)
	start(m.mapLoop)
	start(m.newListingsLoop)
	if len(m.cfg.Currencies) > 0 {
		start(func(ctx context.Context) { every(ctx, m.cfg.FXRefresh, m.refreshFX) })
	}
	start(func(ctx context.Context) { every(ctx, m.cfg.KeyInfoRefresh, m.refreshKeyInfo) })
	start(func(ctx context.Context) { every(ctx, sweepInterval, m.sweep) })
	wg.Wait()
	return nil
}

func (m *Market) pollLoop(ctx context.Context) {
	all := true // the first poll fetches every tier
	every(ctx, m.cfg.PollInterval, func(ctx context.Context) {
		_ = m.poll(ctx, all) // failures are logged and reported by poll
		all = false
	})
}

func (m *Market) sweep(context.Context) {
	now := m.now()
	m.quoteSrc.cache.sweep(now)
	m.infoSrc.cache.sweep(now)
}

// every calls fn immediately and then once per interval, measured from the
// start of each call, until ctx is done.
func every(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	for ctx.Err() == nil {
		start := time.Now()
		fn(ctx)
		if !sleep(ctx, interval-time.Since(start)) {
			return
		}
	}
}

// sleep waits for d, reporting false if ctx was done first.
func sleep(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Snapshot returns the latest published top-N snapshot; never nil.
func (m *Market) Snapshot() *cache.Snapshot { return m.store.Load() }

// Ready reports whether a non-empty snapshot has been published (by a poll
// or by Seed).
func (m *Market) Ready() bool { return m.store.Load().Len() > 0 }

// Status reports poller health, cache sizes and credit usage.
func (m *Market) Status() model.MarketStatus {
	m.mu.Lock()
	st := model.MarketStatus{
		LastPollAt:    m.lastPollAt,
		LastSuccessAt: m.lastSuccessAt,
		LastError:     m.lastError,
		MapFetchedAt:  m.mapFetchedAt,
	}
	m.mu.Unlock()
	st.CacheSize = m.store.Load().Len()
	st.OnDemandCacheSize = m.quoteSrc.cache.size()
	st.TopN = m.cfg.TopN
	st.PollInterval = m.cfg.PollInterval
	st.CreditsUsedToday, st.CreditsUsedMonth, st.CreditLimitMonthly = m.credits.usage(m.now())
	st.UpstreamCalls = m.calls.Load()
	st.UpstreamErrors = m.callErrors.Load()
	return st
}

// record accounts for one upstream call (counters and credits) and reports
// it through OnPollRun. Calls abandoned because ctx was canceled are not
// upstream failures and are not reported.
func (m *Market) record(ctx context.Context, kind string, started time.Time, meta cmc.Meta, assets int, err error) {
	if err != nil && errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	now := m.now()
	m.calls.Add(1)
	run := model.PollRun{
		Kind:          kind,
		StartedAt:     started,
		FinishedAt:    now,
		OK:            err == nil,
		HTTPStatus:    meta.HTTPStatus,
		CreditsUsed:   meta.CreditCount,
		AssetsFetched: assets,
	}
	if err != nil {
		m.callErrors.Add(1)
		run.Error = err.Error()
		var apiErr *cmc.APIError
		if run.HTTPStatus == 0 && errors.As(err, &apiErr) {
			run.HTTPStatus = apiErr.HTTPStatus
		}
	}
	m.credits.add(now, meta.CreditCount)
	if f := m.cfg.OnPollRun; f != nil {
		f(run)
	}
}

// credits counts CMC credits per UTC day and month, and prefers CMC's own
// figures from /v1/key/info once they are known.
type credits struct {
	mu       sync.Mutex
	day      string // UTC "2006-01-02" of today
	month    string // UTC "2006-01" of this month
	today    int
	thisMon  int
	key      *model.KeyUsage
	sinceKey int // credits counted locally since key was fetched
}

func (c *credits) rollLocked(now time.Time) {
	if d := dayKey(now); d != c.day {
		c.day, c.today = d, 0
	}
	if mo := now.UTC().Format("2006-01"); mo != c.month {
		c.month, c.thisMon = mo, 0
	}
}

func (c *credits) add(now time.Time, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollLocked(now)
	c.today += n
	c.thisMon += n
	c.sinceKey += n
}

func (c *credits) setKey(u model.KeyUsage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.key = &u
	c.sinceKey = 0
}

// usage returns credits used today and this month and the monthly limit.
// With CMC's figures available it reports them plus whatever was counted
// locally since they were fetched; CMC's "today" is only trusted on the UTC
// day it was fetched.
func (c *credits) usage(now time.Time) (today, month, limit int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollLocked(now)
	today, month = c.today, c.thisMon
	if k := c.key; k != nil {
		limit = k.CreditLimitMonthly
		month = k.CreditsUsedMonth + c.sinceKey
		if dayKey(k.FetchedAt) == dayKey(now) {
			today = k.CreditsUsedToday + c.sinceKey
		}
	}
	return today, month, limit
}

func (m *Market) refreshKeyInfo(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()
	started := m.now()
	u, err := m.up.KeyInfo(cctx)
	var meta cmc.Meta
	if err == nil {
		meta.HTTPStatus = 200
	}
	m.record(ctx, "keyinfo", started, meta, 0, err)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("key info refresh failed", "err", err)
		}
		return
	}
	if u.FetchedAt.IsZero() {
		u.FetchedAt = m.now()
	}
	m.credits.setKey(u)
}

func dayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

func nextUTCMidnight(t time.Time) time.Time {
	y, mo, d := t.UTC().Date()
	return time.Date(y, mo, d+1, 0, 0, 0, 0, time.UTC)
}

// unavailable wraps an upstream failure as model.ErrUpstreamUnavailable
// inside a *model.RetryAfterError, using CMC's retry hint when it gave one.
func unavailable(what string, err error) error {
	retry := defaultRetryAfter
	var apiErr *cmc.APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		retry = apiErr.RetryAfter
	}
	return &model.RetryAfterError{
		Err:        fmt.Errorf("%s: %w: %w", what, model.ErrUpstreamUnavailable, err),
		RetryAfter: retry,
	}
}

// joinIDs renders ids as a comma-separated key.
func joinIDs(ids []int64) string {
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(id, 10))
	}
	return b.String()
}
