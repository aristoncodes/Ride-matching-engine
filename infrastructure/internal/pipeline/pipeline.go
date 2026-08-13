// Package pipeline connects GPS ingestion to the live location store on a
// fixed cadence.
//
// Two ideas do all the work here.
//
// COALESCING. A driver pings every ~3 seconds, and only their LATEST position
// matters — an older fix is not partial information, it is wrong information
// that a newer one has already superseded. So a window buffers at most one
// ping per driver. Ten pings from one driver in a window become one write, and
// the reduction costs nothing, because the discarded pings were worthless.
//
// BOUNDED, DELIBERATE SHEDDING. Under overload the choice is never "drop
// nothing" — it is "drop something you chose" or "drop everything when the
// process OOMs". The buffer is capped at a number of DISTINCT DRIVERS; past
// that, new drivers are rejected and counted, while updates for already-known
// drivers are still accepted because they cost no additional memory.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aditya/ride-matching/internal/ingest"
	"github.com/aditya/ride-matching/internal/locations"
)

// Config tunes the pipeline.
type Config struct {
	// Window is how often the buffer is flushed to the store.
	//
	// Configurable rather than hardcoded because it is a genuine product
	// tradeoff, not an implementation detail: shorter means fresher driver
	// positions and more Redis traffic; longer means fewer writes and staler
	// matches. It is also the same cadence the matcher batches on (ADR-0005),
	// so the two must be tuned together.
	Window time.Duration

	// MaxBufferedDrivers caps distinct drivers held between flushes. This is
	// the memory bound: at ~64 bytes per entry, 100k drivers is ~6 MB, and the
	// figure is chosen so the worst case is arithmetic rather than a surprise.
	MaxBufferedDrivers int

	// FlushTimeout bounds one write to the store. Longer than the window would
	// let flushes overlap and pile up — the exact unbounded growth this package
	// exists to prevent.
	FlushTimeout time.Duration

	Logger *slog.Logger

	// OnWindow is called after every flush with that window's stats. The hook
	// exists so tests can observe windows deterministically instead of parsing
	// log output.
	OnWindow func(WindowStats)

	// Now is injectable for tests.
	Now func() time.Time
}

// DefaultConfig returns production-shaped defaults.
func DefaultConfig() Config {
	return Config{
		Window:             3 * time.Second, // ADR-0005
		MaxBufferedDrivers: 100_000,
		FlushTimeout:       2 * time.Second,
		Logger:             slog.Default(),
		Now:                time.Now,
	}
}

func (c *Config) applyDefaults() {
	d := DefaultConfig()
	if c.Window <= 0 {
		c.Window = d.Window
	}
	if c.MaxBufferedDrivers <= 0 {
		c.MaxBufferedDrivers = d.MaxBufferedDrivers
	}
	if c.FlushTimeout <= 0 {
		c.FlushTimeout = d.FlushTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// WindowStats is one window's throughput. Logged every window, because "pings
// in, writes out" is the number that tells an operator whether the pipeline is
// keeping up — and the ratio between them shows how much coalescing is buying.
type WindowStats struct {
	Window         int64
	PingsAccepted  int64 // pings taken into the buffer this window
	PingsShed      int64 // rejected: the buffer was full
	DriversFlushed int   // distinct drivers written to the store
	FlushDuration  time.Duration
	FlushError     error
}

// Stats are cumulative counters.
type Stats struct {
	TotalPings    int64
	TotalShed     int64
	TotalFlushed  int64
	TotalWindows  int64
	FlushFailures int64
	Buffered      int
}

// StoreFor returns the location store for a tenant.
//
// A factory rather than a single Repository, because each tenant's driver
// positions live under their own Redis key prefix (ADR-0004) and one ingestd
// now serves many tenants at once (Week 18 put the tenant on the connection,
// not in the process config). Writing every tenant's drivers into one store
// would be a cross-tenant data leak dressed up as a caching decision.
type StoreFor func(tenantID string) locations.Repository

// Pipeline batches pings and flushes them to a location store on a ticker.
type Pipeline struct {
	cfg      Config
	store    locations.Repository // the single-tenant store, when storeFor is nil
	storeFor StoreFor

	// The buffer is a map keyed by driver id, which IS the coalescing: a second
	// ping from the same driver overwrites the first rather than queueing
	// behind it. A slice would preserve an order nobody wants and grow without
	// bound in the process.
	mu     sync.Mutex
	buffer map[string]locations.DriverLocation

	pings         atomic.Int64
	shed          atomic.Int64
	flushed       atomic.Int64
	windows       atomic.Int64
	flushFailures atomic.Int64

	stopOnce sync.Once
	done     chan struct{}
}

// NewMultiTenant creates a pipeline that routes each ping to its tenant's
// store, as identified by the authenticated API key on the connection.
func NewMultiTenant(storeFor StoreFor, cfg Config) (*Pipeline, error) {
	if storeFor == nil {
		return nil, errors.New("pipeline: storeFor must not be nil")
	}
	p, err := New(noopStore{}, cfg)
	if err != nil {
		return nil, err
	}
	p.storeFor = storeFor
	return p, nil
}

// New creates a pipeline. Call Run to start flushing.
func New(store locations.Repository, cfg Config) (*Pipeline, error) {
	if store == nil {
		return nil, errors.New("pipeline: store must not be nil")
	}
	cfg.applyDefaults()
	if cfg.FlushTimeout > cfg.Window {
		// Rejected rather than silently accepted: a flush slower than the
		// window means the next window starts before the previous finished,
		// and queued flushes are how a "bounded" pipeline becomes unbounded.
		return nil, fmt.Errorf("pipeline: FlushTimeout (%v) must not exceed Window (%v)",
			cfg.FlushTimeout, cfg.Window)
	}
	return &Pipeline{
		cfg:    cfg,
		store:  store,
		buffer: make(map[string]locations.DriverLocation, cfg.MaxBufferedDrivers/8),
		done:   make(chan struct{}),
	}, nil
}

// Accept implements ingest.Sink. Called from one goroutine per connection, so
// it must be cheap: it takes a lock, writes one map entry, and returns. Any
// real work belongs in the flush, off the connection's critical path.
func (p *Pipeline) Accept(_ context.Context, ping ingest.Ping) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Keyed by TENANT AND DRIVER, not driver alone. Two tenants can legitimately
	// use the same driver id ("D-001"), and a driver-only key would let one
	// tenant's ping silently overwrite the other's position — a cross-tenant
	// data corruption that would look like a flapping GPS bug.
	bufKey := ping.TenantID + "\x00" + ping.DriverID

	_, known := p.buffer[bufKey]
	if !known && len(p.buffer) >= p.cfg.MaxBufferedDrivers {
		// SHED. Deliberately, and counted — an operator can see it in the
		// window stats. Note that an update for a driver ALREADY buffered is
		// still accepted below: it reuses an existing entry, so it costs no
		// memory, and refusing it would throw away fresher data for nothing.
		p.shed.Add(1)
		return nil
	}

	p.buffer[bufKey] = locations.DriverLocation{
		TenantID: ping.TenantID,
		DriverID: ping.DriverID,
		Lat:      ping.Lat,
		Lng:      ping.Lng,
		// Server arrival time, never the client's clock. Phones have wrong
		// clocks, and a driver whose clock runs fast would otherwise look
		// permanently fresh and never age out of the matching pool.
		LastSeen: p.cfg.Now(),
	}
	p.pings.Add(1)
	return nil
}

// Run flushes on the configured cadence until ctx is cancelled, then performs
// one final flush so a clean shutdown does not discard the last window.
func (p *Pipeline) Run(ctx context.Context) error {
	defer p.stopOnce.Do(func() { close(p.done) })

	ticker := time.NewTicker(p.cfg.Window)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// A fresh context: the caller's is already cancelled, and reusing
			// it would cancel the final flush before it could run.
			flushCtx, cancel := context.WithTimeout(context.Background(), p.cfg.FlushTimeout)
			p.flush(flushCtx)
			cancel()
			return ctx.Err()

		case <-ticker.C:
			flushCtx, cancel := context.WithTimeout(ctx, p.cfg.FlushTimeout)
			p.flush(flushCtx)
			cancel()
		}
	}
}

// Flush writes the current buffer immediately. Exported for tests and for a
// shutdown path that wants to drain without waiting for a tick.
func (p *Pipeline) Flush(ctx context.Context) error {
	stats := p.flush(ctx)
	return stats.FlushError
}

func (p *Pipeline) flush(ctx context.Context) WindowStats {
	// Swap the buffer out under the lock and write OUTSIDE it. Holding the lock
	// across a network call would block every connection goroutine for the
	// duration of a Redis round trip — turning one slow dependency into a
	// stalled ingestion layer.
	p.mu.Lock()
	batch := p.buffer
	p.buffer = make(map[string]locations.DriverLocation, len(batch))
	p.mu.Unlock()

	window := p.windows.Add(1)
	stats := WindowStats{
		Window:         window,
		PingsAccepted:  p.pings.Load(),
		PingsShed:      p.shed.Load(),
		DriversFlushed: len(batch),
	}

	if len(batch) == 0 {
		if p.cfg.OnWindow != nil {
			p.cfg.OnWindow(stats)
		}
		return stats
	}

	// Grouped by tenant, so each batch of writes goes to that tenant's keys.
	byTenant := make(map[string][]locations.DriverLocation, 4)
	for _, l := range batch {
		byTenant[l.TenantID] = append(byTenant[l.TenantID], l)
	}

	started := time.Now()
	var err error
	for tenant, locs := range byTenant {
		store := p.store
		if p.storeFor != nil {
			store = p.storeFor(tenant)
		}
		if writeErr := store.UpsertMany(ctx, locs); writeErr != nil {
			// Recorded, but the loop continues: one tenant's Redis failure must
			// not discard another tenant's positions.
			err = writeErr
		}
	}
	stats.FlushDuration = time.Since(started)
	stats.FlushError = err

	if err != nil {
		// The window is DROPPED, not retried into the next one.
		//
		// That is deliberate: re-queueing failed positions would grow the
		// buffer during exactly the outage that made it fail, and the data is
		// about to be superseded anyway — every driver pings again in 3
		// seconds. Losing a window of positions during a Redis outage is
		// cheap; OOMing the ingestion layer is not.
		p.flushFailures.Add(1)
		p.cfg.Logger.Error("flush failed; window dropped",
			"window", window, "drivers", len(batch), "tenants", len(byTenant), "err", err)
	} else {
		p.flushed.Add(int64(len(batch)))
		p.cfg.Logger.Info("window flushed",
			"window", window,
			"drivers", len(batch),
			"tenants", len(byTenant),
			"duration_ms", stats.FlushDuration.Milliseconds(),
			"pings_total", stats.PingsAccepted,
			"shed_total", stats.PingsShed)
	}

	if p.cfg.OnWindow != nil {
		p.cfg.OnWindow(stats)
	}
	return stats
}

// Stats snapshots the cumulative counters.
func (p *Pipeline) Stats() Stats {
	p.mu.Lock()
	buffered := len(p.buffer)
	p.mu.Unlock()

	return Stats{
		TotalPings:    p.pings.Load(),
		TotalShed:     p.shed.Load(),
		TotalFlushed:  p.flushed.Load(),
		TotalWindows:  p.windows.Load(),
		FlushFailures: p.flushFailures.Load(),
		Buffered:      buffered,
	}
}

// Done is closed when Run returns, so a caller can wait for a real exit rather
// than assume one.
func (p *Pipeline) Done() <-chan struct{} { return p.done }

// Compile-time proof the pipeline is a valid ingestion sink.
var _ ingest.Sink = (*Pipeline)(nil)

// noopStore satisfies locations.Repository for the multi-tenant pipeline,
// whose writes always go through storeFor. It exists so New's nil-check stays
// meaningful for the single-tenant path rather than being loosened.
type noopStore struct{}

func (noopStore) UpsertDriver(context.Context, string, float64, float64) error { return nil }
func (noopStore) UpsertMany(context.Context, []locations.DriverLocation) error { return nil }
func (noopStore) Nearby(context.Context, locations.Query) ([]locations.DriverLocation, error) {
	return nil, nil
}
func (noopStore) NearbyMany(context.Context, []locations.Query) ([][]locations.DriverLocation, error) {
	return nil, nil
}
func (noopStore) RemoveDriver(context.Context, string) error { return nil }
func (noopStore) Reap(context.Context) (int, error)          { return 0, nil }
func (noopStore) Count(context.Context) (int, error)         { return 0, nil }
func (noopStore) Close() error                               { return nil }
