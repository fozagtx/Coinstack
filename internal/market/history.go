package market

import (
	"slices"
	"time"

	"github.com/fozagtx/coinstack/internal/model"
)

// history.go keeps a per-asset ring of rank/price/market-cap/volume
// samples, recorded on every published snapshot, so signals can look at
// how an asset moved rather than only where it is now.

// recordHistoryLocked appends a sample for each quote whose last retained
// sample is at least HistoryBucket old, then drops ids whose newest sample
// is older than HistoryWindow. Caller must hold m.mu.
func (m *Market) recordHistoryLocked(now time.Time, quotes []model.Quote) {
	cutoff := now.Add(-m.cfg.HistoryWindow)
	for _, q := range quotes {
		ring := m.history[q.ID]
		if n := len(ring); n > 0 && now.Sub(ring[n-1].At) < m.cfg.HistoryBucket {
			continue
		}
		m.history[q.ID] = append(ring, model.Sample{
			At:        now,
			Rank:      q.Rank,
			Price:     q.Price,
			MarketCap: q.MarketCap,
			Volume24h: q.Volume24h,
		})
	}
	for id, ring := range m.history {
		// Trim leading samples outside the window; drop the id entirely
		// when nothing remains.
		keep := 0
		for keep < len(ring) && ring[keep].At.Before(cutoff) {
			keep++
		}
		if keep == len(ring) {
			delete(m.history, id)
			continue
		}
		if keep > 0 {
			m.history[id] = append([]model.Sample(nil), ring[keep:]...)
		}
	}
}

// History returns the asset's retained samples, oldest first. The slice is
// a copy; callers may keep it.
func (m *Market) History(id int64) []model.Sample {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]model.Sample(nil), m.history[id]...)
}

// RankAt returns the retained sample nearest to now-ago, provided it lies
// within ±25% of ago (with a 30-minute minimum tolerance).
func (m *Market) RankAt(id int64, ago time.Duration) (model.Sample, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ring := m.history[id]
	if len(ring) == 0 {
		return model.Sample{}, false
	}
	target := m.now().Add(-ago)
	best := model.Sample{}
	bestDist := time.Duration(1<<63 - 1)
	for _, s := range ring {
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
	if bestDist > tol {
		return model.Sample{}, false
	}
	return best, true
}

// HistoryHours returns the age in whole hours of the oldest retained
// sample across all assets, or 0 when the ring is empty.
func (m *Market) HistoryHours() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, h := m.historyStatsLocked()
	return h
}

// historyStatsLocked returns the number of tracked assets and the age in
// whole hours of the oldest sample. Caller must hold m.mu.
func (m *Market) historyStatsLocked() (assets, hours int) {
	var oldest time.Time
	for _, ring := range m.history {
		if len(ring) > 0 && (oldest.IsZero() || ring[0].At.Before(oldest)) {
			oldest = ring[0].At
		}
		assets++
	}
	if oldest.IsZero() {
		return 0, 0
	}
	return assets, int(m.now().Sub(oldest).Hours())
}

// SeedHistory loads a previously persisted history (for example from
// Store.LoadHistory at startup). Samples are appended to whatever the ring
// already holds, deduplicated by At, and trimmed to HistoryWindow.
func (m *Market) SeedHistory(samples map[int64][]model.Sample) {
	now := m.now()
	cutoff := now.Add(-m.cfg.HistoryWindow)
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, ring := range samples {
		cur := m.history[id]
		have := make(map[time.Time]bool, len(cur))
		for _, s := range cur {
			have[s.At] = true
		}
		for _, s := range ring {
			if s.At.Before(cutoff) || have[s.At] {
				continue
			}
			cur = append(cur, s)
			have[s.At] = true
		}
		// Keep oldest-first order.
		slices.SortFunc(cur, func(a, b model.Sample) int { return a.At.Compare(b.At) })
		m.history[id] = cur
	}
}
