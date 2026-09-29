package market

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/fozagtx/coinstack/internal/cmc"
	"github.com/fozagtx/coinstack/internal/model"
)

// newListings is the cached result of the new-listings loop.
type newListings struct {
	quotes    []model.Quote // DateAdded always set; never modified
	fetchedAt time.Time     // last success; zero = never
	err       error         // last attempt's error; nil after a success
	fallback  bool          // listings/new is not in the plan; use the map
}

func (m *Market) mapLoop(ctx context.Context) {
	m.mu.Lock()
	last := m.mapFetchedAt
	m.mu.Unlock()
	var wait time.Duration
	if !last.IsZero() {
		wait = last.Add(m.cfg.MapRefresh).Sub(m.now())
	}
	for sleep(ctx, wait) {
		wait = m.cfg.MapRefresh
		if err := m.refreshMap(ctx); err != nil {
			wait = min(mapRetry, m.cfg.MapRefresh)
			if ctx.Err() == nil {
				m.log.Warn("map refresh failed", "err", err, "retry_in", wait)
			}
		}
	}
}

// refreshMap pages through /cryptocurrency/map until a short page. Only a
// complete map replaces the previous one and is passed to OnMap.
func (m *Market) refreshMap(ctx context.Context) error {
	var entries []model.MapEntry
	seen := make(map[int64]bool)
	for n, start := 0, 1; ; n, start = n+1, start+mapPageSize {
		if n == maxMapPages {
			return fmt.Errorf("map: more than %d pages", maxMapPages)
		}
		cctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
		started := m.now()
		page, meta, err := m.up.Map(cctx, start, mapPageSize)
		cancel()
		m.record(ctx, "map", started, meta, len(page), err)
		if err != nil {
			return err
		}
		for _, e := range page {
			if !seen[e.ID] {
				seen[e.ID] = true
				entries = append(entries, e)
			}
		}
		if len(page) < mapPageSize {
			break
		}
	}
	if len(entries) == 0 {
		return errors.New("map: CMC returned no assets")
	}
	now := m.now()
	m.mu.Lock()
	m.mapEntries = entries
	m.mapFetchedAt = now
	kick := m.newList.fallback && m.newList.fetchedAt.IsZero()
	m.mu.Unlock()
	m.log.Info("map refreshed", "assets", len(entries))
	if f := m.cfg.OnMap; f != nil {
		f(entries, now)
	}
	if kick {
		select {
		case m.newKick <- struct{}{}:
		default:
		}
	}
	return nil
}

func (m *Market) newListingsLoop(ctx context.Context) {
	for {
		start := time.Now()
		m.refreshNewListings(ctx)
		t := time.NewTimer(max(0, m.cfg.NewListingsRefresh-time.Since(start)))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-m.newKick: // the fallback was waiting for a map
			t.Stop()
		}
	}
}

// refreshNewListings reads /listings/new or, once CMC has said the plan
// does not include it, quotes the newest map entries instead.
func (m *Market) refreshNewListings(ctx context.Context) {
	m.mu.Lock()
	fallback := m.newList.fallback
	m.mu.Unlock()

	if !fallback {
		qs, err := m.fetchNewPrimary(ctx)
		var apiErr *cmc.APIError
		if err == nil || !errors.As(err, &apiErr) || !apiErr.PlanRestricted() {
			m.setNewListings(ctx, qs, err)
			return
		}
		m.log.Info("listings/new is not in the CMC plan; using the map fallback", "err", err)
		m.mu.Lock()
		m.newList.fallback = true
		m.mu.Unlock()
	}
	qs, err := m.fetchNewFallback(ctx)
	m.setNewListings(ctx, qs, err)
}

func (m *Market) setNewListings(ctx context.Context, qs []model.Quote, err error) {
	if err != nil && ctx.Err() != nil {
		return
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.newList.err = err
	if err == nil {
		m.newList.quotes = qs
		m.newList.fetchedAt = now
	} else {
		m.log.Warn("new listings refresh failed", "err", err, "fallback", m.newList.fallback)
	}
}

func (m *Market) fetchNewPrimary(ctx context.Context) ([]model.Quote, error) {
	cctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()
	started := m.now()
	qs, meta, err := m.up.ListingsNew(cctx, 1, newListingsLimit)
	m.record(ctx, "new", started, meta, len(qs), err)
	if err != nil {
		return nil, err
	}
	fetched := m.now()
	out := make([]model.Quote, 0, len(qs))
	for _, q := range qs {
		if q.DateAdded.IsZero() {
			continue
		}
		if q.FetchedAt.IsZero() {
			q.FetchedAt = fetched
		}
		out = append(out, q)
	}
	return out, nil
}

// fetchNewFallback quotes the map entries with the most recent
// FirstHistoricalData (up to fallbackMaxAssets, within fallbackWindow).
// A quote without DateAdded takes the entry's FirstHistoricalData.
func (m *Market) fetchNewFallback(ctx context.Context) ([]model.Quote, error) {
	m.mu.Lock()
	entries := m.mapEntries
	m.mu.Unlock()
	if len(entries) == 0 {
		return nil, errors.New("new listings fallback: resolver map not loaded yet")
	}
	cutoff := m.now().Add(-fallbackWindow)
	var recent []model.MapEntry
	for _, e := range entries {
		if !e.FirstHistoricalData.IsZero() && !e.FirstHistoricalData.Before(cutoff) {
			recent = append(recent, e)
		}
	}
	sort.Slice(recent, func(i, j int) bool {
		a, b := recent[i].FirstHistoricalData, recent[j].FirstHistoricalData
		if !a.Equal(b) {
			return a.After(b)
		}
		return recent[i].ID > recent[j].ID
	})
	recent = recent[:min(len(recent), fallbackMaxAssets)]
	if len(recent) == 0 {
		return []model.Quote{}, nil
	}
	ids := make([]int64, len(recent))
	for i, e := range recent {
		ids[i] = e.ID
	}

	cctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()
	started := m.now()
	res, meta, err := m.up.QuotesLatest(cctx, ids)
	m.record(ctx, "new", started, meta, len(res), err)
	if err != nil {
		return nil, err
	}
	fetched := m.now()
	out := make([]model.Quote, 0, len(res))
	for _, e := range recent {
		q, ok := res[e.ID]
		if !ok {
			continue
		}
		if q.DateAdded.IsZero() {
			q.DateAdded = e.FirstHistoricalData
		}
		if q.FetchedAt.IsZero() {
			q.FetchedAt = fetched
		}
		out = append(out, q)
	}
	return out, nil
}

// NewListings returns cached assets added within the last days days,
// newest first. It returns an error wrapping model.ErrUpstreamUnavailable
// when no new-listings data has been fetched yet.
func (m *Market) NewListings(_ context.Context, days int) ([]model.Quote, error) {
	m.mu.Lock()
	st := m.newList
	m.mu.Unlock()
	if st.fetchedAt.IsZero() {
		err := st.err
		if err == nil {
			err = errors.New("not fetched yet")
		}
		return nil, unavailable("new listings", err)
	}
	cutoff := m.now().Add(-time.Duration(days) * 24 * time.Hour)
	out := make([]model.Quote, 0, len(st.quotes))
	for _, q := range st.quotes {
		if !q.DateAdded.Before(cutoff) {
			out = append(out, q)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DateAdded.After(out[j].DateAdded) })
	return out, nil
}
