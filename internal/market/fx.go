package market

import (
	"context"
	"maps"
	"math"
	"sort"
	"time"

	"github.com/fozagtx/coinstack/internal/model"
)

// fxRate is units of a currency per 1 USD and when it was fetched.
type fxRate struct {
	rate float64
	asOf time.Time
}

// refreshFX fetches rates for the configured currencies. A currency CMC
// omits (or a failed call) keeps its last good rate and timestamp.
func (m *Market) refreshFX(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()
	started := m.now()
	rates, meta, err := m.up.FiatRates(cctx, m.cfg.Currencies)
	m.record(ctx, "fx", started, meta, len(rates), err)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("fiat rate refresh failed", "err", err)
		}
		return
	}
	now := m.now()
	next := make(map[string]fxRate, len(m.cfg.Currencies))
	if old := m.fx.Load(); old != nil {
		maps.Copy(next, *old)
	}
	for c, r := range rates {
		c = model.NormalizeCurrency(c)
		if c == "USD" || c == "" || !(r > 0) || math.IsInf(r, 0) {
			continue
		}
		next[c] = fxRate{rate: r, asOf: now}
	}
	m.fx.Store(&next)
}

// FXRate returns units of currency per 1 USD and when the rate was fetched.
// USD always returns (1, now, true); an unknown currency returns ok=false.
func (m *Market) FXRate(currency string) (rate float64, asOf time.Time, ok bool) {
	c := model.NormalizeCurrency(currency)
	if c == "USD" {
		return 1, m.now(), true
	}
	if p := m.fx.Load(); p != nil {
		if r, found := (*p)[c]; found {
			return r.rate, r.asOf, true
		}
	}
	return 0, time.Time{}, false
}

// Currencies returns "USD" followed by every currency with a known rate,
// sorted.
func (m *Market) Currencies() []string {
	var codes []string
	if p := m.fx.Load(); p != nil {
		codes = make([]string, 0, len(*p))
		for c := range *p {
			codes = append(codes, c)
		}
		sort.Strings(codes)
	}
	return append([]string{"USD"}, codes...)
}
