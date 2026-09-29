package api

import (
	"context"
	"time"

	"github.com/fozagtx/coinstack/internal/cache"
	"github.com/fozagtx/coinstack/internal/model"
)

// These interfaces are the contract between the HTTP layer and the rest of
// the service. They are implemented by internal/market (Market),
// internal/resolve (Resolver) and internal/store (Keys, RequestLogger).
// Do not change a signature here without updating every implementation.

// Market is the market-data side the handlers read from.
type Market interface {
	// Snapshot returns the latest published top-N snapshot; never nil.
	Snapshot() *cache.Snapshot

	// Quotes returns USD quotes for ids: from the snapshot when present,
	// otherwise from a short-lived on-demand cache, otherwise via one
	// coalesced upstream call. Ids CMC does not know are absent from the
	// map. On upstream failure it returns whatever it has (possibly expired
	// on-demand entries, with their true LastUpdated) together with an error
	// wrapping model.ErrUpstreamUnavailable or model.ErrBudgetExhausted
	// (optionally inside a *model.RetryAfterError).
	Quotes(ctx context.Context, ids []int64) (map[int64]model.Quote, error)

	// Info returns metadata for ids, cached for about a day. Best effort:
	// callers treat an error as "no extra metadata".
	Info(ctx context.Context, ids []int64) (map[int64]model.Info, error)

	// NewListings returns assets first listed within the last `days` days,
	// newest first, with USD quotes.
	NewListings(ctx context.Context, days int) ([]model.Quote, error)

	// FXRate returns units of currency per 1 USD and when the rate was
	// fetched. USD always returns (1, now, true).
	FXRate(currency string) (rate float64, asOf time.Time, ok bool)

	// Currencies lists the currency codes FXRate can currently serve,
	// including "USD".
	Currencies() []string

	// Status reports poller health for /v1/health.
	Status() model.MarketStatus
}

// Resolver turns agent input (symbol, name, slug or numeric CMC id) into
// CMC assets.
type Resolver interface {
	// Resolve maps one query to exactly one active asset. It returns a
	// *model.AmbiguousError when several assets match and none clearly
	// dominates, and a *model.NotFoundError (with suggestions) when nothing
	// matches.
	Resolve(query string) (model.Resolution, error)

	// Search returns up to limit candidates for query, best first: exact id,
	// symbol, slug and name matches, then fuzzy matches.
	Search(query string, limit int) []model.Candidate

	// Size returns the number of assets in the index.
	Size() int

	// BuiltAt returns when the index was built from CMC's map.
	BuiltAt() time.Time
}

// Keys authenticates agent API keys. Lookups must not hit the database on
// every request (cache results briefly).
type Keys interface {
	// Lookup returns the key record for a raw API key, or
	// model.ErrKeyNotFound when it is unknown or revoked.
	Lookup(ctx context.Context, rawKey string) (model.APIKey, error)
}

// RequestLogger records sampled requests. LogRequest must never block.
type RequestLogger interface {
	LogRequest(model.RequestLog)
}
