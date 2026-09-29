package cache

import (
	"sync"
	"testing"
	"time"

	"github.com/fozagtx/coinstack/internal/model"
)

func TestNewSnapshotOrdersByRankThenMarketCap(t *testing.T) {
	now := time.Now()
	snap := NewSnapshot([]model.Quote{
		{ID: 3, Rank: 0, MarketCap: 10},
		{ID: 2, Rank: 2},
		{ID: 1, Rank: 1},
		{ID: 4, Rank: 0, MarketCap: 20},
	}, now)
	var got []int64
	for _, q := range snap.ByRank {
		got = append(got, q.ID)
	}
	want := []int64{1, 2, 4, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if snap.Len() != 4 {
		t.Fatalf("Len = %d", snap.Len())
	}
}

func TestNewSnapshotKeepsNewestDuplicate(t *testing.T) {
	t0 := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	snap := NewSnapshot([]model.Quote{
		{ID: 1, Price: 2, LastUpdated: t0.Add(time.Minute)},
		{ID: 1, Price: 1, LastUpdated: t0},
	}, t0)
	q, ok := snap.Get(1)
	if !ok || q.Price != 2 {
		t.Fatalf("got %+v", q)
	}
	if !snap.Oldest().Equal(t0.Add(time.Minute)) || !snap.Newest().Equal(t0.Add(time.Minute)) {
		t.Fatalf("oldest/newest wrong")
	}
}

func TestStoreZeroValueAndConcurrentPublish(t *testing.T) {
	var s Store
	if s.Load() == nil || s.Load().Len() != 0 {
		t.Fatal("zero Store must return an empty snapshot")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			s.Publish(NewSnapshot([]model.Quote{{ID: int64(i + 1), Rank: 1}}, time.Now()))
		}(i)
		go func() {
			defer wg.Done()
			_ = s.Load().Len()
		}()
	}
	wg.Wait()
	if s.Load().Len() != 1 {
		t.Fatal("expected one asset after publishes")
	}
}
