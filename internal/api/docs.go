package api

import (
	"bytes"
	"html/template"
)

// docs.go renders the docs landing page once at startup.

var docsTemplate = template.Must(template.New("docs").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>CoinStack &mdash; altcoin discovery API</title>
<style>
  :root { color-scheme: light; }
  body { font-family: ui-sans-serif, system-ui, sans-serif; max-width: 880px; margin: 0 auto; padding: 2rem 1rem; color: #1a1a1a; background: #fafafa; line-height: 1.5; }
  h1 { font-size: 1.6rem; } h2 { font-size: 1.15rem; margin-top: 2rem; border-bottom: 1px solid #ddd; padding-bottom: .3rem; }
  table { border-collapse: collapse; width: 100%; font-size: .9rem; }
  th, td { text-align: left; padding: .35rem .5rem; border-bottom: 1px solid #e5e5e5; vertical-align: top; }
  code, pre { font-family: ui-monospace, monospace; background: #efefef; border-radius: 4px; }
  code { padding: .1rem .3rem; }
  pre { padding: .6rem .8rem; overflow-x: auto; }
  .muted { color: #666; } .flag { font-family: ui-monospace, monospace; }
</style>
</head>
<body>
<h1>CoinStack <span class="muted">{{.Version}}</span></h1>
<p>Altcoin-discovery API for AI agents. It polls the top {{.TopN}} assets from
CoinMarketCap in tiers, keeps a week of hourly history, and scores assets on
transparent signals to surface early, high-upside candidates.</p>
<p><strong>Market data for information only, not financial advice.</strong>
The score ranks candidates for review; it does not predict outcomes.</p>

<h2>Authentication</h2>
<p>Every endpoint except <code>/</code>, <code>/v1/health</code> and
<code>/v1/openapi.json</code> needs a key:
<code>Authorization: Bearer &lt;key&gt;</code> or <code>X-API-Key: &lt;key&gt;</code>.</p>

<h2>Endpoints</h2>
<table>
<tr><th>Endpoint</th><th>What it returns</th><th>Example</th></tr>
<tr><td><code>GET /v1/gems</code></td><td>Ranked altcoin candidates: composite score, signals, risk flags, reasons.</td><td><code>/v1/gems?limit=10</code></td></tr>
<tr><td><code>GET /v1/screen</code></td><td>Filter the cached universe by cap, volume, turnover, price changes, tags, age.</td><td><code>/v1/screen?sort=turnover&amp;min_volume=100000&amp;limit=10</code></td></tr>
<tr><td><code>GET /v1/climbers</code></td><td>Biggest CMC rank moves over 24h or 7d, from retained history.</td><td><code>/v1/climbers?window=24h&amp;direction=up</code></td></tr>
<tr><td><code>GET /v1/new-listings</code></td><td>Assets first listed on CoinMarketCap within the last days.</td><td><code>/v1/new-listings?days=7</code></td></tr>
<tr><td><code>GET /v1/sectors</code></td><td>Sector (tag) aggregates ranked by heat; <code>?sector=</code> for one tag's members.</td><td><code>/v1/sectors?sector=depin</code></td></tr>
<tr><td><code>GET /v1/asset</code></td><td>Full detail for one asset plus its signal breakdown.</td><td><code>/v1/asset?asset=SOL</code></td></tr>
<tr><td><code>GET /v1/resolve</code></td><td>Resolve a symbol/name/slug/id to candidate assets.</td><td><code>/v1/resolve?query=uni</code></td></tr>
<tr><td><code>GET /v1/health</code></td><td>Poller status, cache sizes, credit usage, request metrics.</td><td><code>/v1/health</code></td></tr>
<tr><td><code>GET /v1/openapi.json</code></td><td>OpenAPI 3.0 document for this API.</td><td><code>/v1/openapi.json</code></td></tr>
</table>

<h2>How the score works</h2>
<p>Each gem's composite score (0&ndash;100) is a weighted sum of four signals;
when rank-climb history is unavailable the remaining weights renormalize:</p>
<table>
<tr><th>Signal</th><th>Weight</th><th>Measures</th></tr>
<tr><td>turnover</td><td>0.30</td><td>volume_24h / market_cap on a 0.05&ndash;2 log band; blended with a surge term vs. the asset's own baseline once &ge;12 older samples exist.</td></tr>
<tr><td>new_listing</td><td>0.20</td><td>Recency of the CMC listing; linear decay to zero at 90 days.</td></tr>
<tr><td>rank_climb</td><td>0.30</td><td>CMC rank gains over 24h (60%) and 7d (40%), capped at +30% and +50%.</td></tr>
<tr><td>sector_heat</td><td>0.20</td><td>The asset's hottest tag, where heat is the sector's median 24h change scaled to +15%.</td></tr>
</table>

<h2>Risk flags</h2>
<table>
<tr><th>Flag</th><th>Meaning</th></tr>
<tr><td class="flag">already_pumped</td><td>+100% in 24h or +300% in 7d; composite score is multiplied by 0.6.</td></tr>
<tr><td class="flag">thin_volume</td><td>24h volume under $250k.</td></tr>
<tr><td class="flag">micro_cap</td><td>market cap under $5M.</td></tr>
<tr><td class="flag">new_and_unproven</td><td>listed less than 7 days ago.</td></tr>
<tr><td class="flag">insufficient_history</td><td>no history sample close enough to compute rank climb.</td></tr>
<tr><td class="flag">unranked</td><td>CMC reports no rank for the asset.</td></tr>
</table>

<h2>Conventions</h2>
<ul>
<li>All endpoints are GET and return <code>application/json</code>.</li>
<li>Success envelope: <code>{as_of, age_seconds, source, note?, history_hours?, data, warnings?}</code>.</li>
<li>Amounts are USD; fields ending <code>_pct</code> are percentages.</li>
<li>Unknown query parameters return <code>400 invalid_parameter</code>.</li>
<li>Errors carry <code>{error: {code, message, next_step, ...}}</code>; <code>next_step</code> always says what to try next.</li>
</ul>
</body>
</html>`))

// buildDocsPage renders the docs template.
func buildDocsPage(version string, topN int) []byte {
	if version == "" {
		version = "dev"
	}
	if topN <= 0 {
		topN = 3000
	}
	var buf bytes.Buffer
	if err := docsTemplate.Execute(&buf, map[string]any{"Version": version, "TopN": topN}); err != nil {
		panic("api: docs template: " + err.Error())
	}
	return buf.Bytes()
}
