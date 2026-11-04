// Package background runs the recurring jobs that work independently of request handling.
//
// Each job has a blocking Run method that returns when its context is cancelled. The caller decides which
// goroutine it runs on, and tests call it directly inside a testing/synctest bubble, where time is simulated,
// so no test ever waits for a real interval.
package background

import (
	"context"
	"log/slog"
	"time"

	"objectcacheapi/internal/cache"
)

// ExpiredRemover is the part of the store the cleanup job needs.
type ExpiredRemover interface {
	RemoveExpired() int
}

// Snapshotter is the part of the store the persistence job needs.
type Snapshotter interface {
	Snapshot() []*cache.Entry
}

// Saver writes a snapshot somewhere durable.
type Saver interface {
	Save(ctx context.Context, entries []*cache.Entry) error
}

// Cleanup periodically removes expired entries. Interval must be positive.
type Cleanup struct {
	Store    ExpiredRemover
	Interval time.Duration
	Logger   *slog.Logger
}

// Run removes expired entries every interval until ctx is cancelled.
func (c Cleanup) Run(ctx context.Context) {
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Logger.Debug("performing cache cleanup")
			if removed := c.Store.RemoveExpired(); removed > 0 {
				c.Logger.Info("cache cleanup removed expired entries", "count", removed)
			}
		}
	}
}

// Persistence periodically saves a snapshot of the store. Interval must be positive.
type Persistence struct {
	Store    Snapshotter
	Saver    Saver
	Interval time.Duration
	Logger   *slog.Logger
}

// Run saves a snapshot every interval until ctx is cancelled, then saves once more so a graceful shutdown does
// not lose the last interval's writes. A failed save is logged and retried on the next tick, because the cache
// itself is still healthy.
func (p Persistence) Run(ctx context.Context) {
	// The context is already cancelled by the time this runs, and the final save has to happen regardless.
	defer p.save(context.WithoutCancel(ctx))

	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.save(ctx)
		}
	}
}

func (p Persistence) save(ctx context.Context) {
	if err := p.Saver.Save(ctx, p.Store.Snapshot()); err != nil && ctx.Err() == nil {
		p.Logger.Warn("failed to save cache snapshot", "error", err)
	}
}
