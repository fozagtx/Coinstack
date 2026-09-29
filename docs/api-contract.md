# CoinStack API contract (v1)

This is the wire contract every part of CoinStack is built against: handlers,
OpenAPI spec, docs page and Telegram bot. `GET /v1/openapi.json` is generated
from the same parameter table the handlers validate with.

## Conventions

- All endpoints are `GET` and return `application/json; charset=utf-8`.
- Auth: `Authorization: Bearer <key>` (the header `X-API-Key: <key>` is also
  accepted). `/v1/openapi.json`, `/v1/health` and `/` (docs page) need no key.
- Amounts are USD. Numbers are JSON numbers, never strings. Percent fields
  end in `_pct` (`-2.1` means -2.1 %). Timestamps are ISO 8601 UTC.
- Unknown query parameters are rejected with `400 invalid_parameter`.
- Agents may pass a symbol (`SOL`), a name (`Solana`), a slug (`solana`) or a
  CMC id (`5426`) wherever an asset is expected. Responses always carry the
  CMC `id`, so follow-up calls can be exact.
- Discovery endpoints (`gems`, `screen`, `climbers`, `new-listings`,
  `sectors`, `asset`) add `"note": "Market data for information only, not
  financial advice."` to the envelope; history-dependent ones add
  `"history_hours"`.

## Success envelope

```json
{
  "as_of": "2026-09-30T12:04:00Z",
  "age_seconds": 21,
  "source": "coinmarketcap",
  "note": "Market data for information only, not financial advice.",
  "history_hours": 73,
  "data": [ ... ],
  "warnings": [ { "code": "stale_data", "message": "..." } ]
}
```

- `as_of` is the **oldest** CMC `last_updated` among the returned items (for
  `/v1/resolve`, the time the id map was fetched).
- `age_seconds` = whole seconds between `as_of` and the response. Integer,
  never negative.
- `data` is an array everywhere except `/v1/asset` (one object), `/v1/resolve`
  (object) and a single-sector `/v1/sectors` call (object).
- `warnings` is omitted when empty. Warning codes:
  - `stale_data`: data older than the freshness limit (default 180 s). Adds
    `age_seconds`.
  - `insufficient_history`: the service has <24 h of retained history, so
    rank-climb and turnover-surge signals are unreliable.
  - `symbol_resolved_by_rank`: a ticker matched several assets; the dominant
    one was returned. Adds `query`, `chosen_id`, `candidates`.
  - `outside_top_n`: the asset was fetched on demand rather than served from
    the top-N cache (informational).

## Asset item

The base object returned by every list endpoint:

| field | type | notes |
| --- | --- | --- |
| `id` | integer | CMC id |
| `symbol`, `name` | string | |
| `rank` | integer | CMC rank; omitted when 0 |
| `price`, `market_cap`, `volume_24h` | number | USD |
| `turnover` | number | `volume_24h / market_cap`, 4 dp; 0 when cap is 0 |
| `change_1h_pct`, `change_24h_pct`, `change_7d_pct` | number | USD-based |
| `date_added` | string or null | CMC listing date |
| `tags` | array of string | at most 10 |
| `last_updated` | string | CMC's own timestamp |

Endpoint extras:

- `/v1/gems`: `score` (0–100), `confidence` (`low`|`medium`|`high`),
  `signals` (`{turnover,new_listing,rank_climb,sector_heat}`, each
  `{score, detail}`), `risk_flags` (never null), `why`, `hot_sectors`.
- `/v1/climbers`: `rank_then`, `rank_change` (positive = climbed),
  `rank_change_pct` (2 dp).
- `/v1/new-listings`: `days_listed` (1 dp), `rank_change_since_first_seen`
  (integer or null).
- `/v1/asset`: the base item plus `slug`, `circulating_supply`,
  `total_supply`, `max_supply` (nullable), `category`, `platform`,
  `website`, `score`, `confidence`, `eligible_for_gems`, `signals`,
  `risk_flags`, `why`, `hot_sectors`.
- `/v1/sectors` list rows: `{tag, members, median_change_24h_pct,
  median_change_7d_pct, total_volume_24h, total_market_cap, heat,
  leaders: [{id, symbol, name, change_24h_pct}]}`. With `?sector=`, data is
  `{sector: {...}, members: [item, ...]}`.
- `/v1/resolve`: `{candidates: [{id, symbol, name, slug, rank, platform,
  match}], resolved_id}` — `resolved_id` is what `asset=<query>` resolves
  to, or null.

## Endpoints

| Endpoint | Params | Notes |
| --- | --- | --- |
| `GET /v1/gems` | `max_market_cap` (50M), `min_market_cap` (1M), `min_volume` (100k), `listed_within_days` (0–365), `sector`, `include_pumped` (false), `limit` (10, ≤50) | composite score, sorted desc |
| `GET /v1/screen` | `min/max_market_cap`, `min/max_volume`, `min_turnover`, `min/max_change_1h_pct`, `min/max_change_24h_pct`, `min/max_change_7d_pct`, `tag` (comma list, ANY, ≤10), `listed_within_days`, `exclude_stablecoins` (true), `sort` (default `change_24h_pct`), `order` (`desc`; `asc` for `rank`), `limit` | pure filter, no scoring |
| `GET /v1/climbers` | `window` (`24h`,`7d`), `direction` (`up`,`down`), `min_volume` (100k), `max_market_cap`, `limit` | empty `data` + `insufficient_history` warning when history is too short |
| `GET /v1/new-listings` | `days` (1–30, 7), `min_volume`, `limit` | newest first |
| `GET /v1/sectors` | `sort` (`heat` default), `min_members` (5, 2–200), `sector`, `limit` | unknown tag → 404 `sector_not_found` |
| `GET /v1/asset` | `asset` (required) | one object; scored regardless of eligibility |
| `GET /v1/resolve` | `query` (required), `limit` (5, ≤20) | |
| `GET /v1/openapi.json` | none | OpenAPI 3.0.3, ETag-cached (public) |
| `GET /v1/health` | none | see below (public) |

### `/v1/health`

```json
{
  "status": "ok",
  "version": "dev",
  "preset": "startup",
  "uptime_seconds": 3600,
  "last_poll_at": "...",
  "last_success_at": "...",
  "last_error": "",
  "age_seconds": 21,
  "cache_size": 3000,
  "on_demand_cache_size": 3,
  "resolver_assets": 9876,
  "top_n": 3000,
  "poll_interval_seconds": 120,
  "history_assets": 2900,
  "history_hours": 30,
  "credits_used_today": 1234,
  "credits_used_month": 45678,
  "credit_limit_monthly": 300000,
  "projected_credits_per_day": 2114,
  "upstream_calls": 1500,
  "upstream_errors": 2,
  "requests_total": 10000,
  "requests_by_status": { "200": 9900, "404": 100 },
  "telegram": { "enabled": true, "chats": 1, "last_update_at": "...", "alerts_sent": 5 }
}
```

`status` is `ok` when the last successful poll is within the freshness
limit, `degraded` when stale but within the max-stale limit, `down`
otherwise (HTTP 503 with the same body).

## Errors

```json
{
  "error": {
    "code": "ambiguous_asset",
    "message": "\"UNI\" matches 3 assets.",
    "next_step": "Retry with asset=<id>, e.g. asset=7083 for Uniswap.",
    "param": "asset",
    "query": "UNI",
    "candidates": [ { "id": 7083, "symbol": "UNI", "name": "Uniswap", "slug": "uniswap", "rank": 20 } ]
  }
}
```

| Situation | HTTP | `code` | extra fields |
| --- | --- | --- | --- |
| Ticker matches several assets, none dominant | 409 | `ambiguous_asset` | `param`, `query`, `candidates` |
| Asset not found | 404 | `asset_not_found` | `param`, `query`, `candidates` |
| Unknown sector tag | 404 | `sector_not_found` | `param`; `next_step` lists the hottest tags |
| Upstream CMC unavailable and nothing usable cached | 503 | `upstream_unavailable` | `retry_after_seconds` |
| On-demand credit budget spent | 503 | `upstream_budget_exhausted` | `retry_after_seconds` |
| Bad or unknown parameter | 400 | `invalid_parameter` | `param`, `allowed_values` when enumerable |
| Missing or invalid API key | 401 | `unauthorized` | |
| Per-key rate limit exceeded | 429 | `rate_limited` | `retry_after_seconds` + `Retry-After` |
| Unknown path | 404 | `not_found` | |
| Unexpected failure | 500 | `internal_error` | |

`next_step` is always present: one plain sentence telling the agent what to do.
