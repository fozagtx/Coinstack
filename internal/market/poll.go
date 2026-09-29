package market

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/fozagtx/coinstack/internal/model"
)

// page is one listings call of the top-N poll and its latest data.
type page struct {
	start, limit int
	slow         bool
	quotes       []model.Quote // last successful result (or seeded data); never modified
	refreshedAt  time.Time     // start of the cycle that last fetched it; zero = never
}

// orphan is an asset that vanished from a refreshed page while some other
// page was not refreshed, so it may simply have moved there. It is kept
// until every page has been refreshed since it vanished.
type orphan struct {
	q     model.Quote
	since time.Time
}

type pageResult struct {
	quotes []model.Quote
	err    error
}

// buildPages splits ranks 1..FastN (fast tier) and FastN+1..TopN (slow
// tier) into listings calls of at most PageSize assets.
func buildPages(cfg Config) []page {
	var pages []page
	add := func(from, to int, slow bool) {
		for s := from; s <= to; s += cfg.PageSize {
			pages = append(pages, page{start: s, limit: min(cfg.PageSize, to-s+1), slow: slow})
		}
	}
	add(1, cfg.FastN, false)
	add(cfg.FastN+1, cfg.TopN, true)
	return pages
}

// PollOnce runs one top-N poll with every tier due and publishes the
// result. It returns an error when any page failed; successful pages are
// still published, merged with the previous data for the failed ones.
func (m *Market) PollOnce(ctx context.Context) error { return m.poll(ctx, true) }

// poll fetches the due pages in parallel and publishes a new merged
// snapshot. With all set every page is due; otherwise fast pages always
// are and slow pages once SlowInterval has passed since they last
// succeeded (less half a PollInterval, to absorb ticker jitter).
func (m *Market) poll(ctx context.Context, all bool) error {
	m.pollMu.Lock()
	defer m.pollMu.Unlock()

	now := m.now()
	slowDue := m.cfg.SlowInterval - m.cfg.PollInterval/2
	var due []int
	var specs []page
	m.mu.Lock()
	for i, p := range m.pages {
		if all || !p.slow || p.refreshedAt.IsZero() || now.Sub(p.refreshedAt) >= slowDue {
			due = append(due, i)
			specs = append(specs, page{start: p.start, limit: p.limit})
		}
	}
	m.mu.Unlock()

	results := m.fetchPages(ctx, specs)
	if err := ctx.Err(); err != nil {
		return err
	}

	var failed int
	var firstErr error
	m.mu.Lock()
	var dropped []model.Quote
	for j, r := range results {
		p := &m.pages[due[j]]
		if r.err != nil {
			failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("ranks %d-%d: %w", p.start, p.start+p.limit-1, r.err)
			}
			continue
		}
		dropped = append(dropped, p.quotes...)
		p.quotes = r.quotes
		p.refreshedAt = now
	}
	m.lastPollAt = now
	var err error
	if failed > 0 {
		err = fmt.Errorf("listings poll: %d of %d pages failed: %w", failed, len(results), firstErr)
		m.lastError = err.Error()
	}
	var merged []model.Quote
	var snap *Snapshot
	if failed < len(results) {
		merged = m.mergeLocked(now, dropped)
		snap = NewSnapshot(merged, now)
		m.store.Publish(snap)
		m.recordHistoryLocked(now, merged)
		m.notifySubsLocked(snap)
		m.published = true
		if failed == 0 {
			m.lastSuccessAt = now
			m.lastError = ""
		}
	}
	lastSuccess, staleFor := m.staleLocked(now)
	lastError := m.lastError
	m.mu.Unlock()

	switch {
	case failed == len(results):
		m.log.Warn("listings poll failed; keeping the previous snapshot", "err", err)
	case failed > 0:
		m.log.Warn("listings poll partly failed; published merged data", "err", err, "assets", len(merged))
	default:
		m.log.Debug("listings poll published", "assets", len(merged), "pages", len(results))
	}
	if staleFor > 0 {
		m.log.Warn("top-N data is stale", "last_success_at", lastSuccess, "stale_for", staleFor.Round(time.Second), "last_error", lastError)
	}
	if merged != nil {
		if f := m.cfg.OnSnapshot; f != nil {
			f(merged)
		}
	}
	return err
}

// staleLocked reports, at most once per staleWarnAfter, how long the last
// successful poll (or startup) is behind when that exceeds staleWarnAfter.
func (m *Market) staleLocked(now time.Time) (lastSuccess time.Time, staleFor time.Duration) {
	since := m.lastSuccessAt
	if since.IsZero() {
		since = m.started
	}
	if now.Sub(since) <= staleWarnAfter || (!m.lastStaleWarn.IsZero() && now.Sub(m.lastStaleWarn) < staleWarnAfter) {
		return m.lastSuccessAt, 0
	}
	m.lastStaleWarn = now
	return m.lastSuccessAt, now.Sub(since)
}

// fetchPages fetches specs with up to Workers calls in flight.
func (m *Market) fetchPages(ctx context.Context, specs []page) []pageResult {
	results := make([]pageResult, len(specs))
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(m.cfg.Workers, len(specs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				results[i] = m.fetchPage(ctx, specs[i])
			}
		}()
	}
	for i := range specs {
		next <- i
	}
	close(next)
	wg.Wait()
	return results
}

func (m *Market) fetchPage(ctx context.Context, p page) pageResult {
	cctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()
	started := m.now()
	qs, meta, err := m.up.ListingsLatest(cctx, p.start, p.limit)
	m.record(ctx, "listings", started, meta, len(qs), err)
	if err != nil {
		return pageResult{err: err}
	}
	if len(qs) > p.limit {
		qs = qs[:p.limit]
	}
	fetched := m.now()
	out := make([]model.Quote, len(qs))
	for i, q := range qs {
		if q.FetchedAt.IsZero() {
			q.FetchedAt = fetched
		}
		out[i] = q
	}
	return pageResult{quotes: out}
}

// mergeLocked builds a new top-N set from every page's latest data plus
// orphans, one quote per id (the most recently updated wins). dropped holds
// the previous quotes of the pages refreshed this cycle.
func (m *Market) mergeLocked(now time.Time, dropped []model.Quote) []model.Quote {
	best := make(map[int64]model.Quote, m.cfg.TopN)
	oldest := now
	for _, p := range m.pages {
		for _, q := range p.quotes {
			if cur, ok := best[q.ID]; !ok || newer(q, cur) {
				best[q.ID] = q
			}
		}
		if p.refreshedAt.Before(oldest) {
			oldest = p.refreshedAt
		}
	}
	for _, q := range dropped {
		_, onPage := best[q.ID]
		_, known := m.orphans[q.ID]
		if !onPage && !known {
			m.orphans[q.ID] = orphan{q: q, since: now}
		}
	}
	orphanTTL := 2 * max(m.cfg.SlowInterval, m.cfg.PollInterval)
	for id, o := range m.orphans {
		_, onPage := best[id]
		if onPage || !oldest.Before(o.since) || now.Sub(o.since) > orphanTTL {
			delete(m.orphans, id)
			continue
		}
		best[id] = o.q
	}

	merged := make([]model.Quote, 0, len(best))
	for _, q := range best {
		merged = append(merged, q)
	}
	sort.Slice(merged, func(i, j int) bool {
		a, b := merged[i], merged[j]
		if (a.Rank > 0) != (b.Rank > 0) {
			return a.Rank > 0
		}
		if a.Rank != b.Rank {
			return a.Rank < b.Rank
		}
		return a.ID < b.ID
	})
	return merged
}

// newer reports whether a is more recent data than b.
func newer(a, b model.Quote) bool {
	if !a.LastUpdated.Equal(b.LastUpdated) {
		return a.LastUpdated.After(b.LastUpdated)
	}
	return a.FetchedAt.After(b.FetchedAt)
}

// Seed publishes quotes restored from the database at startup, unless a
// poll has already published or a newer snapshot is in place. The quotes
// also stand in for their pages until those are fetched, so a partly
// failed first poll keeps them for the pages that failed.
func (m *Market) Seed(quotes []model.Quote) {
	if len(quotes) == 0 {
		return
	}
	snap := NewSnapshot(quotes, m.now())
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.published {
		return
	}
	if cur := m.store.Load(); cur.Len() > 0 && !snap.Newest().After(cur.Newest()) {
		return
	}
	m.store.Publish(snap)
	m.recordHistoryLocked(snap.PublishedAt, quotes)
	m.notifySubsLocked(snap)
	for i := range m.pages {
		m.pages[i].quotes = nil
	}
	for _, q := range snap.ByRank {
		for i := range m.pages {
			p := &m.pages[i]
			if q.Rank >= p.start && q.Rank < p.start+p.limit {
				p.quotes = append(p.quotes, *q)
				break
			}
		}
	}
}
