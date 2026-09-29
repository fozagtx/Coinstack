// Command fakecmc is a local stand-in for the CoinMarketCap Pro API. It
// serves a deterministic universe of ~3500 assets whose prices, volumes
// and ranks drift over wall-clock time, so the poller, history ring and
// scoring engine produce non-trivial results without a real API key.
package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const numAssets = 3500

func main() {
	addr := flag.String("addr", ":8181", "listen address")
	latency := flag.Duration("latency", 0, "artificial per-request latency")
	flag.Parse()

	u := newUniverse()
	srv := &server{u: u, latency: *latency}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/cryptocurrency/listings/latest", srv.listingsLatest)
	mux.HandleFunc("/v1/cryptocurrency/listings/new", srv.listingsNew)
	mux.HandleFunc("/v2/cryptocurrency/quotes/latest", srv.quotesLatest)
	mux.HandleFunc("/v1/cryptocurrency/map", srv.cryptoMap)
	mux.HandleFunc("/v2/cryptocurrency/info", srv.info)
	mux.HandleFunc("/v1/key/info", srv.keyInfo)

	slog.Info("fakecmc listening", "addr", *addr, "assets", numAssets, "latency", latency.String())
	if err := http.ListenAndServe(*addr, mux); err != nil {
		slog.Error("fakecmc stopped", "err", err)
	}
}

// server serves the universe over the CMC API shape.
type server struct {
	u       *universe
	latency time.Duration
}

// universe is the generated market plus its per-minute live view cache.
type universe struct {
	sims  []*sim
	byID  map[int64]*sim
	start time.Time

	mu      sync.Mutex
	minute  int64          // unix minute the live view was built for
	quote   map[*sim]liveQ // live price/volume per sim
	ranked  []*sim         // ranked assets by live market cap, rank 1 first
	newest  []*sim         // all assets by dateAdded, newest first
	byRankL []*sim         // ranked then unranked, for /map
}

type liveQ struct {
	price, volume, mcap float64
	rank                int
}

func newUniverse() *universe {
	now := time.Now()
	sims := generate(numAssets, 0xf4e3c2, now)
	u := &universe{
		sims:  sims,
		byID:  map[int64]*sim{},
		start: now,
	}
	for _, s := range sims {
		u.byID[s.c.id] = s
	}
	return u
}

// live computes (and caches) the market state for the current minute.
func (u *universe) live() (map[*sim]liveQ, []*sim) {
	u.mu.Lock()
	defer u.mu.Unlock()
	minute := time.Now().Unix() / 60
	if u.quote != nil && u.minute == minute {
		return u.quote, u.ranked
	}
	now := time.Now()
	hours := now.Sub(u.start).Hours()
	quote := make(map[*sim]liveQ, len(u.sims))
	for _, s := range u.sims {
		price := u.priceOf(s, now, hours, quote)
		vol := s.baseVolume * (0.75 + 0.5*noise(s.c.id^0x517cc1b7, minute))
		if s.runner {
			vol *= runnerMult(s, hours)
		}
		mcap := price * max(s.c.circulating, s.c.selfReported)
		quote[s] = liveQ{price: price, volume: max(vol, 1), mcap: mcap}
	}
	ranked := make([]*sim, 0, len(u.sims))
	var unranked []*sim
	for _, s := range u.sims {
		if s.c.ranked {
			ranked = append(ranked, s)
		} else {
			unranked = append(unranked, s)
		}
	}
	slices.SortFunc(ranked, func(a, b *sim) int {
		return cmpFloat(quote[b].mcap, quote[a].mcap)
	})
	for i, s := range ranked {
		q := quote[s]
		q.rank = i + 1
		quote[s] = q
	}
	slices.SortFunc(unranked, func(a, b *sim) int { return cmpFloat(quote[b].mcap, quote[a].mcap) })
	u.quote, u.ranked = quote, ranked
	u.byRankL = append(append([]*sim(nil), ranked...), unranked...)
	u.newest = append([]*sim(nil), u.sims...)
	slices.SortFunc(u.newest, func(a, b *sim) int {
		if d := b.c.dateAdded.Compare(a.c.dateAdded); d != 0 {
			return d
		}
		return cmpFloat(float64(b.c.id), float64(a.c.id))
	})
	u.minute = minute
	return quote, ranked
}

// priceOf returns s's price now: basePrice modulated by a slow wave and a
// per-minute jitter, times the runner growth; pegged clones track their
// parent.
func (u *universe) priceOf(s *sim, now time.Time, hours float64, quote map[*sim]liveQ) float64 {
	if s.peg != nil {
		if q, ok := quote[s.peg]; ok {
			return q.price * s.pegRatio
		}
		return u.priceOf(s.peg, now, hours, quote) * s.pegRatio
	}
	if s.stable {
		return s.basePrice * (1 + 0.0004*noise(s.c.id, now.Unix()/60))
	}
	minute := now.Unix() / 60
	wave := math.Sin(float64(now.Unix())/3600*0.35+s.phase)*0.06 +
		math.Sin(float64(now.Unix())/86400*0.7+s.phase*2)*0.10
	jitter := noise(s.c.id, minute) * s.sigma * 20
	p := s.basePrice * (1 + wave + jitter)
	if s.runner {
		p *= runnerMult(s, hours)
	}
	return max(p, s.basePrice*0.01)
}

func runnerMult(s *sim, hours float64) float64 {
	return math.Min(1e4, math.Exp(s.runRate*hours))
}

// noise is a deterministic pseudo-random in [-1,1] seeded by (id, bucket).
func noise(id, bucket int64) float64 {
	r := rand.New(rand.NewPCG(uint64(id), uint64(bucket)+0x9e3779b97f4a7c15))
	return r.NormFloat64()
}

// ---- HTTP layer ----

func (s *server) serve(w http.ResponseWriter, r *http.Request, credits int, data any) {
	if s.latency > 0 {
		t := time.NewTimer(s.latency)
		select {
		case <-r.Context().Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"data": data,
		"status": map[string]any{
			"timestamp":     time.Now().UTC().Format(time.RFC3339Nano),
			"error_code":    0,
			"error_message": nil,
			"elapsed":       1,
			"credit_count":  credits,
		},
	})
}

func pageParams(r *http.Request) (start, limit int) {
	start, _ = strconv.Atoi(r.URL.Query().Get("start"))
	limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	if start < 1 {
		start = 1
	}
	if limit <= 0 {
		limit = 100
	}
	return start, limit
}

func pageOf(list []*sim, start, limit int) []*sim {
	if start-1 >= len(list) {
		return nil
	}
	end := min(start-1+limit, len(list))
	return list[start-1 : end]
}

func ceilDiv(n, d int) int { return (n + d - 1) / d }

func (s *server) listingsLatest(w http.ResponseWriter, r *http.Request) {
	start, limit := pageParams(r)
	_, ranked := s.u.live()
	assets := make([]map[string]any, 0, limit)
	for _, sm := range pageOf(ranked, start, limit) {
		assets = append(assets, s.assetJSON(sm, true))
	}
	s.serve(w, r, ceilDiv(len(assets), 200), assets)
}

func (s *server) listingsNew(w http.ResponseWriter, r *http.Request) {
	start, limit := pageParams(r)
	s.u.live()
	assets := make([]map[string]any, 0, limit)
	for _, sm := range pageOf(s.u.newest, start, limit) {
		assets = append(assets, s.assetJSON(sm, true))
	}
	s.serve(w, r, ceilDiv(len(assets), 200), assets)
}

func parseIDs(r *http.Request) []int64 {
	var ids []int64
	for _, p := range strings.Split(r.URL.Query().Get("id"), ",") {
		if n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil {
			ids = append(ids, n)
		}
	}
	return ids
}

func (s *server) quotesLatest(w http.ResponseWriter, r *http.Request) {
	ids := parseIDs(r)
	s.u.live()
	data := map[string]any{}
	for _, id := range ids {
		if sm, ok := s.u.byID[id]; ok {
			data[strconv.FormatInt(id, 10)] = s.assetJSON(sm, true)
		}
	}
	s.serve(w, r, ceilDiv(len(ids), 100), data)
}

func (s *server) cryptoMap(w http.ResponseWriter, r *http.Request) {
	start, limit := pageParams(r)
	s.u.live()
	var out []map[string]any
	for _, sm := range pageOf(s.u.byRankL, start, limit) {
		q := s.u.quote[sm]
		out = append(out, map[string]any{
			"id":                    sm.c.id,
			"name":                  sm.c.name,
			"symbol":                sm.c.symbol,
			"slug":                  sm.c.slug,
			"rank":                  q.rank,
			"is_active":             1,
			"first_historical_data": sm.c.dateAdded.UTC().Format(time.RFC3339),
			"platform":              platformJSON(sm.c.plat),
		})
	}
	s.serve(w, r, 1, out)
}

func (s *server) info(w http.ResponseWriter, r *http.Request) {
	ids := parseIDs(r)
	s.u.live()
	data := map[string]any{}
	for _, id := range ids {
		sm, ok := s.u.byID[id]
		if !ok {
			continue
		}
		cat := "coin"
		if sm.c.plat != nil {
			cat = "token"
		}
		data[strconv.FormatInt(id, 10)] = map[string]any{
			"id":          sm.c.id,
			"name":        sm.c.name,
			"symbol":      sm.c.symbol,
			"slug":        sm.c.slug,
			"category":    cat,
			"description": sm.c.description,
			"tags":        sm.c.tags,
			"date_added":  sm.c.dateAdded.UTC().Format(time.RFC3339),
			"urls":        map[string]any{"website": []string{sm.c.website}},
			"platform":    platformJSON(sm.c.plat),
		}
	}
	s.serve(w, r, ceilDiv(len(ids), 100), data)
}

func (s *server) keyInfo(w http.ResponseWriter, r *http.Request) {
	s.serve(w, r, 0, map[string]any{
		"plan": map[string]any{
			"credit_limit_monthly":       3330000,
			"credit_limit_monthly_reset": "In 12 days, 4 hours",
			"rate_limit_minute":          300,
		},
		"usage": map[string]any{
			"current_day":   map[string]any{"credits_used": 4123},
			"current_month": map[string]any{"credits_used": 90210, "credits_left": 3239790},
		},
	})
}

func platformJSON(p *platform) any {
	if p == nil {
		return nil
	}
	return map[string]any{"id": p.id, "name": p.name, "symbol": p.symbol, "slug": p.slug}
}

// assetJSON renders one asset in the listings/quotes shape the real client
// parses (see internal/cmc/client.go).
func (s *server) assetJSON(sm *sim, withTags bool) map[string]any {
	q := s.u.quote[sm]
	pct := func(ref float64) *float64 {
		if ref <= 0 {
			return nil
		}
		v := (q.price/ref - 1) * 100
		return &v
	}
	var maxSupply any
	if sm.c.maxSupply > 0 {
		maxSupply = sm.c.maxSupply
	}
	var rank any
	if q.rank > 0 {
		rank = q.rank
	}
	out := map[string]any{
		"id":                 sm.c.id,
		"name":               sm.c.name,
		"symbol":             sm.c.symbol,
		"slug":               sm.c.slug,
		"cmc_rank":           rank,
		"circulating_supply": sm.c.circulating,
		"total_supply":       sm.c.total,
		"max_supply":         maxSupply,
		"date_added":         sm.c.dateAdded.UTC().Format(time.RFC3339),
		"platform":           platformJSON(sm.c.plat),
		"quote": map[string]any{"USD": map[string]any{
			"price":              q.price,
			"volume_24h":         q.volume,
			"market_cap":         q.mcap,
			"percent_change_1h":  pct(sm.ref1h),
			"percent_change_24h": pct(sm.ref24h),
			"percent_change_7d":  pct(sm.ref7d),
			"last_updated":       time.Now().UTC().Format(time.RFC3339),
		}},
	}
	if withTags {
		out["tags"] = sm.c.tags
	}
	return out
}
