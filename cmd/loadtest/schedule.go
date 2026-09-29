package main

import (
	"context"
	"io"
	"math"
	"net/http"
	"sync"
	"time"
)

// job is one scheduled request.
type job struct {
	seq  int
	kind string
	path string // path and query, e.g. "/v1/price?asset=SOL"
	due  time.Time
}

// totalRequests is how many requests a run of d at rps offers.
func totalRequests(rps float64, d time.Duration) int {
	n := int(math.Round(rps * d.Seconds()))
	if n < 1 {
		n = 1
	}
	return n
}

// dueAt is when request i of a constant-rate schedule is due: start + i/rps.
// Computing every send time from start (instead of sleeping one interval
// after the previous send) keeps the schedule from drifting.
func dueAt(start time.Time, rps float64, i int) time.Time {
	return start.Add(time.Duration(float64(i) * float64(time.Second) / rps))
}

// dispatch emits n jobs on a fixed schedule: job i is released at
// dueAt(start, rps, i), or immediately when the dispatcher is already late.
// It is open loop: it never waits for a response, so slow responses cannot
// lower the offered rate. out should have room for every job; when workers
// fall behind, jobs queue in out and their queueing time is charged to their
// latency (measured from due). It returns the number of jobs emitted, which
// is less than n only when ctx is cancelled.
func dispatch(ctx context.Context, start time.Time, rps float64, n int, next func(i int) job, out chan<- job) int {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for i := 0; i < n; i++ {
		due := dueAt(start, rps, i)
		if wait := time.Until(due); wait > 0 {
			timer.Reset(wait)
			select {
			case <-ctx.Done():
				return i
			case <-timer.C:
			}
		} else if ctx.Err() != nil {
			return i
		}
		j := next(i)
		j.seq, j.due = i, due
		select {
		case out <- j:
		case <-ctx.Done():
			return i
		}
	}
	return n
}

// runOptions configures one load run.
type runOptions struct {
	baseURL  string
	key      string
	rps      float64
	duration time.Duration
	workers  int
	timeout  time.Duration
	client   *http.Client
}

// runLoad offers opts.rps requests per second for opts.duration, spread over
// opts.workers concurrent workers, and returns one result per scheduled
// request (results[i].done is false for requests that never ran) plus the
// wall time from the first scheduled send to the last completed response.
func runLoad(ctx context.Context, opts runOptions, next func(i int) job) ([]result, time.Duration) {
	n := totalRequests(opts.rps, opts.duration)
	results := make([]result, n)

	// Buffer the whole run so the dispatcher never blocks on workers. Beyond
	// a million queued requests the test is hopeless anyway; cap memory there.
	buf := n
	if buf > 1<<20 {
		buf = 1 << 20
	}
	jobs := make(chan job, buf)

	var wg sync.WaitGroup
	for w := 0; w < opts.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				results[j.seq] = execute(ctx, opts, j)
			}
		}()
	}

	start := time.Now()
	dispatch(ctx, start, opts.rps, n, next, jobs)
	close(jobs)
	wg.Wait()

	var last time.Time
	for _, r := range results {
		if r.done && r.end.After(last) {
			last = r.end
		}
	}
	elapsed := last.Sub(start)
	if elapsed <= 0 {
		elapsed = time.Since(start)
	}
	return results, elapsed
}

// execute sends one request and reads the whole body.
func execute(ctx context.Context, opts runOptions, j job) result {
	r := result{kind: j.kind, due: j.due}
	if ctx.Err() != nil {
		return r // cancelled while queued: never ran
	}
	reqCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()

	r.start = time.Now()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, opts.baseURL+j.path, nil)
	if err != nil {
		r.end, r.done, r.err = time.Now(), true, err.Error()
		return r
	}
	if opts.key != "" {
		req.Header.Set("Authorization", "Bearer "+opts.key)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := opts.client.Do(req)
	if err != nil {
		r.end = time.Now()
		if ctx.Err() != nil {
			return r // interrupted by Ctrl-C, not a server failure
		}
		r.done, r.err = true, err.Error()
		return r
	}
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	r.end, r.done, r.status = time.Now(), true, resp.StatusCode
	if err != nil {
		r.status, r.err = 0, "reading body: "+err.Error()
	}
	return r
}
