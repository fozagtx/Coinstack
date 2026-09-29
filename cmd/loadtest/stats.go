package main

import (
	"math"
	"sort"
	"strconv"
	"time"
)

// result is the outcome of one request.
type result struct {
	kind   string    // endpoint group, e.g. "price"
	done   bool      // false when the request never ran (run cancelled)
	status int       // HTTP status; 0 when the request failed in transport
	err    string    // transport error text
	due    time.Time // when the scheduler wanted the request sent
	start  time.Time // when a worker actually sent it
	end    time.Time // when the response body was fully read
}

// latency is measured from the scheduled send time, so time spent queued
// behind busy workers counts against the server (no coordinated omission).
func (r result) latency() time.Duration { return r.end.Sub(r.due) }

// service is measured from the moment the request left the client.
func (r result) service() time.Duration { return r.end.Sub(r.start) }

// lag is how late the request was sent relative to its schedule.
func (r result) lag() time.Duration { return r.start.Sub(r.due) }

func (r result) failed() bool { return r.status < 200 || r.status > 299 }

// percentile returns the nearest-rank p-th percentile (0 < p <= 100) of
// sorted, which must be in ascending order: the smallest value such that at
// least p % of the values are less than or equal to it. It returns 0 for an
// empty slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[n-1]
	}
	// p*n/100 rather than p/100*n keeps exact ranks exact (95*100/100 = 95);
	// the epsilon absorbs float noise such as 99.9*1000 = 99900.00000000001.
	rank := int(math.Ceil(p*float64(n)/100 - 1e-9))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// summary is the latency and error picture for a group of requests.
type summary struct {
	Count     int
	Errors    int
	ErrorRate float64
	ByStatus  map[string]int // "200", "429", "transport"
	Min       time.Duration
	Mean      time.Duration
	P50       time.Duration
	P95       time.Duration
	P99       time.Duration
	Max       time.Duration
}

// summarize computes a summary over rs using measure (latency or service
// time). Requests that never ran are ignored.
func summarize(rs []result, measure func(result) time.Duration) summary {
	s := summary{ByStatus: map[string]int{}}
	ds := make([]time.Duration, 0, len(rs))
	var total time.Duration
	for _, r := range rs {
		if !r.done {
			continue
		}
		s.Count++
		if r.failed() {
			s.Errors++
		}
		s.ByStatus[statusLabel(r.status)]++
		d := measure(r)
		ds = append(ds, d)
		total += d
	}
	if s.Count == 0 {
		return s
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	s.ErrorRate = float64(s.Errors) / float64(s.Count)
	s.Min = ds[0]
	s.Mean = total / time.Duration(len(ds))
	s.P50 = percentile(ds, 50)
	s.P95 = percentile(ds, 95)
	s.P99 = percentile(ds, 99)
	s.Max = ds[len(ds)-1]
	return s
}

func statusLabel(status int) string {
	if status == 0 {
		return "transport"
	}
	return strconv.Itoa(status)
}
