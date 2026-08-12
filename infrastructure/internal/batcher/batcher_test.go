package batcher_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	matchingv1 "github.com/aditya/ride-matching/gen/matching/v1"
	"github.com/aditya/ride-matching/internal/batcher"
	"github.com/aditya/ride-matching/internal/engine"
	"github.com/aditya/ride-matching/internal/locations"
	"github.com/aditya/ride-matching/internal/queue"
	"github.com/aditya/ride-matching/internal/testutil"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// harness wires a real Redis (queue + driver store) to a real C++ engine.
// Everything here is the production implementation — the batcher's whole job is
// coordinating these four components, and a test with fakes on both sides would
// verify only that the coordination code compiles.
type harness struct {
	queue   *queue.RedisStream
	drivers *locations.RedisRepository
	engine  *engine.Client
	batches chan batcher.BatchMetrics
}

func newHarness(t *testing.T, cfg *batcher.Config) *harness {
	t.Helper()

	redisProc := testutil.StartRedis(t)
	engineProc := testutil.StartEngine(t, false)

	qOpts := queue.DefaultStreamOptions()
	qOpts.TenantID = "test"
	qOpts.Consumer = "batcher-test"
	q, err := queue.NewRedisStream(redisProc.Addr, qOpts)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })

	locOpts := locations.DefaultOptions()
	locOpts.TenantID = "test"
	drivers, err := locations.NewRedis(redisProc.Addr, locOpts)
	if err != nil {
		t.Fatalf("locations: %v", err)
	}
	t.Cleanup(func() { _ = drivers.Close() })

	eng, err := engine.Dial(engineProc.Addr)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	h := &harness{queue: q, drivers: drivers, engine: eng, batches: make(chan batcher.BatchMetrics, 64)}

	cfg.Logger = quiet()
	cfg.OnBatch = func(m batcher.BatchMetrics) {
		select {
		case h.batches <- m:
		default:
		}
	}
	return h
}

func (h *harness) seedDrivers(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("D-%03d", i)
		lat := 12.9716 + float64(i)*0.0005
		if err := h.drivers.UpsertDriver(ctx, id, lat, 77.5946); err != nil {
			t.Fatalf("seed driver: %v", err)
		}
	}
}

func (h *harness) publishRiders(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		_, err := h.queue.Publish(ctx, queue.RideRequest{
			RequestID:   fmt.Sprintf("req-%03d", i),
			TenantID:    "test",
			RiderID:     fmt.Sprintf("R-%03d", i),
			Lat:         12.9716 + float64(i)*0.0005,
			Lng:         77.5950,
			RequestedAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
}

func (h *harness) awaitBatch(t *testing.T, timeout time.Duration) batcher.BatchMetrics {
	t.Helper()
	select {
	case m := <-h.batches:
		return m
	case <-time.After(timeout):
		t.Fatal("no batch was produced in time")
		return batcher.BatchMetrics{}
	}
}

// TestBatchesFormUnderLightLoad is half the Week 12 checkpoint: a small number
// of requests must still be solved promptly, flushed by the TIMER rather than
// left waiting for a full batch that will never arrive.
func TestBatchesFormUnderLightLoad(t *testing.T) {
	cfg := batcher.DefaultConfig()
	cfg.Window = 500 * time.Millisecond
	cfg.SolveTimeout = 400 * time.Millisecond
	cfg.MaxBatchSize = 100 // deliberately far above the load

	h := newHarness(t, &cfg)
	h.seedDrivers(t, 5)
	h.publishRiders(t, 3)

	b, err := batcher.New(h.queue, h.drivers, h.engine, cfg)
	if err != nil {
		t.Fatalf("new batcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	m := h.awaitBatch(t, 10*time.Second)

	if m.Reason != batcher.FlushTimer {
		t.Errorf("flush reason = %q, want %q — a small batch must be timer-flushed",
			m.Reason, batcher.FlushTimer)
	}
	if m.Riders != 3 {
		t.Errorf("riders = %d, want 3", m.Riders)
	}
	if m.Matched != 3 {
		t.Errorf("matched = %d, want 3 (5 drivers were available)", m.Matched)
	}
	if m.MatchRate != 1.0 {
		t.Errorf("match_rate = %v, want 1.0", m.MatchRate)
	}
	if m.SolveDuration <= 0 {
		t.Error("solve duration was not measured")
	}
}

// TestBatchesFormUnderHeavyLoad is the other half: a spike must flush EARLY on
// size rather than accumulating for a full window. Without the size trigger a
// sudden surge builds an enormous batch that blows both the solve budget and
// the memory bound.
func TestBatchesFormUnderHeavyLoad(t *testing.T) {
	cfg := batcher.DefaultConfig()
	cfg.Window = 10 * time.Second // long enough that the timer cannot be what fires
	cfg.SolveTimeout = 3 * time.Second
	cfg.MaxBatchSize = 10

	h := newHarness(t, &cfg)
	h.seedDrivers(t, 30)
	h.publishRiders(t, 40)

	b, err := batcher.New(h.queue, h.drivers, h.engine, cfg)
	if err != nil {
		t.Fatalf("new batcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	m := h.awaitBatch(t, 10*time.Second)

	if m.Reason != batcher.FlushFull {
		t.Fatalf("flush reason = %q, want %q — a spike must flush on SIZE, not "+
			"wait out a 10s window", m.Reason, batcher.FlushFull)
	}
	if m.Riders != cfg.MaxBatchSize {
		t.Errorf("riders = %d, want exactly MaxBatchSize (%d)", m.Riders, cfg.MaxBatchSize)
	}
	if m.Matched == 0 {
		t.Error("nothing matched despite 30 available drivers")
	}
}

func TestPerBatchMetricsAreEmitted(t *testing.T) {
	// Week 20-22's tuning depends on these numbers existing from the start.
	cfg := batcher.DefaultConfig()
	cfg.Window = 400 * time.Millisecond
	cfg.SolveTimeout = 300 * time.Millisecond

	h := newHarness(t, &cfg)
	h.seedDrivers(t, 4)
	h.publishRiders(t, 4)

	b, _ := batcher.New(h.queue, h.drivers, h.engine, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	m := h.awaitBatch(t, 10*time.Second)

	if m.BatchID == "" {
		t.Error("batch has no id — untraceable in logs")
	}
	if m.Drivers == 0 {
		t.Error("driver count not reported")
	}
	if m.TotalDuration <= 0 {
		t.Error("total duration not measured")
	}
	if m.QueueWaitP50 <= 0 {
		t.Error("queue wait not measured — this is the rider-facing latency")
	}
	if m.MatchRate <= 0 {
		t.Error("match rate not computed")
	}

	st := b.Stats()
	if st.Batches == 0 || st.Requests == 0 {
		t.Errorf("cumulative stats not updated: %+v", st)
	}
}

// TestMatchedRequestsAreAckedUnmatchedAreNot is the correctness heart of the
// batcher. Acking on receipt would be simpler and would silently drop every
// in-flight request on a crash.
func TestMatchedRequestsAreAckedUnmatchedAreNot(t *testing.T) {
	cfg := batcher.DefaultConfig()
	cfg.Window = 400 * time.Millisecond
	cfg.SolveTimeout = 300 * time.Millisecond

	// 5 riders, 2 drivers: 2 must match, 3 cannot.
	h := newHarness(t, &cfg)
	h.seedDrivers(t, 2)
	h.publishRiders(t, 5)

	b, _ := batcher.New(h.queue, h.drivers, h.engine, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	m := h.awaitBatch(t, 10*time.Second)

	if m.Matched != 2 {
		t.Fatalf("matched = %d, want 2 (only 2 drivers exist)", m.Matched)
	}
	if m.Unmatched != 3 {
		t.Fatalf("unmatched = %d, want 3", m.Unmatched)
	}

	// The unmatched riders must still be PENDING — they are still waiting, and
	// a later window may have drivers for them. An unmatched rider is not a
	// completed request.
	time.Sleep(200 * time.Millisecond)
	pending, err := h.queue.Pending(context.Background())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if pending != 3 {
		t.Errorf("pending = %d, want 3 — unmatched riders must NOT be acked", pending)
	}
}

func TestEngineCrashRequeuesTheWholeBatch(t *testing.T) {
	// An engine fault is not the riders' fault. Every request must survive it,
	// which means none of them may be acked.
	cfg := batcher.DefaultConfig()
	cfg.Window = 400 * time.Millisecond
	cfg.SolveTimeout = 300 * time.Millisecond

	redisProc := testutil.StartRedis(t)
	engineProc := testutil.StartEngine(t, false)

	qOpts := queue.DefaultStreamOptions()
	qOpts.TenantID = "test"
	qOpts.Consumer = "batcher-crash-test"
	q, err := queue.NewRedisStream(redisProc.Addr, qOpts)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	defer q.Close()

	locOpts := locations.DefaultOptions()
	locOpts.TenantID = "test"
	drivers, err := locations.NewRedis(redisProc.Addr, locOpts)
	if err != nil {
		t.Fatalf("locations: %v", err)
	}
	defer drivers.Close()

	eng, err := engine.Dial(engineProc.Addr, engine.WithTimeout(300*time.Millisecond))
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	defer eng.Close()

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := drivers.UpsertDriver(ctx, fmt.Sprintf("D-%d", i), 12.9716, 77.5946); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := q.Publish(ctx, queue.RideRequest{
			RequestID: fmt.Sprintf("req-%d", i), TenantID: "test",
			RiderID: fmt.Sprintf("R-%d", i), Lat: 12.9716, Lng: 77.5950,
			RequestedAt: time.Now(),
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	// Kill the engine BEFORE the batcher can solve anything.
	engineProc.Kill(t)

	batches := make(chan batcher.BatchMetrics, 8)
	cfg.Logger = quiet()
	cfg.OnBatch = func(m batcher.BatchMetrics) {
		select {
		case batches <- m:
		default:
		}
	}

	b, err := batcher.New(q, drivers, eng, cfg)
	if err != nil {
		t.Fatalf("new batcher: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = b.Run(runCtx) }()

	select {
	case m := <-batches:
		if m.Err == nil {
			t.Fatal("expected the batch to report an engine error")
		}
		if m.Requeued != m.Riders {
			t.Errorf("requeued = %d, want all %d riders", m.Requeued, m.Riders)
		}
		if m.DeadLettered != 0 {
			t.Error("an engine crash is retryable — the batch must NOT be dead-lettered")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no batch outcome reported")
	}

	// Nothing acked: every request is still recoverable.
	time.Sleep(200 * time.Millisecond)
	pending, _ := q.Pending(ctx)
	if pending != 3 {
		t.Errorf("pending = %d, want 3 — an engine crash must not lose requests", pending)
	}
	if dead, _ := q.DeadLetterDepth(ctx); dead != 0 {
		t.Errorf("dead-letter depth = %d, want 0", dead)
	}
}

func TestBatchWithNoDriversIsRequeuedNotRejected(t *testing.T) {
	// A driver may come on shift in seconds, and the rider has not been told
	// anything yet, so requeueing is right and rejecting would be premature.
	cfg := batcher.DefaultConfig()
	cfg.Window = 400 * time.Millisecond
	cfg.SolveTimeout = 300 * time.Millisecond

	h := newHarness(t, &cfg)
	h.publishRiders(t, 3) // no drivers seeded

	b, _ := batcher.New(h.queue, h.drivers, h.engine, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	m := h.awaitBatch(t, 10*time.Second)

	if m.Drivers != 0 {
		t.Errorf("drivers = %d, want 0", m.Drivers)
	}
	if m.Requeued != 3 {
		t.Errorf("requeued = %d, want 3", m.Requeued)
	}
	if m.DeadLettered != 0 {
		t.Error("no drivers is a transient condition, not poison")
	}
}

func TestDriversAreDeduplicatedAcrossRiders(t *testing.T) {
	// A driver near two riders must appear ONCE. Sending them twice would let
	// the solver believe there are two cars where there is one, and it would
	// happily assign both.
	cfg := batcher.DefaultConfig()
	cfg.Window = 400 * time.Millisecond
	cfg.SolveTimeout = 300 * time.Millisecond
	cfg.SearchRadiusMeters = 50000 // wide enough that every rider sees every driver

	h := newHarness(t, &cfg)
	h.seedDrivers(t, 3)
	h.publishRiders(t, 5)

	b, _ := batcher.New(h.queue, h.drivers, h.engine, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	m := h.awaitBatch(t, 10*time.Second)

	if m.Drivers != 3 {
		t.Fatalf("drivers = %d, want 3 — the union must be deduplicated", m.Drivers)
	}
	// And with 3 real drivers, at most 3 riders can be matched.
	if m.Matched > 3 {
		t.Fatalf("matched %d riders with only 3 drivers — a driver was double-booked",
			m.Matched)
	}
}

func TestConfigValidation(t *testing.T) {
	cfg := batcher.DefaultConfig()
	cfg.Logger = quiet()

	t.Run("solve timeout must fit inside the window", func(t *testing.T) {
		bad := cfg
		bad.Window = time.Second
		bad.SolveTimeout = 2 * time.Second
		if _, err := batcher.New(nil, nil, nil, bad); err == nil {
			t.Error("expected an error")
		}
	})

	t.Run("travel time requires a graph id", func(t *testing.T) {
		bad := cfg
		bad.CostMetric = matchingv1.CostMetric_COST_METRIC_TRAVEL_TIME
		bad.RoadGraphID = ""
		if _, err := batcher.New(nil, nil, nil, bad); err == nil {
			t.Error("expected an error")
		}
	})

	t.Run("nil dependencies are refused", func(t *testing.T) {
		if _, err := batcher.New(nil, nil, nil, cfg); err == nil {
			t.Error("expected an error for nil dependencies")
		}
	})
}
