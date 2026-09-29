package api

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// keyLimiter holds one token bucket per API key id. Buckets idle for longer
// than idleTTL are evicted, and the map never holds more than maxKeys.
type keyLimiter struct {
	mu        sync.Mutex
	buckets   map[int64]*bucket
	maxKeys   int
	idleTTL   time.Duration
	lastSweep time.Time
}

type bucket struct {
	lim       *rate.Limiter
	perMinute int
	lastSeen  time.Time
}

func newKeyLimiter(maxKeys int, idleTTL time.Duration) *keyLimiter {
	return &keyLimiter{buckets: make(map[int64]*bucket), maxKeys: maxKeys, idleTTL: idleTTL}
}

// burstFor sizes a bucket: about ten seconds' worth of requests, at least
// five (but never more than the per-minute limit itself).
func burstFor(perMinute int) int {
	return max(perMinute/6, min(5, perMinute), 1)
}

// allow takes one token from key id's bucket, creating or resizing it for
// a limit of perMinute requests per minute. It reports the whole tokens
// left and, when the bucket is empty, how long until the next token.
func (k *keyLimiter) allow(id int64, perMinute int, now time.Time) (ok bool, remaining int, wait time.Duration) {
	limit := rate.Limit(float64(perMinute) / 60)
	k.mu.Lock()
	defer k.mu.Unlock()
	if now.Sub(k.lastSweep) >= k.idleTTL {
		k.sweep(now)
	}
	b := k.buckets[id]
	switch {
	case b == nil:
		if len(k.buckets) >= k.maxKeys {
			k.sweep(now)
			if len(k.buckets) >= k.maxKeys {
				k.evictOldest()
			}
		}
		b = &bucket{lim: rate.NewLimiter(limit, burstFor(perMinute)), perMinute: perMinute}
		k.buckets[id] = b
	case b.perMinute != perMinute:
		b.lim.SetLimitAt(now, limit)
		b.lim.SetBurstAt(now, burstFor(perMinute))
		b.perMinute = perMinute
	}
	b.lastSeen = now
	if b.lim.AllowN(now, 1) {
		return true, int(b.lim.TokensAt(now)), 0
	}
	missing := 1 - b.lim.TokensAt(now)
	return false, 0, time.Duration(missing / float64(limit) * float64(time.Second))
}

// sweep drops buckets idle for longer than idleTTL. The caller holds mu.
func (k *keyLimiter) sweep(now time.Time) {
	k.lastSweep = now
	for id, b := range k.buckets {
		if now.Sub(b.lastSeen) > k.idleTTL {
			delete(k.buckets, id)
		}
	}
}

// evictOldest drops the least recently used bucket. The caller holds mu.
func (k *keyLimiter) evictOldest() {
	var oldestID int64
	var oldest time.Time
	first := true
	for id, b := range k.buckets {
		if first || b.lastSeen.Before(oldest) {
			oldestID, oldest, first = id, b.lastSeen, false
		}
	}
	if !first {
		delete(k.buckets, oldestID)
	}
}

// size returns the number of tracked buckets.
func (k *keyLimiter) size() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.buckets)
}
