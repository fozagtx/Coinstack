package store

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Pool tuning. Neon's pooler (PgBouncer) holds the real server connections,
// so a handful of client connections is plenty for one CoinStack instance.
const (
	defaultMaxConns = 5
	connectTimeout  = 10 * time.Second // Neon computes can take a few seconds to wake
	maxConnLifetime = 30 * time.Minute
	maxConnIdle     = 5 * time.Minute
	pingAfterIdle   = 30 * time.Second
	closeTimeout    = 2 * time.Second
)

var errPoolClosed = errors.New("store: connection pool closed")

// pool is a small connection pool over *pgx.Conn. pgxpool would be the
// natural choice, but its dependency (github.com/jackc/puddle/v2) is not in
// go.sum; this covers the few features the store needs: a hard cap on
// connections, LIFO reuse, lifetime/idle expiry and a liveness ping for
// connections that sat idle.
type pool struct {
	cfg *pgx.ConnConfig
	sem chan struct{} // one token per open-or-opening connection in use

	mu     sync.Mutex
	idle   []*poolConn
	closed bool
}

type poolConn struct {
	*pgx.Conn
	born      time.Time
	idleSince time.Time
}

func newPool(cfg *pgx.ConnConfig, maxConns int) *pool {
	if maxConns <= 0 {
		maxConns = defaultMaxConns
	}
	return &pool{cfg: cfg, sem: make(chan struct{}, maxConns)}
}

// acquire returns a live connection, waiting for a free slot if the pool is
// at capacity. The caller must pass it to release.
func (p *pool) acquire(ctx context.Context) (*poolConn, error) {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			<-p.sem
			return nil, errPoolClosed
		}
		var c *poolConn
		if n := len(p.idle); n > 0 {
			c = p.idle[n-1]
			p.idle[n-1] = nil
			p.idle = p.idle[:n-1]
		}
		p.mu.Unlock()

		if c == nil {
			c, err := p.dial(ctx)
			if err != nil {
				<-p.sem
				return nil, err
			}
			return c, nil
		}
		now := time.Now()
		if c.IsClosed() || now.Sub(c.born) > maxConnLifetime || now.Sub(c.idleSince) > maxConnIdle {
			closeConn(c)
			continue
		}
		if now.Sub(c.idleSince) > pingAfterIdle {
			if err := c.Ping(ctx); err != nil {
				closeConn(c)
				if ctx.Err() != nil {
					<-p.sem
					return nil, ctx.Err()
				}
				continue
			}
		}
		return c, nil
	}
}

func (p *pool) dial(ctx context.Context) (*poolConn, error) {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, p.cfg)
	if err != nil {
		return nil, err
	}
	return &poolConn{Conn: conn, born: time.Now()}, nil
}

// release returns c to the pool, or closes it when it is broken, still inside
// a transaction, or the pool has been closed.
func (p *pool) release(c *poolConn) {
	defer func() { <-p.sem }()
	if c.IsClosed() || c.PgConn().IsBusy() || c.PgConn().TxStatus() != 'I' {
		closeConn(c)
		return
	}
	c.idleSince = time.Now()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		closeConn(c)
		return
	}
	p.idle = append(p.idle, c)
	p.mu.Unlock()
}

// withConn runs fn with a pooled connection.
func (p *pool) withConn(ctx context.Context, fn func(*pgx.Conn) error) error {
	c, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	defer p.release(c)
	return fn(c.Conn)
}

// withTx runs fn inside a transaction, committing when fn returns nil and
// rolling back otherwise. Everything that depends on session state (temp
// tables, the COPY describe round trip) must run inside one, because Neon's
// pooler only pins a server connection for the duration of a transaction.
func (p *pool) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return p.withConn(ctx, func(c *pgx.Conn) error {
		return pgx.BeginFunc(ctx, c, fn)
	})
}

// close closes idle connections and makes every later acquire fail.
// Connections in use are closed when they are released.
func (p *pool) close() {
	p.mu.Lock()
	idle := p.idle
	p.idle = nil
	p.closed = true
	p.mu.Unlock()
	for _, c := range idle {
		closeConn(c)
	}
}

func closeConn(c *poolConn) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_ = c.Close(ctx)
}
