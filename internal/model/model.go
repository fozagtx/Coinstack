// Package model holds the plain data types shared by every part of
// CoinStack: the CMC client, the poller, the resolver, the store and the
// HTTP handlers. It has no dependencies beyond the standard library.
package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Quote is the flattened, latest market data for one asset, priced in USD.
// It is the unit the cache stores, the store persists and the API serves.
// Quotes are immutable once published in a snapshot; copy before changing.
type Quote struct {
	ID                int64
	Symbol            string
	Name              string
	Slug              string
	Rank              int // CMC rank; 0 when CMC reports none
	Price             float64
	MarketCap         float64
	Volume24h         float64
	Change1hPct       float64
	Change24hPct      float64
	Change7dPct       float64
	CirculatingSupply float64
	TotalSupply       float64
	MaxSupply         *float64 // nil when uncapped or unknown
	DateAdded         time.Time
	Tags              []string
	LastUpdated       time.Time // CMC's own last_updated for this quote
	FetchedAt         time.Time // when this service received it from CMC
}

// MapEntry is one row of CMC's /cryptocurrency/map: the identity of an
// asset, used to build the symbol/name/slug -> id resolver index.
type MapEntry struct {
	ID                  int64
	Symbol              string
	Name                string
	Slug                string
	Rank                int // CMC rank; 0 when unranked
	IsActive            bool
	FirstHistoricalData time.Time
	Platform            string // parent chain name for tokens ("Ethereum"); "" for coins
}

// Info is slower-moving metadata from CMC's /cryptocurrency/info.
type Info struct {
	ID           int64
	Symbol       string
	Name         string
	Slug         string
	Category     string // "coin" or "token"
	Description  string
	Tags         []string
	DateAdded    time.Time
	DateLaunched time.Time
	Website      string
	Platform     string
	FetchedAt    time.Time
}

// KeyUsage is the plan and usage information from CMC's /v1/key/info.
type KeyUsage struct {
	CreditLimitMonthly      int
	CreditLimitMonthlyReset string // CMC's human description, e.g. "In 19 days, 2 hours"
	RateLimitMinute         int
	CreditsUsedToday        int
	CreditsUsedMonth        int
	CreditsLeftMonth        int
	FetchedAt               time.Time
}

// PollRun records one upstream call made by the poller, for poll_runs
// history, credit accounting and /v1/health.
type PollRun struct {
	Kind          string // "listings", "quotes", "map", "info", "new", "fx", "keyinfo"
	StartedAt     time.Time
	FinishedAt    time.Time
	OK            bool
	HTTPStatus    int // 0 when the request never got a response
	CreditsUsed   int
	AssetsFetched int
	Error         string
}

// MarketStatus is the poller's view of its own health.
type MarketStatus struct {
	LastPollAt         time.Time // last attempt of the top-N listings poll
	LastSuccessAt      time.Time // last fully successful top-N poll
	LastError          string    // "" when the last poll succeeded
	CacheSize          int       // assets in the published snapshot
	OnDemandCacheSize  int       // assets held by the on-demand cache
	TopN               int
	PollInterval       time.Duration
	CreditsUsedToday   int // per CMC /key/info when available, else counted locally
	CreditsUsedMonth   int
	CreditLimitMonthly int // 0 when unknown
	UpstreamCalls      int64
	UpstreamErrors     int64
	MapFetchedAt       time.Time // last successful resolver-map refresh
}

// Candidate is a short description of an asset, returned to agents when a
// query is ambiguous or not found, and by /v1/resolve.
type Candidate struct {
	ID       int64  `json:"id"`
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Rank     int    `json:"rank,omitempty"`
	Platform string `json:"platform,omitempty"`
	Match    string `json:"match,omitempty"` // "id", "symbol", "slug", "name", "fuzzy"
}

// CandidateOf builds a Candidate from a MapEntry.
func CandidateOf(e MapEntry, match string) Candidate {
	return Candidate{ID: e.ID, Symbol: e.Symbol, Name: e.Name, Slug: e.Slug, Rank: e.Rank, Platform: e.Platform, Match: match}
}

// Resolution is the result of turning one agent query into one asset.
type Resolution struct {
	Asset     MapEntry
	MatchedBy string // "id", "symbol", "slug", "name"
	// Alternatives lists the other assets that share the queried symbol when
	// the resolver picked Asset because it clearly dominates them (for
	// example ETH by rank vs. a rank-3000 token also called ETH). Empty when
	// the query was unique.
	Alternatives []Candidate
}

// AmbiguousError is returned when a query matches several assets and none
// clearly dominates. Candidates are ordered by rank (unranked last).
type AmbiguousError struct {
	Query      string
	Candidates []Candidate
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%q matches %d assets", e.Query, len(e.Candidates))
}

// NotFoundError is returned when a query matches no active asset.
// Suggestions holds the closest matches, best first (may be empty).
type NotFoundError struct {
	Query       string
	Suggestions []Candidate
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("no asset matches %q", e.Query)
}

// ErrUpstreamUnavailable is returned (wrapped) by the market layer when CMC
// could not be reached or refused the call and no usable data is cached.
var ErrUpstreamUnavailable = errors.New("upstream unavailable")

// ErrBudgetExhausted is returned (wrapped) when an on-demand upstream call
// is refused because the per-minute credit budget for on-demand lookups is
// spent. RetryAfter on the wrapping error says when to try again.
var ErrBudgetExhausted = errors.New("on-demand upstream budget exhausted")

// RetryAfterError wraps an error with a hint for how long to wait.
type RetryAfterError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *RetryAfterError) Error() string { return e.Err.Error() }
func (e *RetryAfterError) Unwrap() error { return e.Err }

// APIKey describes one agent API key. The raw key is never stored.
type APIKey struct {
	ID        int64
	Owner     string
	RateLimit int // requests per minute; 0 means use the server default
	Active    bool
	CreatedAt time.Time
}

// ErrKeyNotFound is returned by key stores when a key is unknown or revoked.
var ErrKeyNotFound = errors.New("api key not found")

// RequestLog is one sampled API request, for request_log.
type RequestLog struct {
	TS        time.Time
	KeyID     int64 // 0 when unauthenticated
	Endpoint  string
	Status    int
	LatencyMS float64
}

// NormalizeCurrency upper-cases and trims a currency code.
func NormalizeCurrency(c string) string { return strings.ToUpper(strings.TrimSpace(c)) }
