package main

import (
	"context"
	"crypto/sha256"

	"github.com/fozagtx/coinstack/internal/config"
	"github.com/fozagtx/coinstack/internal/model"
)

// dbKeys is the subset of *store.Store used for key lookups.
type dbKeys interface {
	Lookup(ctx context.Context, raw string) (model.APIKey, error)
}

// keyChain checks keys from COINSTACK_API_KEYS first, then the database
// (db is nil when running memory-only).
type keyChain struct {
	static map[[32]byte]model.APIKey
	db     dbKeys
}

func newKeyChain(static []config.StaticKey, db dbKeys) *keyChain {
	kc := &keyChain{static: make(map[[32]byte]model.APIKey, len(static))}
	for i, k := range static {
		// Negative ids keep static keys apart from database key ids.
		kc.static[sha256.Sum256([]byte(k.Key))] = model.APIKey{ID: -int64(i + 1), Owner: k.Owner, RateLimit: k.RateLimit, Active: true}
	}
	kc.db = db
	return kc
}

func (kc *keyChain) Lookup(ctx context.Context, raw string) (model.APIKey, error) {
	if k, ok := kc.static[sha256.Sum256([]byte(raw))]; ok {
		return k, nil
	}
	if kc.db == nil {
		return model.APIKey{}, model.ErrKeyNotFound
	}
	return kc.db.Lookup(ctx, raw)
}
