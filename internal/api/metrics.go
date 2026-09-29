package api

import (
	"math"
	"strconv"
	"sync/atomic"
	"time"
)

// latencyBoundsMS are the upper bounds of the latency histogram buckets,
// in milliseconds; one more bucket catches everything slower.
var latencyBoundsMS = [...]float64{0.5, 1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000}

// unmatchedEndpoint names requests that matched no route.
const unmatchedEndpoint = "unmatched"

// endpointMetrics are lock-free counters for one route.
type endpointMetrics struct {
	count   atomic.Int64
	totalUS atomic.Int64
	maxUS   atomic.Int64
	buckets [len(latencyBoundsMS) + 1]atomic.Int64
}

// metrics counts requests by status and latency by endpoint for
// /v1/health. The endpoint map is fixed at construction, so reads and
// updates need no lock.
type metrics struct {
	total     atomic.Int64
	byStatus  [600]atomic.Int64
	endpoints map[string]*endpointMetrics
}

func newMetrics(endpoints []string) *metrics {
	m := &metrics{endpoints: make(map[string]*endpointMetrics, len(endpoints)+1)}
	for _, e := range endpoints {
		m.endpoints[e] = new(endpointMetrics)
	}
	m.endpoints[unmatchedEndpoint] = new(endpointMetrics)
	return m
}

// observe records one finished request and returns the endpoint name it
// was counted under (the route pattern, or "unmatched").
func (m *metrics) observe(pattern string, status int, d time.Duration) string {
	em, ok := m.endpoints[pattern]
	if !ok {
		pattern = unmatchedEndpoint
		em = m.endpoints[pattern]
	}
	m.total.Add(1)
	if status >= 100 && status < len(m.byStatus) {
		m.byStatus[status].Add(1)
	}
	us := d.Microseconds()
	em.count.Add(1)
	em.totalUS.Add(us)
	for {
		cur := em.maxUS.Load()
		if us <= cur || em.maxUS.CompareAndSwap(cur, us) {
			break
		}
	}
	msf := float64(us) / 1000
	i := 0
	for i < len(latencyBoundsMS) && msf > latencyBoundsMS[i] {
		i++
	}
	em.buckets[i].Add(1)
	return pattern
}

// endpointSummary is the latency summary of one endpoint in /v1/health.
// Percentiles are histogram bucket upper bounds.
type endpointSummary struct {
	Requests int64   `json:"requests"`
	AvgMS    float64 `json:"avg_ms"`
	P50MS    float64 `json:"p50_ms"`
	P95MS    float64 `json:"p95_ms"`
	P99MS    float64 `json:"p99_ms"`
	MaxMS    float64 `json:"max_ms"`
}

// summary returns request totals, counts by status code and per-endpoint
// latency for endpoints that served at least one request.
func (m *metrics) summary() (total int64, byStatus map[string]int64, endpoints map[string]endpointSummary) {
	byStatus = make(map[string]int64)
	for code := range m.byStatus {
		if n := m.byStatus[code].Load(); n > 0 {
			byStatus[strconv.Itoa(code)] = n
		}
	}
	endpoints = make(map[string]endpointSummary)
	for name, em := range m.endpoints {
		n := em.count.Load()
		if n == 0 {
			continue
		}
		var counts [len(latencyBoundsMS) + 1]int64
		var seen int64
		for i := range em.buckets {
			counts[i] = em.buckets[i].Load()
			seen += counts[i]
		}
		maxMS := float64(em.maxUS.Load()) / 1000
		endpoints[name] = endpointSummary{
			Requests: n,
			AvgMS:    round3(float64(em.totalUS.Load()) / 1000 / float64(n)),
			P50MS:    quantile(counts[:], seen, 0.50, maxMS),
			P95MS:    quantile(counts[:], seen, 0.95, maxMS),
			P99MS:    quantile(counts[:], seen, 0.99, maxMS),
			MaxMS:    round3(maxMS),
		}
	}
	return m.total.Load(), byStatus, endpoints
}

// quantile returns the upper bound of the bucket holding quantile q,
// capped at the observed maximum.
func quantile(counts []int64, total int64, q, maxMS float64) float64 {
	if total == 0 {
		return 0
	}
	rank := int64(math.Ceil(q * float64(total)))
	var cum int64
	for i, c := range counts {
		cum += c
		if cum >= rank {
			if i < len(latencyBoundsMS) {
				return round3(min(latencyBoundsMS[i], maxMS))
			}
			break
		}
	}
	return round3(maxMS)
}

func round3(f float64) float64 { return finite(math.Round(f*1000) / 1000) }
