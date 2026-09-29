// Command coinstack serves CoinMarketCap market data shaped for AI agents.
//
// Usage:
//
//	coinstack [serve]                              run the API (configured by environment variables)
//	coinstack keys create --owner NAME [--rate-limit N]
//	coinstack keys list
//	coinstack keys revoke ID
//	coinstack version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/fozagtx/coinstack/internal/api"
	"github.com/fozagtx/coinstack/internal/cmc"
	"github.com/fozagtx/coinstack/internal/config"
	"github.com/fozagtx/coinstack/internal/market"
	"github.com/fozagtx/coinstack/internal/model"
	"github.com/fozagtx/coinstack/internal/resolve"
	"github.com/fozagtx/coinstack/internal/store"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "coinstack:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve()
	case "keys":
		return keysCmd(args, out)
	case "version":
		fmt.Fprintln(out, version)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(out, usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}
}

const usage = `usage:
  coinstack [serve]
  coinstack keys create --owner NAME [--rate-limit N]
  coinstack keys list
  coinstack keys revoke ID
  coinstack version
`

func serve() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Neon is optional: without it the service runs memory-only.
	var st *store.Store
	storeCtx, stopStore := context.WithCancel(context.Background())
	defer stopStore()
	storeDone := make(chan struct{})
	if cfg.DatabaseURL != "" {
		openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		st, err = store.Open(openCtx, store.Config{DatabaseURL: cfg.DatabaseURL, SnapshotEvery: cfg.SnapshotEvery, Logger: logger})
		cancel()
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		go func() { defer close(storeDone); st.Run(storeCtx) }()
	} else {
		close(storeDone)
		logger.Warn("DATABASE_URL not set: running memory-only (no history, no restart reload, static keys only)")
	}

	resolveOpts := resolve.Options{}
	resolver := &resolve.Live{}
	var seedMap []model.MapEntry
	var mapFetchedAt time.Time
	if st != nil {
		loadCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		seedMap, mapFetchedAt, err = st.LoadMap(loadCtx)
		cancel()
		if err != nil {
			logger.Warn("could not load asset map from database", "err", err)
			seedMap, mapFetchedAt = nil, time.Time{}
		} else if len(seedMap) > 0 {
			resolver.Swap(resolve.Build(seedMap, mapFetchedAt, resolveOpts))
			logger.Info("resolver restored from database", "assets", len(seedMap), "fetched_at", mapFetchedAt)
		}
	}

	client := cmc.New(cmc.Options{
		BaseURL:           cfg.CMCBaseURL,
		APIKey:            cfg.CMCAPIKey,
		RequestsPerMinute: cfg.CMCRPM,
		Logger:            logger,
	})

	mcfg := market.Config{
		TopN:                    cfg.TopN,
		FastN:                   cfg.FastN,
		PageSize:                cfg.PageSize,
		PollInterval:            cfg.PollInterval,
		SlowInterval:            cfg.SlowInterval,
		OnDemandBudgetPerMinute: cfg.OnDemandPerMin,
		OnDemandBudgetPerDay:    cfg.OnDemandPerDay,
		Currencies:              cfg.Currencies,
		LastMapFetch:            mapFetchedAt,
		SeedMap:                 seedMap,
		Logger:                  logger,
		OnMap: func(entries []model.MapEntry, fetchedAt time.Time) {
			resolver.Swap(resolve.Build(entries, fetchedAt, resolveOpts))
			logger.Info("resolver rebuilt", "assets", len(entries))
			if st != nil {
				go func() {
					saveCtx, cancel := context.WithTimeout(storeCtx, 2*time.Minute)
					defer cancel()
					if err := st.SaveMap(saveCtx, entries, fetchedAt); err != nil {
						logger.Warn("saving asset map failed", "err", err)
					}
				}()
			}
		},
	}
	if st != nil {
		mcfg.OnSnapshot = st.EnqueueSnapshot
		mcfg.OnPollRun = st.RecordPoll
	}
	mk := market.New(client, mcfg)

	if st != nil {
		loadCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		quotes, err := st.LoadLatestSnapshot(loadCtx, 24*time.Hour)
		cancel()
		if err != nil {
			logger.Warn("could not reload last snapshot", "err", err)
		} else if len(quotes) > 0 {
			mk.Seed(quotes)
			logger.Info("snapshot restored from database", "assets", len(quotes))
		}
	}

	var dbk dbKeys
	var reqlog api.RequestLogger
	if st != nil {
		dbk, reqlog = st, st
	}
	keys := newKeyChain(cfg.StaticKeys, dbk)
	srv := api.New(api.Config{
		Version:          version,
		DefaultRateLimit: cfg.DefaultRateLimit,
		StaleAfter:       cfg.StaleAfter,
		MaxStale:         cfg.MaxStale,
		TopN:             cfg.TopN,
		AuthDisabled:     !cfg.AuthEnabled,
		Logger:           logger,
	}, mk, resolver, keys, reqlog)
	if !cfg.AuthEnabled {
		logger.Warn("COINSTACK_AUTH=off: every endpoint is public; use only for local development")
	}

	marketCtx, stopMarket := context.WithCancel(context.Background())
	defer stopMarket()
	marketDone := make(chan struct{})
	go func() {
		defer close(marketDone)
		if err := mk.Run(marketCtx); err != nil {
			logger.Error("market poller stopped", "err", err)
		}
	}()

	httpSrv := &http.Server{
		Addr:              net.JoinHostPort("", cfg.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       90 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("coinstack listening", "addr", httpSrv.Addr, "version", version, "upstream", cfg.CMCBaseURL,
			"top_n", cfg.TopN, "poll_interval", cfg.PollInterval.String(), "database", st != nil)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-serveErr:
		if err != nil {
			stopMarket()
			stopStore()
			return fmt.Errorf("http server: %w", err)
		}
	}

	// Finish in-flight requests, stop polling, then flush the write buffer.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown", "err", err)
	}
	stopMarket()
	waitOrTimeout(marketDone, 10*time.Second)
	stopStore()
	waitOrTimeout(storeDone, 10*time.Second)
	if st != nil {
		st.Close()
	}
	logger.Info("stopped")
	return nil
}

func waitOrTimeout(done <-chan struct{}, d time.Duration) {
	select {
	case <-done:
	case <-time.After(d):
	}
}

func keysCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("keys: want create, list or revoke\n" + usage)
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("keys: DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		fs := flag.NewFlagSet("keys create", flag.ContinueOnError)
		owner := fs.String("owner", "", "label for the key owner (required)")
		rpm := fs.Int("rate-limit", 0, "requests per minute (0 = server default)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *owner == "" {
			return errors.New("keys create: --owner is required")
		}
		st, err := store.Open(ctx, store.Config{DatabaseURL: dsn, Logger: logger})
		if err != nil {
			return err
		}
		defer st.Close()
		raw, key, err := st.CreateKey(ctx, *owner, *rpm)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "created key %d for %q (rate limit %s)\n", key.ID, key.Owner, rateLabel(key.RateLimit))
		fmt.Fprintf(out, "\n  %s\n\nStore it now: it is shown only once.\n", raw)
		return nil
	case "list":
		st, err := store.Open(ctx, store.Config{DatabaseURL: dsn, Logger: logger})
		if err != nil {
			return err
		}
		defer st.Close()
		keys, err := st.ListKeys(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tOWNER\tRATE LIMIT\tSTATUS\tCREATED")
		for _, k := range keys {
			status := "active"
			if !k.Active {
				status = "revoked"
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", k.ID, k.Owner, rateLabel(k.RateLimit), status, k.CreatedAt.UTC().Format(time.RFC3339))
		}
		return tw.Flush()
	case "revoke":
		if len(rest) != 1 {
			return errors.New("keys revoke: want exactly one key ID")
		}
		id, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			return fmt.Errorf("keys revoke: bad ID %q", rest[0])
		}
		st, err := store.Open(ctx, store.Config{DatabaseURL: dsn, Logger: logger})
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.RevokeKey(ctx, id); err != nil {
			return err
		}
		fmt.Fprintf(out, "revoked key %d\n", id)
		return nil
	default:
		return fmt.Errorf("keys: unknown subcommand %q\n%s", sub, usage)
	}
}

func rateLabel(n int) string {
	if n <= 0 {
		return "default"
	}
	return strconv.Itoa(n) + "/min"
}
