// Package store persists CoinStack state in Postgres (Neon): periodic market
// snapshots for restarts and history, the asset map, agent API keys, poller
// runs and sampled request logs.
//
// Serving never waits on the database. Writes go through a bounded queue
// drained by one writer goroutine (Run); when the queue is full or the
// database is down, items are dropped and counted rather than blocking the
// caller. Reads used at startup (LoadLatestSnapshot, LoadMap) and key lookups
// (cached) are the only synchronous database calls.
//
// The store is built for Neon's pooled endpoint, which runs PgBouncer in
// transaction mode: it never uses named prepared statements or session
// state outside a transaction.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"
)

// Config configures a Store. Zero values select the documented defaults.
type Config struct {
	// DatabaseURL is the Postgres connection string, normally Neon's pooled
	// endpoint (host contains "-pooler").
	DatabaseURL string
	// SnapshotEvery is the minimum interval between persisted snapshots;
	// default 5m.
	SnapshotEvery time.Duration
	// SnapshotRetention is how long snapshots are kept; default 7 days.
	SnapshotRetention time.Duration
	// LogRetention is how long request_log and poll_runs rows are kept;
	// default 30 days.
	LogRetention time.Duration
	// RequestSampleRate is the fraction of successful requests logged;
	// default 0.1, negative logs none. Requests with Status >= 400 are
	// always logged.
	RequestSampleRate float64
	// KeyCacheTTL is how long a found API key is cached; default 60s.
	// Unknown keys are cached for min(KeyCacheTTL, 10s).
	KeyCacheTTL time.Duration
	// QueueSize is the capacity of the write queue; default 1024.
	QueueSize int
	// MaxConns caps open database connections; default 5.
	MaxConns int
	// Logger receives store logs; default slog.Default().
	Logger *slog.Logger
}

// Defaults applied by Open for zero Config fields.
const (
	DefaultSnapshotEvery     = 5 * time.Minute
	DefaultSnapshotRetention = 7 * 24 * time.Hour
	DefaultLogRetention      = 30 * 24 * time.Hour
	DefaultRequestSampleRate = 0.1
	DefaultKeyCacheTTL       = 60 * time.Second
	DefaultQueueSize         = 1024

	maxNegativeKeyTTL = 10 * time.Second
)

func (c Config) withDefaults() Config {
	if c.SnapshotEvery <= 0 {
		c.SnapshotEvery = DefaultSnapshotEvery
	}
	if c.SnapshotRetention <= 0 {
		c.SnapshotRetention = DefaultSnapshotRetention
	}
	if c.LogRetention <= 0 {
		c.LogRetention = DefaultLogRetention
	}
	switch {
	case c.RequestSampleRate == 0:
		c.RequestSampleRate = DefaultRequestSampleRate
	case c.RequestSampleRate < 0:
		c.RequestSampleRate = 0
	case c.RequestSampleRate > 1:
		c.RequestSampleRate = 1
	}
	if c.KeyCacheTTL <= 0 {
		c.KeyCacheTTL = DefaultKeyCacheTTL
	}
	if c.QueueSize <= 0 {
		c.QueueSize = DefaultQueueSize
	}
	if c.MaxConns <= 0 {
		c.MaxConns = defaultMaxConns
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Stats reports the state of the write path, for /v1/health and logs.
type Stats struct {
	QueueLen    int       // items waiting to be written
	Dropped     int64     // items discarded: queue full, store closed, or write failed
	Written     int64     // items persisted (a snapshot counts as one item)
	LastWriteAt time.Time // last successful write
	LastError   string    // last write failure; "" once a later write succeeds
}

// Store is the Postgres-backed persistence layer. It is safe for concurrent
// use. It implements api.Keys and api.RequestLogger.
type Store struct {
	cfg  Config
	log  *slog.Logger
	pool *pool
	keys *keyCache

	jobs chan job
	now  func() time.Time
	rand func() float64

	lastSnap atomic.Int64 // unix nanos of the last accepted snapshot; 0 = none
	closed   atomic.Bool

	dropped   atomic.Int64
	written   atomic.Int64
	dropWarn  rateGate
	staleWarn rateGate
	keyFlight singleflight.Group

	mu          sync.Mutex // guards the fields below
	lastWriteAt time.Time
	lastErr     string
	errLog      errLogger
	started     bool
	running     bool
	stop        chan struct{} // closed by Close to stop Run
	runDone     chan struct{} // closed when Run returns

	closeOnce sync.Once
}

// Open connects to Postgres, verifies the connection and applies pending
// migrations. Call Run to start the writer and Close when done.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DatabaseURL == "" {
		return nil, errors.New("store: empty DatabaseURL")
	}
	pcfg, err := connConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	s := newStore(cfg, pcfg)
	if err := s.Ping(ctx); err != nil {
		s.pool.close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		s.pool.close()
		return nil, err
	}
	return s, nil
}

// connConfig parses a connection string and adapts it to PgBouncer in
// transaction mode: queries use the extended protocol with the unnamed
// statement (one round trip, no server-side statement cache), so a query
// never depends on a statement prepared on another server connection.
func connConfig(url string) (*pgx.ConnConfig, error) {
	pcfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: parse DatabaseURL: %w", err)
	}
	pcfg.DefaultQueryExecMode = pgx.QueryExecModeExec
	pcfg.StatementCacheCapacity = 0
	pcfg.DescriptionCacheCapacity = 0
	if _, ok := pcfg.RuntimeParams["application_name"]; !ok {
		pcfg.RuntimeParams["application_name"] = "coinstack"
	}
	return pcfg, nil
}

// newStore builds a Store without touching the database.
func newStore(cfg Config, pcfg *pgx.ConnConfig) *Store {
	cfg = cfg.withDefaults()
	return &Store{
		cfg:     cfg,
		log:     cfg.Logger,
		pool:    newPool(pcfg, cfg.MaxConns),
		keys:    newKeyCache(cfg.KeyCacheTTL, min(cfg.KeyCacheTTL, maxNegativeKeyTTL), maxCachedKeys),
		jobs:    make(chan job, cfg.QueueSize),
		now:     time.Now,
		rand:    rand.Float64,
		stop:    make(chan struct{}),
		runDone: make(chan struct{}),
		errLog:  errLogger{log: cfg.Logger},
	}
}

// Ping checks that the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.withConn(ctx, func(c *pgx.Conn) error { return c.Ping(ctx) })
}

// Stats returns a snapshot of the write-path counters.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		QueueLen:    len(s.jobs),
		Dropped:     s.dropped.Load(),
		Written:     s.written.Load(),
		LastWriteAt: s.lastWriteAt,
		LastError:   s.lastErr,
	}
}

// Close stops accepting writes, flushes queued items (waiting at most about
// five seconds) and closes the connection pool. If Run is still running it
// is stopped first. Close is safe to call more than once and after Run has
// returned.
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.mu.Lock()
		running := s.running
		s.running = false
		close(s.stop)
		s.mu.Unlock()

		if running {
			select {
			case <-s.runDone:
			case <-time.After(closeFlushTimeout + time.Second):
				s.log.Warn("store: writer did not stop in time")
			}
		} else {
			s.drain(nil)
		}
		s.pool.close()
	})
}
