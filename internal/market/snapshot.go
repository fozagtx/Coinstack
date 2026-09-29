// snapshot.go holds the latest published market snapshot. The poller
// builds a new Snapshot and publishes it with one atomic pointer swap, so
// HTTP handlers read without locks and never see a half-built snapshot.
package market

import (
	"sort"
	"sync/atomic"
	"time"

	"github.com/fozagtx/coinstack/internal/model"
)

// Snapshot is an immutable view of the top-N assets. Never modify a
// Snapshot or the Quotes it points to after it has been published.
type Snapshot struct {
	// Quotes holds every asset in the snapshot, keyed by CMC id.
	Quotes map[int64]*model.Quote
	// ByRank holds the same quotes ordered by rank ascending; unranked
	// assets (rank 0) come last, ordered by market cap descending.
	ByRank []*model.Quote
	// PublishedAt is when this snapshot was published.
	PublishedAt time.Time
}

// Oldest returns the earliest LastUpdated among the snapshot's quotes,
// or the zero time when the snapshot is empty.
func (s *Snapshot) Oldest() time.Time {
	var oldest time.Time
	for _, q := range s.ByRank {
		if oldest.IsZero() || q.LastUpdated.Before(oldest) {
			oldest = q.LastUpdated
		}
	}
	return oldest
}

// Newest returns the latest LastUpdated among the snapshot's quotes, or the
// zero time when the snapshot is empty.
func (s *Snapshot) Newest() time.Time {
	var newest time.Time
	for _, q := range s.ByRank {
		if q.LastUpdated.After(newest) {
			newest = q.LastUpdated
		}
	}
	return newest
}

// Get returns the quote for id, if present.
func (s *Snapshot) Get(id int64) (*model.Quote, bool) {
	q, ok := s.Quotes[id]
	return q, ok
}

// Len returns the number of assets in the snapshot.
func (s *Snapshot) Len() int { return len(s.ByRank) }

// NewSnapshot builds a snapshot from quotes. It copies each quote, so the
// caller may reuse the slice afterwards. When ids repeat, the quote with the
// latest LastUpdated wins.
func NewSnapshot(quotes []model.Quote, publishedAt time.Time) *Snapshot {
	m := make(map[int64]*model.Quote, len(quotes))
	for i := range quotes {
		q := quotes[i]
		if prev, ok := m[q.ID]; ok && prev.LastUpdated.After(q.LastUpdated) {
			continue
		}
		m[q.ID] = &q
	}
	byRank := make([]*model.Quote, 0, len(m))
	for _, q := range m {
		byRank = append(byRank, q)
	}
	sort.Slice(byRank, func(i, j int) bool {
		a, b := byRank[i], byRank[j]
		switch {
		case a.Rank > 0 && b.Rank > 0:
			if a.Rank != b.Rank {
				return a.Rank < b.Rank
			}
		case a.Rank > 0:
			return true
		case b.Rank > 0:
			return false
		}
		if a.MarketCap != b.MarketCap {
			return a.MarketCap > b.MarketCap
		}
		return a.ID < b.ID
	})
	return &Snapshot{Quotes: m, ByRank: byRank, PublishedAt: publishedAt}
}

var empty = &Snapshot{Quotes: map[int64]*model.Quote{}}

// Store holds the current snapshot. The zero value is ready to use and
// returns an empty snapshot until the first Publish.
type Store struct {
	p atomic.Pointer[Snapshot]
}

// Load returns the current snapshot; never nil.
func (s *Store) Load() *Snapshot {
	if snap := s.p.Load(); snap != nil {
		return snap
	}
	return empty
}

// Publish atomically replaces the current snapshot.
func (s *Store) Publish(snap *Snapshot) {
	s.p.Store(snap)
}
