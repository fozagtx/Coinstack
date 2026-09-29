package market

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/fozagtx/coinstack/internal/cmc"
	"github.com/fozagtx/coinstack/internal/model"
)

// marketAPI mirrors api.Market (internal/api/deps.go). The api package is
// not imported so these tests do not depend on the HTTP layer compiling.
type marketAPI interface {
	Snapshot() *Snapshot
	Quotes(ctx context.Context, ids []int64) (map[int64]model.Quote, error)
	Info(ctx context.Context, ids []int64) (map[int64]model.Info, error)
	NewListings(ctx context.Context, days int) ([]model.Quote, error)
	Status() model.MarketStatus
}

var _ marketAPI = (*Market)(nil)

var t0 = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// clock is a manually advanced time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeUp is an in-memory cmc.Upstream with call counters, injectable
// failures and latency. Every call costs one credit unless credits is set.
type fakeUp struct {
	mu        sync.Mutex
	clock     func() time.Time
	ranked    []model.Quote // listings; index 0 is rank 1
	pageOver  map[int][]model.Quote
	universe  map[int64]model.Quote
	infos     map[int64]model.Info
	mapList   []model.MapEntry
	newList   []model.Quote
	key       model.KeyUsage
	credits   int
	delay     time.Duration // latency of QuotesLatest and Info
	failPage  map[int]error // by listings start
	errQuotes error
	errInfo   error
	errMap    error
	errNew    error
	errKey    error

	calls    map[string]int
	starts   []int     // listings start of every call
	quoteIDs [][]int64 // ids of every QuotesLatest call
}

func newFakeUp(clk func() time.Time) *fakeUp {
	return &fakeUp{
		clock:    clk,
		universe: map[int64]model.Quote{},
		infos:    map[int64]model.Info{},
		failPage: map[int]error{},
		pageOver: map[int][]model.Quote{},
		calls:    map[string]int{},
		credits:  1,
	}
}

func (f *fakeUp) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeUp) set(fn func(f *fakeUp)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeUp) meta() cmc.Meta { return cmc.Meta{CreditCount: f.credits, HTTPStatus: 200} }

func (f *fakeUp) ListingsLatest(ctx context.Context, start, limit int) ([]model.Quote, cmc.Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["listings"]++
	f.starts = append(f.starts, start)
	if err := f.failPage[start]; err != nil {
		return nil, cmc.Meta{}, err
	}
	if qs, ok := f.pageOver[start]; ok {
		return append([]model.Quote(nil), qs...), f.meta(), nil
	}
	var out []model.Quote
	for i := start - 1; i < start-1+limit && i < len(f.ranked); i++ {
		out = append(out, f.ranked[i])
	}
	return out, f.meta(), nil
}

func (f *fakeUp) QuotesLatest(ctx context.Context, ids []int64) (map[int64]model.Quote, cmc.Meta, error) {
	f.mu.Lock()
	f.calls["quotes"]++
	f.quoteIDs = append(f.quoteIDs, append([]int64(nil), ids...))
	delay, err := f.delay, f.errQuotes
	f.mu.Unlock()
	if err := sleepCtx(ctx, delay); err != nil {
		return nil, cmc.Meta{}, err
	}
	if err != nil {
		return nil, cmc.Meta{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]model.Quote{}
	for _, id := range ids {
		if q, ok := f.universe[id]; ok {
			out[id] = q
		}
	}
	return out, f.meta(), nil
}

func (f *fakeUp) Info(ctx context.Context, ids []int64) (map[int64]model.Info, cmc.Meta, error) {
	f.mu.Lock()
	f.calls["info"]++
	delay, err := f.delay, f.errInfo
	f.mu.Unlock()
	if err := sleepCtx(ctx, delay); err != nil {
		return nil, cmc.Meta{}, err
	}
	if err != nil {
		return nil, cmc.Meta{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]model.Info{}
	for _, id := range ids {
		if in, ok := f.infos[id]; ok {
			out[id] = in
		}
	}
	return out, f.meta(), nil
}

func (f *fakeUp) Map(ctx context.Context, start, limit int) ([]model.MapEntry, cmc.Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["map"]++
	if f.errMap != nil {
		return nil, cmc.Meta{}, f.errMap
	}
	var out []model.MapEntry
	for i := start - 1; i < start-1+limit && i < len(f.mapList); i++ {
		out = append(out, f.mapList[i])
	}
	return out, f.meta(), nil
}

func (f *fakeUp) ListingsNew(ctx context.Context, start, limit int) ([]model.Quote, cmc.Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["new"]++
	if f.errNew != nil {
		return nil, cmc.Meta{}, f.errNew
	}
	return append([]model.Quote(nil), f.newList...), f.meta(), nil
}

func (f *fakeUp) KeyInfo(ctx context.Context) (model.KeyUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["keyinfo"]++
	if f.errKey != nil {
		return model.KeyUsage{}, f.errKey
	}
	return f.key, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// assets returns n ranked quotes with ids 1001.., all updated at updated.
func assets(n int, updated time.Time) []model.Quote {
	qs := make([]model.Quote, n)
	for i := range qs {
		qs[i] = asset(int64(1001+i), i+1, updated)
	}
	return qs
}

func asset(id int64, rank int, updated time.Time) model.Quote {
	return model.Quote{
		ID:          id,
		Symbol:      fmt.Sprintf("A%d", id),
		Name:        fmt.Sprintf("Asset %d", id),
		Rank:        rank,
		Price:       float64(id),
		MarketCap:   float64(1e9 / rank),
		LastUpdated: updated,
	}
}

// restamp returns qs with LastUpdated set to updated.
func restamp(qs []model.Quote, updated time.Time) []model.Quote {
	out := make([]model.Quote, len(qs))
	for i, q := range qs {
		q.LastUpdated = updated
		out[i] = q
	}
	return out
}

// recorder collects callbacks from a Market.
type recorder struct {
	mu        sync.Mutex
	runs      []model.PollRun
	snapshots [][]model.Quote
	maps      [][]model.MapEntry
}

func (r *recorder) kinds() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	for _, run := range r.runs {
		out[run.Kind]++
	}
	return out
}

func (r *recorder) lastSnapshot() []model.Quote {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.snapshots) == 0 {
		return nil
	}
	return r.snapshots[len(r.snapshots)-1]
}

type env struct {
	clk *clock
	up  *fakeUp
	rec *recorder
	m   *Market
}

// newEnv builds a Market over a fake upstream and a manual clock. tweak may
// adjust the config before New.
func newEnv(t *testing.T, tweak func(*Config)) *env {
	t.Helper()
	clk := newClock(t0)
	up := newFakeUp(clk.Now)
	rec := &recorder{}
	cfg := Config{
		Now:    clk.Now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnPollRun: func(run model.PollRun) {
			rec.mu.Lock()
			rec.runs = append(rec.runs, run)
			rec.mu.Unlock()
		},
		OnSnapshot: func(qs []model.Quote) {
			rec.mu.Lock()
			rec.snapshots = append(rec.snapshots, qs)
			rec.mu.Unlock()
		},
		OnMap: func(entries []model.MapEntry, _ time.Time) {
			rec.mu.Lock()
			rec.maps = append(rec.maps, entries)
			rec.mu.Unlock()
		},
	}
	if tweak != nil {
		tweak(&cfg)
	}
	return &env{clk: clk, up: up, rec: rec, m: New(up, cfg)}
}

func snapshotIDs(s *Snapshot) []int64 {
	ids := make([]int64, 0, s.Len())
	for _, q := range s.ByRank {
		ids = append(ids, q.ID)
	}
	return ids
}

func universeOf(qs ...model.Quote) map[int64]model.Quote {
	m := make(map[int64]model.Quote, len(qs))
	for _, q := range qs {
		m[q.ID] = q
	}
	return maps.Clone(m)
}

func TestPollOncePublishesSnapshotAndHistory(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.TopN = 4
		c.PageSize = 2
		c.HistoryBucket = time.Hour
	})
	e.up.ranked = assets(4, t0)
	if err := e.m.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := e.m.Snapshot()
	if snap.Len() != 4 {
		t.Fatalf("snapshot has %d assets", snap.Len())
	}
	if got := snapshotIDs(snap); got[0] != 1001 || got[3] != 1004 {
		t.Fatalf("order = %v", got)
	}
	if h := e.m.History(1001); len(h) != 1 || h[0].Rank != 1 {
		t.Fatalf("history = %+v", h)
	}
	if s, ok := e.m.RankAt(1001, 0); !ok || s.Rank != 1 {
		t.Fatalf("RankAt = %+v, %v", s, ok)
	}

	// A second poll inside the same bucket adds no sample; after advancing
	// past the bucket it does.
	e.clk.Advance(30 * time.Minute)
	if err := e.m.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := e.m.History(1001); len(h) != 1 {
		t.Fatalf("history len = %d", len(h))
	}
	e.clk.Advance(31 * time.Minute)
	e.up.ranked = restamp(e.up.ranked, e.clk.Now())
	if err := e.m.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := e.m.History(1001); len(h) != 2 {
		t.Fatalf("history len = %d", len(h))
	}
	if st := e.m.Status(); st.HistoryAssets != 4 || st.HistoryHours != 1 {
		t.Fatalf("status history = %d assets, %d hours", st.HistoryAssets, st.HistoryHours)
	}
}
