package api

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// paramKind says how a query parameter's value is parsed and validated.
type paramKind int

const (
	kindString paramKind = iota // free text
	kindList                    // comma-separated asset queries; min/max bound the item count
	kindInt                     // integer; min/max bound the value
	kindNumber                  // finite number; min bounds the value
	kindEnum                    // one of enum, case-insensitive
	kindBool                    // true/false/1/0
)

// paramSpec describes one query parameter an endpoint accepts. The same
// table drives request validation and is checked against openapi.json in
// tests, so the spec and the handlers cannot drift apart.
type paramSpec struct {
	name     string
	kind     paramKind
	required bool
	def      string   // default value as text; "" when none
	enum     []string // allowed values for kindEnum
	min, max float64  // inclusive bounds; max 0 means unbounded
	maxLen   int      // maximum length of the raw value in bytes
}

var (
	paramLimit50 = paramSpec{name: "limit", kind: kindInt, def: "10", min: 1, max: 50, maxLen: 10}
	// optionalNumber is an unbounded numeric filter; the empty default
	// marks it as not provided, which the handler checks with present.
	optionalNumber = func(name string) paramSpec {
		return paramSpec{name: name, kind: kindNumber, def: "", min: -math.MaxFloat64, maxLen: 30}
	}
)

// endpointParams lists the query parameters each route accepts. Anything
// else is rejected with 400 invalid_parameter.
var endpointParams = map[string][]paramSpec{
	pathGems: {
		{name: "max_market_cap", kind: kindNumber, def: "50000000", min: 0, maxLen: 30},
		{name: "min_market_cap", kind: kindNumber, def: "1000000", min: 0, maxLen: 30},
		{name: "min_volume", kind: kindNumber, def: "100000", min: 0, maxLen: 30},
		{name: "listed_within_days", kind: kindInt, def: "0", min: 0, max: 365, maxLen: 10},
		{name: "sector", kind: kindString, maxLen: 60},
		{name: "include_pumped", kind: kindBool, def: "false", maxLen: 10},
		paramLimit50,
	},
	pathScreen: {
		optionalNumber("min_market_cap"),
		optionalNumber("max_market_cap"),
		optionalNumber("min_volume"),
		optionalNumber("max_volume"),
		optionalNumber("min_turnover"),
		optionalNumber("min_change_1h_pct"),
		optionalNumber("max_change_1h_pct"),
		optionalNumber("min_change_24h_pct"),
		optionalNumber("max_change_24h_pct"),
		optionalNumber("min_change_7d_pct"),
		optionalNumber("max_change_7d_pct"),
		{name: "tag", kind: kindString, maxLen: 300},
		{name: "listed_within_days", kind: kindInt, def: "0", min: 0, max: 365, maxLen: 10},
		{name: "exclude_stablecoins", kind: kindBool, def: "true", maxLen: 10},
		{name: "sort", kind: kindEnum, def: "change_24h_pct", enum: []string{"change_1h_pct", "change_24h_pct", "change_7d_pct", "volume_24h", "market_cap", "turnover", "rank"}, maxLen: 30},
		{name: "order", kind: kindEnum, def: "", enum: []string{"asc", "desc"}, maxLen: 10},
		paramLimit50,
	},
	pathClimbers: {
		{name: "window", kind: kindEnum, def: "24h", enum: []string{"24h", "7d"}, maxLen: 10},
		{name: "direction", kind: kindEnum, def: "up", enum: []string{"up", "down"}, maxLen: 10},
		{name: "min_volume", kind: kindNumber, def: "100000", min: 0, maxLen: 30},
		{name: "max_market_cap", kind: kindNumber, def: "0", min: 0, maxLen: 30},
		paramLimit50,
	},
	pathNewListings: {
		{name: "days", kind: kindInt, def: "7", min: 1, max: 30, maxLen: 10},
		{name: "min_volume", kind: kindNumber, def: "0", min: 0, maxLen: 30},
		paramLimit50,
	},
	pathSectors: {
		{name: "sort", kind: kindEnum, def: "heat", enum: []string{"heat", "change_24h_pct", "change_7d_pct", "volume_24h", "market_cap"}, maxLen: 30},
		{name: "min_members", kind: kindInt, def: "5", min: 2, max: 200, maxLen: 10},
		{name: "sector", kind: kindString, maxLen: 60},
		paramLimit50,
	},
	pathAsset: {
		{name: "asset", kind: kindString, required: true, maxLen: 100},
	},
	pathResolve: {
		{name: "query", kind: kindString, required: true, maxLen: 100},
		{name: "limit", kind: kindInt, def: "5", min: 1, max: 20, maxLen: 10},
	},
	pathOpenAPI: nil,
	pathHealth:  nil,
}

// queryParams is a request's validated query string.
type queryParams struct {
	vals  url.Values
	specs []paramSpec
}

// parseQuery parses the request's query string and rejects parameters the
// endpoint does not accept, repeated scalar parameters and oversized values.
// Repeated list parameters are joined, so both asset=A,B and
// asset=A&asset=B work.
func parseQuery(r *http.Request, path string) (*queryParams, *apiError) {
	specs := endpointParams[path]
	q := &queryParams{specs: specs}
	if r.URL.RawQuery == "" {
		return q, nil
	}
	vals, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, invalidParam("", "The query string is malformed.",
			"Send parameters as name=value pairs joined by &, with values URL-encoded.", nil)
	}
	var unknown []string
	for name, vs := range vals {
		spec, ok := findSpec(specs, name)
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		if len(vs) > 1 {
			if spec.kind != kindList {
				return nil, invalidParam(name, fmt.Sprintf("Parameter %s was given %d times.", name, len(vs)),
					fmt.Sprintf("Pass %s once.", name), nil)
			}
			vals[name] = []string{strings.Join(vs, ",")}
		}
		if len(vals[name][0]) > spec.maxLen {
			return nil, invalidParam(name, fmt.Sprintf("Parameter %s is longer than %d characters.", name, spec.maxLen),
				fmt.Sprintf("Shorten %s.", name), nil)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return nil, unknownParam(path, unknown[0], specs)
	}
	q.vals = vals
	return q, nil
}

func unknownParam(path, name string, specs []paramSpec) *apiError {
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.name
	}
	next := fmt.Sprintf("Remove %s; %s takes no parameters.", truncate(name, 40), path)
	if len(names) > 0 {
		next = fmt.Sprintf("Remove %s; %s accepts only: %s.", truncate(name, 40), path, strings.Join(names, ", "))
	}
	e := invalidParam(truncate(name, 40), fmt.Sprintf("Unknown parameter %q.", truncate(name, 40)), next, names)
	if len(names) == 0 {
		e.detail.AllowedValues = []string{}
	}
	return e
}

func findSpec(specs []paramSpec, name string) (paramSpec, bool) {
	for _, s := range specs {
		if s.name == name {
			return s, true
		}
	}
	return paramSpec{}, false
}

// value returns the trimmed raw value of name and its spec. It panics if
// name is not declared for the endpoint, which is a programming error.
func (q *queryParams) value(name string) (string, paramSpec) {
	spec, ok := findSpec(q.specs, name)
	if !ok {
		panic("api: undeclared parameter " + name)
	}
	return strings.TrimSpace(q.vals.Get(name)), spec
}

// str returns a required or optional free-text parameter.
func (q *queryParams) str(name string) (string, *apiError) {
	v, spec := q.value(name)
	if v == "" {
		if spec.required {
			return "", missingParam(name)
		}
		return spec.def, nil
	}
	return v, nil
}

// int returns an integer parameter within its bounds.
func (q *queryParams) int(name string) (int, *apiError) {
	v, spec := q.value(name)
	if v == "" {
		v = spec.def
	}
	n, err := strconv.Atoi(v)
	if err != nil || float64(n) < spec.min || (spec.max > 0 && float64(n) > spec.max) {
		return 0, invalidParam(name,
			fmt.Sprintf("%s must be a whole number from %g to %g; got %q.", name, spec.min, spec.max, truncate(v, 30)),
			fmt.Sprintf("Retry with %s between %g and %g, or omit it for the default %s.", name, spec.min, spec.max, spec.def), nil)
	}
	return n, nil
}

// number returns a finite, non-negative numeric parameter.
func (q *queryParams) number(name string) (float64, *apiError) {
	v, spec := q.value(name)
	if v == "" {
		v = spec.def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < spec.min {
		return 0, invalidParam(name,
			fmt.Sprintf("%s must be a number of at least %g; got %q.", name, spec.min, truncate(v, 30)),
			fmt.Sprintf("Retry with a plain number such as %s=%s, or omit it for the default.", name, spec.def), nil)
	}
	return f, nil
}

// enum returns an enumerated parameter, lower-cased.
func (q *queryParams) enum(name string) (string, *apiError) {
	v, spec := q.value(name)
	if v == "" {
		return spec.def, nil
	}
	lv := strings.ToLower(v)
	if !slices.Contains(spec.enum, lv) {
		return "", invalidParam(name,
			fmt.Sprintf("%s must be one of %s; got %q.", name, strings.Join(spec.enum, ", "), truncate(v, 30)),
			fmt.Sprintf("Retry with one of allowed_values, or omit %s for the default %s.", name, spec.def), spec.enum)
	}
	return lv, nil
}

// present reports whether the parameter was sent with a non-empty value.
func (q *queryParams) present(name string) bool {
	v, _ := q.value(name)
	return v != ""
}

// optNumber returns an optional numeric parameter: ok is false when it
// was not provided.
func (q *queryParams) optNumber(name string) (float64, bool, *apiError) {
	if !q.present(name) {
		return 0, false, nil
	}
	v, err := q.number(name)
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}

// boolean returns a bool parameter: true, false, 1 or 0.
func (q *queryParams) boolean(name string) (bool, *apiError) {
	v, spec := q.value(name)
	if v == "" {
		v = spec.def
	}
	switch strings.ToLower(v) {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	}
	return false, invalidParam(name,
		fmt.Sprintf("%s must be true or false; got %q.", name, truncate(v, 30)),
		fmt.Sprintf("Retry with %s=true or %s=false, or omit it for the default %s.", name, name, spec.def), nil)
}

// list returns a comma-separated list: entries trimmed, empties dropped and
// case-insensitive duplicates removed (request order kept). The number of
// distinct entries must be within the spec's max; the minimum is checked
// by the caller after resolution, since two spellings may name one asset.
func (q *queryParams) list(name string) ([]string, *apiError) {
	v, spec := q.value(name)
	var out []string
	for part := range strings.SplitSeq(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" || slices.ContainsFunc(out, func(s string) bool { return strings.EqualFold(s, part) }) {
			continue
		}
		out = append(out, part)
		if len(out) > int(spec.max) {
			return nil, invalidParam(name, tooManyMessage(name, spec),
				fmt.Sprintf("Send at most %g assets in %s; split larger sets across calls.", spec.max, name), nil)
		}
	}
	if len(out) == 0 {
		return nil, missingParam(name)
	}
	return out, nil
}

func tooManyMessage(name string, spec paramSpec) string {
	if spec.min == spec.max {
		return fmt.Sprintf("%s takes exactly %g asset.", name, spec.max)
	}
	return fmt.Sprintf("%s takes %g to %g assets.", name, spec.min, spec.max)
}

func missingParam(name string) *apiError {
	return invalidParam(name, fmt.Sprintf("Parameter %s is required.", name),
		fmt.Sprintf("Retry with %s set; see GET /v1/openapi.json for examples.", name), nil)
}

// truncate shortens s to at most n bytes (on a rune boundary) so error
// messages never echo huge inputs.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
