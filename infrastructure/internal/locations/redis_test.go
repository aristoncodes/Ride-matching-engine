package locations_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aditya/ride-matching/internal/locations"
	"github.com/aditya/ride-matching/internal/testutil"
)

// clock is an injectable time source, so freshness tests can jump forward
// instead of sleeping. A TTL test that sleeps 30 real seconds is a test nobody
// runs, and one that shortens the TTL to 50ms tests a different system.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newRepo(t *testing.T, clk *clock) *locations.RedisRepository {
	t.Helper()
	proc := testutil.StartRedis(t)

	opts := locations.DefaultOptions()
	opts.TenantID = "test"
	opts.TTL = 30 * time.Second
	if clk != nil {
		opts.Now = clk.Now
	}

	repo, err := locations.NewRedis(proc.Addr, opts)
	if err != nil {
		t.Fatalf("new redis repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return repo
}

// Bengaluru reference points, roughly 1.1 km apart per 0.01 degrees.
const (
	baseLat = 12.9700
	baseLng = 77.5900
)

func TestUpsertAndNearby(t *testing.T) {
	repo := newRepo(t, nil)
	ctx := context.Background()

	if err := repo.UpsertDriver(ctx, "D-1", baseLat, baseLng); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.UpsertDriver(ctx, "D-2", baseLat+0.02, baseLng); err != nil { // ~2.2 km
		t.Fatalf("upsert: %v", err)
	}

	// A 1 km radius must find the near driver and exclude the far one.
	got, err := repo.Nearby(ctx, locations.Query{Lat: baseLat, Lng: baseLng, Radius: 1000, Limit: 10})
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(got) != 1 || got[0].DriverID != "D-1" {
		t.Fatalf("got %+v, want only D-1", got)
	}
	if got[0].DistanceMeters > 50 {
		t.Errorf("distance = %f, want ~0", got[0].DistanceMeters)
	}

	// Widen the radius and both appear.
	got, err = repo.Nearby(ctx, locations.Query{Lat: baseLat, Lng: baseLng, Radius: 5000, Limit: 10})
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d drivers, want 2", len(got))
	}
	// Nearest first is part of the contract — the matcher's shortlist depends
	// on it when it caps candidates.
	if got[0].DriverID != "D-1" {
		t.Errorf("results not sorted nearest-first: %+v", got)
	}
}

// TestStaleDriversAreNotReturned is the Week 7 checkpoint.
//
// A driver who stopped pinging is still sitting in the geo index at their last
// known position. Returning them means dispatching a car that is not there —
// the rider waits for someone who left ten minutes ago. This is the single
// most important behaviour in the package.
func TestStaleDriversAreNotReturned(t *testing.T) {
	clk := newClock()
	repo := newRepo(t, clk)
	ctx := context.Background()

	if err := repo.UpsertDriver(ctx, "D-fresh", baseLat, baseLng); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.UpsertDriver(ctx, "D-goes-stale", baseLat+0.001, baseLng); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	query := locations.Query{Lat: baseLat, Lng: baseLng, Radius: 5000, Limit: 10}

	if got, err := repo.Nearby(ctx, query); err != nil || len(got) != 2 {
		t.Fatalf("before expiry: got %d (err %v), want 2", len(got), err)
	}

	// 20 s later: both still inside the 30 s TTL.
	clk.Advance(20 * time.Second)
	if err := repo.UpsertDriver(ctx, "D-fresh", baseLat, baseLng); err != nil { // keeps pinging
		t.Fatalf("upsert: %v", err)
	}
	if got, err := repo.Nearby(ctx, query); err != nil || len(got) != 2 {
		t.Fatalf("at 20s: got %d (err %v), want 2", len(got), err)
	}

	// 20 s more. D-fresh pinged at t=20 so it is 20 s old; D-goes-stale last
	// pinged at t=0, making it 40 s old and past the TTL.
	clk.Advance(20 * time.Second)
	got, err := repo.Nearby(ctx, query)
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d drivers, want only the one still pinging: %+v", len(got), got)
	}
	if got[0].DriverID != "D-fresh" {
		t.Fatalf("got %s, want D-fresh", got[0].DriverID)
	}

	// The stale driver is filtered from ANSWERS but still occupies MEMORY —
	// which is exactly why a reaper is needed as well as a read filter.
	if n, err := repo.Count(ctx); err != nil || n != 2 {
		t.Fatalf("count = %d (err %v), want 2 — filtering must not delete", n, err)
	}
}

func TestReapDeletesOnlyStaleDrivers(t *testing.T) {
	clk := newClock()
	repo := newRepo(t, clk)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := repo.UpsertDriver(ctx, fmt.Sprintf("D-old-%d", i), baseLat, baseLng); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	clk.Advance(45 * time.Second) // past the 30 s TTL
	for i := 0; i < 3; i++ {
		if err := repo.UpsertDriver(ctx, fmt.Sprintf("D-new-%d", i), baseLat, baseLng); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	if n, _ := repo.Count(ctx); n != 8 {
		t.Fatalf("count = %d, want 8 before reaping", n)
	}

	removed, err := repo.Reap(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if removed != 5 {
		t.Fatalf("reaped %d, want 5", removed)
	}
	if n, _ := repo.Count(ctx); n != 3 {
		t.Fatalf("count = %d after reap, want 3", n)
	}

	// Reaping twice must be harmless — the reaper runs on a ticker forever.
	if removed, err := repo.Reap(ctx); err != nil || removed != 0 {
		t.Fatalf("second reap removed %d (err %v), want 0", removed, err)
	}
}

func TestUpsertRefreshesPositionAndClock(t *testing.T) {
	clk := newClock()
	repo := newRepo(t, clk)
	ctx := context.Background()

	if err := repo.UpsertDriver(ctx, "D-moving", baseLat, baseLng); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	clk.Advance(10 * time.Second)
	// The driver has moved ~2.2 km north.
	if err := repo.UpsertDriver(ctx, "D-moving", baseLat+0.02, baseLng); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Gone from the old neighbourhood...
	got, err := repo.Nearby(ctx, locations.Query{Lat: baseLat, Lng: baseLng, Radius: 500, Limit: 10})
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("driver still found at the old position: %+v", got)
	}

	// ...and present at the new one, with exactly one entry. A GEOADD of an
	// existing member updates it; if this ever returns two, the store is
	// accumulating ghosts of every position a driver has ever held.
	got, err = repo.Nearby(ctx, locations.Query{Lat: baseLat + 0.02, Lng: baseLng, Radius: 500, Limit: 10})
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries at the new position, want exactly 1", len(got))
	}
	if n, _ := repo.Count(ctx); n != 1 {
		t.Fatalf("count = %d, want 1 — upsert must replace, not append", n)
	}
}

func TestRemoveDriver(t *testing.T) {
	repo := newRepo(t, nil)
	ctx := context.Background()

	if err := repo.UpsertDriver(ctx, "D-1", baseLat, baseLng); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.RemoveDriver(ctx, "D-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	got, err := repo.Nearby(ctx, locations.Query{Lat: baseLat, Lng: baseLng, Radius: 5000, Limit: 10})
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("removed driver still returned: %+v", got)
	}

	// Removing a driver who was never there must not error — the caller races
	// with the reaper and cannot know which of them won.
	if err := repo.RemoveDriver(ctx, "D-never-existed"); err != nil {
		t.Errorf("removing an absent driver: %v", err)
	}
}

func TestLimitCapsResultsAndKeepsNearest(t *testing.T) {
	repo := newRepo(t, nil)
	ctx := context.Background()

	// 30 drivers in a north-south line, progressively further away.
	locs := make([]locations.DriverLocation, 0, 30)
	for i := 0; i < 30; i++ {
		locs = append(locs, locations.DriverLocation{
			DriverID: fmt.Sprintf("D-%02d", i),
			Lat:      baseLat + float64(i)*0.0005,
			Lng:      baseLng,
		})
	}
	if err := repo.UpsertMany(ctx, locs); err != nil {
		t.Fatalf("upsert many: %v", err)
	}

	got, err := repo.Nearby(ctx, locations.Query{Lat: baseLat, Lng: baseLng, Radius: 50000, Limit: 5})
	if err != nil {
		t.Fatalf("nearby: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d, want 5", len(got))
	}
	// It must be the five NEAREST, not any five.
	for i, d := range got {
		if want := fmt.Sprintf("D-%02d", i); d.DriverID != want {
			t.Errorf("position %d = %s, want %s", i, d.DriverID, want)
		}
	}
}

func TestUpsertManyIsAtomicPerCall(t *testing.T) {
	repo := newRepo(t, nil)
	ctx := context.Background()

	// An invalid coordinate must be caught BEFORE anything is written, so a
	// partially applied batch is impossible.
	err := repo.UpsertMany(ctx, []locations.DriverLocation{
		{DriverID: "D-ok", Lat: baseLat, Lng: baseLng},
		{DriverID: "D-bad", Lat: 91.0, Lng: baseLng},
	})
	if err == nil {
		t.Fatal("expected an error for an out-of-range latitude")
	}
	if n, _ := repo.Count(ctx); n != 0 {
		t.Fatalf("count = %d, want 0 — a rejected batch must write nothing", n)
	}

	if err := repo.UpsertMany(ctx, nil); err != nil {
		t.Errorf("empty batch: %v", err)
	}
}

func TestConcurrentUpsertsAreSafe(t *testing.T) {
	// The ingestion layer writes from one goroutine per connection. Run under
	// -race, this is what proves the store can actually be shared.
	repo := newRepo(t, nil)
	ctx := context.Background()

	const writers, perWriter = 20, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				id := fmt.Sprintf("D-%d-%d", w, i)
				if err := repo.UpsertDriver(ctx, id, baseLat+float64(i)*0.0001, baseLng); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent upsert: %v", err)
	}

	if n, _ := repo.Count(ctx); n != writers*perWriter {
		t.Fatalf("count = %d, want %d", n, writers*perWriter)
	}
}

func TestSurvivesRedisRestart(t *testing.T) {
	// Redis going away must be an error, not a hang or a panic — the same
	// isolation argument as the engine, one dependency down.
	proc := testutil.StartRedis(t)

	opts := locations.DefaultOptions()
	opts.TenantID = "test"
	opts.MaxRetries = 1 // fail fast; the point is the error, not the retrying
	repo, err := locations.NewRedis(proc.Addr, opts)
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}
	defer repo.Close()

	ctx := context.Background()
	if err := repo.UpsertDriver(ctx, "D-1", baseLat, baseLng); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	proc.Kill(t)

	if _, err := repo.Nearby(ctx, locations.Query{Lat: baseLat, Lng: baseLng, Radius: 1000, Limit: 5}); err == nil {
		t.Fatal("expected an error once Redis was killed")
	}
}

func TestReaperRunsOnTicker(t *testing.T) {
	clk := newClock()
	repo := newRepo(t, clk)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		if err := repo.UpsertDriver(ctx, fmt.Sprintf("D-%d", i), baseLat, baseLng); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	clk.Advance(60 * time.Second) // everything is now stale

	reapCtx, cancel := context.WithCancel(ctx)
	reaped := make(chan int, 4)
	done := repo.StartReaper(reapCtx, 10*time.Millisecond, func(removed int, err error) {
		if err == nil && removed > 0 {
			select {
			case reaped <- removed:
			default:
			}
		}
	})

	select {
	case n := <-reaped:
		if n != 4 {
			t.Errorf("reaped %d, want 4", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reaper never fired")
	}

	// Cancelling must actually stop the goroutine. Waiting on `done` rather
	// than assuming is the difference between clean shutdown and a leak that
	// only shows up as a slowly growing goroutine count in production.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reaper goroutine did not exit after cancel")
	}
}

func TestOptionsValidation(t *testing.T) {
	opts := locations.DefaultOptions()
	opts.TTL = 0
	if _, err := locations.NewRedis("localhost:6379", opts); err == nil {
		t.Error("expected an error for a zero TTL")
	}

	opts = locations.DefaultOptions()
	opts.TenantID = ""
	if _, err := locations.NewRedis("localhost:6379", opts); err == nil {
		t.Error("expected an error for an empty tenant id")
	}
}

// TestNearbyManyMatchesNearby is the Week 22 correctness guard.
//
// The pipelined lookup exists purely for speed, so the one thing that must not
// change is the ANSWER. An optimisation that quietly returns different drivers
// is a matching bug, not a performance win.
func TestNearbyManyMatchesNearby(t *testing.T) {
	repo := newRepo(t, nil)
	ctx := context.Background()

	for i := 0; i < 60; i++ {
		if err := repo.UpsertDriver(ctx, fmt.Sprintf("D-%02d", i),
			baseLat+float64(i%10)*0.002, baseLng+float64(i/10)*0.002); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	queries := []locations.Query{
		{Lat: baseLat, Lng: baseLng, Radius: 1000, Limit: 5},
		{Lat: baseLat + 0.01, Lng: baseLng, Radius: 5000, Limit: 10},
		{Lat: baseLat, Lng: baseLng + 0.01, Radius: 500, Limit: 3},
		{Lat: 19.0760, Lng: 72.8777, Radius: 1000, Limit: 5}, // Mumbai: no hits
	}

	batched, err := repo.NearbyMany(ctx, queries)
	if err != nil {
		t.Fatalf("NearbyMany: %v", err)
	}
	if len(batched) != len(queries) {
		t.Fatalf("got %d result sets, want %d — results must be positional",
			len(batched), len(queries))
	}

	for i, q := range queries {
		single, err := repo.Nearby(ctx, q)
		if err != nil {
			t.Fatalf("Nearby %d: %v", i, err)
		}
		if len(single) != len(batched[i]) {
			t.Fatalf("query %d: Nearby returned %d drivers, NearbyMany %d",
				i, len(single), len(batched[i]))
		}
		for j := range single {
			if single[j].DriverID != batched[i][j].DriverID {
				t.Errorf("query %d position %d: %s vs %s — the pipelined lookup "+
					"must return the same drivers in the same order",
					i, j, single[j].DriverID, batched[i][j].DriverID)
			}
		}
	}
}

func TestNearbyManyExcludesStaleDriversToo(t *testing.T) {
	// The freshness filter is the part most likely to be lost when batching,
	// because the union-ZMScore is a different shape from the per-query version.
	clk := newClock()
	repo := newRepo(t, clk)
	ctx := context.Background()

	if err := repo.UpsertDriver(ctx, "D-fresh", baseLat, baseLng); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.UpsertDriver(ctx, "D-stale", baseLat+0.0001, baseLng); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	clk.Advance(20 * time.Second)
	if err := repo.UpsertDriver(ctx, "D-fresh", baseLat, baseLng); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	clk.Advance(20 * time.Second) // D-stale is now 40s old, past the 30s TTL

	got, err := repo.NearbyMany(ctx, []locations.Query{
		{Lat: baseLat, Lng: baseLng, Radius: 5000, Limit: 10},
	})
	if err != nil {
		t.Fatalf("NearbyMany: %v", err)
	}
	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("got %v, want exactly the one still-pinging driver", got)
	}
	if got[0][0].DriverID != "D-fresh" {
		t.Errorf("got %s, want D-fresh — the batched path must apply the same "+
			"freshness filter", got[0][0].DriverID)
	}
}

func TestNearbyManyHandlesEmptyInput(t *testing.T) {
	repo := newRepo(t, nil)
	got, err := repo.NearbyMany(context.Background(), nil)
	if err != nil || got != nil {
		t.Errorf("NearbyMany(nil) = %v, %v; want nil, nil", got, err)
	}
}
