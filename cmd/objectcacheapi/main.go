// Command objectcacheapi serves the Object Cache REST API.
//
// This file is the composition root: it is the only place that knows about concrete types and wires them
// together, by hand, in the order the application needs them.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"objectcacheapi/internal/api"
	"objectcacheapi/internal/background"
	"objectcacheapi/internal/cache"
	"objectcacheapi/internal/config"
	"objectcacheapi/internal/persistence"
)

const shutdownTimeout = 30 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	store := cache.NewStore(cfg.Cache.DefaultTTL, cfg.Cache.MaxTTL)

	jobCtx, stopJobs := context.WithCancel(context.Background())
	defer stopJobs()

	var jobs sync.WaitGroup
	jobs.Go(func() {
		background.Cleanup{Store: store, Interval: cfg.Cache.CleanupInterval, Logger: logger}.Run(jobCtx)
	})

	if cfg.Persistence.Enabled {
		snapshots := persistence.NewFile(cfg.Persistence.FilePath)
		// Restored before the server starts, so no request can ever see a partially loaded cache.
		restoreSnapshot(ctx, snapshots, store, logger)
		jobs.Go(func() {
			background.Persistence{Store: store, Saver: snapshots, Interval: cfg.Persistence.SaveInterval, Logger: logger}.Run(jobCtx)
		})
	}

	// net/http has no timeouts by default, so without these a slow client could hold a connection open forever.
	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           api.NewHandler(store, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "address", cfg.Address)
		serveErr <- server.ListenAndServe()
	}()

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutting down")
		// A fresh context, because ctx is already cancelled and Shutdown needs time to drain requests.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		runErr = server.Shutdown(shutdownCtx)
		<-serveErr // ListenAndServe returns as soon as Shutdown starts, so this does not block
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("serve: %w", err)
		}
	}

	// Jobs stop only after the server has, so the final snapshot includes every request that was accepted.
	stopJobs()
	jobs.Wait()
	return runErr
}

// snapshotLoader is the part of the persistence layer that startup needs.
type snapshotLoader interface {
	Load(ctx context.Context) ([]*cache.Entry, error)
}

// restoreSnapshot treats a failed load as non-fatal: a cache can always start empty, so a corrupt snapshot is
// worth a warning but not an outage.
func restoreSnapshot(ctx context.Context, loader snapshotLoader, store *cache.Store, logger *slog.Logger) {
	entries, err := loader.Load(ctx)
	if err != nil {
		logger.Warn("failed to load cache snapshot; starting with an empty cache", "error", err)
		return
	}
	store.Load(entries)
}
