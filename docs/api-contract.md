# CoinStack API contract (v1)

This is the wire contract every part of CoinStack is built against: handlers,
OpenAPI spec, docs page, demo agent and load test. `internal/api/openapi.json`
must match it.

## Conventions

- All endpoints are `GET` and return `application/json; charset=utf-8`.
- Auth: `Authorization: Bearer <key>` (the header `X-API-Key: <key>` is also
  accepted). `/v1/openapi.json`, `/v1/health` and `/` (docs page) need no key.
- Numbers are JSON numbers, never strings. Percent fields end in `_pct` and
  are percentages (`-2.1` means -2.1 %). Timestamps are ISO 8601 UTC with
  second precision (`2026-09-29T12:04:00Z`).
- Unknown query parameters are rejected with `400 invalid_parameter`.
- Agents may pass a symbol (`SOL`), a name (`Solana`), a slug (`solana`) or a
  CMC id (`5426`) wherever an asset is expected. Responses always carry the
  CMC `id`, so follow-up calls can be exact.
- `currency` (optional, default `USD`) is accepted by price, asset, compare,
  movers and new-tokens. Non-USD values are converted from USD at the latest
  fiat rate; `change_*_pct` fields are always measured in USD. Thresholds such
  as `min_volume` are in the requested currency.

## Success envelope

```json
{
  "as_of": "2026-09-29T12:04:00Z",
  "age_seconds": 21,
  "source": "coinmarketcap",
  "currency": "USD",
  "data": [ ... ],
  "warnings": [ { "code": "stale_data", "message": "..." } ]
}
```

- `as_of` is the **oldest** CMC `last_updated` among the returned items (for
  `/v1/resolve`, the time the id map was fetched).
- `age_seconds` = whole seconds between `as_of` and the moment the response is
  built. Integer, never negative.
- `data` is an array everywhere except `/v1/asset`, where it is one object.
- `warnings` is omitted when empty. Warning codes:
  - `stale_data`: data is older than the freshness limit (default 180 s). Adds
    `"age_seconds"` to the warning.
  - `symbol_resolved_by_rank`: a ticker matched several assets and the API
    picked the dominant one. Adds `"query"`, `"chosen_id"` and
    `"candidates"` (the alternatives).
  - `outside_top_n`: some assets were fetched on demand rather than from the
    top-N cache (informational).

## Asset object

Price-level fields, used by `/v1/price`, `/v1/movers`, `/v1/new-tokens`:

| field | type | notes |
| --- | --- | --- |
| `id` | integer | CMC id |
| `symbol` | string | |
| `name` | string | |
| `rank` | integer | CMC rank; omitted when unranked |
| `price` | number | in `currency` |
| `market_cap` | number | in `currency` |
| `volume_24h` | number | in `currency` |
| `change_1h_pct` | number | USD-based |
| `change_24h_pct` | number | USD-based |
| `change_7d_pct` | number | USD-based |
| `last_updated` | string | CMC's own timestamp for this item |

`/v1/new-tokens` items also carry `date_added`.

Detail fields, added by `/v1/asset` and `/v1/compare`:

| field | type | notes |
| --- | --- | --- |
| `slug` | string | |
| `circulating_supply` | number | |
| `total_supply` | number | |
| `max_supply` | number or null | null when uncapped / unknown |
| `date_added` | string | when CMC listed it |
| `tags` | array of string | at most 10 |

`/v1/asset` additionally may carry `category` (`coin`/`token`), `platform`
and `website` when metadata is available (omitted otherwise).

## Endpoints

| Endpoint | Params | Notes |
| --- | --- | --- |
| `GET /v1/price` | `asset` (required, comma list, max 20), `currency` | `data` in request order; duplicates collapse |
| `GET /v1/asset` | `asset` (required, exactly one), `currency` | `data` is one object |
| `GET /v1/compare` | `assets` (required, 2-5, comma list), `currency` | `data` in request order |
| `GET /v1/movers` | `window` (`1h`,`24h`,`7d`; default `24h`), `direction` (`up`,`down`; default `up`), `min_volume` (default `100000`), `min_market_cap` (default `0`), `limit` (default 10, max 50), `currency` | computed over the top-N cache only; `up` sorts by change descending, `down` ascending |
| `GET /v1/new-tokens` | `days` (default 7, 1-30), `min_volume` (default 0), `limit` (default 10, max 50), `currency` | newest first |
| `GET /v1/resolve` | `query` (required), `limit` (default 5, max 20) | `data` = candidates `{id, symbol, name, slug, rank, platform, match}`; top level adds `"resolved_id"` (integer or null) = what `asset=<query>` would resolve to |
| `GET /v1/openapi.json` | none | OpenAPI 3.0 document |
| `GET /v1/health` | none | see below |

### `/v1/health`

```json
{
  "status": "ok",
  "version": "dev",
  "uptime_seconds": 3600,
  "last_poll_at": "...",
  "last_success_at": "...",
  "last_error": "",
  "age_seconds": 21,
  "cache_size": 500,
  "on_demand_cache_size": 3,
  "resolver_assets": 9876,
  "top_n": 500,
  "poll_interval_seconds": 60,
  "credits_used_today": 1234,
  "credits_used_month": 45678,
  "credit_limit_monthly": 300000,
  "upstream_calls": 1500,
  "upstream_errors": 2,
  "requests_total": 10000,
  "requests_by_status": { "200": 9900, "404": 100 },
  "currencies": ["USD", "EUR"]
}
```

`status` is `ok` when the last successful poll is within the freshness limit,
`degraded` when data is stale but within the max-stale limit, and `down`
otherwise. `ok` and `degraded` return HTTP 200; `down` returns HTTP 503 with
the same body, so uptime checkers alert on it.

## Errors

```json
{
  "error": {
    "code": "ambiguous_asset",
    "message": "\"UNI\" matches 3 assets.",
    "next_step": "Retry with asset=<id> using one of the candidates' ids.",
    "param": "asset",
    "query": "UNI",
    "candidates": [ { "id": 7083, "symbol": "UNI", "name": "Uniswap", "slug": "uniswap", "rank": 20 } ]
  }
}
```

| Situation | HTTP | `code` | extra fields |
| --- | --- | --- | --- |
| Ticker matches several assets, none dominant | 409 | `ambiguous_asset` | `param`, `query`, `candidates` |
| Asset not found | 404 | `asset_not_found` | `param`, `query`, `candidates` (closest matches, may be empty) |
| Upstream CMC unavailable and nothing usable cached | 503 | `upstream_unavailable` | `retry_after_seconds`; when older cached data exists the body also carries top-level `as_of`, `age_seconds`, `source`, `currency`, `data` |
| On-demand credit budget spent | 503 | `upstream_budget_exhausted` | `retry_after_seconds` |
| Bad or unknown parameter | 400 | `invalid_parameter` | `param`, `allowed_values` when enumerable |
| Missing or invalid API key | 401 | `unauthorized` | |
| Per-key rate limit exceeded | 429 | `rate_limited` | `retry_after_seconds` (and `Retry-After` header) |
| Unknown path | 404 | `not_found` | |
| Unexpected failure | 500 | `internal_error` | |

`next_step` is always present: one plain sentence telling the agent what to do.
