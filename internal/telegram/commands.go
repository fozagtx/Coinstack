package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fozagtx/coinstack/internal/discover"
	"github.com/fozagtx/coinstack/internal/model"
)

// command runs one bot command and returns the HTML reply.
func (b *Bot) command(ctx context.Context, cmd, arg string) (string, error) {
	switch cmd {
	case "/start", "/help":
		return helpText(), nil
	case "/gems":
		limit, err := argInt(arg, 5, 1, 10)
		if err != nil {
			return "", err
		}
		return b.gemsReply(ctx, limit)
	case "/climbers":
		win := 24 * time.Hour
		if arg == "7d" {
			win = 7 * 24 * time.Hour
		} else if arg != "" && arg != "24h" {
			return "", fmt.Errorf("usage: /climbers [24h|7d]")
		}
		return b.climbersReply(ctx, win, 10)
	case "/new":
		days, err := argInt(arg, 7, 1, 30)
		if err != nil {
			return "", err
		}
		return b.newListingsReply(ctx, days, 10)
	case "/sectors":
		return b.sectorsReply(ctx, 10)
	case "/sector":
		if arg == "" {
			return "", fmt.Errorf("usage: /sector <tag>")
		}
		return b.sectorReply(ctx, arg, 10)
	case "/asset":
		if arg == "" {
			return "", fmt.Errorf("usage: /asset <symbol|slug|id>")
		}
		return b.assetReply(ctx, arg)
	case "/screen":
		return b.screenReply(ctx)
	case "/status":
		return b.statusReply(), nil
	default:
		return "", nil
	}
}

func helpText() string {
	return `<b>CoinStack</b> — altcoin discovery

/gems [n] — top scored candidates
/screen — top 10 by turnover under $50M
/climbers [24h|7d] — biggest rank moves
/new [days] — fresh CMC listings
/sectors — hottest sectors
/sector &lt;tag&gt; — one sector's members
/asset &lt;query&gt; — detail and signals for one asset
/status — service health`
}

func argInt(arg string, def, lo, hi int) (int, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return def, nil
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("want a number between %d and %d", lo, hi)
	}
	return n, nil
}

func (b *Bot) gemsReply(ctx context.Context, limit int) (string, error) {
	res, err := b.engine.Gems(ctx, discover.GemsParams{Limit: limit})
	if err != nil {
		return "", err
	}
	if len(res.Items) == 0 {
		return "No gems right now.", nil
	}
	var sb strings.Builder
	sb.WriteString("<b>Top gems</b>\n")
	for _, g := range res.Items {
		fmt.Fprintf(&sb, "\n<b>%s</b> (%s) — <b>%.0f</b>/100, %s confidence\n", esc(g.Symbol), esc(g.Name), g.Score, g.Confidence)
		fmt.Fprintf(&sb, "mcap %s · vol %s · turnover %.2f · 24h %+.1f%%\n", money(g.MarketCap), money(g.Volume24h), g.Turnover, g.Change24hPct)
		if len(g.Why) > 0 {
			sb.WriteString("Why: ")
			for i, w := range g.Why {
				if i > 0 {
					sb.WriteString("; ")
				}
				sb.WriteString(esc(w))
			}
			sb.WriteString("\n")
		}
		if len(g.RiskFlags) > 0 {
			fmt.Fprintf(&sb, "Risk: %s\n", esc(strings.Join(g.RiskFlags, ", ")))
		}
		if len(g.HotSectors) > 0 {
			fmt.Fprintf(&sb, "Hot sectors: %s\n", esc(strings.Join(g.HotSectors, ", ")))
		}
	}
	return sb.String(), nil
}

func (b *Bot) climbersReply(ctx context.Context, win time.Duration, limit int) (string, error) {
	res, err := b.engine.Climbers(ctx, discover.ClimbersParams{Window: win, MinVolume: 100000, Limit: limit})
	if err != nil {
		return "", err
	}
	if len(res.Items) == 0 {
		return "Not enough history yet — climbers need rank samples from ~24h ago.", nil
	}
	label := "24h"
	if win >= 7*24*time.Hour {
		label = "7d"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Rank climbers (%s)</b>\n", label)
	for _, c := range res.Items {
		fmt.Fprintf(&sb, "\n<b>%s</b> #%d → #%d (%+.1f%%)\nmcap %s · vol %s · 24h %+.1f%%\n",
			esc(c.Symbol), c.RankThen, c.Rank, c.RankChangePct, money(c.MarketCap), money(c.Volume24h), c.Change24hPct)
	}
	return sb.String(), nil
}

func (b *Bot) newListingsReply(ctx context.Context, days, limit int) (string, error) {
	res, err := b.engine.NewListings(ctx, discover.NewListingsParams{Days: days, MinVolume: 100000, Limit: limit})
	if err != nil {
		return "", err
	}
	if len(res.Items) == 0 {
		return fmt.Sprintf("No listings in the last %d days with volume ≥ $100k.", days), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>New listings (last %dd)</b>\n", days)
	for _, n := range res.Items {
		fmt.Fprintf(&sb, "\n<b>%s</b> (%s) — listed %.1f days ago\nmcap %s · vol %s",
			esc(n.Symbol), esc(n.Name), n.DaysListed, money(n.MarketCap), money(n.Volume24h))
		if n.RankChangeSinceFirstSeen != nil {
			fmt.Fprintf(&sb, " · rank %+d since first seen", *n.RankChangeSinceFirstSeen)
		}
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

func (b *Bot) sectorsReply(ctx context.Context, limit int) (string, error) {
	res, err := b.engine.Sectors(ctx, discover.SectorsParams{Sort: "heat", Limit: limit})
	if err != nil {
		return "", err
	}
	if len(res.Sectors) == 0 {
		return "No sectors tracked yet.", nil
	}
	var sb strings.Builder
	sb.WriteString("<b>Sectors by heat</b>\n")
	for _, s := range res.Sectors {
		fmt.Fprintf(&sb, "\n<b>%s</b> heat %.0f · median 24h %+.1f%% · %d members\n",
			esc(s.Tag), s.Heat, s.MedianChange24hPct, s.Members)
		for _, l := range s.Leaders {
			fmt.Fprintf(&sb, "  %s %+.1f%%\n", esc(l.Symbol), l.Change24hPct)
		}
	}
	return sb.String(), nil
}

func (b *Bot) sectorReply(ctx context.Context, tag string, limit int) (string, error) {
	res, err := b.engine.Sectors(ctx, discover.SectorsParams{Sector: tag, Limit: limit})
	var snf *discover.SectorNotFoundError
	if errors.As(err, &snf) {
		return fmt.Sprintf("No sector %q. Hottest right now: %s", tag, strings.Join(snf.Hottest, ", ")), nil
	}
	if err != nil {
		return "", err
	}
	s := res.Detail.Sector
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>%s</b> — heat %.0f, %d members, median 24h %+.1f%%, 7d %+.1f%%\nvol %s · mcap %s\n",
		esc(s.Tag), s.Heat, s.Members, s.MedianChange24hPct, s.MedianChange7dPct, money(s.TotalVolume24h), money(s.TotalMarketCap))
	for _, m := range res.Detail.Members {
		fmt.Fprintf(&sb, "\n<b>%s</b> #%d %+.1f%% 24h · mcap %s", esc(m.Symbol), m.Rank, m.Change24hPct, money(m.MarketCap))
	}
	return sb.String(), nil
}

func (b *Bot) assetReply(ctx context.Context, query string) (string, error) {
	res, err := b.engine.Asset(ctx, query)
	var amb *model.AmbiguousError
	var nf *model.NotFoundError
	switch {
	case errors.As(err, &amb):
		return fmt.Sprintf("%q is ambiguous — %d assets match. Try a CMC id.", query, len(amb.Candidates)), nil
	case errors.As(err, &nf):
		return fmt.Sprintf("No asset matches %q.", query), nil
	case err != nil:
		return "", err
	}
	a := res.Item
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>%s</b> (%s) — #%d\nprice $%s · mcap %s · vol %s\n1h %+.1f%% · 24h %+.1f%% · 7d %+.1f%%\nscore <b>%.0f</b>/100, %s confidence",
		esc(a.Symbol), esc(a.Name), a.Rank, price(a.Price), money(a.MarketCap), money(a.Volume24h),
		a.Change1hPct, a.Change24hPct, a.Change7dPct, a.Score, a.Confidence)
	if len(a.Why) > 0 {
		sb.WriteString("\nWhy: ")
		for i, w := range a.Why {
			if i > 0 {
				sb.WriteString("; ")
			}
			sb.WriteString(esc(w))
		}
	}
	if len(a.RiskFlags) > 0 {
		fmt.Fprintf(&sb, "\nRisk: %s", esc(strings.Join(a.RiskFlags, ", ")))
	}
	return sb.String(), nil
}

func (b *Bot) screenReply(ctx context.Context) (string, error) {
	res, err := b.engine.Screen(ctx, discover.ScreenParams{
		MaxMarketCap:       50e6,
		ExcludeStablecoins: true,
		Sort:               "turnover",
		Order:              "desc",
		Limit:              10,
	})
	if err != nil {
		return "", err
	}
	if len(res.Items) == 0 {
		return "Nothing passes the screen right now.", nil
	}
	var sb strings.Builder
	sb.WriteString("<b>Screen: top turnover under $50M</b>\n")
	for _, it := range res.Items {
		fmt.Fprintf(&sb, "\n<b>%s</b> turnover %.2f · mcap %s · vol %s · 24h %+.1f%%",
			esc(it.Symbol), it.Turnover, money(it.MarketCap), money(it.Volume24h), it.Change24hPct)
	}
	return sb.String(), nil
}

func (b *Bot) statusReply() string {
	st := b.engine.Status()
	state := "ok"
	switch {
	case st.LastSuccessAt.IsZero() || st.CacheSize == 0:
		state = "down"
	case time.Since(st.LastSuccessAt) > 30*time.Minute:
		state = "down"
	case time.Since(st.LastSuccessAt) > 3*time.Minute:
		state = "degraded"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Status: %s</b>\ncache %d assets · top %d · poll every %s\nhistory: %d assets, %dh deep\ncredits today %d, month %d / %d\nupstream calls %d, errors %d",
		state, st.CacheSize, st.TopN, st.PollInterval, st.HistoryAssets, st.HistoryHours,
		st.CreditsUsedToday, st.CreditsUsedMonth, st.CreditLimitMonthly,
		st.UpstreamCalls, st.UpstreamErrors)
	if st.LastError != "" {
		fmt.Fprintf(&sb, "\nlast error: %s", esc(st.LastError))
	}
	return sb.String()
}
