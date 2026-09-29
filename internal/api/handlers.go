package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/fozagtx/coinstack/internal/discover"
)

// handleGems serves GET /v1/gems: scored altcoin candidates.
func (s *Server) handleGems(w http.ResponseWriter, r *http.Request) *apiError {
	q, e := parseQuery(r, pathGems)
	if e != nil {
		return e
	}
	maxCap, e := q.number("max_market_cap")
	if e != nil {
		return e
	}
	minCap, e := q.number("min_market_cap")
	if e != nil {
		return e
	}
	minVol, e := q.number("min_volume")
	if e != nil {
		return e
	}
	days, e := q.int("listed_within_days")
	if e != nil {
		return e
	}
	sector, e := q.str("sector")
	if e != nil {
		return e
	}
	pumped, e := q.boolean("include_pumped")
	if e != nil {
		return e
	}
	limit, e := q.int("limit")
	if e != nil {
		return e
	}
	res, err := s.engine.Gems(r.Context(), discover.GemsParams{
		MaxMarketCap:     maxCap,
		MinMarketCap:     minCap,
		MinVolume:        minVol,
		ListedWithinDays: days,
		Sector:           sector,
		IncludePumped:    pumped,
		Limit:            limit,
	})
	if err != nil {
		return s.resolveError("", "", err)
	}
	env := &envelope{
		Note:         disclaimer,
		HistoryHours: &res.HistoryHours,
		Data:         res.Items,
		Warnings:     toWarnings(res.Warnings),
	}
	return s.respond(w, env, res.AsOf, true)
}

// handleScreen serves GET /v1/screen: uniscored filtering over the top-N.
func (s *Server) handleScreen(w http.ResponseWriter, r *http.Request) *apiError {
	q, e := parseQuery(r, pathScreen)
	if e != nil {
		return e
	}
	var p discover.ScreenParams
	var err *apiError
	setNum := func(name string, dst *float64, flag *bool) bool {
		v, ok, ae := q.optNumber(name)
		if ae != nil {
			err = ae
			return false
		}
		*dst, *flag = v, ok
		return true
	}
	if !setNum("min_market_cap", &p.MinMarketCap, new(bool)) ||
		!setNum("max_market_cap", &p.MaxMarketCap, new(bool)) ||
		!setNum("min_volume", &p.MinVolume, new(bool)) ||
		!setNum("max_volume", &p.MaxVolume, new(bool)) ||
		!setNum("min_turnover", &p.MinTurnover, new(bool)) ||
		!setNum("min_change_1h_pct", &p.MinChange1h, &p.HasMin1h) ||
		!setNum("max_change_1h_pct", &p.MaxChange1h, &p.HasMax1h) ||
		!setNum("min_change_24h_pct", &p.MinChange24h, &p.HasMin24h) ||
		!setNum("max_change_24h_pct", &p.MaxChange24h, &p.HasMax24h) ||
		!setNum("min_change_7d_pct", &p.MinChange7d, &p.HasMin7d) ||
		!setNum("max_change_7d_pct", &p.MaxChange7d, &p.HasMax7d) {
		return err
	}
	tag, e := q.str("tag")
	if e != nil {
		return e
	}
	for part := range strings.SplitSeq(tag, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p.Tags = append(p.Tags, part)
		if len(p.Tags) > 10 {
			return invalidParam("tag", "Parameter tag accepts at most 10 tags.",
				"Remove some tags; tag matches assets carrying any of the listed tags.", nil)
		}
	}
	if p.ListedWithinDays, e = q.int("listed_within_days"); e != nil {
		return e
	}
	if p.ExcludeStablecoins, e = q.boolean("exclude_stablecoins"); e != nil {
		return e
	}
	if p.Sort, e = q.enum("sort"); e != nil {
		return e
	}
	if p.Order, e = q.enum("order"); e != nil {
		return e
	}
	if p.Order == "" {
		p.Order = "desc"
		if p.Sort == "rank" {
			p.Order = "asc"
		}
	}
	if p.Limit, e = q.int("limit"); e != nil {
		return e
	}
	res, rerr := s.engine.Screen(r.Context(), p)
	if rerr != nil {
		return s.resolveError("", "", rerr)
	}
	return s.respond(w, &envelope{Data: res.Items}, res.AsOf, true)
}

// handleClimbers serves GET /v1/climbers: biggest rank moves over a window.
func (s *Server) handleClimbers(w http.ResponseWriter, r *http.Request) *apiError {
	q, e := parseQuery(r, pathClimbers)
	if e != nil {
		return e
	}
	window, e := q.enum("window")
	if e != nil {
		return e
	}
	dir, e := q.enum("direction")
	if e != nil {
		return e
	}
	minVol, e := q.number("min_volume")
	if e != nil {
		return e
	}
	maxCap, e := q.number("max_market_cap")
	if e != nil {
		return e
	}
	limit, e := q.int("limit")
	if e != nil {
		return e
	}
	win := 24 * time.Hour
	if window == "7d" {
		win = 7 * 24 * time.Hour
	}
	res, err := s.engine.Climbers(r.Context(), discover.ClimbersParams{
		Window:       win,
		Down:         dir == "down",
		MinVolume:    minVol,
		MaxMarketCap: maxCap,
		Limit:        limit,
	})
	if err != nil {
		return s.resolveError("", "", err)
	}
	items := res.Items
	if items == nil {
		items = []discover.ClimberItem{}
	}
	env := &envelope{
		Note:         disclaimer,
		HistoryHours: &res.HistoryHours,
		Data:         items,
		Warnings:     toWarnings(res.Warnings),
	}
	return s.respond(w, env, res.AsOf, true)
}

// handleNewListings serves GET /v1/new-listings: freshly listed assets.
func (s *Server) handleNewListings(w http.ResponseWriter, r *http.Request) *apiError {
	q, e := parseQuery(r, pathNewListings)
	if e != nil {
		return e
	}
	days, e := q.int("days")
	if e != nil {
		return e
	}
	minVol, e := q.number("min_volume")
	if e != nil {
		return e
	}
	limit, e := q.int("limit")
	if e != nil {
		return e
	}
	res, err := s.engine.NewListings(r.Context(), discover.NewListingsParams{
		Days:      days,
		MinVolume: minVol,
		Limit:     limit,
	})
	if err != nil {
		return s.resolveError("", "", err)
	}
	if res.Items == nil {
		res.Items = []discover.NewListingItem{}
	}
	env := &envelope{
		Note:         disclaimer,
		HistoryHours: &res.HistoryHours,
		Data:         res.Items,
		Warnings:     toWarnings(res.Warnings),
	}
	return s.respond(w, env, res.AsOf, true)
}

// handleSectors serves GET /v1/sectors: tag aggregates or one sector's detail.
func (s *Server) handleSectors(w http.ResponseWriter, r *http.Request) *apiError {
	q, e := parseQuery(r, pathSectors)
	if e != nil {
		return e
	}
	sortBy, e := q.enum("sort")
	if e != nil {
		return e
	}
	minMembers, e := q.int("min_members")
	if e != nil {
		return e
	}
	sector, e := q.str("sector")
	if e != nil {
		return e
	}
	limit, e := q.int("limit")
	if e != nil {
		return e
	}
	res, err := s.engine.Sectors(r.Context(), discover.SectorsParams{
		Sort:       sortBy,
		MinMembers: minMembers,
		Sector:     sector,
		Limit:      limit,
	})
	if err != nil {
		return s.resolveError("sector", sector, err)
	}
	env := &envelope{Note: disclaimer}
	if res.Detail != nil {
		env.Data = res.Detail
	} else {
		if res.Sectors == nil {
			res.Sectors = []discover.SectorOut{}
		}
		env.Data = res.Sectors
	}
	return s.respond(w, env, res.AsOf, true)
}

// handleAsset serves GET /v1/asset: one asset's detail plus its signals.
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) *apiError {
	q, e := parseQuery(r, pathAsset)
	if e != nil {
		return e
	}
	query, e := q.str("asset")
	if e != nil {
		return e
	}
	res, err := s.engine.Asset(r.Context(), query)
	if err != nil {
		return s.resolveError("asset", query, err)
	}
	env := &envelope{
		Note:     disclaimer,
		Data:     res.Item,
		Warnings: toWarnings(res.Warnings),
	}
	return s.respond(w, env, res.AsOf, true)
}

// handleResolve serves GET /v1/resolve: candidate list for a query.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) *apiError {
	q, e := parseQuery(r, pathResolve)
	if e != nil {
		return e
	}
	query, e := q.str("query")
	if e != nil {
		return e
	}
	limit, e := q.int("limit")
	if e != nil {
		return e
	}
	res, asOf, err := s.engine.Resolve(r.Context(), query, limit)
	if err != nil {
		return s.resolveError("query", query, err)
	}
	return s.respond(w, &envelope{Data: res}, asOf, false)
}

// health is the /v1/health body.
type health struct {
	Status                 string                     `json:"status"`
	Version                string                     `json:"version"`
	Preset                 string                     `json:"preset,omitempty"`
	UptimeSeconds          int64                      `json:"uptime_seconds"`
	LastPollAt             *string                    `json:"last_poll_at"`
	LastSuccessAt          *string                    `json:"last_success_at"`
	LastError              string                     `json:"last_error"`
	AgeSeconds             int64                      `json:"age_seconds"`
	CacheSize              int                        `json:"cache_size"`
	OnDemandCacheSize      int                        `json:"on_demand_cache_size"`
	ResolverAssets         int                        `json:"resolver_assets"`
	TopN                   int                        `json:"top_n"`
	PollIntervalSeconds    int64                      `json:"poll_interval_seconds"`
	HistoryAssets          int                        `json:"history_assets"`
	HistoryHours           int                        `json:"history_hours"`
	CreditsUsedToday       int                        `json:"credits_used_today"`
	CreditsUsedMonth       int                        `json:"credits_used_month"`
	CreditLimitMonthly     int                        `json:"credit_limit_monthly"`
	ProjectedCreditsPerDay int                        `json:"projected_credits_per_day"`
	UpstreamCalls          int64                      `json:"upstream_calls"`
	UpstreamErrors         int64                      `json:"upstream_errors"`
	RequestsTotal          int64                      `json:"requests_total"`
	RequestsByStatus       map[string]int64           `json:"requests_by_status"`
	Endpoints              map[string]endpointSummary `json:"endpoints,omitempty"`
	Telegram               TelegramStatus             `json:"telegram"`
}

// handleHealth serves GET /v1/health; it is the only endpoint that answers
// 503 with a normal (non-error) body when the service is down.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) *apiError {
	st := s.market.Status()
	total, byStatus, endpoints := s.metrics.summary()
	age := ageSeconds(s.now(), st.LastSuccessAt)
	status := "ok"
	httpStatus := http.StatusOK
	switch {
	case st.LastSuccessAt.IsZero() || st.CacheSize == 0:
		status, httpStatus = "down", http.StatusServiceUnavailable
	case time.Duration(age)*time.Second > s.cfg.MaxStale:
		status, httpStatus = "down", http.StatusServiceUnavailable
	case time.Duration(age)*time.Second > s.cfg.StaleAfter:
		status = "degraded"
	}
	var tg TelegramStatus
	if s.cfg.Telegram != nil {
		tg = s.cfg.Telegram()
	}
	body := health{
		Status:                 status,
		Version:                s.cfg.Version,
		Preset:                 s.cfg.Preset,
		UptimeSeconds:          int64(s.now().Sub(s.started) / time.Second),
		LastPollAt:             optionalTime(st.LastPollAt),
		LastSuccessAt:          optionalTime(st.LastSuccessAt),
		LastError:              st.LastError,
		AgeSeconds:             age,
		CacheSize:              st.CacheSize,
		OnDemandCacheSize:      st.OnDemandCacheSize,
		ResolverAssets:         s.resolver.Size(),
		TopN:                   st.TopN,
		PollIntervalSeconds:    int64(st.PollInterval / time.Second),
		HistoryAssets:          st.HistoryAssets,
		HistoryHours:           st.HistoryHours,
		CreditsUsedToday:       st.CreditsUsedToday,
		CreditsUsedMonth:       st.CreditsUsedMonth,
		CreditLimitMonthly:     st.CreditLimitMonthly,
		ProjectedCreditsPerDay: st.ProjectedCreditsPerDay,
		UpstreamCalls:          st.UpstreamCalls,
		UpstreamErrors:         st.UpstreamErrors,
		RequestsTotal:          total,
		RequestsByStatus:       byStatus,
		Endpoints:              endpoints,
		Telegram:               tg,
	}
	s.writeJSON(w, httpStatus, body, "no-store")
	return nil
}

// handleOpenAPI serves the OpenAPI document, cached by ETag.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) *apiError {
	if match := r.Header.Get("If-None-Match"); match != "" && match == s.openETag {
		w.Header().Set("ETag", s.openETag)
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	w.Header().Set("ETag", s.openETag)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.openapi)
	return nil
}

// handleDocs serves the HTML docs landing page.
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) *apiError {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.docs)
	return nil
}
