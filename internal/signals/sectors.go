package signals

import (
	"slices"
	"sort"
	"strings"

	"github.com/fozagtx/coinstack/internal/market"
	"github.com/fozagtx/coinstack/internal/model"
)

// Sector is one tag's aggregate stats across the snapshot, and its heat:
// how strongly the median member moved in the last 24h.
type Sector struct {
	Tag                string  `json:"tag"`
	Members            int     `json:"members"`
	MedianChange24hPct float64 `json:"median_change_24h_pct"`
	MedianChange7dPct  float64 `json:"median_change_7d_pct"`
	TotalVolume24h     float64 `json:"total_volume_24h"`
	TotalMarketCap     float64 `json:"total_market_cap"`
	Heat               float64 `json:"heat"` // 0..100
	Leaders            []int64 `json:"leaders,omitempty"`
}

// Sectors is the per-snapshot tag index used by the sector-heat signal.
type Sectors struct {
	byTag map[string]*Sector
	all   []*Sector
}

// excludedSectorTags are tags too generic (consensus, wrapping, VC
// portfolios) to mean anything as a sector.
var excludedSectorTags = map[string]bool{
	"mineable": true, "pow": true, "pos": true, "dpos": true,
	"hybrid-pow-pos": true, "hybrid-pos-pow": true,
	"stablecoin": true, "asset-backed-stablecoin": true,
	"fiat-stablecoin": true, "wrapped-tokens": true,
}

// BuildSectors groups the snapshot's quotes by tag. Tags with fewer than
// minMembers members, and noise tags, are dropped. Leaders are a tag's
// top-3 members by 24h change among those with volume >= minVolume.
func BuildSectors(snap *market.Snapshot, minMembers int, minVolume float64) *Sectors {
	sec := &Sectors{byTag: map[string]*Sector{}}
	if snap == nil {
		return sec
	}
	groups := map[string][]*model.Quote{}
	for _, q := range snap.ByRank {
		for _, t := range q.Tags {
			if excludedSectorTags[t] || strings.HasSuffix(t, "-portfolio") {
				continue
			}
			groups[t] = append(groups[t], q)
		}
	}
	for tag, members := range groups {
		if len(members) < minMembers {
			continue
		}
		s := &Sector{Tag: tag, Members: len(members)}
		var c24, c7 []float64
		for _, q := range members {
			c24 = append(c24, q.Change24hPct)
			c7 = append(c7, q.Change7dPct)
			s.TotalVolume24h += q.Volume24h
			s.TotalMarketCap += q.MarketCap
		}
		s.MedianChange24hPct = median(c24)
		s.MedianChange7dPct = median(c7)
		s.Heat = clamp01(s.MedianChange24hPct/15) * 100

		leaders := make([]*model.Quote, 0, len(members))
		for _, q := range members {
			if q.Volume24h >= minVolume {
				leaders = append(leaders, q)
			}
		}
		sort.Slice(leaders, func(i, j int) bool {
			a, b := leaders[i], leaders[j]
			if a.Change24hPct != b.Change24hPct {
				return a.Change24hPct > b.Change24hPct
			}
			return a.ID < b.ID
		})
		for i, q := range leaders {
			if i == 3 {
				break
			}
			s.Leaders = append(s.Leaders, q.ID)
		}
		sec.byTag[tag] = s
		sec.all = append(sec.all, s)
	}
	return sec
}

// Sector returns the sector for tag, if tracked.
func (s *Sectors) Sector(tag string) (Sector, bool) {
	v, ok := s.byTag[tag]
	if !ok {
		return Sector{}, false
	}
	return *v, true
}

// Sorted returns every tracked sector ordered by the given key: "heat",
// "change_24h_pct", "change_7d_pct", "volume_24h" or "market_cap"
// (descending; unknown keys fall back to heat).
func (s *Sectors) Sorted(by string) []Sector {
	less := func(a, b *Sector) bool { return a.Heat > b.Heat }
	switch by {
	case "change_24h_pct":
		less = func(a, b *Sector) bool { return a.MedianChange24hPct > b.MedianChange24hPct }
	case "change_7d_pct":
		less = func(a, b *Sector) bool { return a.MedianChange7dPct > b.MedianChange7dPct }
	case "volume_24h":
		less = func(a, b *Sector) bool { return a.TotalVolume24h > b.TotalVolume24h }
	case "market_cap":
		less = func(a, b *Sector) bool { return a.TotalMarketCap > b.TotalMarketCap }
	}
	out := slices.Clone(s.all)
	sort.SliceStable(out, func(i, j int) bool {
		if less(out[i], out[j]) != less(out[j], out[i]) {
			return less(out[i], out[j])
		}
		return out[i].Tag < out[j].Tag
	})
	res := make([]Sector, len(out))
	for i, s := range out {
		res[i] = *s
	}
	return res
}
