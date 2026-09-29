package api

import (
	"context"

	"github.com/fozagtx/coinstack/internal/discover"
	"github.com/fozagtx/coinstack/internal/model"
)

// These interfaces are the contract between the HTTP layer and the rest of
// the service. Market and Resolver are the interfaces defined by
// internal/discover, which *market.Market and the resolve package
// implement; Keys and RequestLogger are implemented by internal/store.

// Market is the market-data side the handlers read from.
type Market = discover.Market

// Resolver turns agent input (symbol, name, slug or numeric CMC id) into
// CMC assets.
type Resolver = discover.Resolver

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
