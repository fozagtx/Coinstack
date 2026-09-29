package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fozagtx/coinstack/internal/model"
)

// maxTags caps the tags returned per asset.
const maxTags = 10

// maxCandidates caps the candidates listed in an error.
const maxCandidates = 10

// assetPrice is the price-level asset object.
type assetPrice struct {
	ID           int64   `json:"id"`
	Symbol       string  `json:"symbol"`
	Name         string  `json:"name"`
	Rank         int     `json:"rank,omitempty"`
	Price        float64 `json:"price"`
	MarketCap    float64 `json:"market_cap"`
	Volume24h    float64 `json:"volume_24h"`
	Change1hPct  float64 `json:"change_1h_pct"`
	Change24hPct float64 `json:"change_24h_pct"`
	Change7dPct  float64 `json:"change_7d_pct"`
	LastUpdated  string  `json:"last_updated"`
}

// newToken is a /v1/new-tokens item: price fields plus the listing date.
type newToken struct {
	assetPrice
	DateAdded *string `json:"date_added"`
}

// assetDetail is the /v1/asset and /v1/compare item.
type assetDetail struct {
	assetPrice
	Slug              string   `json:"slug"`
	CirculatingSupply float64  `json:"circulating_supply"`
	TotalSupply       float64  `json:"total_supply"`
	MaxSupply         *float64 `json:"max_supply"`
	DateAdded         *string  `json:"date_added"`
	Tags              []string `json:"tags"`
	Category          string   `json:"category,omitempty"`
	Platform          string   `json:"platform,omitempty"`
	Website           string   `json:"website,omitempty"`
}

// fx converts USD amounts into the requested currency.
type fx struct {
	code string
	rate float64
}

var usd = fx{code: "USD", rate: 1}

func (f fx) conv(v float64) float64 { return finite(v * f.rate) }

func priceOf(q *model.Quote, f fx) assetPrice {
	return assetPrice{
		ID:           q.ID,
		Symbol:       q.Symbol,
		Name:         q.Name,
		Rank:         max(q.Rank, 0),
		Price:        f.conv(q.Price),
		MarketCap:    f.conv(q.MarketCap),
		Volume24h:    f.conv(q.Volume24h),
		Change1hPct:  finite(q.Change1hPct),
		Change24hPct: finite(q.Change24hPct),
		Change7dPct:  finite(q.Change7dPct),
		LastUpdated:  formatTime(q.LastUpdated),
	}
}

func detailOf(q *model.Quote, f fx) assetDetail {
	d := assetDetail{
		assetPrice:        priceOf(q, f),
		Slug:              q.Slug,
		CirculatingSupply: finite(q.CirculatingSupply),
		TotalSupply:       finite(q.TotalSupply),
		DateAdded:         optionalTime(q.DateAdded),
		Tags:              capTags(q.Tags),
	}
	if q.MaxSupply != nil && !math.IsNaN(*q.MaxSupply) && !math.IsInf(*q.MaxSupply, 0) {
		v := *q.MaxSupply
		d.MaxSupply = &v
	}
	return d
}

func capTags(tags []string) []string {
	if len(tags) > maxTags {
		tags = tags[:maxTags]
	}
	return append(make([]string, 0, len(tags)), tags...)
}

func pricesOf(qs []model.Quote, f fx) []assetPrice {
	out := make([]assetPrice, len(qs))
	for i := range qs {
		out[i] = priceOf(&qs[i], f)
	}
	return out
}

func detailsOf(qs []model.Quote, f fx) []assetDetail {
	out := make([]assetDetail, len(qs))
	for i := range qs {
		out[i] = detailOf(&qs[i], f)
	}
	return out
}

// oldest returns the earliest LastUpdated among qs, or the zero time.
func oldest(qs []model.Quote) time.Time {
	var t time.Time
	for i := range qs {
		if t.IsZero() || qs[i].LastUpdated.Before(t) {
			t = qs[i].LastUpdated
		}
	}
	return t
}

// currency validates the currency parameter and returns its converter.
func (s *Server) currency(q *queryParams) (fx, *apiError) {
	raw, _ := q.value("currency")
	if raw == "" {
		return usd, nil
	}
	code := model.NormalizeCurrency(raw)
	if code == "USD" {
		return usd, nil
	}
	rate, _, ok := s.market.FXRate(code)
	if !ok || !(rate > 0) || math.IsInf(rate, 0) {
		allowed := s.market.Currencies()
		if len(allowed) == 0 {
			allowed = []string{"USD"}
		}
		return fx{}, invalidParam("currency", fmt.Sprintf("Currency %q is not supported.", truncate(raw, 20)),
			"Retry with one of allowed_values, or omit currency for USD.", allowed)
	}
	return fx{code: code, rate: rate}, nil
}

// resolvedAsset is one agent query and the asset it resolved to.
type resolvedAsset struct {
	query string
	res   model.Resolution
}

// resolveAll resolves queries in order, dropping queries that name an
// asset already in the list. It returns symbol_resolved_by_rank warnings
// for tickers the resolver settled by rank.
func (s *Server) resolveAll(param string, queries []string) ([]resolvedAsset, []warning, *apiError) {
	out := make([]resolvedAsset, 0, len(queries))
	var warns []warning
	for _, query := range queries {
		res, err := s.resolver.Resolve(query)
		if err != nil {
			return nil, nil, s.resolveError(param, query, len(queries) > 1, err)
		}
		dup := false
		for _, prev := range out {
			if prev.res.Asset.ID == res.Asset.ID {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		out = append(out, resolvedAsset{query: query, res: res})
		if len(res.Alternatives) > 0 {
			warns = append(warns, rankWarning(query, res))
		}
	}
	return out, warns, nil
}

func rankWarning(query string, res model.Resolution) warning {
	a := res.Asset
	rank := ""
	if a.Rank > 0 {
		rank = fmt.Sprintf(", rank %d", a.Rank)
	}
	return warning{
		Code: warnByRank,
		Message: fmt.Sprintf("%q matches %d assets; returned %s (id %d%s), the dominant one. Pass an id from candidates to get another.",
			query, len(res.Alternatives)+1, a.Name, a.ID, rank),
		Query:      query,
		ChosenID:   a.ID,
		Candidates: capCandidates(res.Alternatives),
	}
}

// resolveError maps a Resolver error to an API error.
func (s *Server) resolveError(param, query string, multi bool, err error) *apiError {
	var amb *model.AmbiguousError
	var nf *model.NotFoundError
	switch {
	case errors.As(err, &amb):
		e := newError(http.StatusConflict, codeAmbiguous,
			fmt.Sprintf("%q matches %d assets.", query, len(amb.Candidates)), ambiguousNextStep(param, query, multi, amb.Candidates))
		e.detail.Param, e.detail.Query = param, query
		e.detail.Candidates = capCandidates(amb.Candidates)
		return e
	case errors.As(err, &nf):
		next := fmt.Sprintf("Check the spelling, or search with GET /v1/resolve?query=%s.", url.QueryEscape(query))
		if len(nf.Suggestions) > 0 {
			c := nf.Suggestions[0]
			next = fmt.Sprintf("If you meant one of the candidates, retry with its id (e.g. %s=%d for %s); otherwise search with GET /v1/resolve?query=%s.",
				param, c.ID, c.Name, url.QueryEscape(query))
		}
		e := newError(http.StatusNotFound, codeAssetNotFound, fmt.Sprintf("No asset matches %q.", query), next)
		e.detail.Param, e.detail.Query = param, query
		e.detail.Candidates = capCandidates(nf.Suggestions)
		return e
	default:
		s.log.Error("resolver failed", "query", query, "err", err)
		return errInternal()
	}
}

func ambiguousNextStep(param, query string, multi bool, cands []model.Candidate) string {
	if len(cands) == 0 {
		return fmt.Sprintf("Retry with %s=<id> using a CMC id; GET /v1/resolve?query=%s lists the options.", param, url.QueryEscape(query))
	}
	c := cands[0]
	if multi {
		return fmt.Sprintf("Replace %q in %s with one of the candidates' ids, e.g. %d for %s.", query, param, c.ID, c.Name)
	}
	return fmt.Sprintf("Retry with %s=<id>, e.g. %s=%d for %s.", param, param, c.ID, c.Name)
}

// capCandidates returns at most maxCandidates, never nil.
func capCandidates(c []model.Candidate) []model.Candidate {
	if len(c) > maxCandidates {
		c = c[:maxCandidates]
	}
	return append(make([]model.Candidate, 0, len(c)), c...)
}

// loadQuotes fetches quotes for resolved assets, in their order. On
// upstream trouble it returns a 503 carrying whatever quotes did arrive,
// built into response data by attach. It adds an outside_top_n warning
// when some assets came from outside the top-N snapshot.
func (s *Server) loadQuotes(ctx context.Context, param string, assets []resolvedAsset, cur fx,
	attach func([]model.Quote) any) ([]model.Quote, []warning, *apiError) {
	ids := make([]int64, len(assets))
	for i, a := range assets {
		ids[i] = a.res.Asset.ID
	}
	got, err := s.market.Quotes(ctx, ids)
	quotes := make([]model.Quote, 0, len(assets))
	var missing []resolvedAsset
	for _, a := range assets {
		if q, ok := got[a.res.Asset.ID]; ok {
			quotes = append(quotes, q)
		} else {
			missing = append(missing, a)
		}
	}
	if len(missing) > 0 {
		if err != nil {
			return nil, nil, s.upstreamError(err, len(missing), quotes, cur, attach)
		}
		m := missing[0]
		e := newError(http.StatusNotFound, codeAssetNotFound,
			fmt.Sprintf("CoinMarketCap has no current market quote for %s (id %d).", m.res.Asset.Name, m.res.Asset.ID),
			fmt.Sprintf("The asset may be inactive or untracked; drop %q from %s and retry.", m.query, param))
		e.detail.Param, e.detail.Query = param, m.query
		e.detail.Candidates = []model.Candidate{}
		return nil, nil, e
	}
	if err != nil {
		s.log.Warn("serving cached quotes despite upstream error", "err", err)
	}
	var warns []warning
	snap := s.market.Snapshot()
	var outside []string
	for i := range quotes {
		if _, ok := snap.Get(quotes[i].ID); !ok {
			outside = append(outside, quotes[i].Symbol)
		}
	}
	if len(outside) > 0 {
		top := "top-N"
		if n := s.topN(); n > 0 {
			top = fmt.Sprintf("top %d", n)
		}
		warns = append(warns, warning{
			Code: warnOutsideTop,
			Message: fmt.Sprintf("%s %s outside the %s cache and %s fetched on demand.",
				strings.Join(outside, ", "), pick(len(outside) == 1, "is", "are"), top, pick(len(outside) == 1, "was", "were")),
		})
	}
	return quotes, warns, nil
}

// upstreamError maps a Market error that left some assets without data.
func (s *Server) upstreamError(err error, missing int, have []model.Quote, cur fx, attach func([]model.Quote) any) *apiError {
	var ra *model.RetryAfterError
	var retry time.Duration
	if errors.As(err, &ra) {
		retry = ra.RetryAfter
	}
	if errors.Is(err, model.ErrBudgetExhausted) {
		secs := retrySeconds(retry)
		e := newError(http.StatusServiceUnavailable, codeBudget,
			"The CoinMarketCap credit budget for on-demand lookups is spent for now.",
			fmt.Sprintf("Retry in %d seconds; assets in the top-N cache stay available meanwhile.", secs))
		e.detail.RetryAfterSeconds = secs
		return e
	}
	if !errors.Is(err, model.ErrUpstreamUnavailable) {
		s.log.Warn("market quotes failed", "err", err)
	}
	e := upstreamUnavailable(fmt.Sprintf("CoinMarketCap is unavailable and %d requested %s no usable cached data.",
		missing, pick(missing == 1, "asset has", "assets have")), retry)
	if len(have) > 0 {
		e.detail.NextStep = fmt.Sprintf("Retry in %d seconds; the attached data covers only the assets that were cached.", e.detail.RetryAfterSeconds)
		e.stale = &staleData{asOf: oldest(have), currency: cur.code, data: attach(have)}
	}
	return e
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
