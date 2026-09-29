package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/fozagtx/coinstack/internal/discover"
	"github.com/fozagtx/coinstack/internal/model"
)

// resolveError maps a resolver or engine error to an API error.
func (s *Server) resolveError(param, query string, err error) *apiError {
	var amb *model.AmbiguousError
	var nf *model.NotFoundError
	var nq *discover.NoQuoteError
	var snf *discover.SectorNotFoundError
	switch {
	case errors.As(err, &amb):
		e := newError(http.StatusConflict, codeAmbiguous,
			fmt.Sprintf("%q matches %d assets.", query, len(amb.Candidates)),
			ambiguousNextStep(param, query, amb.Candidates))
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
	case errors.As(err, &nq):
		e := newError(http.StatusNotFound, codeAssetNotFound,
			fmt.Sprintf("CoinMarketCap has no current market quote for %s (id %d).", nq.Name, nq.ID),
			"The asset may be inactive or untracked; retry with another asset.")
		e.detail.Param, e.detail.Query = param, query
		e.detail.Candidates = []model.Candidate{}
		return e
	case errors.As(err, &snf):
		next := "Check the tag spelling; GET /v1/sectors lists the tracked sectors."
		if len(snf.Hottest) > 0 {
			next = fmt.Sprintf("Try one of the current hottest sectors: %s.", joinStrings(snf.Hottest))
		}
		e := newError(http.StatusNotFound, codeSectorNotFound,
			fmt.Sprintf("No sector named %q is tracked.", snf.Tag), next)
		e.detail.Param = "sector"
		return e
	case errors.Is(err, model.ErrBudgetExhausted) || errors.Is(err, model.ErrUpstreamUnavailable):
		return s.upstreamError(err, "CoinMarketCap is unavailable and the request has no usable cached data.")
	default:
		s.log.Error("request failed", "param", param, "query", query, "err", err)
		return errInternal()
	}
}

func ambiguousNextStep(param, query string, cands []model.Candidate) string {
	if len(cands) == 0 {
		return fmt.Sprintf("Retry with %s=<id> using a CMC id; GET /v1/resolve?query=%s lists the options.", param, url.QueryEscape(query))
	}
	c := cands[0]
	return fmt.Sprintf("Retry with %s=<id>, e.g. %s=%d for %s.", param, param, c.ID, c.Name)
}

// upstreamError maps a Market upstream failure to a 503.
func (s *Server) upstreamError(err error, message string) *apiError {
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
		s.log.Warn("upstream call failed", "err", err)
	}
	return upstreamUnavailable(message, retry)
}

// maxCandidates caps the candidates listed in an error.
const maxCandidates = 10

// capCandidates returns at most maxCandidates, never nil.
func capCandidates(c []model.Candidate) []model.Candidate {
	if len(c) > maxCandidates {
		c = c[:maxCandidates]
	}
	return append(make([]model.Candidate, 0, len(c)), c...)
}

func joinStrings(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// toWarnings converts engine warnings into envelope warnings.
func toWarnings(ws []discover.Warning) []warning {
	if len(ws) == 0 {
		return nil
	}
	out := make([]warning, len(ws))
	for i, w := range ws {
		out[i] = warning{Code: w.Code, Message: w.Message, Query: w.Query, ChosenID: w.ChosenID, Candidates: w.Candidates}
	}
	return out
}
