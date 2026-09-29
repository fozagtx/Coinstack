package market

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/fozagtx/coinstack/internal/cmc"
	"github.com/fozagtx/coinstack/internal/model"
)

// Quotes returns USD quotes for ids: from the snapshot when present, else
// from the on-demand cache when fetched within OnDemandTTL, else through
// one upstream call per distinct id set, shared by concurrent callers. Ids
// CMC does not know are absent (and remembered for OnDemandTTL). On
// failure it returns what it has, including expired on-demand quotes
// fetched within MaxOnDemandStale, with an error wrapping
// model.ErrUpstreamUnavailable or model.ErrBudgetExhausted inside a
// *model.RetryAfterError.
func (m *Market) Quotes(ctx context.Context, ids []int64) (map[int64]model.Quote, error) {
	out := make(map[int64]model.Quote, len(ids))
	snap := m.store.Load()
	var rest []int64
	for _, id := range ids {
		if q, ok := snap.Get(id); ok {
			out[id] = *q
		} else if id > 0 {
			rest = append(rest, id)
		}
	}
	if len(rest) == 0 {
		return out, nil
	}
	return out, lookup(ctx, m, m.quoteSrc, rest, out)
}

// Info returns metadata for ids, cached for InfoTTL and fetched like
// Quotes (coalesced, budgeted). On failure it returns whatever is cached,
// however old, with the error.
func (m *Market) Info(ctx context.Context, ids []int64) (map[int64]model.Info, error) {
	out := make(map[int64]model.Info, len(ids))
	var valid []int64
	for _, id := range ids {
		if id > 0 {
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 {
		return out, nil
	}
	return out, lookup(ctx, m, m.infoSrc, valid, out)
}

// source is one kind of on-demand lookup.
type source[V any] struct {
	kind  string // PollRun kind and singleflight key prefix
	cache *lookupCache[V]
	fetch func(ctx context.Context, ids []int64) (map[int64]V, cmc.Meta, error)
	stamp func(v V, now time.Time) V // fills FetchedAt when the client did not
}

// lookup fills out with ids from src's cache or, for the rest, from
// coalesced upstream calls of at most maxBatch ids.
func lookup[V any](ctx context.Context, m *Market, src *source[V], ids []int64, out map[int64]V) error {
	missing := src.cache.get(ids, m.now(), out)
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	missing = slices.Compact(missing)

	var err error
	for batch := range slices.Chunk(missing, maxBatch) {
		if err = ctx.Err(); err != nil {
			break
		}
		ch := m.flights.DoChan(src.kind+":"+joinIDs(batch), func() (any, error) {
			return fetchBatch(ctx, m, src, batch)
		})
		select {
		case r := <-ch:
			got, _ := r.Val.(map[int64]V)
			for id, v := range got {
				out[id] = v
			}
			if r.Err != nil && err == nil {
				err = r.Err
			}
		case <-ctx.Done():
			err = ctx.Err()
		}
		if ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		src.cache.fallback(missing, m.now(), out)
	}
	return err
}

// fetchBatch runs inside a singleflight call. The returned map is shared by
// every waiter and never modified afterwards. The upstream call is detached
// from the caller's cancellation so one impatient agent cannot fail the
// others waiting on the same flight.
func fetchBatch[V any](ctx context.Context, m *Market, src *source[V], ids []int64) (map[int64]V, error) {
	got := make(map[int64]V, len(ids))
	now := m.now()
	// A flight that just finished may have filled some of these ids.
	ids = src.cache.get(ids, now, got)
	if len(ids) == 0 {
		return got, nil
	}
	if err := m.budget.spend(now, src.kind); err != nil {
		return got, err
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upstreamTimeout)
	defer cancel()
	res, meta, err := src.fetch(cctx, ids)
	m.record(cctx, src.kind, now, meta, len(res), err)
	if err != nil {
		m.log.Warn("on-demand lookup failed", "kind", src.kind, "ids", len(ids), "err", err)
		return got, unavailable("on-demand "+src.kind, err)
	}
	fetched := m.now()
	stamped := make(map[int64]V, len(res))
	for id, v := range res {
		stamped[id] = src.stamp(v, fetched)
	}
	src.cache.put(ids, stamped, fetched)
	for id, v := range stamped {
		got[id] = v
	}
	return got, nil
}

// entry is one cached lookup result; found is false for ids CMC did not
// return (a negative entry).
type entry[V any] struct {
	val   V
	found bool
	at    time.Time
}

// lookupCache is a bounded id -> value cache with separate lifetimes for
// fresh hits, negative answers and fallback use.
type lookupCache[V any] struct {
	ttl    time.Duration // positive entries are fresh for ttl
	negTTL time.Duration // negative entries are honored for negTTL
	keep   time.Duration // expired positive entries serve as fallback until this age

	mu      sync.Mutex
	entries map[int64]entry[V]
}

func newLookupCache[V any](ttl, negTTL, keep time.Duration) *lookupCache[V] {
	return &lookupCache[V]{ttl: ttl, negTTL: negTTL, keep: max(keep, ttl), entries: make(map[int64]entry[V])}
}

// get copies fresh entries for ids into out and returns the ids that need
// an upstream call. Fresh negative entries are neither copied nor returned.
func (c *lookupCache[V]) get(ids []int64, now time.Time, out map[int64]V) (missing []int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		if _, done := out[id]; done {
			continue
		}
		e, ok := c.entries[id]
		switch {
		case ok && e.found && now.Sub(e.at) < c.ttl:
			out[id] = e.val
		case ok && !e.found && now.Sub(e.at) < c.negTTL:
		default:
			missing = append(missing, id)
		}
	}
	return missing
}

// fallback copies expired but still usable entries for ids missing from out.
func (c *lookupCache[V]) fallback(ids []int64, now time.Time, out map[int64]V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		if _, done := out[id]; done {
			continue
		}
		if e, ok := c.entries[id]; ok && e.found && now.Sub(e.at) <= c.keep {
			out[id] = e.val
		}
	}
}

// put stores got and a negative entry for every asked id missing from it,
// then trims the cache to onDemandMax entries, oldest first.
func (c *lookupCache[V]) put(asked []int64, got map[int64]V, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, v := range got {
		c.entries[id] = entry[V]{val: v, found: true, at: now}
	}
	for _, id := range asked {
		if _, ok := got[id]; !ok {
			c.entries[id] = entry[V]{at: now}
		}
	}
	if len(c.entries) <= onDemandMax {
		return
	}
	c.sweepLocked(now)
	if excess := len(c.entries) - onDemandMax; excess > 0 {
		type aged struct {
			id int64
			at time.Time
		}
		all := make([]aged, 0, len(c.entries))
		for id, e := range c.entries {
			all = append(all, aged{id, e.at})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
		for _, a := range all[:excess] {
			delete(c.entries, a.id)
		}
	}
}

// sweep drops entries that can no longer be served.
func (c *lookupCache[V]) sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(now)
}

func (c *lookupCache[V]) sweepLocked(now time.Time) {
	for id, e := range c.entries {
		age := now.Sub(e.at)
		if (e.found && age > c.keep) || (!e.found && age >= c.negTTL) {
			delete(c.entries, id)
		}
	}
}

// size returns the number of cached assets (negative entries excluded).
func (c *lookupCache[V]) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.entries {
		if e.found {
			n++
		}
	}
	return n
}

// budget limits on-demand upstream calls per rolling minute and per UTC day.
type budget struct {
	perMinute, perDay int

	mu     sync.Mutex
	recent []time.Time // start times of calls within the last minute, oldest first
	day    string
	used   int // calls on day
}

func newBudget(perMinute, perDay int) *budget {
	return &budget{perMinute: perMinute, perDay: perDay}
}

// spend takes one call from the budget, or returns a *model.RetryAfterError
// wrapping model.ErrBudgetExhausted that says when the next call fits.
func (b *budget) spend(now time.Time, kind string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d := dayKey(now); d != b.day {
		b.day, b.used = d, 0
	}
	if b.used >= b.perDay {
		return exhausted(kind, "daily", nextUTCMidnight(now).Sub(now))
	}
	cutoff := now.Add(-time.Minute)
	i := 0
	for i < len(b.recent) && !b.recent[i].After(cutoff) {
		i++
	}
	b.recent = b.recent[i:]
	if len(b.recent) >= b.perMinute {
		return exhausted(kind, "per-minute", b.recent[0].Sub(cutoff))
	}
	b.recent = append(b.recent, now)
	b.used++
	return nil
}

func exhausted(kind, which string, wait time.Duration) error {
	wait = max(time.Second, (wait + time.Second - 1).Truncate(time.Second))
	return &model.RetryAfterError{
		Err:        fmt.Errorf("on-demand %s: %s budget spent: %w", kind, which, model.ErrBudgetExhausted),
		RetryAfter: wait,
	}
}
