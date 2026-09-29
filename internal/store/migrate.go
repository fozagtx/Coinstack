package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID is the pg_advisory_xact_lock key serialising migrations
// across instances starting at the same time.
const migrationLockID int64 = 0x436f696e53746b31 // "CoinStk1"

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations returns the embedded migrations ordered by version. File
// names must start with a positive integer version followed by "_".
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	seen := make(map[int]string)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("store: migration %q: name must start with a positive version and '_'", e.Name())
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("store: migrations %q and %q share version %d", prev, e.Name(), v)
		}
		seen[v] = e.Name()
		b, err := migrationFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// migrate applies pending migrations in one transaction under a
// transaction-scoped advisory lock. Session-level locks are not safe behind
// PgBouncer in transaction mode; the xact variant is released at commit.
func (s *Store) migrate(ctx context.Context) error {
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	var applied []int
	err = s.pool.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1::bigint)", migrationLockID); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version    integer PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}
		rows, err := tx.Query(ctx, "SELECT version FROM schema_migrations")
		if err != nil {
			return err
		}
		done, err := pgx.CollectRows(rows, pgx.RowTo[int32])
		if err != nil {
			return err
		}
		have := make(map[int]bool, len(done))
		for _, v := range done {
			have[int(v)] = true
		}
		for _, m := range migs {
			if have[m.version] {
				continue
			}
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return fmt.Errorf("apply %s: %w", m.name, err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1::integer)", m.version); err != nil {
				return fmt.Errorf("record %s: %w", m.name, err)
			}
			applied = append(applied, m.version)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	if len(applied) > 0 {
		s.log.Info("store: applied migrations", "versions", applied)
	}
	return nil
}
