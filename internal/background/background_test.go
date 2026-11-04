package background

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"objectcacheapi/internal/cache"
)

var discard = slog.New(slog.DiscardHandler)

type fakeRemover struct{ calls atomic.Int32 }

func (f *fakeRemover) RemoveExpired() int {
	f.calls.Add(1)
	return 0
}

type fakeSnapshotter struct{}

func (fakeSnapshotter) Snapshot() []*cache.Entry { return nil }

type fakeSaver struct {
	calls     atomic.Int32
	err       error
	cancelled atomic.Int32 // saves that were handed an already-cancelled context
}

func (f *fakeSaver) Save(ctx context.Context, _ []*cache.Entry) error {
	f.calls.Add(1)
	if ctx.Err() != nil {
		f.cancelled.Add(1)
	}
	return f.err
}

// lockedBuffer is a log destination the test can read while the job's goroutine writes to it. synctest.Wait doesn't
// make a plain bytes.Buffer safe here, because the race detector still sees the two accesses as unsynchronized.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// advance moves simulated time forward, then waits until every goroutine in the bubble has finished reacting.
func advance(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func TestCleanup_Run(t *testing.T) {
	t.Run("removes expired entries when the interval elapses", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store := &fakeRemover{}
			go Cleanup{Store: store, Interval: 30 * time.Second, Logger: discard}.Run(ctx)

			advance(30 * time.Second)

			if got := store.calls.Load(); got != 1 {
				t.Fatalf("calls = %d, want 1", got)
			}
		})
	})

	t.Run("runs on every interval", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store := &fakeRemover{}
			go Cleanup{Store: store, Interval: 10 * time.Second, Logger: discard}.Run(ctx)

			advance(10 * time.Second)
			advance(10 * time.Second)

			if got := store.calls.Load(); got != 2 {
				t.Fatalf("calls = %d, want 2", got)
			}
		})
	})

	t.Run("does nothing before the interval elapses", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store := &fakeRemover{}
			go Cleanup{Store: store, Interval: 30 * time.Second, Logger: discard}.Run(ctx)

			advance(5 * time.Second)

			if got := store.calls.Load(); got != 0 {
				t.Fatalf("calls = %d, want 0", got)
			}
		})
	})

	t.Run("returns when cancelled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() {
				Cleanup{Store: &fakeRemover{}, Interval: 30 * time.Second, Logger: discard}.Run(ctx)
				close(done)
			}()

			cancel()
			synctest.Wait()

			select {
			case <-done:
			default:
				t.Fatal("Run did not return after cancellation")
			}
		})
	})
}

func TestPersistence_Run(t *testing.T) {
	t.Run("saves when the interval elapses", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			saver := &fakeSaver{}
			go Persistence{Store: fakeSnapshotter{}, Saver: saver, Interval: 30 * time.Second, Logger: discard}.Run(ctx)

			advance(30 * time.Second)

			if got := saver.calls.Load(); got != 1 {
				t.Fatalf("saves = %d, want 1", got)
			}
		})
	})

	t.Run("saves on every interval", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			saver := &fakeSaver{}
			go Persistence{Store: fakeSnapshotter{}, Saver: saver, Interval: 10 * time.Second, Logger: discard}.Run(ctx)

			advance(10 * time.Second)
			advance(10 * time.Second)

			if got := saver.calls.Load(); got != 2 {
				t.Fatalf("saves = %d, want 2", got)
			}
		})
	})

	t.Run("stopping before the first interval performs one final save", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			saver := &fakeSaver{}
			go Persistence{Store: fakeSnapshotter{}, Saver: saver, Interval: 30 * time.Second, Logger: discard}.Run(ctx)

			cancel()
			synctest.Wait()

			if got := saver.calls.Load(); got != 1 {
				t.Fatalf("saves = %d, want exactly 1", got)
			}
			if got := saver.cancelled.Load(); got != 0 {
				t.Fatal("the final save was handed a cancelled context, so a real saver would have refused to write")
			}
		})
	})

	t.Run("a failed save is logged and retried on the next tick", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var logs lockedBuffer
			saver := &fakeSaver{err: errors.New("disk full")}
			job := Persistence{Store: fakeSnapshotter{}, Saver: saver, Interval: 10 * time.Second, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
			go job.Run(ctx)

			advance(10 * time.Second)
			if !strings.Contains(logs.String(), "disk full") {
				t.Fatalf("failure was not logged: %q", logs.String())
			}

			advance(10 * time.Second)
			if got := saver.calls.Load(); got != 2 {
				t.Fatalf("saves = %d, want 2: the job stopped after a failed save", got)
			}
		})
	})
}
