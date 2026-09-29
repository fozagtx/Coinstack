package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/fozagtx/coinstack/internal/model"
)

// source is the value of every envelope's "source" field.
const source = "coinmarketcap"

// disclaimer rides on every discovery endpoint's envelope.
const disclaimer = "Market data for information only, not financial advice."

// Error codes, as listed in docs/api-contract.md.
const (
	codeAmbiguous        = "ambiguous_asset"
	codeAssetNotFound    = "asset_not_found"
	codeSectorNotFound   = "sector_not_found"
	codeUpstream         = "upstream_unavailable"
	codeBudget           = "upstream_budget_exhausted"
	codeInvalidParam     = "invalid_parameter"
	codeUnauthorized     = "unauthorized"
	codeRateLimited      = "rate_limited"
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
	codeInternal         = "internal_error"
)

// Warning codes.
const (
	warnStale        = "stale_data"
	warnByRank       = "symbol_resolved_by_rank"
	warnOutsideTop   = "outside_top_n"
	warnShortHistory = "insufficient_history"
)

// defaultRetryAfter is suggested to agents when upstream gives no hint.
const defaultRetryAfter = 30 * time.Second

// envelope is the success shape shared by the data endpoints.
type envelope struct {
	AsOf       string `json:"as_of"`
	AgeSeconds int64  `json:"age_seconds"`
	Source     string `json:"source"`
	Note       string `json:"note,omitempty"`
	// HistoryHours is the depth of retained history; a pointer so 0 is
	// emitted (the ring is still warming up) while nil omits the field.
	HistoryHours *int      `json:"history_hours,omitempty"`
	Data         any       `json:"data"`
	Warnings     []warning `json:"warnings,omitempty"`
}

// warning is one entry of an envelope's "warnings" array.
type warning struct {
	Code       string            `json:"code"`
	Message    string            `json:"message"`
	AgeSeconds int64             `json:"age_seconds,omitempty"`
	Query      string            `json:"query,omitempty"`
	ChosenID   int64             `json:"chosen_id,omitempty"`
	Candidates []model.Candidate `json:"candidates,omitempty"`
}

// errorDetail is the "error" object of an error response.
type errorDetail struct {
	Code              string            `json:"code"`
	Message           string            `json:"message"`
	NextStep          string            `json:"next_step"`
	Param             string            `json:"param,omitempty"`
	Query             string            `json:"query,omitempty"`
	AllowedValues     []string          `json:"allowed_values,omitzero"`
	Candidates        []model.Candidate `json:"candidates,omitzero"`
	RetryAfterSeconds int               `json:"retry_after_seconds,omitempty"`
}

// errorResponse is the body of every error. When CMC is unavailable but
// older data exists, the data and its freshness ride along at top level.
type errorResponse struct {
	Error      *errorDetail `json:"error"`
	AsOf       string       `json:"as_of,omitempty"`
	AgeSeconds *int64       `json:"age_seconds,omitempty"`
	Source     string       `json:"source,omitempty"`
	Data       any          `json:"data,omitempty"`
}

// apiError is an error the handlers turn into an HTTP error response.
type apiError struct {
	status int
	detail errorDetail
	stale  *staleData // data attached to an upstream_unavailable response
}

// staleData is older data served alongside a 503 upstream_unavailable.
type staleData struct {
	asOf time.Time
	data any
}

func (e *apiError) Error() string { return e.detail.Code + ": " + e.detail.Message }

func newError(status int, code, message, next string) *apiError {
	return &apiError{status: status, detail: errorDetail{Code: code, Message: message, NextStep: next}}
}

func invalidParam(param, message, next string, allowed []string) *apiError {
	e := newError(http.StatusBadRequest, codeInvalidParam, message, next)
	e.detail.Param = param
	e.detail.AllowedValues = allowed
	return e
}

func errInternal() *apiError {
	return newError(http.StatusInternalServerError, codeInternal, "The server hit an unexpected error.",
		"Retry the request; if it keeps failing, report the X-Request-ID response header.")
}

func upstreamUnavailable(message string, retry time.Duration) *apiError {
	secs := retrySeconds(retry)
	e := newError(http.StatusServiceUnavailable, codeUpstream, message,
		fmt.Sprintf("Retry in %d seconds.", secs))
	e.detail.RetryAfterSeconds = secs
	return e
}

func retrySeconds(d time.Duration) int {
	if d <= 0 {
		d = defaultRetryAfter
	}
	return int(math.Ceil(d.Seconds()))
}

// respond fills in the freshness fields of env from asOf and writes it.
// Data older than StaleAfter gets a stale_data warning; with
// enforceMaxStale, data older than MaxStale is instead returned as a 503
// upstream_unavailable carrying the old data.
func (s *Server) respond(w http.ResponseWriter, env *envelope, asOf time.Time, enforceMaxStale bool) *apiError {
	now := s.now()
	asOf = asOf.UTC().Truncate(time.Second)
	age := ageSeconds(now, asOf)
	if enforceMaxStale && time.Duration(age)*time.Second > s.cfg.MaxStale {
		e := upstreamUnavailable(fmt.Sprintf(
			"CoinMarketCap data has not refreshed: the newest data available is %d s old, past the %d s limit.",
			age, int64(s.cfg.MaxStale/time.Second)), defaultRetryAfter)
		e.detail.NextStep = fmt.Sprintf("Retry in %d seconds; use the attached data only if its age is acceptable.", e.detail.RetryAfterSeconds)
		e.stale = &staleData{asOf: asOf, data: env.Data}
		return e
	}
	env.AsOf = formatTime(asOf)
	env.AgeSeconds = age
	env.Source = source
	if time.Duration(age)*time.Second > s.cfg.StaleAfter {
		env.Warnings = append(env.Warnings, warning{
			Code: warnStale,
			Message: fmt.Sprintf("Data is %d s old, past the %d s freshness limit; CoinMarketCap updates are delayed.",
				age, int64(s.cfg.StaleAfter/time.Second)),
			AgeSeconds: age,
		})
	}
	s.writeJSON(w, http.StatusOK, env, "no-store")
	return nil
}

// writeError writes e as an error response.
func (s *Server) writeError(w http.ResponseWriter, e *apiError) {
	body := errorResponse{Error: &e.detail}
	if e.stale != nil {
		asOf := e.stale.asOf.UTC().Truncate(time.Second)
		age := ageSeconds(s.now(), asOf)
		body.AsOf = formatTime(asOf)
		body.AgeSeconds = &age
		body.Source = source
		body.Data = e.stale.data
	}
	if e.detail.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.detail.RetryAfterSeconds))
	}
	s.writeJSON(w, e.status, body, "no-store")
}

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// fallbackErrorBody is written if a response cannot be encoded.
var fallbackErrorBody = []byte(`{"error":{"code":"internal_error","message":"The server could not encode the response.","next_step":"Retry the request; if it keeps failing, report the X-Request-ID response header."}}` + "\n")

// writeJSON encodes v and writes it with the given status.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any, cacheControl string) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= 1<<20 {
			bufPool.Put(buf)
		}
	}()
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	body := fallbackErrorBody
	if err := enc.Encode(v); err != nil {
		s.log.Error("encoding response", "err", err)
		status = http.StatusInternalServerError
	} else {
		body = buf.Bytes()
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("X-Content-Type-Options", "nosniff")
	if cacheControl != "" {
		h.Set("Cache-Control", cacheControl)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ageSeconds returns whole seconds from asOf to now, never negative.
func ageSeconds(now, asOf time.Time) int64 {
	d := now.Sub(asOf)
	if d < 0 {
		return 0
	}
	return int64(d / time.Second)
}

// formatTime renders t as ISO 8601 UTC with second precision.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// optionalTime renders t, or nil (JSON null) for the zero time.
func optionalTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := formatTime(t)
	return &s
}

// finite replaces NaN and infinities, which JSON cannot carry, with 0.
func finite(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}
