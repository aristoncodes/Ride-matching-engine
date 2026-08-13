// Package batcher is the microservice that turns a stream of individual ride
// requests into the batched matrices the C++ engine wants.
//
// It is where every previous week meets:
//
//	queue (W10)      -> pops durable ride requests
//	locations (W7)   -> finds candidate drivers near each rider
//	engine (W6)      -> solves the batch optimally in C++
//	taxonomy (W6)    -> decides ack vs requeue vs dead-letter for each failure
//
// The single most important property is that a request is ACKED ONLY AFTER it
// has been matched or deliberately rejected. Acking on receipt would be simpler
// and would silently drop every request in flight during a crash — which is
// precisely the failure ADR-0002 and ADR-0006 exist to prevent.
package batcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	matchingv1 "github.com/aditya/ride-matching/gen/matching/v1"
	"github.com/aditya/ride-matching/internal/engine"
	"github.com/aditya/ride-matching/internal/locations"
	"github.com/aditya/ride-matching/internal/queue"
)

// Config tunes the batcher.
type Config struct {
	// Window is the maximum time a request waits before its batch is solved
	// (ADR-0005). This is the LATENCY half of the dual trigger.
	Window time.Duration

	// MaxBatchSize forces an early flush once this many requests have
	// accumulated. This is the MEMORY/COMPUTE half of the dual trigger.
	//
	// Both are needed and they protect different things. Time alone means a
	// sudden spike builds an enormous batch that blows the solve budget and the
	// memory bound. Size alone means a quiet period leaves a lone rider waiting
	// indefinitely for company that never arrives.
	MaxBatchSize int

	// SearchRadiusMeters bounds the candidate driver search per rider.
	SearchRadiusMeters float64

	// MaxCandidatesPerRider caps the shortlist sent to the engine — the k that
	// Week 5 measured as the difference between a 0.4 ms and a 3.8 ms solve.
	MaxCandidatesPerRider int

	// CostMetric selects euclidean or road-time pricing.
	CostMetric matchingv1.CostMetric

	// RoadGraphID is required when CostMetric is TRAVEL_TIME.
	RoadGraphID string

	// SolveTimeout bounds one call into the C++ engine. Must leave room inside
	// the window, or batches overlap.
	SolveTimeout time.Duration

	// ReclaimEvery is how often to look for requests abandoned by a crashed
	// batcher, and ReclaimMinIdle is how long a request must sit untouched
	// before it is considered abandoned rather than merely in progress.
	ReclaimEvery   time.Duration
	ReclaimMinIdle time.Duration

	// MaxMatchAttempts is how many windows a rider may go unmatched before the
	// system gives up and tells them so.
	//
	// This is a PRODUCT decision, not an infrastructure one, and keeping it
	// separate from the queue's MaxDeliveries is the whole point: "no car was
	// available five times running" is a legitimate answer to give a customer,
	// whereas "the consumer crashed five times" is an operational fault they
	// should never see. Conflating them dead-letters real riders as poison.
	MaxMatchAttempts int

	Logger *slog.Logger

	// OnBatch receives per-batch metrics. Week 20-22's tuning needs these
	// numbers, and a hook keeps tests deterministic instead of scraping logs.
	OnBatch func(BatchMetrics)

	Now func() time.Time
}

// DefaultConfig returns production-shaped defaults.
func DefaultConfig() Config {
	return Config{
		Window:                3 * time.Second, // ADR-0005
		MaxBatchSize:          500,
		SearchRadiusMeters:    5000,
		MaxCandidatesPerRider: 8,
		CostMetric:            matchingv1.CostMetric_COST_METRIC_EUCLIDEAN,
		SolveTimeout:          2 * time.Second,
		ReclaimEvery:          30 * time.Second,
		ReclaimMinIdle:        60 * time.Second,
		// ~1 minute of trying at a 3s window. Long enough to ride out a quiet
		// patch or a driver shift change; short enough that a rider in a dead
		// zone is told "no cars" rather than left waiting indefinitely.
		MaxMatchAttempts: 20,
		Logger:           slog.Default(),
		Now:              time.Now,
	}
}

func (c *Config) applyDefaults() {
	d := DefaultConfig()
	if c.Window <= 0 {
		c.Window = d.Window
	}
	if c.MaxBatchSize <= 0 {
		c.MaxBatchSize = d.MaxBatchSize
	}
	if c.SearchRadiusMeters <= 0 {
		c.SearchRadiusMeters = d.SearchRadiusMeters
	}
	if c.MaxCandidatesPerRider < 0 {
		c.MaxCandidatesPerRider = d.MaxCandidatesPerRider
	}
	if c.SolveTimeout <= 0 {
		c.SolveTimeout = d.SolveTimeout
	}
	if c.ReclaimEvery <= 0 {
		c.ReclaimEvery = d.ReclaimEvery
	}
	if c.ReclaimMinIdle <= 0 {
		c.ReclaimMinIdle = d.ReclaimMinIdle
	}
	if c.MaxMatchAttempts <= 0 {
		c.MaxMatchAttempts = d.MaxMatchAttempts
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// FlushReason records which half of the dual trigger fired. Worth measuring:
// batches that are consistently size-triggered mean the window is too long for
// the load, and consistently time-triggered means there is spare capacity.
type FlushReason string

const (
	FlushTimer    FlushReason = "timer"
	FlushFull     FlushReason = "size"
	FlushShutdown FlushReason = "shutdown"
)

// BatchMetrics is one batch's outcome. Emitted for every batch.
type BatchMetrics struct {
	BatchID       string
	Reason        FlushReason
	Riders        int
	Drivers       int
	Matched       int
	Unmatched     int
	Requeued      int
	Exhausted     int // gave up: no driver found within MaxMatchAttempts
	DeadLettered  int
	QueueWaitP50  time.Duration // how long the median request waited to be batched
	SolveDuration time.Duration // time inside the C++ engine
	TotalDuration time.Duration
	Err           error

	// MatchRate is matched/riders — the number that says whether the system is
	// doing its job, as distinct from whether it is fast.
	MatchRate float64
}

// Stats are cumulative counters.
type Stats struct {
	Batches      int64
	Requests     int64
	Matched      int64
	Unmatched    int64
	Requeued     int64
	Exhausted    int64
	DeadLettered int64
	SolveErrors  int64
}

// Batcher pops ride requests, batches them, and solves them.
type Batcher struct {
	cfg     Config
	queue   queue.Queue
	drivers locations.Repository
	engine  *engine.Client

	batches      atomic.Int64
	requests     atomic.Int64
	matched      atomic.Int64
	unmatched    atomic.Int64
	requeued     atomic.Int64
	exhausted    atomic.Int64
	deadLettered atomic.Int64
	solveErrors  atomic.Int64

	done chan struct{}
}

// New creates a batcher.
func New(q queue.Queue, drivers locations.Repository, eng *engine.Client, cfg Config) (*Batcher, error) {
	if q == nil {
		return nil, errors.New("batcher: queue must not be nil")
	}
	if drivers == nil {
		return nil, errors.New("batcher: driver store must not be nil")
	}
	if eng == nil {
		return nil, errors.New("batcher: engine client must not be nil")
	}
	cfg.applyDefaults()
	if cfg.SolveTimeout >= cfg.Window {
		// Same invariant as the Week 9 pipeline: work that can outlast its own
		// window means windows overlap and queue up behind each other.
		return nil, fmt.Errorf("batcher: SolveTimeout (%v) must be shorter than Window (%v)",
			cfg.SolveTimeout, cfg.Window)
	}
	if cfg.CostMetric == matchingv1.CostMetric_COST_METRIC_TRAVEL_TIME && cfg.RoadGraphID == "" {
		return nil, errors.New("batcher: RoadGraphID is required for TRAVEL_TIME pricing")
	}
	return &Batcher{cfg: cfg, queue: q, drivers: drivers, engine: eng, done: make(chan struct{})}, nil
}

// Stats snapshots the counters.
func (b *Batcher) Stats() Stats {
	return Stats{
		Batches:      b.batches.Load(),
		Requests:     b.requests.Load(),
		Matched:      b.matched.Load(),
		Unmatched:    b.unmatched.Load(),
		Requeued:     b.requeued.Load(),
		Exhausted:    b.exhausted.Load(),
		DeadLettered: b.deadLettered.Load(),
		SolveErrors:  b.solveErrors.Load(),
	}
}

// Done is closed when Run returns.
func (b *Batcher) Done() <-chan struct{} { return b.done }

// Run pops requests and processes batches until ctx is cancelled.
func (b *Batcher) Run(ctx context.Context) error {
	defer close(b.done)

	// The reclaimer is a separate loop because it addresses a different
	// failure: not "this batch failed" but "a previous batcher died holding
	// work". Without it, a crashed instance's in-flight requests sit pending
	// forever — durably stored and delivered to nobody.
	reclaimDone := b.startReclaimer(ctx)
	defer func() { <-reclaimDone }()

	var pending []queue.Message
	timer := time.NewTimer(b.cfg.Window)
	defer timer.Stop()

	// windowStart is when the OLDEST request in the current batch arrived, so
	// the timer measures a rider's actual wait rather than loop iterations.
	windowStart := b.cfg.Now()

	for {
		select {
		case <-ctx.Done():
			if len(pending) > 0 {
				// Do not abandon a partial batch on shutdown. The requests are
				// already claimed and unacked, so they WOULD be recovered
				// eventually — but making a rider wait for a reclaim cycle when
				// we could just solve the batch is a poor trade.
				solveCtx, cancel := context.WithTimeout(context.Background(), b.cfg.SolveTimeout)
				b.processBatch(solveCtx, pending, FlushShutdown)
				cancel()
			}
			return ctx.Err()

		case <-timer.C:
			if len(pending) > 0 {
				b.processBatch(ctx, pending, FlushTimer)
				pending = nil
			}
			windowStart = b.cfg.Now()
			timer.Reset(b.cfg.Window)

		default:
			// Block for whatever remains of the window rather than a fixed
			// duration, so a request arriving late in a window is not held for
			// a further full window before being solved.
			remaining := b.cfg.Window - b.cfg.Now().Sub(windowStart)
			if remaining <= 0 {
				remaining = time.Millisecond
			}

			room := b.cfg.MaxBatchSize - len(pending)
			msgs, err := b.queue.Consume(ctx, room, remaining)
			if err != nil {
				if ctx.Err() != nil {
					continue // shutting down; the ctx.Done case handles it
				}
				b.cfg.Logger.Error("consume failed", "err", err)
				select {
				case <-ctx.Done():
				case <-time.After(100 * time.Millisecond): // avoid a hot spin
				}
				continue
			}

			pending = append(pending, msgs...)

			// The SIZE half of the dual trigger.
			if len(pending) >= b.cfg.MaxBatchSize {
				b.processBatch(ctx, pending, FlushFull)
				pending = nil
				windowStart = b.cfg.Now()
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(b.cfg.Window)
				continue
			}

			// The TIME half, checked here too because Consume may have returned
			// early with a partial batch.
			if len(pending) > 0 && b.cfg.Now().Sub(windowStart) >= b.cfg.Window {
				b.processBatch(ctx, pending, FlushTimer)
				pending = nil
				windowStart = b.cfg.Now()
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(b.cfg.Window)
			}
		}
	}
}

// processBatch solves one batch and settles every message in it.
func (b *Batcher) processBatch(ctx context.Context, msgs []queue.Message, reason FlushReason) {
	if len(msgs) == 0 {
		return
	}
	started := b.cfg.Now()
	batchID := fmt.Sprintf("batch-%d", started.UnixNano())

	metrics := BatchMetrics{BatchID: batchID, Reason: reason, Riders: len(msgs)}
	b.batches.Add(1)
	b.requests.Add(int64(len(msgs)))

	metrics.QueueWaitP50 = medianWait(msgs, started)

	// ---- Candidate drivers ------------------------------------------------
	// One radius query per rider. The union is deduplicated because a driver
	// near two riders must appear ONCE in the batch: sending them twice would
	// let the solver believe there are two cars where there is one.
	// ONE pipelined round trip for the whole batch, not one per rider.
	//
	// Week 20's profile showed the batcher spending ~59% of its CPU inside
	// Redis calls from this function — not because any single query was slow,
	// but because a 500-rider batch meant 500 sequential round trips. This is
	// the Week 22 optimisation, and it was chosen because the profiler pointed
	// at it rather than because it looked slow.
	queries := make([]locations.Query, len(msgs))
	for i, m := range msgs {
		queries[i] = locations.Query{
			Lat:    m.Request.Lat,
			Lng:    m.Request.Lng,
			Radius: b.cfg.SearchRadiusMeters,
			Limit:  b.cfg.MaxCandidatesPerRider * 4, // slack: the solver picks
		}
	}

	results, err := b.drivers.NearbyMany(ctx, queries)
	if err != nil {
		// The driver store is down. Every request here is still valid, so
		// requeue the whole batch rather than failing riders for an outage
		// that is not their fault.
		b.cfg.Logger.Error("driver lookup failed; requeueing batch",
			"batch_id", batchID, "err", err)
		metrics.Err = err
		metrics.Requeued = len(msgs)
		b.requeued.Add(int64(len(msgs)))
		b.emit(metrics, started)
		return // no ack: the messages stay pending and are reclaimed
	}

	driverSet := map[string]locations.DriverLocation{}
	for _, near := range results {
		for _, d := range near {
			driverSet[d.DriverID] = d
		}
	}

	metrics.Drivers = len(driverSet)

	if len(driverSet) == 0 {
		// No drivers anywhere near this batch. Requeue rather than reject:
		// a driver may come on shift in the next few seconds, and the rider has
		// not been told anything yet.
		b.cfg.Logger.Info("no candidate drivers; requeueing batch",
			"batch_id", batchID, "riders", len(msgs))
		metrics.Requeued = len(msgs)
		b.requeued.Add(int64(len(msgs)))
		b.emit(metrics, started)
		return
	}

	// ---- Solve -------------------------------------------------------------
	req := &matchingv1.MatchBatchRequest{
		TenantId:              msgs[0].Request.TenantID,
		BatchId:               batchID,
		CostMetric:            b.cfg.CostMetric,
		RoadGraphId:           b.cfg.RoadGraphID,
		MaxCandidatesPerRider: int32(b.cfg.MaxCandidatesPerRider),
	}
	for _, m := range msgs {
		req.Riders = append(req.Riders, &matchingv1.Rider{
			Id:     m.Request.RequestID,
			Pickup: &matchingv1.LatLng{Lat: m.Request.Lat, Lng: m.Request.Lng},
		})
	}
	for _, d := range driverSet {
		req.CandidateDrivers = append(req.CandidateDrivers, &matchingv1.Driver{
			Id:       d.DriverID,
			Location: &matchingv1.LatLng{Lat: d.Lat, Lng: d.Lng},
		})
	}

	solveCtx, cancel := context.WithTimeout(ctx, b.cfg.SolveTimeout)
	solveStart := time.Now()
	resp, err := b.engine.SolveBatch(solveCtx, req)
	metrics.SolveDuration = time.Since(solveStart)
	cancel()

	if err != nil {
		b.solveErrors.Add(1)
		metrics.Err = err

		// THE decision the Week 6 taxonomy exists for.
		if engine.Retryable(err) {
			// The engine crashed or timed out. Nothing is wrong with these
			// requests, so leave them unacked: they stay pending and a
			// reclaimer picks them up. No rider is dropped for an engine fault.
			b.cfg.Logger.Error("solve failed; leaving batch for redelivery",
				"batch_id", batchID, "err", err)
			metrics.Requeued = len(msgs)
			b.requeued.Add(int64(len(msgs)))
		} else {
			// Poison: malformed batch, missing graph, too large. Retrying is
			// futile and would block the queue forever, so it goes aside.
			b.cfg.Logger.Error("solve failed permanently; dead-lettering batch",
				"batch_id", batchID, "err", err)
			for _, m := range msgs {
				if dlErr := b.queue.DeadLetter(ctx, m, err.Error()); dlErr != nil {
					b.cfg.Logger.Error("dead-letter failed", "err", dlErr)
				}
			}
			metrics.DeadLettered = len(msgs)
			b.deadLettered.Add(int64(len(msgs)))
		}
		b.emit(metrics, started)
		return
	}

	// ---- Settle ------------------------------------------------------------
	matchedRequests := make(map[string]bool, len(resp.GetMatches()))
	for _, m := range resp.GetMatches() {
		matchedRequests[m.GetRiderId()] = true
	}

	var ackIDs []string
	var requeue, exhausted int
	for _, m := range msgs {
		if matchedRequests[m.Request.RequestID] {
			// Matched: the work is done, so the request can finally be acked.
			ackIDs = append(ackIDs, m.ID)
			continue
		}

		// Unmatched — a normal outcome, not a failure. The rider is still
		// waiting, so the request goes back for a later window when more
		// drivers may be on shift.
		//
		// REPUBLISHED rather than left pending, and that distinction is a bug
		// fix rather than a style choice. Leaving it pending accumulates the
		// queue's DELIVERY count, which exists to detect poison messages — so a
		// rider who simply cannot find a car would eventually be dead-lettered
		// as though their request were malformed. A chaos test caught exactly
		// that: delivery counts climbing to 4 of 5 on perfectly valid riders.
		//
		// Republishing resets the infrastructure counter and advances a
		// separate, business-level MatchAttempts instead.
		if m.Request.MatchAttempts+1 >= b.cfg.MaxMatchAttempts {
			// Genuinely out of options. This IS a real answer for the rider —
			// "no cars available" — so it is surfaced deliberately rather than
			// retried forever.
			if err := b.queue.DeadLetter(ctx, m, fmt.Sprintf(
				"no driver found after %d match attempts", m.Request.MatchAttempts+1)); err != nil {
				b.cfg.Logger.Error("could not dead-letter an exhausted request",
					"request_id", m.Request.RequestID, "err", err)
				continue
			}
			exhausted++
			continue
		}

		if err := b.queue.Republish(ctx, m); err != nil {
			// The republish failed, so do NOT ack: the message stays pending
			// and is reclaimed. Slower, but it cannot be lost.
			b.cfg.Logger.Error("could not republish an unmatched request",
				"request_id", m.Request.RequestID, "err", err)
		}
		requeue++
	}

	if len(ackIDs) > 0 {
		if err := b.queue.Ack(ctx, ackIDs...); err != nil {
			// At-least-once in action: the matches happened, but the acks did
			// not. Those requests will be redelivered and re-matched, which is
			// why the pipeline must be idempotent on RequestID.
			b.cfg.Logger.Error("ack failed; matched requests will be redelivered",
				"batch_id", batchID, "count", len(ackIDs), "err", err)
			metrics.Err = err
		}
	}

	metrics.Matched = len(matchedRequests)
	metrics.Unmatched = requeue + exhausted
	metrics.Requeued = requeue
	metrics.Exhausted = exhausted
	b.matched.Add(int64(len(matchedRequests)))
	b.unmatched.Add(int64(requeue + exhausted))
	b.requeued.Add(int64(requeue))
	b.exhausted.Add(int64(exhausted))

	b.emit(metrics, started)
}

func (b *Batcher) emit(m BatchMetrics, started time.Time) {
	m.TotalDuration = b.cfg.Now().Sub(started)
	if m.Riders > 0 {
		m.MatchRate = float64(m.Matched) / float64(m.Riders)
	}

	b.cfg.Logger.Info("batch complete",
		"batch_id", m.BatchID,
		"reason", string(m.Reason),
		"riders", m.Riders,
		"drivers", m.Drivers,
		"matched", m.Matched,
		"unmatched", m.Unmatched,
		"exhausted", m.Exhausted,
		"match_rate", fmt.Sprintf("%.2f", m.MatchRate),
		"queue_wait_p50_ms", m.QueueWaitP50.Milliseconds(),
		"solve_ms", m.SolveDuration.Milliseconds(),
		"total_ms", m.TotalDuration.Milliseconds())

	if b.cfg.OnBatch != nil {
		b.cfg.OnBatch(m)
	}
}

// startReclaimer recovers requests abandoned by a crashed batcher.
func (b *Batcher) startReclaimer(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(b.cfg.ReclaimEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				msgs, err := b.queue.Reclaim(ctx, b.cfg.ReclaimMinIdle, b.cfg.MaxBatchSize)
				if err != nil {
					b.cfg.Logger.Error("reclaim failed", "err", err)
					continue
				}
				if len(msgs) == 0 {
					continue
				}
				b.cfg.Logger.Warn("reclaimed abandoned requests",
					"count", len(msgs))
				b.processBatch(ctx, msgs, FlushTimer)
			}
		}
	}()
	return done
}

// medianWait is how long the median request waited between being accepted by
// the API and being batched. The p50 rather than the mean, because one very old
// reclaimed request would drag an average and hide the typical experience.
func medianWait(msgs []queue.Message, now time.Time) time.Duration {
	waits := make([]time.Duration, 0, len(msgs))
	for _, m := range msgs {
		if !m.Request.RequestedAt.IsZero() {
			waits = append(waits, now.Sub(m.Request.RequestedAt))
		}
	}
	if len(waits) == 0 {
		return 0
	}
	for i := 1; i < len(waits); i++ { // insertion sort: batches are small
		for j := i; j > 0 && waits[j] < waits[j-1]; j-- {
			waits[j], waits[j-1] = waits[j-1], waits[j]
		}
	}
	return waits[len(waits)/2]
}
