package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// asserter records contract assertions for one question.
type asserter struct {
	n     int
	fails []string
}

// that records one assertion and reports whether it held.
func (a *asserter) that(ok bool, format string, args ...any) bool {
	a.n++
	if !ok {
		a.fails = append(a.fails, fmt.Sprintf(format, args...))
	}
	return ok
}

// ---- JSON accessors (bodies are decoded with UseNumber) ----

func obj(v any) (map[string]any, bool) { m, ok := v.(map[string]any); return m, ok }
func arr(v any) ([]any, bool)          { s, ok := v.([]any); return s, ok }

func str(v any) (string, bool) { s, ok := v.(string); return s, ok }

// num accepts JSON numbers only; a number sent as a string fails.
func num(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

// integer accepts JSON numbers without a fractional part or exponent.
func integer(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	return i, err == nil
}

// timestamp parses an ISO 8601 UTC timestamp with second precision, exactly
// as the contract writes them: 2026-09-29T12:04:00Z.
func timestamp(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || t.UTC().Format("2006-01-02T15:04:05Z") != s {
		return time.Time{}, false
	}
	return t, true
}

func describe(v any) string {
	switch x := v.(type) {
	case nil:
		return "missing/null"
	case json.Number:
		return "number " + string(x)
	case string:
		if len(x) > 40 {
			x = x[:40] + "..."
		}
		return strconv.Quote(x)
	case bool:
		return "bool"
	case []any:
		return fmt.Sprintf("array(%d)", len(x))
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// ---- contract validators ----

type envelopeWant struct {
	dataObject bool   // /v1/asset returns one object; every other endpoint an array
	currency   string // expected currency; "" when the endpoint has none (/v1/resolve)
}

// checkEnvelope validates the success envelope and returns its data.
func checkEnvelope(a *asserter, body map[string]any, want envelopeWant, now time.Time, maxSkew time.Duration) any {
	asOf, okAsOf := timestamp(body["as_of"])
	a.that(okAsOf, "as_of must be an ISO 8601 UTC timestamp with second precision, got %s", describe(body["as_of"]))
	age, okAge := integer(body["age_seconds"])
	a.that(okAge && age >= 0, "age_seconds must be a non-negative integer, got %s", describe(body["age_seconds"]))
	if okAsOf && okAge && maxSkew > 0 {
		implied := int64(now.Sub(asOf).Seconds())
		diff := implied - age
		if diff < 0 {
			diff = -diff
		}
		a.that(time.Duration(diff)*time.Second <= maxSkew,
			"age_seconds %d disagrees with as_of (implies %ds; allowed clock skew %s)", age, implied, maxSkew)
	}
	src, _ := str(body["source"])
	a.that(src == "coinmarketcap", `source must be "coinmarketcap", got %s`, describe(body["source"]))
	if want.currency != "" {
		cur, _ := str(body["currency"])
		a.that(cur == want.currency, "currency must be %q, got %s", want.currency, describe(body["currency"]))
	}
	if w, present := body["warnings"]; present {
		list, ok := arr(w)
		a.that(ok && len(list) > 0, "warnings must be a non-empty array when present (omitted when empty), got %s", describe(w))
		for i, item := range list {
			m, ok := obj(item)
			code, _ := str(m["code"])
			msg, _ := str(m["message"])
			a.that(ok && code != "" && msg != "", "warnings[%d] must have code and message strings", i)
		}
	}
	data, present := body["data"]
	if want.dataObject {
		_, ok := obj(data)
		a.that(present && ok, "data must be an object, got %s", describe(data))
	} else {
		_, ok := arr(data)
		a.that(present && ok, "data must be an array, got %s", describe(data))
	}
	return data
}

// checkPriceFields validates the price-level asset fields shared by
// /v1/price, /v1/movers, /v1/new-tokens, /v1/asset and /v1/compare.
func checkPriceFields(a *asserter, label string, item map[string]any) {
	id, ok := integer(item["id"])
	a.that(ok && id > 0, "%s.id must be a positive integer, got %s", label, describe(item["id"]))
	for _, f := range []string{"symbol", "name"} {
		s, _ := str(item[f])
		a.that(s != "", "%s.%s must be a non-empty string, got %s", label, f, describe(item[f]))
	}
	if r, present := item["rank"]; present {
		n, ok := integer(r)
		a.that(ok && n > 0, "%s.rank must be a positive integer when present (omitted when unranked), got %s", label, describe(r))
	}
	for _, f := range []string{"price", "market_cap", "volume_24h", "change_1h_pct", "change_24h_pct", "change_7d_pct"} {
		_, ok := num(item[f])
		a.that(ok, "%s.%s must be a JSON number, got %s", label, f, describe(item[f]))
	}
	_, ok = timestamp(item["last_updated"])
	a.that(ok, "%s.last_updated must be an ISO 8601 UTC timestamp, got %s", label, describe(item["last_updated"]))
	for k, v := range item {
		_, nested := v.(map[string]any)
		a.that(!nested, "%s.%s is a nested object; responses must stay flat", label, k)
	}
}

// checkDetailFields validates the detail fields added by /v1/asset and
// /v1/compare.
func checkDetailFields(a *asserter, label string, item map[string]any) {
	slug, _ := str(item["slug"])
	a.that(slug != "", "%s.slug must be a non-empty string, got %s", label, describe(item["slug"]))
	for _, f := range []string{"circulating_supply", "total_supply"} {
		_, ok := num(item[f])
		a.that(ok, "%s.%s must be a JSON number, got %s", label, f, describe(item[f]))
	}
	ms, present := item["max_supply"]
	_, isNum := num(ms)
	a.that(present && (ms == nil || isNum), "%s.max_supply must be present as a number or null, got %s", label, describe(ms))
	_, ok := timestamp(item["date_added"])
	a.that(ok, "%s.date_added must be an ISO 8601 UTC timestamp, got %s", label, describe(item["date_added"]))
	tags, ok := arr(item["tags"])
	a.that(ok && len(tags) <= 10, "%s.tags must be an array of at most 10 strings, got %s", label, describe(item["tags"]))
	for i, t := range tags {
		_, ok := str(t)
		a.that(ok, "%s.tags[%d] must be a string", label, i)
	}
}

// checkCandidate validates one candidate from an error or /v1/resolve.
func checkCandidate(a *asserter, label string, c map[string]any) {
	id, ok := integer(c["id"])
	a.that(ok && id > 0, "%s.id must be a positive integer, got %s", label, describe(c["id"]))
	for _, f := range []string{"symbol", "name", "slug"} {
		s, _ := str(c[f])
		a.that(s != "", "%s.%s must be a non-empty string, got %s", label, f, describe(c[f]))
	}
}

// checkError validates the error envelope and returns the error object.
func checkError(a *asserter, status int, body map[string]any, wantStatus int, wantCode string) map[string]any {
	a.that(status == wantStatus, "HTTP status must be %d, got %d", wantStatus, status)
	e, ok := obj(body["error"])
	if !a.that(ok, "body must carry an error object, got %s", describe(body["error"])) {
		return map[string]any{}
	}
	code, _ := str(e["code"])
	a.that(code == wantCode, "error.code must be %q, got %s", wantCode, describe(e["code"]))
	msg, _ := str(e["message"])
	a.that(msg != "", "error.message must be a non-empty string")
	next, _ := str(e["next_step"])
	a.that(strings.TrimSpace(next) != "", "error.next_step must tell the agent what to do next")
	return e
}

// items returns data as a list of objects, recording an assertion per item.
func items(a *asserter, data any) []map[string]any {
	list, _ := arr(data)
	out := make([]map[string]any, 0, len(list))
	for i, v := range list {
		m, ok := obj(v)
		if a.that(ok, "data[%d] must be an object", i) {
			out = append(out, m)
		}
	}
	return out
}

func fnum(m map[string]any, k string) float64 { f, _ := num(m[k]); return f }
func fstr(m map[string]any, k string) string  { s, _ := str(m[k]); return s }
func fint(m map[string]any, k string) int64   { i, _ := integer(m[k]); return i }
