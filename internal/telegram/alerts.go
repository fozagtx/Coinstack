package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fozagtx/coinstack/internal/discover"
	"github.com/fozagtx/coinstack/internal/market"
)

const alertCooldown = 24 * time.Hour

// evaluate runs the alert checks on each published snapshot. The first
// snapshot after startup only seeds the seen sets — no alert storm.
func (b *Bot) evaluate(snap *market.Snapshot) {
	b.mu.Lock()
	seeded := b.seeded
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	gems, gerr := b.engine.Gems(ctx, discover.GemsParams{Limit: 10})
	climbs, cerr := b.engine.Climbers(ctx, discover.ClimbersParams{Window: 24 * time.Hour, MinVolume: 100000, Limit: 50})
	news, nerr := b.engine.NewListings(ctx, discover.NewListingsParams{Days: 7, MinVolume: 100000})

	b.mu.Lock()
	if !seeded {
		// Seed all seen sets from the first snapshot's results.
		if gerr == nil {
			for _, g := range gems.Items {
				b.gemSeen[g.ID] = b.now()
			}
		}
		if cerr == nil {
			for _, c := range climbs.Items {
				if qualifiesClimb(c) {
					b.climbSeen[c.ID] = b.now()
				}
			}
		}
		if nerr == nil {
			for _, n := range news.Items {
				b.listingSeen[n.ID] = true
			}
		}
		b.seeded = true
		b.mu.Unlock()
		return
	}

	var messages []string
	now := b.now()
	if b.alertOn("gems") && gerr == nil {
		for _, g := range gems.Items {
			if t, seen := b.gemSeen[g.ID]; seen && now.Sub(t) < alertCooldown {
				continue
			}
			b.gemSeen[g.ID] = now
			messages = append(messages, gemAlert(g))
		}
	}
	if b.alertOn("climbers") && cerr == nil {
		for _, c := range climbs.Items {
			if !qualifiesClimb(c) {
				continue
			}
			if t, seen := b.climbSeen[c.ID]; seen && now.Sub(t) < alertCooldown {
				continue
			}
			b.climbSeen[c.ID] = now
			messages = append(messages, climbAlert(c))
		}
	}
	if b.alertOn("listings") && nerr == nil {
		for _, n := range news.Items {
			if b.listingSeen[n.ID] {
				continue
			}
			b.listingSeen[n.ID] = true
			messages = append(messages, listingAlert(n))
		}
	}
	b.mu.Unlock()

	for _, m := range messages {
		b.broadcast(m + signoff)
	}
}

func qualifiesClimb(c discover.ClimberItem) bool {
	// RankChangePct is a percentage (25 means +25% of ranks gained).
	return c.RankChange >= 50 && c.RankChangePct >= 25 && c.Volume24h >= 100000
}

func gemAlert(g discover.GemItem) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "New gem: <b>%s</b> (%s) — score <b>%.0f</b>/100, %s confidence\n", esc(g.Symbol), esc(g.Name), g.Score, g.Confidence)
	fmt.Fprintf(&sb, "mcap %s · vol 24h %s · turnover %.2f · 24h %+.1f%%", money(g.MarketCap), money(g.Volume24h), g.Turnover, g.Change24hPct)
	for _, w := range g.Why {
		fmt.Fprintf(&sb, "\n· %s", esc(w))
	}
	if len(g.RiskFlags) > 0 {
		fmt.Fprintf(&sb, "\nRisk: %s", esc(strings.Join(g.RiskFlags, ", ")))
	}
	if len(g.HotSectors) > 0 {
		fmt.Fprintf(&sb, "\nHot sectors: %s", esc(strings.Join(g.HotSectors, ", ")))
	}
	return sb.String()
}

func climbAlert(c discover.ClimberItem) string {
	return fmt.Sprintf("Rank climber: <b>%s</b> (%s) #%d → #%d (%+.1f%%)\nmcap %s · vol %s · 24h %+.1f%%",
		esc(c.Symbol), esc(c.Name), c.RankThen, c.Rank, c.RankChangePct,
		money(c.MarketCap), money(c.Volume24h), c.Change24hPct)
}

func listingAlert(n discover.NewListingItem) string {
	return fmt.Sprintf("New listing: <b>%s</b> (%s) — %.1f days old\nmcap %s · vol 24h %s",
		esc(n.Symbol), esc(n.Name), n.DaysListed, money(n.MarketCap), money(n.Volume24h))
}

// digestLoop fires the daily digest at DigestHour UTC.
func (b *Bot) digestLoop(ctx context.Context) {
	if !b.alertOn("digest") {
		return
	}
	for {
		wait := b.untilNextDigest()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		b.sendDigest()
	}
}

func (b *Bot) untilNextDigest() time.Duration {
	now := b.now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), b.opts.DigestHour, 0, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next.Sub(now)
}

// sendDigest broadcasts the daily summary.
func (b *Bot) sendDigest() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var sb strings.Builder
	sb.WriteString("<b>Daily digest</b>\n")

	gems, err := b.engine.Gems(ctx, discover.GemsParams{Limit: 5})
	if err == nil && len(gems.Items) > 0 {
		sb.WriteString("\n<b>Top gems</b>\n")
		for _, g := range gems.Items {
			fmt.Fprintf(&sb, "· <b>%s</b> %.0f/100 — mcap %s, 24h %+.1f%%\n", esc(g.Symbol), g.Score, money(g.MarketCap), g.Change24hPct)
		}
	}
	climbs, err := b.engine.Climbers(ctx, discover.ClimbersParams{Window: 24 * time.Hour, MinVolume: 100000, Limit: 3})
	switch {
	case err == nil && len(climbs.Items) > 0:
		sb.WriteString("\n<b>Climbers</b>\n")
		for _, c := range climbs.Items {
			fmt.Fprintf(&sb, "· <b>%s</b> #%d → #%d (%+.1f%%)\n", esc(c.Symbol), c.RankThen, c.Rank, c.RankChangePct)
		}
	case err == nil:
		sb.WriteString("\n<b>Climbers</b>: not enough history yet\n")
	}
	sectors, err := b.engine.Sectors(ctx, discover.SectorsParams{Sort: "heat", Limit: 3})
	if err == nil && len(sectors.Sectors) > 0 {
		sb.WriteString("\n<b>Hot sectors</b>\n")
		for _, s := range sectors.Sectors {
			var leads []string
			for _, l := range s.Leaders {
				leads = append(leads, l.Symbol)
			}
			fmt.Fprintf(&sb, "· <b>%s</b> heat %.0f (%+.1f%% median) — %s\n", esc(s.Tag), s.Heat, s.MedianChange24hPct, esc(strings.Join(leads, ", ")))
		}
	}
	b.mu.Lock()
	b.lastDigestAt = b.now()
	b.mu.Unlock()
	b.broadcast(sb.String() + signoff)
}
