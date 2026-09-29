// Package api is CoinStack's HTTP layer: the agent-facing REST endpoints,
// their middleware (auth, per-key rate limiting, metrics, logging), the
// embedded OpenAPI document and the docs landing page. It reads market data
// only through the interfaces in deps.go.
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Route paths served by the API.
const (
	pathDocs      = "/"
	pathPrice     = "/v1/price"
	pathAsset     = "/v1/asset"
	pathCompare   = "/v1/compare"
	pathMovers    = "/v1/movers"
	pathNewTokens = "/v1/new-tokens"
	pathResolve   = "/v1/resolve"
	pathOpenAPI   = "/v1/openapi.json"
	pathHealth    = "/v1/health"
)

// Config tunes the HTTP layer. Zero values get the documented defaults.
type Config struct {
	// Version is reported by /v1/health; "dev" when empty.
	Version string
	// DefaultRateLimit is the limit, in requests per minute, for keys whose
	// RateLimit is 0; 60 when zero.
	DefaultRateLimit int
	// StaleAfter is the data age beyond which responses carry a stale_data
	// warning; 180 s when zero.
	StaleAfter time.Duration
	// MaxStale is the data age beyond which data endpoints answer 503
	// upstream_unavailable with the old data attached; 30 min when zero.
	MaxStale time.Duration
	// TopN is the size of the top-N cache, shown in the docs and health.
	TopN int
	// AuthDisabled turns off API-key checks and rate limiting. For local
	// development only; Keys may be nil then.
	AuthDisabled bool
	// Now returns the current time; time.Now when nil.
	Now func() time.Time
	// Logger receives errors and, at Debug level, one access-log line per
	// request; slog.Default() when nil.
	Logger *slog.Logger
}

// Server is the CoinStack HTTP API. Create one with New and serve its
// Handler. It is safe for concurrent use.
type Server struct {
	cfg      Config
	market   Market
	resolver Resolver
	keys     Keys
	reqlog   RequestLogger
	log      *slog.Logger
	started  time.Time
	limiter  *keyLimiter
	metrics  *metrics
	reqIDs   *requestIDs
	openapi  openAPIDoc
	docs     docsPage
	router   chi.Router
}

// handlerFunc is an endpoint handler: it writes a success response itself
// and returns an error for the shared error writer.
type handlerFunc func(w http.ResponseWriter, r *http.Request) *apiError

// route is one registered endpoint.
type route struct {
	path    string
	public  bool
	handler handlerFunc
}

// New builds a Server. Market and Resolver are required, as is Keys unless
// cfg.AuthDisabled is set; New panics on a missing dependency. reqlog may
// be nil.
func New(cfg Config, m Market, r Resolver, keys Keys, reqlog RequestLogger) *Server {
	if m == nil || r == nil {
		panic("api: New needs a Market and a Resolver")
	}
	if keys == nil && !cfg.AuthDisabled {
		panic("api: New needs Keys unless Config.AuthDisabled is set")
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.DefaultRateLimit <= 0 {
		cfg.DefaultRateLimit = 60
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 180 * time.Second
	}
	if cfg.MaxStale <= 0 {
		cfg.MaxStale = 30 * time.Minute
	}
	if cfg.MaxStale < cfg.StaleAfter {
		cfg.MaxStale = cfg.StaleAfter
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{
		cfg:      cfg,
		market:   m,
		resolver: r,
		keys:     keys,
		reqlog:   reqlog,
		log:      cfg.Logger,
		started:  cfg.Now(),
		limiter:  newKeyLimiter(10000, 15*time.Minute),
		reqIDs:   newRequestIDs(),
		openapi:  newOpenAPIDoc(),
		docs:     newDocsPage(cfg.Version, cfg.TopN),
	}
	routes := s.routes()
	names := make([]string, 0, len(routes))
	for _, rt := range routes {
		names = append(names, rt.path)
	}
	s.metrics = newMetrics(names)
	s.router = s.buildRouter(routes)
	return s
}

// Handler returns the HTTP handler serving every route.
func (s *Server) Handler() http.Handler { return s.router }

func (s *Server) routes() []route {
	return []route{
		{path: pathDocs, public: true, handler: s.handleDocs},
		{path: pathOpenAPI, public: true, handler: s.handleOpenAPI},
		{path: pathHealth, public: true, handler: s.handleHealth},
		{path: pathPrice, handler: s.handlePrice},
		{path: pathAsset, handler: s.handleAsset},
		{path: pathCompare, handler: s.handleCompare},
		{path: pathMovers, handler: s.handleMovers},
		{path: pathNewTokens, handler: s.handleNewTokens},
		{path: pathResolve, handler: s.handleResolve},
	}
}

func (s *Server) buildRouter(routes []route) chi.Router {
	r := chi.NewRouter()
	r.Use(s.instrument, s.recoverer, middleware.StripSlashes)
	r.NotFound(s.wrap(s.handleNotFound))
	r.MethodNotAllowed(s.wrap(s.handleMethodNotAllowed))
	protected := r.With(s.authenticate, s.rateLimit)
	for _, rt := range routes {
		if rt.public {
			r.Get(rt.path, s.wrap(rt.handler))
		} else {
			protected.Get(rt.path, s.wrap(rt.handler))
		}
	}
	return r
}

// wrap adapts a handlerFunc to http.HandlerFunc, writing returned errors.
func (s *Server) wrap(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if e := h(w, r); e != nil {
			s.writeError(w, e)
		}
	}
}

func (s *Server) now() time.Time { return s.cfg.Now() }

// topN is the configured cache size, falling back to what the market reports.
func (s *Server) topN() int {
	if s.cfg.TopN > 0 {
		return s.cfg.TopN
	}
	return s.market.Status().TopN
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) *apiError {
	return newError(http.StatusNotFound, codeNotFound, "No endpoint at this path.",
		"Use one of /v1/price, /v1/asset, /v1/compare, /v1/movers, /v1/new-tokens or /v1/resolve; GET /v1/openapi.json describes them all.")
}

func (s *Server) handleMethodNotAllowed(w http.ResponseWriter, r *http.Request) *apiError {
	w.Header().Set("Allow", http.MethodGet)
	return newError(http.StatusMethodNotAllowed, codeMethodNotAllowed,
		"Method "+r.Method+" is not allowed here.", "Use GET; every CoinStack endpoint is read-only.")
}
