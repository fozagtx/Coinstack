package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/fozagtx/coinstack/internal/model"
)

type stateKey struct{}

// reqState is per-request bookkeeping shared by the middleware layers.
// Inner layers fill it in; instrument reads it once the request is done.
type reqState struct {
	w     statusWriter
	id    string
	key   model.APIKey
	authn bool // key holds an authenticated key
}

func stateFrom(ctx context.Context) *reqState {
	st, _ := ctx.Value(stateKey{}).(*reqState)
	return st
}

// statusWriter records the status and size of a response.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// requestIDs generates request ids: a random per-process prefix plus a
// counter, which is unique enough for log correlation and cheap.
type requestIDs struct {
	prefix string
	seq    atomic.Uint64
}

func newRequestIDs() *requestIDs {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return &requestIDs{prefix: hex.EncodeToString(b) + "-"}
}

func (g *requestIDs) next() string {
	return g.prefix + strconv.FormatUint(g.seq.Add(1), 36)
}

// requestID returns the caller's X-Request-ID when it is safe to echo, or
// a fresh id.
func (s *Server) requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-ID"); validRequestID(id) {
		return id
	}
	return s.reqIDs.next()
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// instrument assigns a request id, then after the request records metrics,
// writes the Debug access log and hands a record to the RequestLogger.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ts := s.now()
		st := &reqState{id: s.requestID(r)}
		st.w.ResponseWriter = w
		w.Header().Set("X-Request-ID", st.id)
		ctx := context.WithValue(r.Context(), stateKey{}, st)

		next.ServeHTTP(&st.w, r.WithContext(ctx))

		elapsed := time.Since(start)
		status := st.w.status
		if status == 0 {
			status = http.StatusOK
		}
		endpoint := s.metrics.observe(chi.RouteContext(r.Context()).RoutePattern(), status, elapsed)
		var keyID int64
		if st.authn {
			keyID = st.key.ID
		}
		if s.log.Enabled(ctx, slog.LevelDebug) {
			s.log.LogAttrs(ctx, slog.LevelDebug, "request",
				slog.String("request_id", st.id),
				slog.String("method", r.Method),
				slog.String("endpoint", endpoint),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", st.w.bytes),
				slog.Float64("latency_ms", ms(elapsed)),
				slog.Int64("key_id", keyID),
			)
		}
		if s.reqlog != nil {
			s.reqlog.LogRequest(model.RequestLog{
				TS:        ts,
				KeyID:     keyID,
				Endpoint:  endpoint,
				Status:    status,
				LatencyMS: ms(elapsed),
			})
		}
	})
}

// recoverer turns a panic into a 500 internal_error response.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			st := stateFrom(r.Context())
			var id string
			if st != nil {
				id = st.id
			}
			s.log.Error("panic serving request", "request_id", id, "path", r.URL.Path,
				"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			if st == nil || st.w.status == 0 {
				s.writeError(w, errInternal())
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// maxKeyLen bounds raw API keys before they reach the key store.
const maxKeyLen = 256

// authenticate requires a valid API key, sent as "Authorization: Bearer
// <key>" or "X-API-Key: <key>".
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthDisabled {
			next.ServeHTTP(w, r)
			return
		}
		raw := apiKeyFrom(r)
		if raw == "" {
			s.writeError(w, newError(http.StatusUnauthorized, codeUnauthorized, "Missing API key.",
				"Send your key as 'Authorization: Bearer <key>' or 'X-API-Key: <key>'."))
			return
		}
		var key model.APIKey
		err := model.ErrKeyNotFound
		if len(raw) <= maxKeyLen {
			key, err = s.keys.Lookup(r.Context(), raw)
		}
		switch {
		case errors.Is(err, model.ErrKeyNotFound) || (err == nil && !key.Active):
			s.writeError(w, newError(http.StatusUnauthorized, codeUnauthorized, "The API key is invalid or revoked.",
				"Check the key you send in 'Authorization: Bearer <key>'; ask the operator for a new key if it was revoked."))
			return
		case err != nil:
			s.log.Error("api key lookup failed", "err", err)
			e := errInternal()
			e.detail.Message = "The API key could not be checked."
			e.detail.NextStep = "Retry in a few seconds."
			s.writeError(w, e)
			return
		}
		if st := stateFrom(r.Context()); st != nil {
			st.key, st.authn = key, true
		}
		next.ServeHTTP(w, r)
	})
}

// apiKeyFrom extracts the raw API key from the request headers.
func apiKeyFrom(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, token, ok := strings.Cut(strings.TrimSpace(h), " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			if token = strings.TrimSpace(token); token != "" {
				return token
			}
		}
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

// rateLimit applies the authenticated key's token bucket.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := stateFrom(r.Context())
		if st == nil || !st.authn {
			next.ServeHTTP(w, r)
			return
		}
		limit := st.key.RateLimit
		if limit <= 0 {
			limit = s.cfg.DefaultRateLimit
		}
		ok, remaining, wait := s.limiter.allow(st.key.ID, limit, s.now())
		h := w.Header()
		h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
		h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		if !ok {
			secs := max(1, int(math.Ceil(wait.Seconds())))
			e := newError(http.StatusTooManyRequests, codeRateLimited,
				fmt.Sprintf("This key is limited to %d requests per minute.", limit),
				fmt.Sprintf("Wait %d seconds, then retry; batch assets into one /v1/price call to save requests.", secs))
			e.detail.RetryAfterSeconds = secs
			s.writeError(w, e)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
