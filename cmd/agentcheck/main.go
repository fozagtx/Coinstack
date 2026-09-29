// Command agentcheck is CoinStack's week-2 exit gate, run without an LLM: ten
// questions an agent would ask, each answered with ONE HTTP call (plus one
// retry for the ambiguous ticker), with every response checked against the
// wire contract in docs/api-contract.md: as_of, age_seconds, source, flat
// data fields with numbers as numbers, and errors carrying next_step.
//
//	go run ./cmd/agentcheck -url http://localhost:8080 -key KEY
//
// It prints a readable trace and a pass/fail summary. Exit code 0 means
// every question passed, 1 means at least one failed, 2 means it could not
// run (server unreachable, bad flags).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type options struct {
	url       string
	key       string
	timeout   time.Duration
	ambiguous string // a ticker that matches several assets with none dominant
	typo      string // a misspelled asset name
	typoWant  string // the asset name the typo's suggestions must include
	maxSkew   time.Duration
	verbose   bool
}

func main() {
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "agentcheck:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, opts, os.Stdout))
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("agentcheck", flag.ContinueOnError)
	var o options
	fs.StringVar(&o.url, "url", envOr("COINSTACK_URL", "http://localhost:8080"), "CoinStack base URL (env COINSTACK_URL)")
	fs.StringVar(&o.key, "key", os.Getenv("COINSTACK_KEY"), "agent API key, sent as a Bearer token (env COINSTACK_KEY)")
	fs.DurationVar(&o.timeout, "timeout", 15*time.Second, "per-request timeout")
	fs.StringVar(&o.ambiguous, "ambiguous", "GMT", "ticker expected to be ambiguous (409 ambiguous_asset)")
	fs.StringVar(&o.typo, "typo", "Etherium", "misspelled asset expected to give 404 asset_not_found with suggestions")
	fs.StringVar(&o.typoWant, "typo-want", "Ethereum", "asset name the typo's suggestions must include")
	fs.DurationVar(&o.maxSkew, "max-skew", 30*time.Second, "allowed disagreement between age_seconds and now - as_of (clock skew); 0 disables")
	fs.BoolVar(&o.verbose, "v", false, "print every response body")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	o.url = strings.TrimRight(o.url, "/")
	if !strings.HasPrefix(o.url, "http://") && !strings.HasPrefix(o.url, "https://") {
		return o, fmt.Errorf("-url must start with http:// or https://")
	}
	return o, nil
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// call is one HTTP exchange.
type call struct {
	path    string // display form, query unescaped
	status  int
	latency time.Duration
	ctype   string
	raw     []byte
	body    map[string]any // nil when the body is not a JSON object
	err     error
}

type client struct {
	base, key string
	http      *http.Client
}

func (c *client) get(ctx context.Context, path string, q url.Values) *call {
	target := path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	display, err := url.QueryUnescape(target)
	if err != nil {
		display = target
	}
	cl := &call{path: display}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+target, nil)
	if err != nil {
		cl.err = err
		return cl
	}
	req.Header.Set("Accept", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		cl.latency, cl.err = time.Since(start), err
		return cl
	}
	defer resp.Body.Close()
	cl.raw, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	cl.latency, cl.status, cl.ctype = time.Since(start), resp.StatusCode, resp.Header.Get("Content-Type")
	if err != nil {
		cl.err = err
		return cl
	}
	dec := json.NewDecoder(bytes.NewReader(cl.raw))
	dec.UseNumber()
	var body map[string]any
	if dec.Decode(&body) == nil {
		cl.body = body
	}
	return cl
}

// session runs the questions and prints the trace.
type session struct {
	ctx   context.Context
	c     *client
	o     options
	out   io.Writer
	a     *asserter // current question's assertions
	calls int       // HTTP calls for the current question
	now   func() time.Time
}

// get performs one call for the current question, prints it and checks the
// transport-level contract (JSON object body, JSON content type).
func (s *session) get(path string, q url.Values) *call {
	s.calls++
	cl := s.c.get(s.ctx, path, q)
	if cl.err != nil {
		fmt.Fprintf(s.out, "     GET %s -> transport error: %v\n", cl.path, cl.err)
		s.a.that(false, "GET %s failed: %v", cl.path, cl.err)
		cl.body = map[string]any{}
		return cl
	}
	label := ""
	if e, ok := obj(cl.body["error"]); ok {
		label = " " + fstr(e, "code")
	}
	fmt.Fprintf(s.out, "     GET %s -> %d%s  %s\n", cl.path, cl.status, label, fmtLatency(cl.latency))
	mt, params, _ := mime.ParseMediaType(cl.ctype)
	s.a.that(mt == "application/json", "Content-Type must be application/json, got %q", cl.ctype)
	if cs := params["charset"]; cs != "" {
		s.a.that(strings.EqualFold(cs, "utf-8"), "charset must be utf-8, got %q", cs)
	}
	if !s.a.that(cl.body != nil, "body must be a JSON object") {
		cl.body = map[string]any{}
	}
	if s.o.verbose {
		fmt.Fprintf(s.out, "       %s\n", indentJSON(cl.raw))
	}
	return cl
}

// envelope checks a 200 response's envelope and returns data.
func (s *session) envelope(cl *call, want envelopeWant) any {
	if !s.a.that(cl.status == http.StatusOK, "HTTP status must be 200, got %d", cl.status) {
		s.showBody(cl)
	}
	return checkEnvelope(s.a, cl.body, want, s.now(), s.o.maxSkew)
}

func (s *session) showBody(cl *call) {
	if !s.o.verbose && len(cl.raw) > 0 {
		b := cl.raw
		if len(b) > 400 {
			b = append(b[:400:400], "..."...)
		}
		fmt.Fprintf(s.out, "       body: %s\n", strings.TrimSpace(string(b)))
	}
}

// question is one agent question with its expected number of calls.
type question struct {
	ask   string
	calls int // expected HTTP calls: 1, or 2 when the agent must retry
	run   func(s *session) string
}

type outcome struct {
	ask        string
	calls      int
	assertions int
	fails      []string
}

func run(ctx context.Context, o options, out io.Writer) int {
	c := &client{base: o.url, key: o.key, http: &http.Client{Timeout: o.timeout}}
	fmt.Fprintf(out, "CoinStack agentcheck -> %s\n", o.url)

	// Preflight: /v1/health needs no key and says whether data is loaded.
	h := c.get(ctx, "/v1/health", nil)
	if h.err != nil {
		fmt.Fprintf(out, "cannot reach %s/v1/health: %v\n", o.url, h.err)
		return 2
	}
	fmt.Fprintf(out, "health: HTTP %d status=%s cache_size=%s resolver_assets=%s credits_used_today=%s age_seconds=%s\n\n",
		h.status, fstr(h.body, "status"), numText(h.body["cache_size"]), numText(h.body["resolver_assets"]),
		numText(h.body["credits_used_today"]), numText(h.body["age_seconds"]))

	s := &session{ctx: ctx, c: c, o: o, out: out, now: time.Now}
	qs := questions(o)
	var results []outcome
	totalCalls := 0
	for i, q := range qs {
		if ctx.Err() != nil {
			break
		}
		s.a, s.calls = &asserter{}, 0
		fmt.Fprintf(out, "[%2d] %s\n", i+1, q.ask)
		answer := q.run(s)
		s.a.that(s.calls == q.calls, "answered in %d HTTP calls, want %d", s.calls, q.calls)
		if answer != "" {
			fmt.Fprintf(out, "     answer: %s\n", answer)
		}
		if len(s.a.fails) == 0 {
			fmt.Fprintf(out, "     PASS  %d calls, %d assertions\n\n", s.calls, s.a.n)
		} else {
			fmt.Fprintf(out, "     FAIL  %d of %d assertions failed:\n", len(s.a.fails), s.a.n)
			for _, f := range s.a.fails {
				fmt.Fprintf(out, "       - %s\n", f)
			}
			fmt.Fprintln(out)
		}
		totalCalls += s.calls
		results = append(results, outcome{ask: q.ask, calls: s.calls, assertions: s.a.n, fails: s.a.fails})
	}

	passed, assertions := 0, 0
	for _, r := range results {
		assertions += r.assertions
		if len(r.fails) == 0 {
			passed++
		}
	}
	fmt.Fprintf(out, "Summary: %d/%d questions passed, %d HTTP calls for %d questions, %d contract assertions\n",
		passed, len(qs), totalCalls, len(qs), assertions)
	for i, r := range results {
		if len(r.fails) > 0 {
			fmt.Fprintf(out, "  failed [%d] %s (%d assertion failures)\n", i+1, r.ask, len(r.fails))
		}
	}
	if passed == len(qs) {
		fmt.Fprintln(out, "PASS")
		return 0
	}
	fmt.Fprintln(out, "FAIL")
	return 1
}

func fmtLatency(d time.Duration) string {
	return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
}

func numText(v any) string {
	if n, ok := v.(json.Number); ok {
		return string(n)
	}
	if v == nil {
		return "?"
	}
	return fmt.Sprint(v)
}

func indentJSON(raw []byte) string {
	var b bytes.Buffer
	if json.Indent(&b, raw, "       ", "  ") != nil {
		return string(raw)
	}
	return b.String()
}
