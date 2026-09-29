package resolve

import (
	"cmp"
	"slices"
	"strings"

	"github.com/fozagtx/coinstack/internal/model"
)

// maxEdits is the largest edit distance tolerated for a string of n runes.
// Short strings get none, so a two-letter query such as "BT" does not
// match half the universe of two- and three-letter tickers.
func maxEdits(n int) int {
	switch {
	case n <= 2:
		return 0
	case n <= 5:
		return 1
	default:
		return 2
	}
}

// near returns up to limit assets that almost match the normalized query q,
// excluding those in skip: first assets with a symbol, slug or name that
// starts with q, then assets within a small Damerau-Levenshtein distance,
// closest first. Within each group better-ranked assets come first. Every
// candidate has Match "fuzzy".
//
// Prefix matches use a binary search over the sorted keys. Edit distance
// needs a scan, bounded to keys of nearly the same length and cut short as
// soon as a row of the distance table exceeds the budget.
func (ix *Index) near(q string, limit int, skip map[int32]struct{}) []model.Candidate {
	type scored struct {
		tier int // 0 for a prefix match, else the edit distance
		pos  int32
	}
	var found []scored

	i, _ := slices.BinarySearchFunc(ix.keys, q, func(k key, t string) int { return strings.Compare(k.s, t) })
	for ; i < len(ix.keys) && strings.HasPrefix(ix.keys[i].s, q); i++ {
		if ix.keys[i].s == q {
			continue
		}
		for _, h := range ix.keys[i].hits {
			found = append(found, scored{0, h.pos})
		}
	}

	qr := []rune(q)
	n := len(qr)
	if k := maxEdits(n); k > 0 {
		var d distancer
		for m := max(1, n-k); m <= n+k && m < len(ix.byLen); m++ {
			budget := min(k, maxEdits(m))
			if abs(n-m) > budget {
				continue
			}
			for _, ki := range ix.byLen[m] {
				kd := &ix.keys[ki]
				if strings.HasPrefix(kd.s, q) {
					continue // exact or already found as a prefix match
				}
				if dist := d.osa(qr, kd.s, budget); dist <= budget {
					for _, h := range kd.hits {
						found = append(found, scored{dist, h.pos})
					}
				}
			}
		}
	}

	slices.SortFunc(found, func(a, b scored) int {
		if c := cmp.Compare(a.tier, b.tier); c != 0 {
			return c
		}
		return cmp.Compare(a.pos, b.pos)
	})
	out := make([]model.Candidate, 0, min(limit, len(found)))
	taken := make(map[int32]struct{}, min(limit, len(found)))
	for _, s := range found {
		if len(out) >= limit {
			break
		}
		if _, ok := skip[s.pos]; ok {
			continue
		}
		if _, ok := taken[s.pos]; ok {
			continue
		}
		taken[s.pos] = struct{}{}
		out = append(out, model.CandidateOf(ix.entries[s.pos], MatchFuzzy))
	}
	return out
}

// distancer computes bounded edit distances, reusing its buffers between
// calls. The zero value is ready to use; it is not safe for concurrent use.
type distancer struct {
	b                []rune
	prev2, prev, cur []int
}

// osa returns the optimal string alignment distance (Levenshtein plus
// transposition of adjacent runes) between a and s, or budget+1 when the
// distance exceeds budget.
func (d *distancer) osa(a []rune, s string, budget int) int {
	d.b = d.b[:0]
	for _, r := range s {
		d.b = append(d.b, r)
	}
	b := d.b
	la, lb := len(a), len(b)
	if abs(la-lb) > budget {
		return budget + 1
	}
	if cap(d.cur) < lb+1 {
		d.prev2, d.prev, d.cur = make([]int, lb+1), make([]int, lb+1), make([]int, lb+1)
	}
	prev2, prev, cur := d.prev2[:lb+1], d.prev[:lb+1], d.cur[:lb+1]
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		rowMin := i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			v := min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				v = min(v, prev2[j-2]+1)
			}
			cur[j] = v
			rowMin = min(rowMin, v)
		}
		if rowMin > budget {
			return budget + 1
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return min(prev[lb], budget+1)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
