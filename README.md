# CoinStack

Altcoin-discovery API for AI agents. CoinStack polls the top assets from
CoinMarketCap in tiers, keeps a week of hourly history, and scores every
asset on four transparent signals — turnover, listing recency, rank climb
and sector heat — to surface early, high-upside candidates. The score ranks
candidates for review; it does not predict outcomes. **Market data for
information only, not financial advice.**

## Quickstart — local, no API key

CoinStack ships a local fake CoinMarketCap server:

```sh
go run ./cmd/fakecmc            # serves CMC-shaped data on :8181
```

In a second terminal:

```sh
CMC_BASE_URL=http://localhost:8181 COINSTACK_AUTH=off go run ./cmd/coinstack
curl http://localhost:8080/v1/gems?limit=5
curl http://localhost:8080/v1/health
```

## Quickstart — real key

```sh
cp .env.example .env            # the binary loads ./.env automatically
# edit .env: set CMC_API_KEY, choose a preset, set auth keys
go run ./cmd/coinstack
```

## Endpoints

All endpoints are `GET`, return `application/json`, and need
`Authorization: Bearer <key>` (or `X-API-Key:`) unless marked public.

| Endpoint | What it returns |
| --- | --- |
| `/v1/gems` | Ranked candidates: composite score, per-signal breakdown, risk flags, plain-English reasons |
| `/v1/screen` | Filter the cached universe by cap, volume, turnover, price changes, tags, listing age |
| `/v1/climbers` | Biggest CMC rank moves over 24h or 7d, from retained history |
| `/v1/new-listings` | Assets first listed within the last N days |
| `/v1/sectors` | Tag aggregates ranked by sector heat; `?sector=` for members |
| `/v1/asset` | Full detail for one asset plus its signal breakdown |
| `/v1/resolve` | Resolve symbol/name/slug/id to candidates |
| `/v1/health` | Poller status, cache sizes, credit usage (public) |
| `/v1/openapi.json` | OpenAPI 3.0 document (public) |
| `/` | Human-readable docs (public) |

## How the score works

Composite = weighted sum, renormalized when rank-climb history is missing:

| Signal | Weight | What it measures |
| --- | --- | --- |
| turnover | 0.30 | `volume_24h / market_cap` on a 0.05–2 log band; blended with a surge term vs. the asset's own baseline |
| new_listing | 0.20 | listing recency, linear decay to 0 at 90 days |
| rank_climb | 0.30 | rank gains over 24h (60%) and 7d (40%) |
| sector_heat | 0.20 | hottest tag the asset carries; heat = sector median 24h change scaled to +15% |

Risk flags: `already_pumped` (score ×0.6), `thin_volume`, `micro_cap`,
`new_and_unproven`, `insufficient_history`, `unranked`. Confidence is
`low`/`medium`/`high` from history depth (<24h / <72h / ≥72h).

## Credit budget

`COINSTACK_PRESET` picks a polling schedule; individual `COINSTACK_*` env
vars override it. Projected CMC credit burn (1 credit per 200 assets per
call, plus overhead):

| Preset | TopN | FastN | Poll / slow | ~Credits/day | ~Credits/month |
| --- | --- | --- | --- | --- | --- |
| free | 1000 | 200 | 10m / 30m | ~390 | ~12k |
| startup | 3000 | 200 | 2m / 15m | ~2.1k | ~63k |
| standard | 5000 | 500 | 1m / 10m | ~7.7k | ~230k |

`/v1/health` reports `projected_credits_per_day` and actual usage.

## Telegram bot

Optional; disabled while `TELEGRAM_BOT_TOKEN` is empty.

1. Create a bot with @BotFather, copy the token into `.env`.
2. Start the service, send `/start` to the bot from your account — the bot
   replies `not authorised, your chat id is N`.
3. Put that id in `TELEGRAM_CHAT_IDS` (comma-separated for several chats)
   and restart.
4. Commands: `/gems`, `/screen`, `/climbers [24h|7d]`, `/new [days]`,
   `/sectors`, `/sector <tag>`, `/asset <query>`, `/status`, `/help`.
5. Alerts (`TELEGRAM_ALERTS`, default all): `gems` (new entrants into the
   top-10 score), `climbers` (≥50 ranks and ≥25% in 24h), `listings` (new
   listings with ≥$100k volume), `digest` (daily summary at
   `TELEGRAM_DIGEST_HOUR` UTC). Alert state is in memory; a restart
   re-seeds silently.

## Deploy (Railway / Render)

```sh
docker build -t coinstack .
```

- Dockerfile is a multi-stage distroless build; the binary listens on
  `PORT` (both platforms inject it).
- Set env vars: `CMC_API_KEY`, `COINSTACK_PRESET`, `COINSTACK_API_KEYS`
  (or `DATABASE_URL` + `coinstack keys create`), optionally
  `TELEGRAM_*` and `COINSTACK_AUTH=on`.
- No `DATABASE_URL` → runs memory-only (no restart reload, static keys).

### Render (one click)

`render.yaml` is a Blueprint: Render dashboard → New → Blueprint → select
this repo. It creates the web service (Docker, health check on
`/v1/health`) and a small Postgres, and prompts for the secrets:
`CMC_API_KEY`, `COINSTACK_API_KEYS`, `TELEGRAM_BOT_TOKEN`,
`TELEGRAM_CHAT_IDS`. Use a paid instance — the free tier sleeps when idle,
which stops polling and the Telegram bot. To run without Postgres, delete
the `databases` block and the `DATABASE_URL` entry.

## API keys CLI

```sh
DATABASE_URL=postgres://... go run ./cmd/coinstack keys create --owner alice --rate-limit 120
go run ./cmd/coinstack keys list
go run ./cmd/coinstack keys revoke 3
```

Static keys without a database: `COINSTACK_API_KEYS=key1:owner:rpm,key2`.

## Layout

- `cmd/coinstack` — service + keys CLI; `cmd/fakecmc` — local fake CMC.
- `internal/cmc` — rate-limited CMC client; `internal/market` — tiered
  poller, snapshot, history ring; `internal/discover` — shared discovery
  engine used by `internal/api` (HTTP) and `internal/telegram` (bot).
- `internal/signals` — scoring; `internal/resolve` — asset resolution;
  `internal/store` — Postgres persistence; `internal/config` — env config.

`docs/api-contract.md` is the wire contract; `docs/PRD.md` is the product spec.
