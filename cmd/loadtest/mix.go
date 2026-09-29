package main

import (
	"fmt"
	"math/rand/v2"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// defaultAssets are liquid, unambiguous top assets. Every one of them sits in
// the top-N cache, so the default mix measures the cached path.
var defaultAssets = []string{
	"BTC", "ETH", "SOL", "XRP", "BNB", "DOGE", "ADA", "TRX", "AVAX", "LINK",
	"DOT", "LTC", "BCH", "XLM", "NEAR",
}

// defaultResolveQueries mix symbols, names and slugs, as agents send them.
var defaultResolveQueries = []string{
	"bitcoin", "ethereum", "Solana", "SOL", "dogecoin", "Cardano", "chainlink",
	"xrp", "avalanche", "BNB", "Litecoin", "polkadot",
}

// defaultWeights is the request mix, in relative weights.
var defaultWeights = map[string]int{
	"price":       30, // one asset
	"price_multi": 15, // 3-5 assets in one call
	"asset":       15,
	"movers":      20,
	"compare":     10,
	"resolve":     10,
	"new_tokens":  0, // opt in with -mix new_tokens=5
}

// kindOrder fixes the report order.
var kindOrder = []string{"price", "price_multi", "asset", "movers", "compare", "resolve", "new_tokens"}

// mix picks weighted request kinds and builds their URLs. It is not safe for
// concurrent use; the dispatcher is its only caller.
type mix struct {
	rng     *rand.Rand
	assets  []string
	queries []string
	kinds   []string
	cum     []int // cumulative weights, parallel to kinds
	total   int
}

func newMix(seed uint64, weights map[string]int, assets, queries []string) (*mix, error) {
	if len(assets) < 5 {
		return nil, fmt.Errorf("need at least 5 assets for the mix, got %d", len(assets))
	}
	if len(queries) == 0 {
		queries = defaultResolveQueries
	}
	m := &mix{rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), assets: assets, queries: queries}
	for _, k := range kindOrder {
		w := weights[k]
		if w <= 0 {
			continue
		}
		m.total += w
		m.kinds = append(m.kinds, k)
		m.cum = append(m.cum, m.total)
	}
	if m.total == 0 {
		return nil, fmt.Errorf("the request mix has no positive weights")
	}
	return m, nil
}

// next returns the job for request i.
func (m *mix) next(int) job {
	x := m.rng.IntN(m.total)
	k := m.kinds[sort.SearchInts(m.cum, x+1)]
	return job{kind: k, path: m.path(k)}
}

func (m *mix) path(kind string) string {
	q := url.Values{}
	switch kind {
	case "price":
		q.Set("asset", m.pick())
		return "/v1/price?" + q.Encode()
	case "price_multi":
		q.Set("asset", strings.Join(m.pickN(3+m.rng.IntN(3)), ","))
		return "/v1/price?" + q.Encode()
	case "asset":
		q.Set("asset", m.pick())
		return "/v1/asset?" + q.Encode()
	case "movers":
		q.Set("window", []string{"1h", "24h", "7d"}[m.rng.IntN(3)])
		q.Set("direction", []string{"up", "down"}[m.rng.IntN(2)])
		q.Set("limit", strconv.Itoa([]int{5, 10, 20}[m.rng.IntN(3)]))
		if m.rng.IntN(2) == 0 {
			q.Set("min_volume", "1000000")
		}
		return "/v1/movers?" + q.Encode()
	case "compare":
		q.Set("assets", strings.Join(m.pickN(2+m.rng.IntN(4)), ","))
		return "/v1/compare?" + q.Encode()
	case "resolve":
		q.Set("query", m.queries[m.rng.IntN(len(m.queries))])
		return "/v1/resolve?" + q.Encode()
	case "new_tokens":
		q.Set("days", strconv.Itoa([]int{1, 7, 30}[m.rng.IntN(3)]))
		return "/v1/new-tokens?" + q.Encode()
	}
	panic("unknown request kind " + kind)
}

func (m *mix) pick() string { return m.assets[m.rng.IntN(len(m.assets))] }

// pickN returns n distinct assets.
func (m *mix) pickN(n int) []string {
	if n > len(m.assets) {
		n = len(m.assets)
	}
	idx := m.rng.Perm(len(m.assets))[:n]
	out := make([]string, n)
	for i, j := range idx {
		out[i] = m.assets[j]
	}
	return out
}

// parseWeights parses "price=30,movers=20" on top of the defaults.
func parseWeights(s string) (map[string]int, error) {
	w := make(map[string]int, len(defaultWeights))
	for k, v := range defaultWeights {
		w[k] = v
	}
	if strings.TrimSpace(s) == "" {
		return w, nil
	}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return nil, fmt.Errorf("mix entry %q: want kind=weight", part)
		}
		if _, known := defaultWeights[k]; !known {
			return nil, fmt.Errorf("mix entry %q: unknown kind (use %s)", part, strings.Join(kindOrder, ", "))
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("mix entry %q: weight must be a non-negative integer", part)
		}
		w[k] = n
	}
	return w, nil
}

// splitList splits a comma-separated flag value.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
