package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func ds(ms ...int) []time.Duration {
	out := make([]time.Duration, len(ms))
	for i, m := range ms {
		out[i] = time.Duration(m) * time.Millisecond
	}
	return out
}

func TestPercentileNearestRank(t *testing.T) {
	hundred := make([]time.Duration, 100)
	for i := range hundred {
		hundred[i] = time.Duration(i+1) * time.Millisecond
	}
	thousand := make([]time.Duration, 1000)
	for i := range thousand {
		thousand[i] = time.Duration(i+1) * time.Millisecond
	}
	cases := []struct {
		name   string
		sorted []time.Duration
		p      float64
		want   time.Duration
	}{
		{"empty", nil, 95, 0},
		{"single p50", ds(7), 50, 7 * time.Millisecond},
		{"single p99", ds(7), 99, 7 * time.Millisecond},
		{"1..100 p50", hundred, 50, 50 * time.Millisecond},
		{"1..100 p95", hundred, 95, 95 * time.Millisecond},
		{"1..100 p99", hundred, 99, 99 * time.Millisecond},
		{"1..100 p100", hundred, 100, 100 * time.Millisecond},
		{"1..100 p0", hundred, 0, 1 * time.Millisecond},
		{"1..1000 p99.9 (float noise)", thousand, 99.9, 999 * time.Millisecond},
		{"1..1000 p95", thousand, 95, 950 * time.Millisecond},
		// Nearest rank: ceil(0.95*10) = 10th value, ceil(0.5*10) = 5th.
		{"ten p95", ds(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 95, 10 * time.Millisecond},
		{"ten p50", ds(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 50, 5 * time.Millisecond},
		// ceil(0.95*20) = 19th value: one slow outlier out of 20 stays above p95.
		{"one outlier in 20", ds(1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 500), 95, 1 * time.Millisecond},
		{"two outliers in 20", ds(1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 500, 500), 95, 500 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := percentile(c.sorted, c.p); got != c.want {
				t.Fatalf("percentile(p=%v) = %v, want %v", c.p, got, c.want)
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	mk := func(status int, latMs int) result {
		return result{kind: "price", done: true, status: status, due: base, start: base, end: base.Add(time.Duration(latMs) * time.Millisecond)}
	}
	rs := []result{mk(200, 30), mk(200, 10), mk(404, 20), mk(0, 40), {kind: "price"} /* never ran */}
	s := summarize(rs, result.latency)
	if s.Count != 4 || s.Errors != 2 {
		t.Fatalf("count=%d errors=%d, want 4 and 2", s.Count, s.Errors)
	}
	if s.ErrorRate != 0.5 {
		t.Fatalf("error rate = %v, want 0.5", s.ErrorRate)
	}
	if s.ByStatus["200"] != 2 || s.ByStatus["404"] != 1 || s.ByStatus["transport"] != 1 {
		t.Fatalf("by status = %v", s.ByStatus)
	}
	if s.Min != 10*time.Millisecond || s.Max != 40*time.Millisecond || s.P50 != 20*time.Millisecond || s.Mean != 25*time.Millisecond {
		t.Fatalf("min=%v p50=%v mean=%v max=%v", s.Min, s.P50, s.Mean, s.Max)
	}
	if empty := summarize(nil, result.latency); empty.Count != 0 || empty.P95 != 0 {
		t.Fatalf("empty summary = %+v", empty)
	}
}

func TestLatencyIncludesQueueing(t *testing.T) {
	due := time.Unix(1_700_000_000, 0)
	r := result{due: due, start: due.Add(30 * time.Millisecond), end: due.Add(50 * time.Millisecond)}
	if r.latency() != 50*time.Millisecond || r.service() != 20*time.Millisecond || r.lag() != 30*time.Millisecond {
		t.Fatalf("latency=%v service=%v lag=%v", r.latency(), r.service(), r.lag())
	}
}

func TestScheduleMath(t *testing.T) {
	if n := totalRequests(50, 60*time.Second); n != 3000 {
		t.Fatalf("totalRequests(50, 60s) = %d, want 3000", n)
	}
	if n := totalRequests(0.1, time.Second); n != 1 {
		t.Fatalf("totalRequests(0.1, 1s) = %d, want at least 1", n)
	}
	start := time.Unix(1_700_000_000, 0)
	if got := dueAt(start, 50, 1).Sub(start); got != 20*time.Millisecond {
		t.Fatalf("dueAt(50 rps, 1) = +%v, want +20ms", got)
	}
	if got := dueAt(start, 50, 50).Sub(start); got != time.Second {
		t.Fatalf("dueAt(50 rps, 50) = +%v, want +1s", got)
	}
	if got := dueAt(start, 3, 3).Sub(start); got != time.Second {
		t.Fatalf("dueAt(3 rps, 3) = +%v, want +1s (no drift)", got)
	}
}

// With nobody consuming jobs at all (infinitely slow workers), the
// dispatcher still releases every job on schedule: the offered rate does not
// depend on response times.
func TestDispatchIsOpenLoop(t *testing.T) {
	const rps, n = 200.0, 40 // 200 ms of schedule
	out := make(chan job, n)
	start := time.Now()
	got := dispatch(context.Background(), start, rps, n, func(i int) job { return job{kind: "price"} }, out)
	took := time.Since(start)
	if got != n || len(out) != n {
		t.Fatalf("dispatched %d (queued %d), want %d", got, len(out), n)
	}
	if took < 190*time.Millisecond || took > 2*time.Second {
		t.Fatalf("dispatch took %v, want about 195ms", took)
	}
	close(out)
	i := 0
	for j := range out {
		if j.seq != i {
			t.Fatalf("job %d has seq %d", i, j.seq)
		}
		if want := dueAt(start, rps, i); !j.due.Equal(want) {
			t.Fatalf("job %d due %v, want %v", i, j.due, want)
		}
		i++
	}
}

func TestDispatchStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	out := make(chan job, 1000)
	got := dispatch(ctx, time.Now(), 100, 1000, func(int) job { return job{} }, out)
	if got >= 1000 || got < 1 {
		t.Fatalf("dispatched %d after cancel, want a handful", got)
	}
}

// A server slower than workers can absorb: 2 workers x 40 ms = 50 rps of
// capacity against 100 rps offered. A closed-loop client would quietly
// drop to 50 rps; the open-loop run still sends all 30 requests and charges
// the queueing to latency.
func TestRunLoadChargesQueueingToLatency(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer k-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		time.Sleep(40 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	opts := runOptions{baseURL: srv.URL, key: "k-123", rps: 100, duration: 300 * time.Millisecond,
		workers: 2, timeout: 5 * time.Second, client: srv.Client()}
	results, elapsed := runLoad(context.Background(), opts, func(i int) job { return job{kind: "price", path: "/v1/price?asset=SOL"} })

	if len(results) != 30 || hits.Load() != 30 {
		t.Fatalf("scheduled %d, server saw %d, want 30 and 30", len(results), hits.Load())
	}
	lat := summarize(results, result.latency)
	svc := summarize(results, result.service)
	if lat.Count != 30 || lat.Errors != 0 {
		t.Fatalf("latency summary %+v", lat)
	}
	if svc.P50 < 40*time.Millisecond {
		t.Fatalf("service p50 %v, want >= 40ms", svc.P50)
	}
	// The last requests waited behind ~15 queued requests per worker.
	if lat.Max < 200*time.Millisecond || lat.Max <= svc.Max {
		t.Fatalf("latency max %v vs service max %v: queueing not charged", lat.Max, svc.Max)
	}
	if elapsed < 500*time.Millisecond {
		t.Fatalf("elapsed %v, want >= 600ms (30 x 40ms / 2 workers)", elapsed)
	}
}

func TestMixBuildsValidRequests(t *testing.T) {
	m, err := newMix(42, defaultWeights, defaultAssets, nil)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	const n = 20000
	for i := 0; i < n; i++ {
		j := m.next(i)
		counts[j.kind]++
		u, err := url.Parse(j.path)
		if err != nil {
			t.Fatalf("bad path %q: %v", j.path, err)
		}
		q := u.Query()
		switch j.kind {
		case "price", "asset":
			if a := q.Get("asset"); a == "" || strings.Contains(a, ",") {
				t.Fatalf("%s: want one asset, got %q", j.kind, j.path)
			}
		case "price_multi":
			if k := distinct(q.Get("asset")); k < 3 || k > 5 {
				t.Fatalf("price_multi: want 3-5 distinct assets, got %q", j.path)
			}
		case "compare":
			if k := distinct(q.Get("assets")); k < 2 || k > 5 {
				t.Fatalf("compare: want 2-5 distinct assets, got %q", j.path)
			}
		case "movers":
			if !strings.HasPrefix(u.Path, "/v1/movers") || q.Get("window") == "" || q.Get("direction") == "" {
				t.Fatalf("movers: %q", j.path)
			}
		case "resolve":
			if q.Get("query") == "" {
				t.Fatalf("resolve: %q", j.path)
			}
		default:
			t.Fatalf("unexpected kind %q (weight 0 kinds must not appear)", j.kind)
		}
	}
	total := 0
	for _, w := range defaultWeights {
		total += w
	}
	for k, w := range defaultWeights {
		want := float64(w) / float64(total) * n
		if got := float64(counts[k]); got < want*0.9-50 || got > want*1.1+50 {
			t.Errorf("%s: %v requests, want about %.0f", k, got, want)
		}
	}
}

func TestMixIsDeterministic(t *testing.T) {
	a, _ := newMix(7, defaultWeights, defaultAssets, nil)
	b, _ := newMix(7, defaultWeights, defaultAssets, nil)
	for i := 0; i < 100; i++ {
		if ja, jb := a.next(i), b.next(i); ja != jb {
			t.Fatalf("request %d differs: %+v vs %+v", i, ja, jb)
		}
	}
}

func distinct(list string) int {
	seen := map[string]bool{}
	for _, s := range strings.Split(list, ",") {
		seen[s] = true
	}
	return len(seen)
}

func TestParseWeights(t *testing.T) {
	w, err := parseWeights("price=40, new_tokens=5")
	if err != nil {
		t.Fatal(err)
	}
	if w["price"] != 40 || w["new_tokens"] != 5 || w["movers"] != defaultWeights["movers"] {
		t.Fatalf("weights = %v", w)
	}
	for _, bad := range []string{"price", "nope=3", "price=-1", "price=x"} {
		if _, err := parseWeights(bad); err == nil {
			t.Errorf("parseWeights(%q) succeeded, want error", bad)
		}
	}
	if _, err := newMix(1, map[string]int{}, defaultAssets, nil); err == nil {
		t.Error("newMix with no weights succeeded")
	}
}

func i64(v int64) *int64 { return &v }

func TestCreditsReport(t *testing.T) {
	before := health{CreditsUsedToday: i64(1000), RequestsTotal: i64(10)}
	after := health{CreditsUsedToday: i64(1006), RequestsTotal: i64(3010), TopN: 500, PollIntervalSeconds: 60}
	c := creditsReport(before, after, 3000, 60*time.Second)
	if c.Delta != 6 || c.PerThousand != 2 || c.ServerRequests != 3000 {
		t.Fatalf("report %+v", c)
	}
	// One 60 s cycle of a top-500 poll is ceil(500/200) = 3 credits.
	if c.PollerEstimate != 3 || c.AgentPerThousand != 1 {
		t.Fatalf("poller estimate %v, agent per 1000 %v; want 3 and 1", c.PollerEstimate, c.AgentPerThousand)
	}
	rolled := creditsReport(health{CreditsUsedToday: i64(900)}, health{CreditsUsedToday: i64(3)}, 3000, time.Minute)
	if rolled.Delta >= 0 || rolled.PerThousand != 0 || rolled.Note == "" {
		t.Fatalf("rollover report %+v", rolled)
	}
}

// End to end through run(): a fast fake server passes the gate, and the
// JSON report is well formed.
func TestRunGate(t *testing.T) {
	var credits atomic.Int64
	credits.Store(100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/health":
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "credits_used_today": credits.Add(1), "requests_total": 0, "top_n": 500, "poll_interval_seconds": 60})
		case r.URL.Path == "/v1/movers" && r.URL.Query().Get("window") == "7d":
			w.WriteHeader(http.StatusServiceUnavailable) // one failing kind
		default:
			w.Write([]byte(`{"data":[]}`))
		}
	}))
	defer srv.Close()

	o, err := parseFlags([]string{"-url", srv.URL + "/", "-rps", "200", "-duration", "250ms", "-workers", "4", "-json"})
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	code, err := run(context.Background(), o, &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	var rep report
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil {
		t.Fatalf("bad JSON report: %v\n%s", err, out.String())
	}
	if rep.Overall.Count != 50 || rep.Credits == nil || rep.Credits.Delta != 1 {
		t.Fatalf("report: count=%d credits=%+v", rep.Overall.Count, rep.Credits)
	}
	// ~20% movers, a third of them 7d: the error rate is far above 1%.
	if code != 1 || rep.Gate.Pass || rep.Overall.ByStatus["503"] == 0 {
		t.Fatalf("code=%d gate=%+v statuses=%v, want a failing gate", code, rep.Gate, rep.Overall.ByStatus)
	}

	o2, _ := parseFlags([]string{"-url", srv.URL, "-rps", "200", "-duration", "250ms", "-mix", "movers=0"})
	out.Reset()
	if code, err := run(context.Background(), o2, &out, &errOut); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v, want pass\n%s", code, err, out.String())
	}
	if !strings.Contains(out.String(), "PASS") || !strings.Contains(out.String(), "credits per 1,000 requests") {
		t.Fatalf("text report missing verdict or credits:\n%s", out.String())
	}
}

func TestParseFlagsRejectsBadInput(t *testing.T) {
	for _, args := range [][]string{
		{"-url", "localhost:8080"},
		{"-rps", "0"},
		{"-workers", "0"},
		{"-max-error-rate", "2"},
		{"extra"},
	} {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("parseFlags(%v) succeeded, want error", args)
		}
	}
}
