// Package resolve turns agent input (a ticker symbol, a name, a slug or a
// numeric CMC id) into exactly one CoinMarketCap asset.
//
// An Index is built from the rows of CMC's /cryptocurrency/map and is
// read-only once built, so lookups need no locks. Live holds the current
// Index behind an atomic pointer so a daily rebuild can be published with a
// single swap while requests keep resolving against the previous one.
//
// Duplicate tickers are common in CMC data (ETH, SOL, PEPE and even
// "BITCOIN" are reused by many small tokens). When a query matches several
// assets, the best-ranked one is chosen only if it clearly dominates the
// runner-up; otherwise Resolve reports the query as ambiguous and lists the
// candidates so the agent can retry with an exact id.
package resolve

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fozagtx/coinstack/internal/model"
)

const (
	defaultDominanceFactor = 5
	defaultMaxSuggestions  = 5
	maxSearchLimit         = 50
)

// Match kinds reported in model.Resolution.MatchedBy and model.Candidate.Match.
const (
	MatchID     = "id"
	MatchSymbol = "symbol"
	MatchSlug   = "slug"
	MatchName   = "name"
	MatchFuzzy  = "fuzzy"
)

// Options tunes how an Index settles duplicate matches and misses.
type Options struct {
	// DominanceFactor decides duplicate matches: the best-ranked asset wins
	// only if the runner-up is unranked or its rank is at least
	// DominanceFactor times the winner's. Zero or negative means 5.
	DominanceFactor float64
	// MaxSuggestions caps the suggestions carried by a *model.NotFoundError.
	// Zero or negative means 5.
	MaxSuggestions int
}

func (o Options) withDefaults() Options {
	if !(o.DominanceFactor > 0) { // also catches NaN
		o.DominanceFactor = defaultDominanceFactor
	}
	if o.MaxSuggestions <= 0 {
		o.MaxSuggestions = defaultMaxSuggestions
	}
	return o
}

// Field bits recording how an asset matched a key.
const (
	fieldSymbol uint8 = 1 << iota
	fieldSlug
	fieldName
)

// hit is one asset reachable through a key.
type hit struct {
	pos    int32 // index into Index.entries
	fields uint8 // fieldSymbol | fieldSlug | fieldName
}

// key is one distinct normalized symbol, slug or name.
type key struct {
	s     string
	runes int
	hits  []hit // ascending pos, so best-ranked first
}

// Index resolves queries against one build of the CMC map. It is immutable
// after Build and safe for concurrent use.
type Index struct {
	builtAt time.Time
	opts    Options

	// entries holds the active assets in canonical order: ranked assets by
	// rank ascending, then unranked assets by id. Every position-ordered
	// list in the index is therefore also rank-ordered.
	entries []model.MapEntry
	byID    map[int64]int32
	exact   map[string]int32 // normalized key -> index into keys
	keys    []key            // sorted by s, for prefix scans
	byLen   [][]int32        // rune length -> indexes into keys, for fuzzy scans
}

// Build indexes the active entries of CMC's map. Inactive entries are
// ignored, and when an id appears more than once the first active row wins.
// The entries slice is not retained.
func Build(entries []model.MapEntry, builtAt time.Time, opts Options) *Index {
	ix := &Index{builtAt: builtAt, opts: opts.withDefaults()}

	active := make([]model.MapEntry, 0, len(entries))
	seen := make(map[int64]struct{}, len(entries))
	for _, e := range entries {
		if !e.IsActive {
			continue
		}
		if _, dup := seen[e.ID]; dup {
			continue
		}
		seen[e.ID] = struct{}{}
		active = append(active, e)
	}
	slices.SortFunc(active, compareEntries)
	ix.entries = active

	ix.byID = make(map[int64]int32, len(active))
	postings := make(map[string][]hit, 2*len(active))
	add := func(s string, pos int32, f uint8) {
		k := normalize(s)
		if k == "" {
			return
		}
		hs := postings[k]
		if n := len(hs); n > 0 && hs[n-1].pos == pos {
			hs[n-1].fields |= f
			return
		}
		postings[k] = append(hs, hit{pos: pos, fields: f})
	}
	for i, e := range active {
		pos := int32(i)
		ix.byID[e.ID] = pos
		add(e.Symbol, pos, fieldSymbol)
		add(e.Slug, pos, fieldSlug)
		add(e.Name, pos, fieldName)
	}

	ix.keys = make([]key, 0, len(postings))
	for s, hs := range postings {
		ix.keys = append(ix.keys, key{s: s, runes: utf8.RuneCountInString(s), hits: slices.Clip(hs)})
	}
	slices.SortFunc(ix.keys, func(a, b key) int { return strings.Compare(a.s, b.s) })

	ix.exact = make(map[string]int32, len(ix.keys))
	for i, k := range ix.keys {
		ix.exact[k.s] = int32(i)
		for len(ix.byLen) <= k.runes {
			ix.byLen = append(ix.byLen, nil)
		}
		ix.byLen[k.runes] = append(ix.byLen[k.runes], int32(i))
	}
	return ix
}

// compareEntries orders assets by rank ascending with unranked assets last,
// breaking ties by id.
func compareEntries(a, b model.MapEntry) int {
	ar, br := a.Rank > 0, b.Rank > 0
	switch {
	case ar && br && a.Rank != b.Rank:
		return cmp.Compare(a.Rank, b.Rank)
	case ar && !br:
		return -1
	case !ar && br:
		return 1
	}
	return cmp.Compare(a.ID, b.ID)
}

// Size returns the number of active assets in the index.
func (ix *Index) Size() int { return len(ix.entries) }

// BuiltAt returns the time passed to Build.
func (ix *Index) BuiltAt() time.Time { return ix.builtAt }

// Lookup returns the active asset with the given CMC id.
func (ix *Index) Lookup(id int64) (model.MapEntry, bool) {
	pos, ok := ix.byID[id]
	if !ok {
		return model.MapEntry{}, false
	}
	return ix.entries[pos], true
}

// Resolve maps one query to exactly one active asset.
//
// The query is trimmed, one leading "$" is dropped and matching ignores
// case. An all-digit query is first tried as a CMC id. Otherwise every asset
// whose symbol, slug or name equals the query is a candidate; a single
// candidate wins outright, and among several the best-ranked one wins only
// if it dominates the runner-up (see Options.DominanceFactor), in which case
// the others are returned as Alternatives. Resolve returns a
// *model.AmbiguousError when no candidate dominates and a
// *model.NotFoundError with suggestions when nothing matches.
func (ix *Index) Resolve(query string) (model.Resolution, error) {
	q := normalize(query)
	if q == "" {
		return model.Resolution{}, ix.notFound(query, q)
	}
	if e, ok := ix.lookupDigits(q); ok {
		return model.Resolution{Asset: e, MatchedBy: MatchID}, nil
	}
	ki, ok := ix.exact[q]
	if !ok {
		return model.Resolution{}, ix.notFound(query, q)
	}
	hits := ix.keys[ki].hits
	res := model.Resolution{Asset: ix.entries[hits[0].pos], MatchedBy: matchKind(hits[0].fields)}
	if len(hits) == 1 {
		return res, nil
	}
	if !dominates(res.Asset, ix.entries[hits[1].pos], ix.options().DominanceFactor) {
		return model.Resolution{}, &model.AmbiguousError{
			Query:      strings.TrimSpace(query),
			Candidates: ix.candidates(hits),
		}
	}
	res.Alternatives = ix.candidates(hits[1:])
	return res, nil
}

// Search returns up to limit candidates for query, best first: the asset
// whose id is the query, then exact symbol, slug and name matches by rank,
// then prefix matches, then near misses by edit distance (Match "fuzzy").
// Each asset appears once. limit is clamped to 1..50. When the query
// resolves, the resolved asset is the first candidate. The result is never
// nil.
func (ix *Index) Search(query string, limit int) []model.Candidate {
	limit = min(max(limit, 1), maxSearchLimit)
	out := []model.Candidate{}
	q := normalize(query)
	if q == "" {
		return out
	}
	seen := make(map[int32]struct{})
	if e, ok := ix.lookupDigits(q); ok {
		out = append(out, model.CandidateOf(e, MatchID))
		seen[ix.byID[e.ID]] = struct{}{}
	}
	if ki, ok := ix.exact[q]; ok {
		for _, h := range ix.keys[ki].hits {
			if len(out) == limit {
				return out
			}
			if _, dup := seen[h.pos]; dup {
				continue
			}
			seen[h.pos] = struct{}{}
			out = append(out, model.CandidateOf(ix.entries[h.pos], matchKind(h.fields)))
		}
	}
	if len(out) < limit {
		out = append(out, ix.near(q, limit-len(out), seen)...)
	}
	return out
}

func (ix *Index) options() Options { return ix.opts.withDefaults() }

// lookupDigits resolves an all-digit query as a CMC id.
func (ix *Index) lookupDigits(q string) (model.MapEntry, bool) {
	if !isDigits(q) {
		return model.MapEntry{}, false
	}
	id, err := strconv.ParseInt(q, 10, 64)
	if err != nil {
		return model.MapEntry{}, false
	}
	return ix.Lookup(id)
}

func (ix *Index) notFound(query, q string) *model.NotFoundError {
	sugg := []model.Candidate{}
	if q != "" {
		sugg = ix.near(q, ix.options().MaxSuggestions, nil)
	}
	return &model.NotFoundError{Query: strings.TrimSpace(query), Suggestions: sugg}
}

func (ix *Index) candidates(hits []hit) []model.Candidate {
	out := make([]model.Candidate, len(hits))
	for i, h := range hits {
		out[i] = model.CandidateOf(ix.entries[h.pos], matchKind(h.fields))
	}
	return out
}

// dominates reports whether best (the better-ranked of two matches) clearly
// outranks the runner-up.
func dominates(best, runnerUp model.MapEntry, factor float64) bool {
	if best.Rank <= 0 {
		return false
	}
	return runnerUp.Rank <= 0 || float64(runnerUp.Rank) >= float64(best.Rank)*factor
}

// matchKind names the strongest way an asset matched: symbol, then slug,
// then name.
func matchKind(fields uint8) string {
	switch {
	case fields&fieldSymbol != 0:
		return MatchSymbol
	case fields&fieldSlug != 0:
		return MatchSlug
	default:
		return MatchName
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// normalize canonicalizes queries and index keys alike: surrounding space
// and one leading "$" are removed, inner runs of white space collapse to
// one space, and letters are lower-cased.
func normalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "$")
	s = strings.TrimSpace(s)
	if isCanonicalASCII(s) {
		return s
	}
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// isCanonicalASCII reports whether an already trimmed s is ASCII with no
// upper-case letters and no white space other than single spaces, so that
// normalize can return it without allocating.
func isCanonicalASCII(s string) bool {
	prevSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= utf8.RuneSelf, 'A' <= c && c <= 'Z':
			return false
		case c == ' ':
			if prevSpace {
				return false
			}
			prevSpace = true
		case c == '\t', c == '\n', c == '\v', c == '\f', c == '\r':
			return false
		default:
			prevSpace = false
		}
	}
	return true
}
