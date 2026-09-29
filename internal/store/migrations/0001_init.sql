-- Asset identity, merged from CMC's map (daily) and from listings quotes.
CREATE TABLE IF NOT EXISTS assets (
    cmc_id                bigint PRIMARY KEY,
    symbol                text NOT NULL DEFAULT '',
    name                  text NOT NULL DEFAULT '',
    slug                  text NOT NULL DEFAULT '',
    rank                  integer,
    is_active             boolean NOT NULL DEFAULT true,
    platform              text,
    first_historical_data timestamptz,
    date_added            timestamptz,
    tags                  text[],
    updated_at            timestamptz NOT NULL DEFAULT now()
);

-- Periodic copies of the top-N cache (USD), kept for a rolling window.
CREATE TABLE IF NOT EXISTS snapshots (
    cmc_id             bigint NOT NULL,
    rank               integer,
    price              double precision NOT NULL,
    market_cap         double precision NOT NULL,
    volume_24h         double precision NOT NULL,
    change_1h_pct      double precision NOT NULL,
    change_24h_pct     double precision NOT NULL,
    change_7d_pct      double precision NOT NULL,
    circulating_supply double precision NOT NULL,
    total_supply       double precision NOT NULL,
    max_supply         double precision,
    cmc_last_updated   timestamptz,
    fetched_at         timestamptz NOT NULL
);
-- "Latest row per asset" (DISTINCT ON (cmc_id) ... ORDER BY cmc_id, fetched_at DESC)
-- and per-asset history.
CREATE INDEX IF NOT EXISTS snapshots_cmc_id_fetched_at_idx ON snapshots (cmc_id, fetched_at DESC);
-- Retention deletes and restore-window scans.
CREATE INDEX IF NOT EXISTS snapshots_fetched_at_idx ON snapshots (fetched_at);

CREATE TABLE IF NOT EXISTS api_keys (
    id         bigserial PRIMARY KEY,
    key_hash   text NOT NULL UNIQUE,
    key_prefix text NOT NULL,
    owner      text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    rate_limit integer NOT NULL DEFAULT 0 CHECK (rate_limit >= 0),
    status     text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    revoked_at timestamptz
);

CREATE TABLE IF NOT EXISTS poll_runs (
    id             bigserial PRIMARY KEY,
    kind           text NOT NULL,
    started_at     timestamptz NOT NULL,
    finished_at    timestamptz,
    ok             boolean NOT NULL,
    http_status    integer NOT NULL DEFAULT 0,
    credits_used   integer NOT NULL DEFAULT 0,
    assets_fetched integer NOT NULL DEFAULT 0,
    error          text
);
CREATE INDEX IF NOT EXISTS poll_runs_started_at_idx ON poll_runs (started_at);

CREATE TABLE IF NOT EXISTS request_log (
    ts         timestamptz NOT NULL,
    key_id     bigint, -- NULL when unauthenticated
    endpoint   text NOT NULL,
    status     integer NOT NULL,
    latency_ms double precision NOT NULL
);
CREATE INDEX IF NOT EXISTS request_log_ts_idx ON request_log (ts);

CREATE TABLE IF NOT EXISTS meta (
    key        text PRIMARY KEY,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
