package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fozagtx/coinstack/internal/model"
)

// API key format: "cs_" followed by 32 random bytes in lower-case hex.
const (
	keyPrefix      = "cs_"
	keyRandomBytes = 32
	rawKeyLen      = len(keyPrefix) + 2*keyRandomBytes
	keyPrefixLen   = len(keyPrefix) + 8 // stored in key_prefix to identify a key without revealing it

	maxCachedKeys = 10000            // per cache (found and not-found), bounding memory
	keyStaleGrace = 15 * time.Minute // how long past its TTL a found key may be served while the database errors
	lookupTimeout = 5 * time.Second
)

// HashKey returns the stored form of a raw API key: its SHA-256 in hex.
func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func newRawKey() (string, error) {
	b := make([]byte, keyRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return keyPrefix + hex.EncodeToString(b), nil
}

// wellFormedKey reports whether raw could be a key issued by CreateKey, so
// garbage never reaches the cache or the database.
func wellFormedKey(raw string) bool {
	if len(raw) != rawKeyLen || !strings.HasPrefix(raw, keyPrefix) {
		return false
	}
	for _, c := range raw[len(keyPrefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// CreateKey issues a new API key for owner. rateLimit is requests per
// minute (0 = server default). The raw key is returned once and only its
// hash is stored.
func (s *Store) CreateKey(ctx context.Context, owner string, rateLimit int) (string, model.APIKey, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return "", model.APIKey{}, errors.New("store: create key: empty owner")
	}
	if rateLimit < 0 {
		return "", model.APIKey{}, errors.New("store: create key: negative rate limit")
	}
	raw, err := newRawKey()
	if err != nil {
		return "", model.APIKey{}, fmt.Errorf("store: create key: %w", err)
	}
	hash := HashKey(raw)
	key := model.APIKey{Owner: owner, RateLimit: rateLimit, Active: true}
	err = s.pool.withConn(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `
			INSERT INTO api_keys (key_hash, key_prefix, owner, rate_limit)
			VALUES ($1::text, $2::text, $3::text, $4::integer)
			RETURNING id, created_at`,
			hash, raw[:keyPrefixLen], owner, rateLimit).Scan(&key.ID, &key.CreatedAt)
	})
	if err != nil {
		return "", model.APIKey{}, fmt.Errorf("store: create key: %w", err)
	}
	key.CreatedAt = key.CreatedAt.UTC()
	s.keys.forget(hash)
	return raw, key, nil
}

// RevokeKey revokes the key with id. Revoking an already revoked key is a
// no-op; an unknown id returns model.ErrKeyNotFound. This process stops
// accepting the key at once; other instances within KeyCacheTTL.
func (s *Store) RevokeKey(ctx context.Context, id int64) error {
	var hash string
	err := s.pool.withConn(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `
			UPDATE api_keys SET status = 'revoked', revoked_at = COALESCE(revoked_at, now())
			WHERE id = $1::bigint
			RETURNING key_hash`, id).Scan(&hash)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrKeyNotFound
	}
	if err != nil {
		return fmt.Errorf("store: revoke key: %w", err)
	}
	s.keys.forget(hash)
	return nil
}

// ListKeys returns every key, active and revoked, ordered by id.
func (s *Store) ListKeys(ctx context.Context) ([]model.APIKey, error) {
	var out []model.APIKey
	err := s.pool.withConn(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, "SELECT id, owner, rate_limit, status, created_at FROM api_keys ORDER BY id")
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, scanKey)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list keys: %w", err)
	}
	return out, nil
}

func scanKey(row pgx.CollectableRow) (model.APIKey, error) {
	var (
		k      model.APIKey
		status string
	)
	if err := row.Scan(&k.ID, &k.Owner, &k.RateLimit, &status, &k.CreatedAt); err != nil {
		return k, err
	}
	k.Active = status == "active"
	k.CreatedAt = k.CreatedAt.UTC()
	return k, nil
}

// Lookup returns the active key for raw, or model.ErrKeyNotFound when it is
// malformed, unknown or revoked. Results are cached (found keys for
// KeyCacheTTL, unknown ones briefly) and concurrent misses for one key share
// a single query. While the database is failing, a recently found key keeps
// being accepted for a grace period so an outage does not lock agents out.
func (s *Store) Lookup(ctx context.Context, raw string) (model.APIKey, error) {
	if !wellFormedKey(raw) {
		return model.APIKey{}, model.ErrKeyNotFound
	}
	hash := HashKey(raw)
	if key, found, ok := s.keys.get(hash, time.Now()); ok {
		if !found {
			return model.APIKey{}, model.ErrKeyNotFound
		}
		return key, nil
	}

	ch := s.keyFlight.DoChan(hash, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lookupTimeout)
		defer cancel()
		return s.lookupDB(ctx, hash)
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return model.APIKey{}, res.Err
		}
		return res.Val.(model.APIKey), nil
	case <-ctx.Done():
		return model.APIKey{}, ctx.Err()
	}
}

// lookupDB reads one key by hash and caches the outcome.
func (s *Store) lookupDB(ctx context.Context, hash string) (model.APIKey, error) {
	var key model.APIKey
	err := s.pool.withConn(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, "SELECT id, owner, rate_limit, status, created_at FROM api_keys WHERE key_hash = $1::text", hash)
		if err != nil {
			return err
		}
		key, err = pgx.CollectExactlyOneRow(rows, scanKey)
		return err
	})
	now := time.Now()
	switch {
	case err == nil && key.Active:
		s.keys.putFound(hash, key, now)
		return key, nil
	case err == nil || errors.Is(err, pgx.ErrNoRows):
		s.keys.putMissing(hash, now)
		return model.APIKey{}, model.ErrKeyNotFound
	}
	if stale, ok := s.keys.stale(hash, now); ok {
		if s.staleWarn.allow(now, warnEvery) {
			s.log.Warn("store: key lookup failed; serving cached key", "err", err)
		}
		return stale, nil
	}
	return model.APIKey{}, fmt.Errorf("store: lookup key: %w", err)
}

// keyCache is a bounded TTL cache of key lookups by hash. Found and
// not-found results live in separate maps so a flood of bogus keys cannot
// evict real ones.
type keyCache struct {
	foundTTL, missingTTL time.Duration
	limit                int

	mu      sync.Mutex
	found   map[string]foundEntry
	missing map[string]time.Time // hash -> expiry
}

type foundEntry struct {
	key     model.APIKey
	expires time.Time
}

func newKeyCache(foundTTL, missingTTL time.Duration, limit int) *keyCache {
	return &keyCache{
		foundTTL:   foundTTL,
		missingTTL: missingTTL,
		limit:      limit,
		found:      make(map[string]foundEntry),
		missing:    make(map[string]time.Time),
	}
}

// get returns a fresh cached result: ok is false on a miss or expiry.
func (c *keyCache) get(hash string, now time.Time) (key model.APIKey, found, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, hit := c.found[hash]; hit && now.Before(e.expires) {
		return e.key, true, true
	}
	if exp, hit := c.missing[hash]; hit && now.Before(exp) {
		return model.APIKey{}, false, true
	}
	return model.APIKey{}, false, false
}

// stale returns a found key whose TTL lapsed less than keyStaleGrace ago.
func (c *keyCache) stale(hash string, now time.Time) (model.APIKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, hit := c.found[hash]
	if !hit || !now.Before(e.expires.Add(keyStaleGrace)) {
		return model.APIKey{}, false
	}
	return e.key, true
}

func (c *keyCache) putFound(hash string, key model.APIKey, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.missing, hash)
	if _, hit := c.found[hash]; !hit && len(c.found) >= c.limit {
		evict(c.found, c.limit, func(e foundEntry) bool { return !now.Before(e.expires.Add(keyStaleGrace)) })
	}
	c.found[hash] = foundEntry{key: key, expires: now.Add(c.foundTTL)}
}

func (c *keyCache) putMissing(hash string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.found, hash)
	if _, hit := c.missing[hash]; !hit && len(c.missing) >= c.limit {
		evict(c.missing, c.limit, func(exp time.Time) bool { return !now.Before(exp) })
	}
	c.missing[hash] = now.Add(c.missingTTL)
}

func (c *keyCache) forget(hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.found, hash)
	delete(c.missing, hash)
}

// evict makes room in a full map: it removes expired entries, then
// arbitrary ones until the map is at most 90% full, so eviction runs at
// most once per limit/10 inserts.
func evict[V any](m map[string]V, limit int, expired func(V) bool) {
	for k, v := range m {
		if expired(v) {
			delete(m, k)
		}
	}
	target := limit - limit/10
	for k := range m {
		if len(m) <= target {
			break
		}
		delete(m, k)
	}
}
