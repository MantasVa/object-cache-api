package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

const (
	testDefaultTTL = 60 * time.Second
	testMaxTTL     = 300 * time.Second
)

func vals(items ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(items))
	for i, item := range items {
		out[i] = json.RawMessage(`"` + item + `"`)
	}
	return out
}

func mustCreate(t *testing.T, s *Store, key string, ttl time.Duration, items ...string) *Entry {
	t.Helper()
	entry, err := s.Create(key, vals(items...), ttl)
	if err != nil {
		t.Fatalf("Create(%q): %v", key, err)
	}
	return entry
}

func assertValues(t *testing.T, e *Entry, want ...string) {
	t.Helper()
	got := e.Values()
	if len(got) != len(want) {
		t.Fatalf("values = %s, want %v", got, want)
	}
	for i := range want {
		if string(got[i]) != `"`+want[i]+`"` {
			t.Fatalf("values[%d] = %s, want %q", i, got[i], want[i])
		}
	}
}

func assertExpiry(t *testing.T, e *Entry, want time.Time) {
	t.Helper()
	if !e.ExpiresAt().Equal(want) {
		t.Fatalf("expiry = %v, want %v", e.ExpiresAt(), want)
	}
}

// Inside a synctest bubble time.Now is simulated and time.Sleep returns as soon as every goroutine is idle, so
// these tests exercise real TTL behavior deterministically and without any real waiting.

func TestCreate(t *testing.T) {
	t.Run("new key succeeds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := NewStore(testDefaultTTL, testMaxTTL)
			entry := mustCreate(t, s, "k1", 30*time.Second, "a", "b")
			assertValues(t, entry, "a", "b")
			assertExpiry(t, entry, time.Now().Add(30*time.Second))
		})
	})

	t.Run("zero ttl uses the default", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := NewStore(45*time.Second, testMaxTTL)
			assertExpiry(t, mustCreate(t, s, "k1", 0, "a"), time.Now().Add(45*time.Second))
		})
	})

	t.Run("existing live key fails", func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		mustCreate(t, s, "k1", 30*time.Second, "a")
		entry, err := s.Create("k1", vals("b"), 30*time.Second)
		if !errors.Is(err, ErrKeyExists) || entry != nil {
			t.Fatalf("got (%v, %v), want ErrKeyExists", entry, err)
		}
	})

	t.Run("ttl above max fails", func(t *testing.T) {
		s := NewStore(testDefaultTTL, 100*time.Second)
		entry, err := s.Create("k1", vals("a"), 101*time.Second)
		if !errors.Is(err, ErrTTLExceedsMax) || entry != nil {
			t.Fatalf("got (%v, %v), want ErrTTLExceedsMax", entry, err)
		}
	})

	t.Run("expired key counts as absent", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := NewStore(testDefaultTTL, testMaxTTL)
			mustCreate(t, s, "k1", 10*time.Second, "old")
			time.Sleep(11 * time.Second)
			assertValues(t, mustCreate(t, s, "k1", 30*time.Second, "new"), "new")
		})
	})

	t.Run("does not alias the caller's slice", func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		input := vals("a")
		entry, err := s.Create("k1", input, 0)
		if err != nil {
			t.Fatal(err)
		}
		input[0] = json.RawMessage(`"changed"`)
		assertValues(t, entry, "a")
	})
}

func TestGet(t *testing.T) {
	t.Run("live key returns entry", func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		mustCreate(t, s, "k1", 30*time.Second, "a")
		entry, err := s.Get("k1")
		if err != nil {
			t.Fatal(err)
		}
		assertValues(t, entry, "a")
	})

	t.Run("expired key is not found", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := NewStore(testDefaultTTL, testMaxTTL)
			mustCreate(t, s, "k1", 10*time.Second, "a")
			time.Sleep(11 * time.Second)
			if _, err := s.Get("k1"); !errors.Is(err, ErrKeyNotFound) {
				t.Fatalf("err = %v, want ErrKeyNotFound", err)
			}
		})
	})

	t.Run("expiry is inclusive", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := NewStore(testDefaultTTL, testMaxTTL)
			mustCreate(t, s, "k1", 10*time.Second, "a")
			time.Sleep(10 * time.Second)
			if _, err := s.Get("k1"); !errors.Is(err, ErrKeyNotFound) {
				t.Fatalf("err = %v, want ErrKeyNotFound at the exact expiry moment", err)
			}
		})
	})

	t.Run("missing key is not found", func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		if _, err := s.Get("missing"); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("err = %v, want ErrKeyNotFound", err)
		}
	})

	t.Run("returned values cannot change the store", func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		mustCreate(t, s, "k1", 0, "a")
		first, _ := s.Get("k1")
		first.Values()[0] = json.RawMessage(`"changed"`)
		second, _ := s.Get("k1")
		assertValues(t, second, "a")
	})
}

func TestExtend(t *testing.T) {
	t.Run("adds onto the stored expiry", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := NewStore(testDefaultTTL, testMaxTTL)
			created := mustCreate(t, s, "k1", 10*time.Second, "a")
			time.Sleep(5 * time.Second)
			entry, err := s.Extend("k1", 60*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			assertExpiry(t, entry, created.ExpiresAt().Add(60*time.Second))
		})
	})

	t.Run("zero uses the default", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := NewStore(45*time.Second, testMaxTTL)
			created := mustCreate(t, s, "k1", 10*time.Second, "a")
			entry, err := s.Extend("k1", 0)
			if err != nil {
				t.Fatal(err)
			}
			assertExpiry(t, entry, created.ExpiresAt().Add(45*time.Second))
		})
	})

	t.Run("is capped at max from now", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := NewStore(5*time.Second, 20*time.Second)
			mustCreate(t, s, "k1", 10*time.Second, "a")
			entry, err := s.Extend("k1", 1000*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			assertExpiry(t, entry, time.Now().Add(20*time.Second))
		})
	})

	t.Run("missing key is not found", func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		if _, err := s.Extend("missing", time.Second); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("err = %v, want ErrKeyNotFound", err)
		}
	})

	t.Run("then append accumulates rather than overwrites", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := NewStore(testDefaultTTL, 1000*time.Second)
			created := mustCreate(t, s, "k1", 10*time.Second, "a")
			if _, err := s.Extend("k1", 60*time.Second); err != nil {
				t.Fatal(err)
			}
			entry, err := s.Append("k1", vals("b"), 120*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			assertExpiry(t, entry, created.ExpiresAt().Add(180*time.Second))
		})
	})
}

func TestAppend(t *testing.T) {
	t.Run("merges values and extends ttl", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := NewStore(testDefaultTTL, testMaxTTL)
			created := mustCreate(t, s, "k1", 10*time.Second, "a")
			entry, err := s.Append("k1", vals("b", "c"), 60*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			assertValues(t, entry, "a", "b", "c")
			assertExpiry(t, entry, created.ExpiresAt().Add(60*time.Second))
		})
	})

	t.Run("zero uses the default", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := NewStore(45*time.Second, testMaxTTL)
			created := mustCreate(t, s, "k1", 10*time.Second, "a")
			entry, err := s.Append("k1", vals("b"), 0)
			if err != nil {
				t.Fatal(err)
			}
			assertExpiry(t, entry, created.ExpiresAt().Add(45*time.Second))
		})
	})

	t.Run("cumulative extensions are capped at max from now", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := NewStore(5*time.Second, 50*time.Second)
			mustCreate(t, s, "k1", 10*time.Second, "a")
			if _, err := s.Append("k1", vals("b"), 30*time.Second); err != nil {
				t.Fatal(err)
			}
			entry, err := s.Append("k1", vals("c"), 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			assertExpiry(t, entry, time.Now().Add(50*time.Second))
		})
	})

	t.Run("missing key fails and does not create", func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		if _, err := s.Append("missing", vals("a"), 30*time.Second); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("err = %v, want ErrKeyNotFound", err)
		}
		if _, err := s.Get("missing"); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("append created the key: err = %v", err)
		}
	})

	t.Run("expired key fails", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := NewStore(testDefaultTTL, testMaxTTL)
			mustCreate(t, s, "k1", 10*time.Second, "a")
			time.Sleep(11 * time.Second)
			if _, err := s.Append("k1", vals("b"), 30*time.Second); !errors.Is(err, ErrKeyNotFound) {
				t.Fatalf("err = %v, want ErrKeyNotFound", err)
			}
		})
	})

	t.Run("leaves the previous entry unchanged", func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		before := mustCreate(t, s, "k1", 0, "a")
		if _, err := s.Append("k1", vals("b"), 0); err != nil {
			t.Fatal(err)
		}
		assertValues(t, before, "a")
	})
}

func TestDelete(t *testing.T) {
	t.Run("existing key is removed", func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		mustCreate(t, s, "k1", 30*time.Second, "a")
		if err := s.Delete("k1"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get("k1"); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("err = %v, want ErrKeyNotFound", err)
		}
	})

	t.Run("missing key is not found", func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		if err := s.Delete("missing"); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("err = %v, want ErrKeyNotFound", err)
		}
	})
}

func TestRemoveExpired(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		mustCreate(t, s, "short", 10*time.Second, "a")
		mustCreate(t, s, "long", 100*time.Second, "b")
		time.Sleep(11 * time.Second)

		if removed := s.RemoveExpired(); removed != 1 {
			t.Fatalf("removed = %d, want 1", removed)
		}
		if _, err := s.Get("short"); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("short: err = %v, want ErrKeyNotFound", err)
		}
		if _, err := s.Get("long"); err != nil {
			t.Fatalf("long: %v", err)
		}
	})
}

func TestSnapshot_ExcludesExpiredEntries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		mustCreate(t, s, "short", 10*time.Second, "a")
		mustCreate(t, s, "long", 100*time.Second, "b")
		time.Sleep(11 * time.Second)

		snapshot := s.Snapshot()
		if len(snapshot) != 1 || snapshot[0].Key() != "long" {
			t.Fatalf("snapshot = %v, want only \"long\"", snapshot)
		}
	})
}

func TestLoad_SkipsExpiredEntries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		now := time.Now()
		s.Load([]*Entry{
			NewEntry("expired", vals("a"), now.Add(-time.Second)),
			NewEntry("fresh", vals("b"), now.Add(60*time.Second)),
		})

		if _, err := s.Get("expired"); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("expired: err = %v, want ErrKeyNotFound", err)
		}
		if _, err := s.Get("fresh"); err != nil {
			t.Fatalf("fresh: %v", err)
		}
	})
}

// The race detector needs cgo and a 64-bit target, so these tests also assert on outcomes directly: a lost update
// fails them even without -race.

func TestAppend_ConcurrentWritersLoseNoValues(t *testing.T) {
	const writers, perWriter = 50, 100

	s := NewStore(time.Hour, 2*time.Hour)
	mustCreate(t, s, "k", 0, "seed")

	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				if _, err := s.Append("k", vals(fmt.Sprintf("%d-%d", w, i)), time.Second); err != nil {
					t.Errorf("Append: %v", err)
				}
			}
		})
	}
	wg.Wait()

	entry, err := s.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(entry.Values()), writers*perWriter+1; got != want {
		t.Fatalf("got %d values, want %d: an update was lost", got, want)
	}
}

func TestCreate_ConcurrentCreatorsHaveExactlyOneWinner(t *testing.T) {
	s := NewStore(time.Hour, 2*time.Hour)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for range 100 {
		wg.Go(func() {
			_, err := s.Create("k", vals("a"), 0)
			switch {
			case err == nil:
				mu.Lock()
				wins++
				mu.Unlock()
			case !errors.Is(err, ErrKeyExists):
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d creators won, want exactly 1", wins)
	}
}

// Entries must be immutable all the way down: json.RawMessage is itself a byte slice, so copying only the outer
// slice would still let callers change the stored bytes.
func TestEntry_ValueBytesAreNotShared(t *testing.T) {
	t.Run("changing bytes from Values does not change the entry", func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		mustCreate(t, s, "k1", 0, "a")
		first, _ := s.Get("k1")
		first.Values()[0][1] = 'X'
		second, _ := s.Get("k1")
		assertValues(t, second, "a")
	})

	t.Run("changing the bytes passed to Create does not change the entry", func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		input := vals("a")
		entry, err := s.Create("k1", input, 0)
		if err != nil {
			t.Fatal(err)
		}
		input[0][1] = 'X'
		assertValues(t, entry, "a")
	})

	t.Run("changing the bytes passed to Append does not change the entry", func(t *testing.T) {
		s := NewStore(testDefaultTTL, testMaxTTL)
		mustCreate(t, s, "k1", 0, "a")
		input := vals("b")
		entry, err := s.Append("k1", input, 0)
		if err != nil {
			t.Fatal(err)
		}
		input[0][1] = 'X'
		assertValues(t, entry, "a", "b")
	})

	t.Run("a nil value list stays nil", func(t *testing.T) {
		if got := NewEntry("k1", nil, time.Now()).Values(); got != nil {
			t.Fatalf("Values() = %v, want nil", got)
		}
	})

	t.Run("a JSON null value survives", func(t *testing.T) {
		entry := NewEntry("k1", []json.RawMessage{json.RawMessage("null"), nil}, time.Now())
		got := entry.Values()
		if string(got[0]) != "null" || got[1] != nil {
			t.Fatalf("Values() = %q", got)
		}
	})
}
