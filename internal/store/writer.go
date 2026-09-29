package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fozagtx/coinstack/internal/model"
)

// Writer tuning.
const (
	flushInterval       = time.Second // how long queued items may wait to be batched
	maxBatch            = 512
	writeTimeout        = 30 * time.Second
	closeFlushTimeout   = 5 * time.Second
	maxBackoff          = 30 * time.Second
	retentionFirstDelay = time.Minute
	retentionEvery      = time.Hour
	retentionTimeout    = 5 * time.Minute
	retentionChunk      = 10000
	warnEvery           = time.Minute
)

type jobKind uint8

const (
	jobSnapshot jobKind = iota + 1
	jobPoll
	jobRequest
)

// job is one queued write.
type job struct {
	kind   jobKind
	quotes []model.Quote // jobSnapshot
	token  int64         // jobSnapshot: the throttle slot it took, released if it is dropped
	poll   model.PollRun
	req    model.RequestLog
}

// EnqueueSnapshot queues quotes to be persisted as one snapshot, at most once
// per SnapshotEvery; calls in between are ignored. Writing a snapshot also
// upserts the identity fields of the quotes' assets (symbol, name, slug,
// rank, date_added, tags). It never blocks. The slice is copied, so the
// caller may reuse it.
func (s *Store) EnqueueSnapshot(quotes []model.Quote) {
	if len(quotes) == 0 {
		return
	}
	now := s.now().UnixNano()
	last := s.lastSnap.Load()
	if last != 0 && now >= last && now-last < int64(s.cfg.SnapshotEvery) {
		return
	}
	if !s.lastSnap.CompareAndSwap(last, now) {
		return // a concurrent call took this slot
	}
	if !s.enqueue(job{kind: jobSnapshot, quotes: slices.Clone(quotes), token: now}) {
		s.releaseSnapshotSlot(now)
	}
}

// releaseSnapshotSlot lets the next EnqueueSnapshot through when the
// snapshot that took slot token was not written.
func (s *Store) releaseSnapshotSlot(token int64) {
	s.lastSnap.CompareAndSwap(token, 0)
}

// RecordPoll queues one poller run for poll_runs. It never blocks.
func (s *Store) RecordPoll(run model.PollRun) {
	if run.StartedAt.IsZero() {
		run.StartedAt = s.now()
	}
	s.enqueue(job{kind: jobPoll, poll: run})
}

// LogRequest queues a request for request_log, sampled at
// RequestSampleRate; requests with Status >= 400 are always kept. It never
// blocks.
func (s *Store) LogRequest(r model.RequestLog) {
	if !shouldLog(r.Status, s.cfg.RequestSampleRate, s.rand) {
		return
	}
	if r.TS.IsZero() {
		r.TS = s.now()
	}
	s.enqueue(job{kind: jobRequest, req: r})
}

// shouldLog reports whether a request with status is kept at sample rate.
func shouldLog(status int, rate float64, random func() float64) bool {
	switch {
	case status >= 400 || rate >= 1:
		return true
	case rate <= 0:
		return false
	default:
		return random() < rate
	}
}

// enqueue adds j to the queue without blocking; when the queue is full or
// the store is closed, j is dropped and counted.
func (s *Store) enqueue(j job) bool {
	if !s.closed.Load() {
		select {
		case s.jobs <- j:
			return true
		default:
		}
	}
	n := s.dropped.Add(1)
	if now := time.Now(); s.dropWarn.allow(now, warnEvery) {
		s.log.Warn("store: write queue full or closed; dropping items", "dropped_total", n, "queue_size", cap(s.jobs))
	}
	return false
}

// Run is the single writer. It batches queued items into transactions,
// backs off while the database fails, and deletes rows past their retention
// every hour. When ctx is done (or Close is called) it drains the queue,
// flushes it with a short timeout and returns. Run must be called at most
// once.
func (s *Store) Run(ctx context.Context) {
	s.mu.Lock()
	if s.started || s.closed.Load() {
		s.mu.Unlock()
		s.log.Warn("store: Run called on a closed or already running store")
		return
	}
	s.started, s.running = true, true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		close(s.runDone)
	}()

	flush := time.NewTicker(flushInterval)
	defer flush.Stop()
	retention := time.NewTimer(retentionFirstDelay)
	defer retention.Stop()

	var (
		pending []job
		backoff time.Duration
		retryAt time.Time
	)
	flushPending := func() {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		err := s.write(wctx, pending)
		cancel()
		clear(pending)
		pending = pending[:0]
		if err == nil {
			backoff = 0
			return
		}
		backoff = min(max(2*backoff, time.Second), maxBackoff)
		retryAt = time.Now().Add(backoff)
	}

	for {
		// While backing off, leave items in the channel: it buffers a short
		// outage, and once it fills callers drop instead of blocking.
		in := s.jobs
		if time.Now().Before(retryAt) {
			in = nil
		}
		select {
		case <-ctx.Done():
			s.drain(pending)
			return
		case <-s.stop:
			s.drain(pending)
			return
		case j := <-in:
			pending = append(pending, j)
			if len(pending) >= maxBatch {
				flushPending()
			}
		case <-flush.C:
			if len(pending) > 0 {
				flushPending()
			}
		case <-retention.C:
			if err := s.retain(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("store: retention failed", "err", err)
			}
			retention.Reset(retentionEvery)
		}
	}
}

// drain writes pending plus everything still queued, bounded by
// closeFlushTimeout.
func (s *Store) drain(pending []job) {
loop:
	for {
		select {
		case j := <-s.jobs:
			pending = append(pending, j)
		default:
			break loop
		}
	}
	if len(pending) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeFlushTimeout)
	defer cancel()
	_ = s.write(ctx, pending)
}

// write persists a batch: request logs and poll runs together in one
// transaction, each snapshot in its own. It updates the counters and
// returns the first error.
func (s *Store) write(ctx context.Context, batch []job) error {
	var (
		polls []model.PollRun
		reqs  []model.RequestLog
		snaps []job
	)
	for _, j := range batch {
		switch j.kind {
		case jobPoll:
			polls = append(polls, j.poll)
		case jobRequest:
			reqs = append(reqs, j.req)
		case jobSnapshot:
			snaps = append(snaps, j)
		}
	}

	var firstErr error
	if n := len(polls) + len(reqs); n > 0 {
		err := s.pool.withTx(ctx, func(tx pgx.Tx) error {
			if err := copyPollRuns(ctx, tx, polls); err != nil {
				return err
			}
			return copyRequestLogs(ctx, tx, reqs)
		})
		s.recordResult(n, "logs", err)
		firstErr = err
	}
	for _, j := range snaps {
		fallback := s.now()
		err := s.pool.withTx(ctx, func(tx pgx.Tx) error {
			return writeSnapshot(ctx, tx, j.quotes, fallback)
		})
		s.recordResult(1, "snapshot", err)
		if err != nil {
			s.releaseSnapshotSlot(j.token)
			firstErr = cmp.Or(firstErr, err)
		}
	}
	return firstErr
}

// recordResult updates the write counters for n items.
func (s *Store) recordResult(n int, what string, err error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.written.Add(int64(n))
		s.lastWriteAt = now
		s.lastErr = ""
		s.errLog.success()
		return
	}
	s.dropped.Add(int64(n))
	s.lastErr = fmt.Sprintf("%s: %v", what, err)
	s.errLog.failure(now, what, err, n)
}

// errLogger logs write failures with backoff: the first failure at once,
// then at most one summary per warnEvery while failures continue, and one
// line when writes recover. Guarded by Store.mu.
type errLogger struct {
	log        *slog.Logger
	failures   int // consecutive failed writes
	unlogged   int // items dropped since the last log line
	lastLogged time.Time
}

func (l *errLogger) failure(now time.Time, what string, err error, items int) {
	l.failures++
	l.unlogged += items
	if l.failures > 1 && now.Sub(l.lastLogged) < warnEvery {
		return
	}
	l.log.Error("store: write failed, items dropped",
		"what", what, "err", err, "dropped", l.unlogged, "consecutive_failures", l.failures)
	l.lastLogged = now
	l.unlogged = 0
}

func (l *errLogger) success() {
	if l.failures == 0 {
		return
	}
	l.log.Info("store: writes recovered", "failed_writes", l.failures, "dropped_since_last_log", l.unlogged)
	l.failures, l.unlogged = 0, 0
}

// rateGate lets one caller through per interval, lock-free.
type rateGate struct{ last atomic.Int64 }

func (g *rateGate) allow(now time.Time, every time.Duration) bool {
	last := g.last.Load()
	if last != 0 && now.UnixNano()-last < int64(every) {
		return false
	}
	return g.last.CompareAndSwap(last, now.UnixNano())
}

// retain deletes snapshots older than SnapshotRetention and request_log and
// poll_runs rows older than LogRetention, in chunks so a long backlog never
// becomes one huge transaction.
func (s *Store) retain(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, retentionTimeout)
	defer cancel()
	now := s.now()
	targets := []struct {
		table, column string
		cutoff        time.Time
	}{
		{"snapshots", "fetched_at", now.Add(-s.cfg.SnapshotRetention)},
		{"request_log", "ts", now.Add(-s.cfg.LogRetention)},
		{"poll_runs", "started_at", now.Add(-s.cfg.LogRetention)},
	}
	var errs []error
	for _, t := range targets {
		sql := fmt.Sprintf(`DELETE FROM %[1]s WHERE ctid = ANY (ARRAY(
			SELECT ctid FROM %[1]s WHERE %[2]s < $1::timestamptz LIMIT %[3]d))`, t.table, t.column, retentionChunk)
		var total int64
		for {
			var n int64
			err := s.pool.withConn(ctx, func(c *pgx.Conn) error {
				tag, err := c.Exec(ctx, sql, t.cutoff)
				n = tag.RowsAffected()
				return err
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", t.table, err))
				break
			}
			total += n
			if n < retentionChunk {
				break
			}
		}
		if total > 0 {
			s.log.Debug("store: retention", "table", t.table, "deleted", total)
		}
	}
	return errors.Join(errs...)
}
