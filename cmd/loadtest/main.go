// Command loadtest offers a constant request rate to a CoinStack server and
// reports per-endpoint latency percentiles, errors by status, the achieved
// rate and the CMC credits the server spent while under load.
//
// The scheduler is open loop: request i is due at start + i/rps no matter how
// long earlier requests take, and latency is measured from that due time, so
// a slow server cannot hide behind a slower offered rate.
//
//	go run ./cmd/loadtest -url http://localhost:8080 -key KEY -rps 50 -duration 60s
//
// It exits 1 when overall p95 latency exceeds -max-p95 or the error rate
// exceeds -max-error-rate, and 2 when it cannot run at all.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

type options struct {
	url        string
	key        string
	rps        float64
	duration   time.Duration
	workers    int
	timeout    time.Duration
	maxP95     time.Duration
	maxErrRate float64
	jsonOut    bool
	seed       uint64
	assets     []string
	queries    []string
	weights    map[string]int
}

func main() {
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err := run(ctx, opts, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
	}
	os.Exit(code)
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	var o options
	var assets, queries, mixSpec string
	var seed int64
	fs.StringVar(&o.url, "url", envOr("COINSTACK_URL", "http://localhost:8080"), "CoinStack base URL (env COINSTACK_URL)")
	fs.StringVar(&o.key, "key", os.Getenv("COINSTACK_KEY"), "agent API key, sent as a Bearer token (env COINSTACK_KEY)")
	fs.Float64Var(&o.rps, "rps", 50, "offered requests per second (constant, open loop)")
	fs.DurationVar(&o.duration, "duration", 60*time.Second, "how long to offer load")
	fs.IntVar(&o.workers, "workers", 32, "concurrent workers (connections)")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Second, "per-request timeout")
	fs.DurationVar(&o.maxP95, "max-p95", 100*time.Millisecond, "fail when overall p95 latency is above this")
	fs.Float64Var(&o.maxErrRate, "max-error-rate", 0.01, "fail when the error rate (non-2xx or transport) is above this fraction")
	fs.BoolVar(&o.jsonOut, "json", false, "print a machine-readable JSON report instead of text")
	fs.Int64Var(&seed, "seed", 1, "random seed for the request mix (same seed, same requests)")
	fs.StringVar(&assets, "assets", strings.Join(defaultAssets, ","), "comma-separated assets used by price/asset/compare requests")
	fs.StringVar(&queries, "queries", strings.Join(defaultResolveQueries, ","), "comma-separated /v1/resolve queries")
	fs.StringVar(&mixSpec, "mix", "", "override mix weights, e.g. price=40,movers=10,new_tokens=5 (kinds: "+strings.Join(kindOrder, ", ")+")")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	o.url = strings.TrimRight(o.url, "/")
	o.seed = uint64(seed)
	o.assets = splitList(assets)
	o.queries = splitList(queries)
	w, err := parseWeights(mixSpec)
	if err != nil {
		return o, err
	}
	o.weights = w
	switch {
	case !strings.HasPrefix(o.url, "http://") && !strings.HasPrefix(o.url, "https://"):
		return o, fmt.Errorf("-url must start with http:// or https://")
	case o.rps <= 0:
		return o, fmt.Errorf("-rps must be positive")
	case o.duration <= 0:
		return o, fmt.Errorf("-duration must be positive")
	case o.workers < 1:
		return o, fmt.Errorf("-workers must be at least 1")
	case o.timeout <= 0:
		return o, fmt.Errorf("-timeout must be positive")
	case o.maxErrRate < 0 || o.maxErrRate > 1:
		return o, fmt.Errorf("-max-error-rate must be between 0 and 1")
	}
	return o, nil
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// run executes the load test and writes the report. It returns the process
// exit code.
func run(ctx context.Context, o options, stdout, stderr io.Writer) (int, error) {
	m, err := newMix(o.seed, o.weights, o.assets, o.queries)
	if err != nil {
		return 2, err
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        o.workers * 2,
		MaxIdleConnsPerHost: o.workers * 2,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}}

	before, err := fetchHealth(ctx, client, o.url)
	if err != nil {
		return 2, fmt.Errorf("cannot reach %s/v1/health: %w", o.url, err)
	}
	if !o.jsonOut {
		fmt.Fprintf(stderr, "loadtest: %s  rps=%g duration=%s workers=%d (%d requests)  server status=%s\n",
			o.url, o.rps, o.duration, o.workers, totalRequests(o.rps, o.duration), before.Status)
	}

	results, elapsed := runLoad(ctx, runOptions{
		baseURL: o.url, key: o.key, rps: o.rps, duration: o.duration,
		workers: o.workers, timeout: o.timeout, client: client,
	}, m.next)

	after, afterErr := fetchHealth(context.Background(), client, o.url)
	rep := buildReport(o, results, elapsed, before, after, afterErr)
	if ctx.Err() != nil {
		rep.Interrupted = true
		rep.Gate.Pass = false
		rep.Gate.Reasons = append(rep.Gate.Reasons, "run interrupted before the full duration")
	}

	if o.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return 2, err
		}
	} else {
		writeText(stdout, rep)
	}
	if !rep.Gate.Pass {
		return 1, nil
	}
	return 0, nil
}

// health is the part of /v1/health the load test reads.
type health struct {
	Status              string `json:"status"`
	CreditsUsedToday    *int64 `json:"credits_used_today"`
	RequestsTotal       *int64 `json:"requests_total"`
	TopN                int    `json:"top_n"`
	PollIntervalSeconds int    `json:"poll_interval_seconds"`
}

func fetchHealth(ctx context.Context, c *http.Client, base string) (health, error) {
	var h health
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/health", nil)
	if err != nil {
		return h, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	// /v1/health answers 503 with the same body when the service is down.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return h, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return h, fmt.Errorf("decoding health: %w", err)
	}
	return h, nil
}

// ---- report ----

type latencyStats struct {
	Endpoint  string         `json:"endpoint"`
	Count     int            `json:"count"`
	Errors    int            `json:"errors"`
	ErrorRate float64        `json:"error_rate"`
	ByStatus  map[string]int `json:"by_status"`
	P50Ms     float64        `json:"p50_ms"`
	P95Ms     float64        `json:"p95_ms"`
	P99Ms     float64        `json:"p99_ms"`
	MaxMs     float64        `json:"max_ms"`
	MeanMs    float64        `json:"mean_ms"`
}

type creditReport struct {
	Before           int64   `json:"credits_used_today_before"`
	After            int64   `json:"credits_used_today_after"`
	Delta            int64   `json:"credits_delta"`
	PerThousand      float64 `json:"credits_per_1000_requests"`
	PollerEstimate   float64 `json:"background_poller_estimate"`
	AgentPerThousand float64 `json:"credits_per_1000_requests_excluding_poller"`
	ServerRequests   int64   `json:"server_requests_total_delta,omitempty"`
	Note             string  `json:"note"`
}

type gate struct {
	Pass         bool     `json:"pass"`
	MaxP95Ms     float64  `json:"max_p95_ms"`
	MaxErrorRate float64  `json:"max_error_rate"`
	Reasons      []string `json:"reasons,omitempty"`
}

type report struct {
	URL          string         `json:"url"`
	OfferedRPS   float64        `json:"offered_rps"`
	AchievedRPS  float64        `json:"achieved_rps"`
	DurationS    float64        `json:"duration_s"`
	ElapsedS     float64        `json:"elapsed_s"`
	Workers      int            `json:"workers"`
	Scheduled    int            `json:"scheduled"`
	Overall      latencyStats   `json:"overall"`
	Endpoints    []latencyStats `json:"endpoints"`
	ServiceP95Ms float64        `json:"service_p95_ms"`
	MaxSendLagMs float64        `json:"max_send_lag_ms"`
	LateSends    int            `json:"late_sends"`
	Credits      *creditReport  `json:"credits,omitempty"`
	CreditsError string         `json:"credits_error,omitempty"`
	SampleErrors []string       `json:"sample_errors,omitempty"`
	Warnings     []string       `json:"warnings,omitempty"`
	Interrupted  bool           `json:"interrupted,omitempty"`
	Gate         gate           `json:"gate"`
}

// lateSend is how far behind schedule a send may start before it counts as
// late, which means the load generator itself could not keep up.
const lateSend = 10 * time.Millisecond

func buildReport(o options, results []result, elapsed time.Duration, before, after health, afterErr error) report {
	rep := report{
		URL:        o.url,
		OfferedRPS: o.rps,
		DurationS:  o.duration.Seconds(),
		ElapsedS:   round(elapsed.Seconds(), 3),
		Workers:    o.workers,
		Scheduled:  len(results),
	}
	overall := summarize(results, result.latency)
	rep.Overall = toStats("overall", overall)
	if elapsed > 0 {
		rep.AchievedRPS = round(float64(overall.Count)/elapsed.Seconds(), 2)
	}
	rep.ServiceP95Ms = ms(summarize(results, result.service).P95)

	byKind := map[string][]result{}
	var maxLag time.Duration
	for _, r := range results {
		if !r.done {
			continue
		}
		byKind[r.kind] = append(byKind[r.kind], r)
		if l := r.lag(); l > maxLag {
			maxLag = l
		}
		if r.lag() > lateSend {
			rep.LateSends++
		}
		if r.err != "" && len(rep.SampleErrors) < 5 {
			rep.SampleErrors = append(rep.SampleErrors, r.kind+": "+r.err)
		}
	}
	rep.MaxSendLagMs = ms(maxLag)
	for _, k := range kindOrder {
		if rs := byKind[k]; len(rs) > 0 {
			rep.Endpoints = append(rep.Endpoints, toStats(k, summarize(rs, result.latency)))
		}
	}

	if n := overall.ByStatus["429"]; n > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"%d responses were 429 rate_limited: the key allows fewer requests per minute than %.0f; "+
				"use a key with a higher limit (coinstack keys create --owner loadtest --rate-limit %d, or COINSTACK_API_KEYS=key:loadtest:%d)",
			n, o.rps*60, int(math.Ceil(o.rps*60*1.2)), int(math.Ceil(o.rps*60*1.2))))
	}
	if n := overall.ByStatus["401"]; n > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%d responses were 401 unauthorized: pass -key or set COINSTACK_KEY", n))
	}
	if overall.Count > 0 && float64(rep.LateSends)/float64(overall.Count) > 0.01 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"%d sends started more than %s behind schedule: all %d workers were busy, so queueing time is included in latency; "+
				"raise -workers if the server is not the bottleneck", rep.LateSends, lateSend, o.workers))
	}

	switch {
	case afterErr != nil:
		rep.CreditsError = "could not read /v1/health after the run: " + afterErr.Error()
	case before.CreditsUsedToday == nil || after.CreditsUsedToday == nil:
		rep.CreditsError = "/v1/health did not report credits_used_today"
	default:
		rep.Credits = creditsReport(before, after, overall.Count, elapsed)
	}

	rep.Gate = gate{Pass: true, MaxP95Ms: ms(o.maxP95), MaxErrorRate: o.maxErrRate}
	if overall.Count == 0 {
		rep.Gate.Pass = false
		rep.Gate.Reasons = append(rep.Gate.Reasons, "no requests completed")
	}
	if overall.P95 > o.maxP95 {
		rep.Gate.Pass = false
		rep.Gate.Reasons = append(rep.Gate.Reasons, fmt.Sprintf("p95 %s > %s", fmtDur(overall.P95), fmtDur(o.maxP95)))
	}
	if overall.ErrorRate > o.maxErrRate {
		rep.Gate.Pass = false
		rep.Gate.Reasons = append(rep.Gate.Reasons, fmt.Sprintf("error rate %.2f%% > %.2f%%", overall.ErrorRate*100, o.maxErrRate*100))
	}
	return rep
}

// creditsReport turns the credits_used_today counters read before and after
// the run into credits per 1,000 requests. The server's background poller
// keeps spending credits during the run whatever the agent traffic is, so the
// report also shows an estimate with the poller's share taken out.
func creditsReport(before, after health, requests int, elapsed time.Duration) *creditReport {
	c := &creditReport{Before: *before.CreditsUsedToday, After: *after.CreditsUsedToday}
	c.Delta = c.After - c.Before
	if before.RequestsTotal != nil && after.RequestsTotal != nil {
		c.ServerRequests = *after.RequestsTotal - *before.RequestsTotal
	}
	if c.Delta < 0 {
		c.Note = "credits_used_today went down (the UTC day or CMC's counter rolled over during the run); the delta is not meaningful"
		return c
	}
	if requests > 0 {
		c.PerThousand = round(float64(c.Delta)*1000/float64(requests), 2)
	}
	if after.PollIntervalSeconds > 0 && after.TopN > 0 {
		cycles := elapsed.Seconds() / float64(after.PollIntervalSeconds)
		perCycle := math.Ceil(float64(after.TopN) / 200) // CMC: 1 credit per 200 assets returned
		c.PollerEstimate = round(cycles*perCycle, 1)
		if requests > 0 {
			c.AgentPerThousand = round(math.Max(0, float64(c.Delta)-c.PollerEstimate)*1000/float64(requests), 2)
		}
	}
	c.Note = "the delta includes the background poller; credits_used_today may lag when it comes from CMC /v1/key/info, so run for 5+ minutes for a stable figure"
	return c
}

func toStats(name string, s summary) latencyStats {
	return latencyStats{
		Endpoint: name, Count: s.Count, Errors: s.Errors, ErrorRate: round(s.ErrorRate, 4), ByStatus: s.ByStatus,
		P50Ms: ms(s.P50), P95Ms: ms(s.P95), P99Ms: ms(s.P99), MaxMs: ms(s.Max), MeanMs: ms(s.Mean),
	}
}

func writeText(w io.Writer, r report) {
	fmt.Fprintf(w, "\nCoinStack load test  %s\n", r.URL)
	fmt.Fprintf(w, "offered %.1f rps for %gs with %d workers: %d scheduled, %d completed in %.1fs -> achieved %.1f rps\n\n",
		r.OfferedRPS, r.DurationS, r.Workers, r.Scheduled, r.Overall.Count, r.ElapsedS, r.AchievedRPS)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "endpoint\tcount\terrors\tp50\tp95\tp99\tmax\t")
	for _, e := range append(r.Endpoints, r.Overall) {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\t%s\t\n", e.Endpoint, e.Count, e.Errors,
			fmtMs(e.P50Ms), fmtMs(e.P95Ms), fmtMs(e.P99Ms), fmtMs(e.MaxMs))
	}
	tw.Flush()

	fmt.Fprintf(w, "\nlatency is measured from each request's scheduled send time; service-time p95 (from actual send) %s\n", fmtMs(r.ServiceP95Ms))
	fmt.Fprintf(w, "responses by status: %s\n", fmtStatuses(r.Overall.ByStatus))
	fmt.Fprintf(w, "error rate: %.2f%%   late sends (>%s behind schedule): %d   max send lag: %s\n",
		r.Overall.ErrorRate*100, lateSend, r.LateSends, fmtMs(r.MaxSendLagMs))
	for _, e := range r.SampleErrors {
		fmt.Fprintf(w, "  error sample: %s\n", e)
	}

	if c := r.Credits; c != nil {
		fmt.Fprintf(w, "\nCMC credits (credits_used_today from /v1/health): %d -> %d, delta %d over %d requests\n",
			c.Before, c.After, c.Delta, r.Overall.Count)
		if c.Delta >= 0 {
			fmt.Fprintf(w, "  = %.2f credits per 1,000 requests (including the background poller)\n", c.PerThousand)
			if c.PollerEstimate > 0 {
				fmt.Fprintf(w, "  = %.2f credits per 1,000 requests after removing ~%.1f expected poller credits\n", c.AgentPerThousand, c.PollerEstimate)
			}
		}
		if c.ServerRequests > 0 {
			fmt.Fprintf(w, "  server counted %d requests during the run\n", c.ServerRequests)
		}
		fmt.Fprintf(w, "  note: %s\n", c.Note)
	} else if r.CreditsError != "" {
		fmt.Fprintf(w, "\nCMC credits: unavailable (%s)\n", r.CreditsError)
	}

	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "\nwarning: %s\n", warn)
	}

	if r.Gate.Pass {
		fmt.Fprintf(w, "\nPASS  p95 %s <= %s, error rate %.2f%% <= %.2f%%\n",
			fmtMs(r.Overall.P95Ms), fmtMs(r.Gate.MaxP95Ms), r.Overall.ErrorRate*100, r.Gate.MaxErrorRate*100)
	} else {
		fmt.Fprintf(w, "\nFAIL  %s\n", strings.Join(r.Gate.Reasons, "; "))
	}
}

func fmtStatuses(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, m[k])
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

func ms(d time.Duration) float64 { return round(float64(d)/float64(time.Millisecond), 3) }

func fmtMs(v float64) string {
	if v < 10 {
		return fmt.Sprintf("%.2fms", v)
	}
	return fmt.Sprintf("%.1fms", v)
}

func fmtDur(d time.Duration) string { return fmtMs(ms(d)) }

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
