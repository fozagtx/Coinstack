# PRD: CMC for Agents (a simple, fast API on top of CoinMarketCap data)

Sep 29, 2026 · product name: **CoinStack**

## 1. Overview

**CMC for Agents is a small REST API that gives AI agents the market data they need through a handful of plainly named endpoints, so an agent never has to learn CoinMarketCap's many endpoints, IDs, credit costs or error codes.** It is built on the CMC API for the "Build with CMC: API Hackathon", in the AI Agents and Automation track, and written in Go so one lightweight service can serve many agents at once.

**Problem.** CMC's API is broad. To answer "what is SOL trading at?" an agent has to pick the right endpoint, turn a ticker into a CMC ID (and avoid duplicate tickers), request the right fields, parse a deeply nested response, and handle credit limits and rate limits. Agents waste tokens and make mistakes on every one of those steps, and a naive setup spends CMC credits on every agent call.

**Solution.** A thin layer between agents and CMC:

- Five simple endpoints with plain names and one flat JSON shape.
- Ticker and name resolution done for the agent, with a clear error when a ticker is ambiguous.
- Responses served from a cache that a background poller keeps fresh, so replies take milliseconds and agent traffic does not consume CMC credits.
- An OpenAPI spec so any agent framework can read the API and call it directly.

**One-line pitch.** "CMC data, shaped for agents: five endpoints, one JSON shape, answers in milliseconds."

## 2. Goals, non-goals and success metrics

**Goals**

- An agent can get a correct answer for common market questions in one call, with no knowledge of CMC.
- Every response uses the same field names and includes `as_of` and `age_seconds`, so an agent knows how fresh the data is.
- Ticker and name resolution is correct, including duplicate tickers, or the API returns a clear error listing the choices.
- Responses are fast and cheap: served from cache, with no CMC credits spent per agent request.
- The service stays within the CMC Startup-tier credit budget no matter how many agents call it.
- Ship a public, documented, running API with an OpenAPI spec, a demo agent and a demo video before the hackathon closes.

**Non-goals (v1)**

- No Telegram, Discord or any messaging or alerting.
- No LLM inside the API, no MCP server, no chat interface. It is plain REST.
- No wallets, portfolios, trading or any write operations. Read-only market data.
- No sub-minute freshness claims; data is as fresh as CMC's roughly 60-second refresh.
- No re-selling of raw CMC data or bulk export; responses are shaped answers.

**Success metrics**

| Metric | Target | How measured |
| --- | --- | --- |
| Response latency on cached endpoints | p95 under 100 ms at 50 requests per second | Load test with a fixed script |
| CMC credits spent per 1,000 API requests | Near zero for top-asset queries | Credit counter vs request counter |
| Symbol resolution accuracy | 99% or higher on a 200-symbol test set that includes duplicate tickers | Automated test |
| Steps for an agent to answer "price of X" | 1 call | Demo agent trace |
| Responses carrying `as_of` and `age_seconds` | 100% | Automated test on every endpoint |
| Uptime during the judging window | 99% or higher | Health check log |

## 3. Users and user stories

1. As an agent developer, I call `/v1/price?asset=SOL` and get the price, 24h change, market cap and volume in one flat object.
2. As an agent, I ask for `asset=ETH,BTC,SOL` in one call and get all three, so I don't loop over separate requests.
3. As an agent, when I send an ambiguous ticker I get an error that lists the candidate assets with their IDs, so I can choose correctly.
4. As an agent, I ask for the top movers over 24 hours with a minimum volume, so tiny illiquid tokens don't fill the list.
5. As an agent, I ask for tokens listed in the last 7 days and get their listing date and volume.
6. As a developer, I read one OpenAPI file and my agent framework can call every endpoint with no extra glue.
7. As a developer, I see `as_of` and `age_seconds` on every response, so I know the data is not stale.
8. As a developer, I get a clear message when the upstream CMC API is down, not a silent wrong answer.

## 4. API design

See [api-contract.md](api-contract.md) for the exact wire contract.

| ID | Endpoint | What it answers | Priority |
| --- | --- | --- | --- |
| A1 | `/v1/price` | Current price and basics for one or more assets | P0 |
| A2 | `/v1/asset` | One asset in more detail: rank, supply, tags, listing date, 1h/24h/7d change | P0 |
| A3 | `/v1/compare` | Two to five assets side by side on the same fields | P1 |
| A4 | `/v1/movers` | Top gainers or losers over a window | P0 |
| A5 | `/v1/new-tokens` | Recently listed assets | P1 |
| A6 | `/v1/resolve` | Turns a symbol or name into candidate CMC assets | P1 |
| A7 | `/v1/openapi.json` | The OpenAPI spec for the API | P0 |
| A8 | `/v1/health` | Last successful CMC poll, cache size, credits used today | P0 |

**Design rules**

- Agents send symbols, names or IDs; the API always returns the CMC `id` so follow-up calls are exact.
- Numbers are numbers, not strings. Percent fields end in `_pct`. Timestamps are ISO 8601 UTC.
- Responses stay small: only the fields listed, with no nested CMC structures.
- Defaults are safe: `limit` defaults to 10 and is capped at 50.
- The OpenAPI spec includes a one-sentence description and an example for every endpoint and parameter.

## 5. CMC data plan

| Used for | CMC endpoint | Notes |
| --- | --- | --- |
| Price, volume, market cap, changes for the top assets, and movers | `/v1/cryptocurrency/listings/latest` | The poller reads the top N by market cap every cycle |
| Assets outside the top N, fetched on demand | `/v2/cryptocurrency/quotes/latest` by ID | Short cache; identical simultaneous requests share one call |
| Ticker and name resolution | `/v1/cryptocurrency/map` | Cached and refreshed daily |
| Detail: tags, listing date, supply | `/v2/cryptocurrency/info` | Cached for a day |
| New listings | `/v1/cryptocurrency/listings/new` | Fallback: recent `first_historical_data` in the map |

- A background poller refreshes the top N assets (N = 500) every 60 seconds. Agents read from memory, never straight from CMC for common assets.
- Movers, compare and price for top assets are computed from that cache with no extra CMC calls.
- For an asset outside the top N, the API fetches it once and caches it for 60 seconds, coalescing identical concurrent requests.
- Every cached record keeps CMC's `last_updated` and the fetch time, so `age_seconds` is always exact.
- If CMC fails, the API keeps serving the last good data with its real age and a `stale_data` warning, up to a set limit, then returns `upstream_unavailable`.

Credit budget: `calls_per_day = 1440 × ⌈N / P⌉`. Keep projected use under ~70 % of the plan's monthly credits; otherwise lower N, then slow the cycle for lower-ranked assets.

## 6. Architecture in Go

| Component | Go approach |
| --- | --- |
| Poller | One goroutine with a ticker; parallel page fetches with a small worker pool; rate limiter |
| Resolver index | Built from the CMC map; read-only after build, lock-free lookups |
| Cache | Copy-on-write snapshot in an `atomic.Pointer` |
| On-demand fetcher | `singleflight`; 60-second cache |
| HTTP handlers | `net/http` with chi |
| Writer | Buffered channel plus one goroutine writing batches to Neon |
| Health and metrics | In-memory counters exposed on `/v1/health` |

Neon tables: `assets`, `snapshots`, `api_keys`, `poll_runs`, `request_log`. Serve from memory first; Neon is for history, restarts and metrics. Load the last snapshot on startup. Keep a rolling 7-day window of snapshots.

## 7. Non-functional requirements

- p95 under 100 ms at 50 rps on one small instance; a failed CMC call never blocks agent requests.
- Clean restart with snapshot reload; graceful shutdown flushes the write buffer.
- CMC key only in environment variables. Agent keys hashed, revocable, rate limited (default 60/min).
- Inputs validated and bounded; unknown parameters rejected. Read-only; no personal data.
- Every response states its source and age; market data for information only, not financial advice.
- Structured logs; `/v1/health`; log line when the last successful poll is older than 5 minutes.
