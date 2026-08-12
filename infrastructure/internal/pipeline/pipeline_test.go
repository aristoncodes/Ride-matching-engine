package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aditya/ride-matching/internal/ingest"
	"github.com/aditya/ride-matching/internal/locations"
	"github.com/aditya/ride-matching/internal/pipeline"
	"github.com/aditya/ride-matching/internal/testutil"
)

// fakeStore is an in-memory locations.Repository. The pipeline's job is
// batching and shedding, not storage, so testing it against Redis would only
// add latency and flakiness to assertions about buffering.
type fakeStore struct {
	mu       sync.Mutex
	upserts  [][]locations.DriverLocation
	latest   map[string]locations.DriverLocation
	failWith error
	delay    time.Duration
}

func newFakeStore() *fakeStore {
	return &fakeStore{latest: map[string]locations.DriverLocation{}}
}

func (f *fakeStore) UpsertMany(ctx context.Context, locs []locations.DriverLocation) error {
	f.mu.Lock()
	delay, failWith := f.delay, f.failWith
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if failWith != nil {
		return failWith
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, locs)
	for _, l := range locs {
		f.latest[l.DriverID] = l
	}
	return nil
}

func (f *fakeStore) UpsertDriver(ctx context.Context, id string, lat, lng float64) error {
	return f.UpsertMany(ctx, []locations.DriverLocation{{DriverID: id, Lat: lat, Lng: lng}})
}
func (f *fakeStore) Nearby(context.Context, locations.Query) ([]locations.DriverLocation, error) {
	return nil, nil
}
func (f *fakeStore) RemoveDriver(context.Context, string) error { return nil }
func (f *fakeStore) Reap(context.Context) (int, error)          { return 0, nil }
func (f *fakeStore) Close() error                               { return nil }

func (f *fakeStore) Count(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.latest), nil
}

func (f *fakeStore) batches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.upserts)
}

func (f *fakeStore) driver(id string) (locations.DriverLocation, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.latest[id]
	return l, ok
}

func (f *fakeStore) setFailure(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = err
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCoalescesPingsPerDriver(t *testing.T) {
	// The central claim: only a driver's LATEST position matters, so ten pings
	// in one window must produce one write carrying the final position.
	store := newFakeStore()
	p, err := pipeline.New(store, pipeline.Config{Window: time.Hour, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := p.Accept(ctx, ingest.Ping{
			DriverID: "D-1", Lat: 12.97 + float64(i)*0.001, Lng: 77.59,
		}); err != nil {
			t.Fatalf("accept: %v", err)
		}
	}

	if got := p.Stats().Buffered; got != 1 {
		t.Fatalf("buffered = %d, want 1 — pings must coalesce by driver", got)
	}

	if err := p.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	got, ok := store.driver("D-1")
	if !ok {
		t.Fatal("driver was never written")
	}
	// Computed exactly as the loop computed it. `12.97 + 9*0.001` would be
	// folded by the compiler at arbitrary precision and then rounded once,
	// which is NOT the same float64 as multiplying and adding at runtime — the
	// two print identically and compare unequal.
	if want := 12.97 + float64(9)*0.001; got.Lat != want {
		t.Errorf("lat = %v, want %v — the LAST position must win", got.Lat, want)
	}
	if n := store.batches(); n != 1 {
		t.Errorf("%d store calls, want 1", n)
	}
}

func TestFlushesOnTheConfiguredWindow(t *testing.T) {
	store := newFakeStore()
	windows := make(chan pipeline.WindowStats, 16)

	p, err := pipeline.New(store, pipeline.Config{
		Window:       50 * time.Millisecond,
		FlushTimeout: 40 * time.Millisecond,
		Logger:       quietLogger(),
		OnWindow:     func(s pipeline.WindowStats) { windows <- s },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = p.Run(ctx) }()

	_ = p.Accept(ctx, ingest.Ping{DriverID: "D-1", Lat: 12.97, Lng: 77.59})

	select {
	case s := <-windows:
		if s.DriversFlushed != 1 {
			t.Errorf("flushed %d drivers, want 1", s.DriversFlushed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no window fired")
	}

	cancel()
	select {
	case <-p.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestStaysBoundedUnderOverload is the Week 9 checkpoint.
//
// The requirement is not "never drop" — it is that the system SHEDS
// DELIBERATELY rather than growing until it OOMs. So: hammer it with far more
// distinct drivers than the buffer allows and assert the buffer never exceeds
// its cap, that the excess is counted, and that the process is still working
// afterwards.
func TestStaysBoundedUnderOverload(t *testing.T) {
	const cap = 100
	store := newFakeStore()
	p, err := pipeline.New(store, pipeline.Config{
		Window:             time.Hour, // never flush; overload the buffer alone
		MaxBufferedDrivers: cap,
		Logger:             quietLogger(),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	ctx := context.Background()
	const flood = 10_000
	for i := 0; i < flood; i++ {
		if err := p.Accept(ctx, ingest.Ping{
			DriverID: fmt.Sprintf("D-%d", i), Lat: 12.97, Lng: 77.59,
		}); err != nil {
			t.Fatalf("accept must not error under overload: %v", err)
		}
		if n := p.Stats().Buffered; n > cap {
			t.Fatalf("buffer grew to %d, above the cap of %d", n, cap)
		}
	}

	stats := p.Stats()
	if stats.Buffered != cap {
		t.Errorf("buffered = %d, want exactly the cap %d", stats.Buffered, cap)
	}
	if stats.TotalShed != flood-cap {
		t.Errorf("shed = %d, want %d — shedding must be counted, not silent",
			stats.TotalShed, flood-cap)
	}
	if stats.TotalPings != cap {
		t.Errorf("accepted = %d, want %d", stats.TotalPings, cap)
	}

	// Still functional after the flood, and still accepting UPDATES for drivers
	// already buffered: those reuse an entry, so refusing them would discard
	// fresher data for no memory saving.
	if err := p.Accept(ctx, ingest.Ping{DriverID: "D-0", Lat: 13.5, Lng: 77.6}); err != nil {
		t.Fatalf("accept after overload: %v", err)
	}
	if n := p.Stats().Buffered; n != cap {
		t.Errorf("buffered = %d after updating a known driver, want %d", n, cap)
	}
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("flush after overload: %v", err)
	}
	got, ok := store.driver("D-0")
	if !ok || got.Lat != 13.5 {
		t.Error("an update to an already-buffered driver was lost")
	}
}

func TestFlushFailureDropsTheWindowRatherThanGrowing(t *testing.T) {
	// During a Redis outage the buffer must NOT accumulate. The positions are
	// worthless within seconds anyway — every driver pings again — so dropping
	// the window is much cheaper than OOMing the ingestion layer.
	store := newFakeStore()
	store.setFailure(errors.New("redis is down"))

	p, err := pipeline.New(store, pipeline.Config{Window: time.Hour, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		_ = p.Accept(ctx, ingest.Ping{DriverID: fmt.Sprintf("D-%d", i), Lat: 12.97, Lng: 77.59})
	}
	if err := p.Flush(ctx); err == nil {
		t.Fatal("expected the flush to report the store failure")
	}

	if n := p.Stats().Buffered; n != 0 {
		t.Fatalf("buffer holds %d after a failed flush, want 0 — a failed window "+
			"must be dropped, not retried into the next one", n)
	}
	if p.Stats().FlushFailures != 1 {
		t.Errorf("flush failures = %d, want 1", p.Stats().FlushFailures)
	}

	// Recovery is automatic: the next window just works.
	store.setFailure(nil)
	_ = p.Accept(ctx, ingest.Ping{DriverID: "D-new", Lat: 12.98, Lng: 77.60})
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("flush after recovery: %v", err)
	}
	if _, ok := store.driver("D-new"); !ok {
		t.Error("pipeline did not recover after the store came back")
	}
}

func TestSlowStoreCannotBlockIngestion(t *testing.T) {
	// The lock must NOT be held across the store write. If it were, every
	// connection goroutine would block for the duration of a Redis round trip
	// and one slow dependency would stall the whole ingestion layer.
	store := newFakeStore()
	store.mu.Lock()
	store.delay = 300 * time.Millisecond
	store.mu.Unlock()

	p, err := pipeline.New(store, pipeline.Config{
		Window:       time.Hour,
		FlushTimeout: time.Second,
		Logger:       quietLogger(),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	_ = p.Accept(ctx, ingest.Ping{DriverID: "D-1", Lat: 12.97, Lng: 77.59})

	flushDone := make(chan struct{})
	go func() { defer close(flushDone); _ = p.Flush(ctx) }()

	time.Sleep(50 * time.Millisecond) // the flush is now in the slow store call

	start := time.Now()
	if err := p.Accept(ctx, ingest.Ping{DriverID: "D-2", Lat: 12.98, Lng: 77.60}); err != nil {
		t.Fatalf("accept during flush: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Accept blocked for %v during a slow flush — the lock is held "+
			"across the store write", elapsed)
	}
	<-flushDone
}

func TestFinalFlushOnShutdown(t *testing.T) {
	store := newFakeStore()
	p, err := pipeline.New(store, pipeline.Config{
		Window:       time.Hour, // no tick will ever fire
		FlushTimeout: time.Second,
		Logger:       quietLogger(),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = p.Run(ctx) }()

	_ = p.Accept(ctx, ingest.Ping{DriverID: "D-last", Lat: 12.97, Lng: 77.59})

	cancel()
	select {
	case <-p.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}

	// The final window must still be written — a clean shutdown that silently
	// discards the last three seconds of driver positions is not clean.
	if _, ok := store.driver("D-last"); !ok {
		t.Error("the final flush did not happen on shutdown")
	}
}

func TestConcurrentAcceptIsSafe(t *testing.T) {
	// One goroutine per connection is the real shape. Under -race this is what
	// proves the buffer can actually be shared.
	store := newFakeStore()
	p, err := pipeline.New(store, pipeline.Config{
		Window:             10 * time.Millisecond,
		FlushTimeout:       5 * time.Millisecond,
		MaxBufferedDrivers: 100_000,
		Logger:             quietLogger(),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = p.Run(ctx) }()

	const writers, perWriter = 40, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				_ = p.Accept(ctx, ingest.Ping{
					DriverID: fmt.Sprintf("D-%d", w),
					Lat:      12.97 + float64(i)*0.0001,
					Lng:      77.59,
				})
			}
		}(w)
	}
	wg.Wait()

	cancel()
	<-p.Done()

	// 40 distinct drivers, whatever the interleaving — coalescing guarantees it.
	if n, _ := store.Count(ctx); n != writers {
		t.Errorf("store holds %d drivers, want %d", n, writers)
	}
	if s := p.Stats(); s.TotalPings != writers*perWriter {
		t.Errorf("accepted %d pings, want %d", s.TotalPings, writers*perWriter)
	}
}

func TestRejectsFlushTimeoutLongerThanWindow(t *testing.T) {
	// A flush slower than the window means windows overlap and queue — the
	// unbounded growth this package exists to prevent. Caught at construction.
	_, err := pipeline.New(newFakeStore(), pipeline.Config{
		Window:       time.Second,
		FlushTimeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected an error when FlushTimeout exceeds Window")
	}
	if _, err := pipeline.New(nil, pipeline.Config{}); err == nil {
		t.Error("expected an error for a nil store")
	}
}

// TestEndToEndPingsReachRedis is the Week 9 checkpoint's other half: mock GPS
// pings flow in over a real WebSocket and are visible in a real Redis via a
// radius query.
func TestEndToEndPingsReachRedis(t *testing.T) {
	proc := testutil.StartRedis(t)

	opts := locations.DefaultOptions()
	opts.TenantID = "e2e"
	store, err := locations.NewRedis(proc.Addr, opts)
	if err != nil {
		t.Fatalf("redis: %v", err)
	}
	defer store.Close()

	windows := make(chan pipeline.WindowStats, 8)
	p, err := pipeline.New(store, pipeline.Config{
		Window:       50 * time.Millisecond,
		FlushTimeout: 40 * time.Millisecond,
		Logger:       quietLogger(),
		OnWindow:     func(s pipeline.WindowStats) { windows <- s },
	})
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	// Three drivers around MG Road.
	for i := 0; i < 3; i++ {
		_ = p.Accept(ctx, ingest.Ping{
			DriverID: fmt.Sprintf("D-%d", i),
			Lat:      12.9700 + float64(i)*0.0005,
			Lng:      77.5900,
		})
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case s := <-windows:
			if s.DriversFlushed == 0 {
				continue
			}
			found, err := store.Nearby(ctx, locations.Query{
				Lat: 12.9700, Lng: 77.5900, Radius: 2000, Limit: 10,
			})
			if err != nil {
				t.Fatalf("nearby: %v", err)
			}
			if len(found) != 3 {
				t.Fatalf("Redis holds %d drivers, want 3", len(found))
			}
			return
		case <-deadline:
			t.Fatal("pings never reached Redis")
		}
	}
}
