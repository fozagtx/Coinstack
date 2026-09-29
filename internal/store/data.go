package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fozagtx/coinstack/internal/model"
)

const metaMapFetchedAt = "map_fetched_at"

// writeSnapshot upserts the quotes' assets and appends one snapshot row per
// asset. It must run inside a transaction (temp table, COPY).
func writeSnapshot(ctx context.Context, tx pgx.Tx, quotes []model.Quote, fallbackFetchedAt time.Time) error {
	quotes = latestPerID(quotes)
	now := time.Now()

	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE snapshot_assets (
		cmc_id bigint, symbol text, name text, slug text, rank integer, date_added timestamptz, tags text[]
	) ON COMMIT DROP`); err != nil {
		return err
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"snapshot_assets"},
		[]string{"cmc_id", "symbol", "name", "slug", "rank", "date_added", "tags"},
		pgx.CopyFromSlice(len(quotes), func(i int) ([]any, error) {
			q := &quotes[i]
			return []any{q.ID, q.Symbol, q.Name, q.Slug, nullInt(q.Rank), nullTime(q.DateAdded), q.Tags}, nil
		}),
	); err != nil {
		return fmt.Errorf("copy snapshot assets: %w", err)
	}
	// Unchanged rows are skipped to keep write amplification (and Neon
	// storage churn) low; a nil date_added or tags keeps the stored value.
	if _, err := tx.Exec(ctx, `
		INSERT INTO assets AS a (cmc_id, symbol, name, slug, rank, is_active, date_added, tags, updated_at)
		SELECT cmc_id, symbol, name, slug, rank, true, date_added, tags, $1::timestamptz FROM snapshot_assets
		ON CONFLICT (cmc_id) DO UPDATE SET
			symbol     = EXCLUDED.symbol,
			name       = EXCLUDED.name,
			slug       = EXCLUDED.slug,
			rank       = EXCLUDED.rank,
			is_active  = true,
			date_added = COALESCE(EXCLUDED.date_added, a.date_added),
			tags       = COALESCE(EXCLUDED.tags, a.tags),
			updated_at = EXCLUDED.updated_at
		WHERE (a.symbol, a.name, a.slug, a.rank, a.is_active, a.date_added, a.tags)
			IS DISTINCT FROM (EXCLUDED.symbol, EXCLUDED.name, EXCLUDED.slug, EXCLUDED.rank, true,
				COALESCE(EXCLUDED.date_added, a.date_added), COALESCE(EXCLUDED.tags, a.tags))`,
		now); err != nil {
		return fmt.Errorf("upsert assets: %w", err)
	}

	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"snapshots"},
		[]string{"cmc_id", "rank", "price", "market_cap", "volume_24h", "change_1h_pct", "change_24h_pct",
			"change_7d_pct", "circulating_supply", "total_supply", "max_supply", "cmc_last_updated", "fetched_at"},
		pgx.CopyFromSlice(len(quotes), func(i int) ([]any, error) {
			q := &quotes[i]
			fetched := q.FetchedAt
			if fetched.IsZero() {
				fetched = fallbackFetchedAt
			}
			return []any{q.ID, nullInt(q.Rank), q.Price, q.MarketCap, q.Volume24h, q.Change1hPct, q.Change24hPct,
				q.Change7dPct, q.CirculatingSupply, q.TotalSupply, q.MaxSupply, nullTime(q.LastUpdated), fetched}, nil
		}),
	); err != nil {
		return fmt.Errorf("copy snapshots: %w", err)
	}
	return nil
}

// latestPerID drops repeated ids, keeping the quote with the latest
// LastUpdated, so one snapshot never holds two rows for an asset.
func latestPerID(quotes []model.Quote) []model.Quote {
	idx := make(map[int64]int, len(quotes))
	out := quotes[:0:0]
	for _, q := range quotes {
		if i, ok := idx[q.ID]; ok {
			if q.LastUpdated.After(out[i].LastUpdated) {
				out[i] = q
			}
			continue
		}
		idx[q.ID] = len(out)
		out = append(out, q)
	}
	return out
}

func copyPollRuns(ctx context.Context, tx pgx.Tx, runs []model.PollRun) error {
	if len(runs) == 0 {
		return nil
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"poll_runs"},
		[]string{"kind", "started_at", "finished_at", "ok", "http_status", "credits_used", "assets_fetched", "error"},
		pgx.CopyFromSlice(len(runs), func(i int) ([]any, error) {
			r := &runs[i]
			return []any{r.Kind, r.StartedAt, nullTime(r.FinishedAt), r.OK, r.HTTPStatus,
				r.CreditsUsed, r.AssetsFetched, nullString(r.Error)}, nil
		}),
	)
	if err != nil {
		return fmt.Errorf("copy poll_runs: %w", err)
	}
	return nil
}

func copyRequestLogs(ctx context.Context, tx pgx.Tx, reqs []model.RequestLog) error {
	if len(reqs) == 0 {
		return nil
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"request_log"},
		[]string{"ts", "key_id", "endpoint", "status", "latency_ms"},
		pgx.CopyFromSlice(len(reqs), func(i int) ([]any, error) {
			r := &reqs[i]
			var keyID any
			if r.KeyID != 0 {
				keyID = r.KeyID
			}
			return []any{r.TS, keyID, r.Endpoint, r.Status, r.LatencyMS}, nil
		}),
	)
	if err != nil {
		return fmt.Errorf("copy request_log: %w", err)
	}
	return nil
}

// LoadLatestSnapshot returns the most recent persisted quote of every asset
// fetched within maxAge (no limit when maxAge <= 0), with identity fields
// (symbol, name, slug, date_added, tags) from the assets table. Use it to
// warm the cache on startup; each quote keeps its true LastUpdated and
// FetchedAt.
func (s *Store) LoadLatestSnapshot(ctx context.Context, maxAge time.Duration) ([]model.Quote, error) {
	since := time.Unix(0, 0)
	if maxAge > 0 {
		since = s.now().Add(-maxAge)
	}
	var out []model.Quote
	err := s.pool.withConn(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `
			SELECT s.cmc_id, COALESCE(a.symbol, ''), COALESCE(a.name, ''), COALESCE(a.slug, ''), COALESCE(s.rank, 0),
				s.price, s.market_cap, s.volume_24h, s.change_1h_pct, s.change_24h_pct, s.change_7d_pct,
				s.circulating_supply, s.total_supply, s.max_supply, a.date_added, a.tags,
				s.cmc_last_updated, s.fetched_at
			FROM (
				SELECT DISTINCT ON (cmc_id) *
				FROM snapshots
				WHERE fetched_at >= $1::timestamptz
				ORDER BY cmc_id, fetched_at DESC
			) s
			LEFT JOIN assets a ON a.cmc_id = s.cmc_id
			ORDER BY s.cmc_id`, since)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				q                  model.Quote
				dateAdded, lastUpd *time.Time
			)
			if err := rows.Scan(&q.ID, &q.Symbol, &q.Name, &q.Slug, &q.Rank,
				&q.Price, &q.MarketCap, &q.Volume24h, &q.Change1hPct, &q.Change24hPct, &q.Change7dPct,
				&q.CirculatingSupply, &q.TotalSupply, &q.MaxSupply, &dateAdded, &q.Tags,
				&lastUpd, &q.FetchedAt); err != nil {
				return err
			}
			q.DateAdded = derefTime(dateAdded)
			q.LastUpdated = derefTime(lastUpd)
			q.FetchedAt = q.FetchedAt.UTC()
			out = append(out, q)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: load latest snapshot: %w", err)
	}
	return out, nil
}

// LoadHistory rebuilds per-asset history rings from persisted snapshots:
// one sample per (cmc_id, hour bucket), the latest row of each bucket,
// within window, ordered oldest first.
func (s *Store) LoadHistory(ctx context.Context, window time.Duration) (map[int64][]model.Sample, error) {
	since := time.Unix(0, 0)
	if window > 0 {
		since = s.now().Add(-window)
	}
	out := map[int64][]model.Sample{}
	err := s.pool.withConn(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `
			SELECT DISTINCT ON (cmc_id, date_trunc('hour', fetched_at))
				cmc_id, COALESCE(rank, 0), price, market_cap, volume_24h, fetched_at
			FROM snapshots
			WHERE fetched_at >= $1::timestamptz
			ORDER BY cmc_id, date_trunc('hour', fetched_at), fetched_at DESC`, since)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sm model.Sample
			var id int64
			var at time.Time
			if err := rows.Scan(&id, &sm.Rank, &sm.Price, &sm.MarketCap, &sm.Volume24h, &at); err != nil {
				return err
			}
			sm.At = at.UTC()
			out[id] = append(out[id], sm)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: load history: %w", err)
	}
	return out, nil
}

// SaveMap upserts CMC's id map into assets and marks every asset absent from
// entries inactive, in one transaction, then records fetchedAt as the map's
// age. The ~10-30k rows are bulk-loaded with COPY into a temporary table.
// An empty map is rejected rather than deactivating everything.
func (s *Store) SaveMap(ctx context.Context, entries []model.MapEntry, fetchedAt time.Time) error {
	if len(entries) == 0 {
		return errors.New("store: save map: no entries")
	}
	if fetchedAt.IsZero() {
		fetchedAt = s.now()
	}
	fetchedAt = fetchedAt.UTC()
	err := s.pool.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE map_import (
			cmc_id bigint, symbol text, name text, slug text, rank integer, is_active boolean,
			platform text, first_historical_data timestamptz
		) ON COMMIT DROP`); err != nil {
			return err
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"map_import"},
			[]string{"cmc_id", "symbol", "name", "slug", "rank", "is_active", "platform", "first_historical_data"},
			pgx.CopyFromSlice(len(entries), func(i int) ([]any, error) {
				e := &entries[i]
				return []any{e.ID, e.Symbol, e.Name, e.Slug, nullInt(e.Rank), e.IsActive,
					nullString(e.Platform), nullTime(e.FirstHistoricalData)}, nil
			}),
		); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO assets AS a (cmc_id, symbol, name, slug, rank, is_active, platform, first_historical_data, updated_at)
			SELECT DISTINCT ON (cmc_id) cmc_id, symbol, name, slug, rank, is_active, platform, first_historical_data, $1::timestamptz
			FROM map_import
			ORDER BY cmc_id
			ON CONFLICT (cmc_id) DO UPDATE SET
				symbol                = EXCLUDED.symbol,
				name                  = EXCLUDED.name,
				slug                  = EXCLUDED.slug,
				rank                  = EXCLUDED.rank,
				is_active             = EXCLUDED.is_active,
				platform              = EXCLUDED.platform,
				first_historical_data = COALESCE(EXCLUDED.first_historical_data, a.first_historical_data),
				updated_at            = EXCLUDED.updated_at
			WHERE (a.symbol, a.name, a.slug, a.rank, a.is_active, a.platform, a.first_historical_data)
				IS DISTINCT FROM (EXCLUDED.symbol, EXCLUDED.name, EXCLUDED.slug, EXCLUDED.rank, EXCLUDED.is_active,
					EXCLUDED.platform, COALESCE(EXCLUDED.first_historical_data, a.first_historical_data))`,
			fetchedAt); err != nil {
			return fmt.Errorf("upsert: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE assets a SET is_active = false, updated_at = $1::timestamptz
			WHERE a.is_active AND NOT EXISTS (SELECT 1 FROM map_import m WHERE m.cmc_id = a.cmc_id)`,
			fetchedAt); err != nil {
			return fmt.Errorf("deactivate: %w", err)
		}
		return setMeta(ctx, tx, metaMapFetchedAt, fetchedAt.Format(time.RFC3339Nano))
	})
	if err != nil {
		return fmt.Errorf("store: save map: %w", err)
	}
	return nil
}

func setMeta(ctx context.Context, tx pgx.Tx, key, value string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO meta (key, value, updated_at) VALUES ($1::text, $2::text, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
		key, value)
	return err
}

// LoadMap returns the active assets ordered by id, and when the map was
// last saved (from SaveMap, else the latest asset update; zero when the
// table is empty).
func (s *Store) LoadMap(ctx context.Context) ([]model.MapEntry, time.Time, error) {
	var (
		out       []model.MapEntry
		fetchedAt time.Time
	)
	err := s.pool.withConn(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `
			SELECT cmc_id, symbol, name, slug, COALESCE(rank, 0), is_active, COALESCE(platform, ''), first_historical_data
			FROM assets WHERE is_active ORDER BY cmc_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				e     model.MapEntry
				first *time.Time
			)
			if err := rows.Scan(&e.ID, &e.Symbol, &e.Name, &e.Slug, &e.Rank, &e.IsActive, &e.Platform, &first); err != nil {
				return err
			}
			e.FirstHistoricalData = derefTime(first)
			out = append(out, e)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		var value string
		err = c.QueryRow(ctx, "SELECT value FROM meta WHERE key = $1::text", metaMapFetchedAt).Scan(&value)
		switch {
		case err == nil:
			fetchedAt, err = time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return fmt.Errorf("parse %s %q: %w", metaMapFetchedAt, value, err)
			}
			fetchedAt = fetchedAt.UTC()
			return nil
		case errors.Is(err, pgx.ErrNoRows):
			var latest *time.Time
			if err := c.QueryRow(ctx, "SELECT max(updated_at) FROM assets").Scan(&latest); err != nil {
				return err
			}
			fetchedAt = derefTime(latest)
			return nil
		default:
			return err
		}
	})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: load map: %w", err)
	}
	return out, fetchedAt, nil
}

// CreditsUsedToday sums poll_runs.credits_used for runs started since the
// most recent UTC midnight. Runs still in the write queue are not counted.
func (s *Store) CreditsUsedToday(ctx context.Context) (int, error) {
	var sum int64
	err := s.pool.withConn(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx,
			"SELECT COALESCE(sum(credits_used), 0)::bigint FROM poll_runs WHERE started_at >= $1::timestamptz",
			utcMidnight(s.now())).Scan(&sum)
	})
	if err != nil {
		return 0, fmt.Errorf("store: credits used today: %w", err)
	}
	return int(sum), nil
}

// utcMidnight returns the start of t's day in UTC.
func utcMidnight(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// nullInt maps 0 to NULL (CMC's "no rank").
func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}
