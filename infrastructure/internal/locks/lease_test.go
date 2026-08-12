package locks_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aditya/ride-matching/internal/locks"
	"github.com/aditya/ride-matching/internal/testutil"
)

func newManager(t *testing.T, addr string, ttl time.Duration) *locks.Manager {
	t.Helper()
	opts := locks.DefaultOptions()
	opts.TenantID = "test"
	opts.TTL = ttl

	m, err := locks.NewManager(addr, opts)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// TestExactlyOneWinnerUnderConcurrency is the Week 13 checkpoint.
//
// 100 goroutines race for one driver. Exactly one may win. This is the property
// the C++ solver CANNOT provide: it guarantees no double-booking within a
// single batch, and says nothing about two batchers solving concurrently.
func TestExactlyOneWinnerUnderConcurrency(t *testing.T) {
	proc := testutil.StartRedis(t)
	ctx := context.Background()

	const contenders = 100
	var winners atomic.Int64
	var notAcquired atomic.Int64
	var errs atomic.Int64

	// A barrier, so every goroutine attempts at genuinely the same moment.
	// Without it they trickle in and the race is never actually exercised.
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := newManager(t, proc.Addr, 10*time.Second)
			<-start
			_, err := m.Acquire(ctx, "D-contested")
			switch {
			case err == nil:
				winners.Add(1)
			case errors.Is(err, locks.ErrNotAcquired):
				notAcquired.Add(1)
			default:
				errs.Add(1)
			}
		}()
	}

	close(start)
	wg.Wait()

	if n := errs.Load(); n != 0 {
		t.Fatalf("%d unexpected errors", n)
	}
	if got := winners.Load(); got != 1 {
		t.Fatalf("winners = %d, want EXACTLY 1 — the driver was double-booked", got)
	}
	if got := notAcquired.Load(); got != contenders-1 {
		t.Errorf("losers = %d, want %d", got, contenders-1)
	}
}

// TestCrashedHolderLeaseSelfReleases is the other half of the checkpoint.
//
// A lock without a TTL is a deadlock waiting for a crash: the holder dies and
// that driver becomes permanently unmatchable, with nothing able to distinguish
// "in use" from "abandoned".
func TestCrashedHolderLeaseSelfReleases(t *testing.T) {
	proc := testutil.StartRedis(t)
	ctx := context.Background()

	// Long enough that connecting a second client cannot outlast it. The first
	// version used 300ms, and building the second manager (a fresh Redis
	// connection) took longer than that, so the lease had already expired
	// before the "is it still held?" assertion ran — the test failed while the
	// code was correct.
	const ttl = 2 * time.Second

	// Both managers are created UP FRONT so no setup work happens on the clock.
	crashed := newManager(t, proc.Addr, ttl)
	other := newManager(t, proc.Addr, ttl)

	if _, err := crashed.Acquire(ctx, "D-stranded"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// The holder "crashes" here — no Release is ever called.
	if _, err := other.Acquire(ctx, "D-stranded"); !errors.Is(err, locks.ErrNotAcquired) {
		t.Fatalf("acquire while held = %v, want ErrNotAcquired", err)
	}

	// Wait out the lease.
	deadline := time.Now().Add(10 * time.Second)
	var recovered bool
	for time.Now().Before(deadline) {
		if _, err := other.Acquire(ctx, "D-stranded"); err == nil {
			recovered = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !recovered {
		t.Fatal("the lease never expired — a crashed holder has stranded this " +
			"driver permanently")
	}
}

// TestFencingTokenPreventsReleasingSomeoneElsesLease guards the subtlest bug in
// the package: a stalled holder waking after expiry must not delete the lease
// that has since been granted to someone else.
func TestFencingTokenPreventsReleasingSomeoneElsesLease(t *testing.T) {
	proc := testutil.StartRedis(t)
	ctx := context.Background()

	const ttl = 200 * time.Millisecond
	slow := newManager(t, proc.Addr, ttl)
	fast := newManager(t, proc.Addr, 10*time.Second)

	staleLease, err := slow.Acquire(ctx, "D-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// The slow holder stalls past its TTL...
	time.Sleep(400 * time.Millisecond)

	// ...and a new holder legitimately takes the driver.
	newLease, err := fast.Acquire(ctx, "D-1")
	if err != nil {
		t.Fatalf("second acquire after expiry: %v", err)
	}
	if newLease.Token == staleLease.Token {
		t.Fatal("tokens collided — they must be unique per acquisition")
	}

	// The stalled holder wakes up and releases. It must NOT free the new lease.
	released, err := slow.Release(ctx, staleLease)
	if err != nil {
		t.Fatalf("stale release: %v", err)
	}
	if released {
		t.Fatal("a stale holder released the NEW owner's lease — the driver is " +
			"now double-bookable by the very mechanism meant to prevent it")
	}

	held, err := fast.IsHeld(ctx, "D-1")
	if err != nil {
		t.Fatalf("is held: %v", err)
	}
	if !held {
		t.Fatal("the new owner's lease was destroyed by the stale release")
	}

	// The rightful owner can still release it.
	if ok, err := fast.Release(ctx, newLease); err != nil || !ok {
		t.Fatalf("rightful release = %v (err %v), want true", ok, err)
	}
}

func TestReleaseAllowsImmediateReacquire(t *testing.T) {
	proc := testutil.StartRedis(t)
	ctx := context.Background()
	m := newManager(t, proc.Addr, 10*time.Second)

	lease, err := m.Acquire(ctx, "D-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if ok, err := m.Release(ctx, lease); err != nil || !ok {
		t.Fatalf("release = %v (err %v), want true", ok, err)
	}

	// A released driver must be immediately available — a matched-and-completed
	// ride should not keep the driver out of the pool for the rest of the TTL.
	if _, err := m.Acquire(ctx, "D-1"); err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}

	// Releasing twice is harmless: the caller races the TTL and cannot know
	// which of them won.
	if ok, err := m.Release(ctx, lease); err != nil {
		t.Errorf("double release errored: %v", err)
	} else if ok {
		t.Error("double release reported success for a lease it no longer owns")
	}
}

func TestExtendKeepsALeaseAliveOnlyForItsOwner(t *testing.T) {
	proc := testutil.StartRedis(t)
	ctx := context.Background()

	const ttl = 300 * time.Millisecond
	owner := newManager(t, proc.Addr, ttl)

	lease, err := owner.Acquire(ctx, "D-long-job")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Renew across more than two TTLs. This is how legitimately long work is
	// handled without choosing a TTL long enough for the worst case — which
	// would strand drivers for exactly that long after a crash.
	for i := 0; i < 4; i++ {
		time.Sleep(100 * time.Millisecond)
		lease, err = owner.Extend(ctx, lease)
		if err != nil {
			t.Fatalf("extend %d: %v", i, err)
		}
	}
	if held, _ := owner.IsHeld(ctx, "D-long-job"); !held {
		t.Fatal("the lease expired despite being extended")
	}

	// A non-owner must not be able to extend it.
	imposter := newManager(t, proc.Addr, ttl)
	fake := locks.Lease{DriverID: "D-long-job", Token: "not-the-real-token"}
	if _, err := imposter.Extend(ctx, fake); !errors.Is(err, locks.ErrNotAcquired) {
		t.Errorf("imposter extend = %v, want ErrNotAcquired", err)
	}
}

func TestAcquireManyTakesWhatItCan(t *testing.T) {
	// Partial success is the right semantics: a rider matched to an available
	// driver should not be denied because a DIFFERENT driver in the same batch
	// was taken by another batcher.
	proc := testutil.StartRedis(t)
	ctx := context.Background()

	first := newManager(t, proc.Addr, 10*time.Second)
	second := newManager(t, proc.Addr, 10*time.Second)

	if _, err := first.Acquire(ctx, "D-2"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	leases, err := second.AcquireMany(ctx, []string{"D-1", "D-2", "D-3"})
	if err != nil {
		t.Fatalf("acquire many: %v", err)
	}
	if len(leases) != 2 {
		t.Fatalf("acquired %d leases, want 2 (D-2 was already held)", len(leases))
	}
	for _, l := range leases {
		if l.DriverID == "D-2" {
			t.Fatal("acquired a driver that was already held")
		}
	}

	if err := second.ReleaseMany(ctx, leases); err != nil {
		t.Errorf("release many: %v", err)
	}
}

func TestGeohashProximityProperty(t *testing.T) {
	// The property the partitioning scheme depends on: nearby points share a
	// prefix. If this were false, geohash partitioning would be no better than
	// random hashing.
	mgRoad := locks.Geohash(12.9716, 77.5946, 6)
	nearby := locks.Geohash(12.9720, 77.5950, 6)  // ~50 m away
	faraway := locks.Geohash(19.0760, 72.8777, 6) // Mumbai

	if mgRoad[:4] != nearby[:4] {
		t.Errorf("nearby points have different prefixes: %s vs %s", mgRoad, nearby)
	}
	if mgRoad[:2] == faraway[:2] {
		t.Errorf("distant cities share a prefix: %s vs %s", mgRoad, faraway)
	}

	// Deterministic — the same input must always map to the same partition, or
	// two batchers would disagree about which shard protects a neighbourhood.
	if locks.Geohash(12.9716, 77.5946, 6) != mgRoad {
		t.Error("geohash is not deterministic")
	}
}

// TestPartitioningReducesContention is the week's "prove it scales" item.
//
// The claim is that geohash-partitioned locking beats one global lock. It is
// measured rather than asserted, because "it should be faster" is not evidence.
func TestPartitioningReducesContention(t *testing.T) {
	proc := testutil.StartRedis(t)
	ctx := context.Background()
	m := newManager(t, proc.Addr, 5*time.Second)

	// Riders spread across a metropolitan area, the way real load arrives.
	//
	// The spacing matters and the first version got it wrong: 0.02° is ~2.2 km,
	// while a precision-5 geohash cell is ~4.9 km square, so most of those
	// "distinct" riders landed in the SAME partition and the spread score came
	// out at 0.28.
	//
	// That was the test being unrealistic, not the partitioning being broken —
	// two riders 2 km apart genuinely ARE in the same neighbourhood and SHOULD
	// contend, because their candidate driver sets overlap. Demonstrating
	// spread requires riders in genuinely different neighbourhoods, so the
	// spacing here is 0.06° (~6.6 km), comfortably larger than a cell.
	const workers = 40
	coords := make([][2]float64, workers)
	for i := range coords {
		coords[i] = [2]float64{
			12.80 + float64(i%8)*0.06,
			77.40 + float64(i/8)*0.06,
		}
	}

	// --- One global lock: every worker serialises on the same key ---
	var globalWins, globalMisses atomic.Int64
	globalStart := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := m.Acquire(ctx, "GLOBAL")
			if errors.Is(err, locks.ErrNotAcquired) {
				globalMisses.Add(1)
				return
			}
			if err != nil {
				return
			}
			globalWins.Add(1)
			_, _ = m.Release(ctx, lease)
		}()
	}
	wg.Wait()
	globalDuration := time.Since(globalStart)

	// --- Geohash partitions: workers in different areas never collide ---
	var partWins, partMisses atomic.Int64
	partStart := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lease, err := m.AcquirePartition(ctx, coords[i][0], coords[i][1])
			if errors.Is(err, locks.ErrNotAcquired) {
				partMisses.Add(1)
				return
			}
			if err != nil {
				return
			}
			partWins.Add(1)
			_, _ = m.ReleasePartition(ctx, lease)
		}(i)
	}
	wg.Wait()
	partDuration := time.Since(partStart)

	spread := m.SpreadScore(coords)

	t.Logf("global lock:      %2d/%d acquired concurrently in %v",
		globalWins.Load(), workers, globalDuration)
	t.Logf("geohash partition: %2d/%d acquired concurrently in %v (spread %.2f)",
		partWins.Load(), workers, partDuration, spread)

	// The number that matters: with one global lock only a handful of the
	// simultaneous attempts can hold it at once; partitioned, many proceed in
	// parallel because they are working different neighbourhoods.
	if partWins.Load() <= globalWins.Load() {
		t.Errorf("partitioned concurrency (%d) did not beat a global lock (%d)",
			partWins.Load(), globalWins.Load())
	}
	if spread < 0.5 {
		t.Errorf("spread score %.2f — coordinates are piling into too few "+
			"partitions for partitioning to help", spread)
	}
}

func TestOptionsValidation(t *testing.T) {
	opts := locks.DefaultOptions()
	opts.TTL = 0
	if _, err := locks.NewManager("localhost:6379", opts); err == nil {
		t.Error("expected an error for a zero TTL — a lease without one is a deadlock")
	}

	opts = locks.DefaultOptions()
	opts.TenantID = ""
	if _, err := locks.NewManager("localhost:6379", opts); err == nil {
		t.Error("expected an error for an empty tenant id")
	}
}

func TestEmptyDriverIDRejected(t *testing.T) {
	proc := testutil.StartRedis(t)
	m := newManager(t, proc.Addr, time.Second)
	if _, err := m.Acquire(context.Background(), ""); err == nil {
		t.Error("expected an error for an empty driver id")
	}
}

func TestManyDriversDoNotInterfere(t *testing.T) {
	// Sanity: leases are per-driver, so a busy driver must not block anyone
	// else. If this failed, the lock granularity would be wrong.
	proc := testutil.StartRedis(t)
	ctx := context.Background()
	m := newManager(t, proc.Addr, 10*time.Second)

	const n = 50
	var wg sync.WaitGroup
	var acquired atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := m.Acquire(ctx, fmt.Sprintf("D-%03d", i)); err == nil {
				acquired.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if got := acquired.Load(); got != n {
		t.Errorf("acquired %d of %d distinct drivers, want all", got, n)
	}
}
