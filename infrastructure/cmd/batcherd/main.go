// batcherd is the Match Batcher microservice (Week 12): it pops ride requests
// from the durable queue, aggregates them into windows, and hands each batch to
// the C++ engine.
//
// This is the process where the whole system converges — queue, driver
// locations, and the C++ solver — and the one that can be scaled horizontally:
// multiple instances share one consumer group, so each request goes to exactly
// one of them.
//
// Usage:
//
//	batcherd [--redis localhost:6379] [--engine localhost:50051] [--window 3s]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	matchingv1 "github.com/aditya/ride-matching/gen/matching/v1"
	"github.com/aditya/ride-matching/internal/batcher"
	"github.com/aditya/ride-matching/internal/engine"
	"github.com/aditya/ride-matching/internal/locations"
	"github.com/aditya/ride-matching/internal/queue"
)

func main() {
	var (
		redisAddr   = flag.String("redis", "localhost:6379", "Redis address or unix socket path")
		engineAddr  = flag.String("engine", "localhost:50051", "C++ matching engine gRPC address")
		metricsAddr = flag.String("metrics-addr", ":8082", "address for /healthz and /stats")
		tenant      = flag.String("tenant", "default", "tenant id")
		consumer    = flag.String("consumer", "", "unique consumer name (default: hostname-pid)")
		window      = flag.Duration("window", 3*time.Second, "batch window (ADR-0005)")
		maxBatch    = flag.Int("max-batch", 500, "flush early once this many requests accumulate")
		radius      = flag.Float64("radius-m", 5000, "candidate driver search radius, metres")
		candidates  = flag.Int("candidates", 8, "max candidate drivers per rider (k)")
		useRoads    = flag.Bool("travel-time", false, "price by road travel time instead of straight-line distance")
		graphID     = flag.String("graph", "blr-central", "road graph id (with --travel-time)")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// A unique consumer name per process is mandatory: two instances sharing
	// one would share a pending list, and each would treat the other's
	// in-flight work as abandoned and reclaim it out from under them.
	name := *consumer
	if name == "" {
		host, _ := os.Hostname()
		name = fmt.Sprintf("%s-%d", host, os.Getpid())
	}

	// ---- Dependencies ----------------------------------------------------
	qOpts := queue.DefaultStreamOptions()
	qOpts.TenantID = *tenant
	qOpts.Consumer = name

	q, err := queue.NewRedisStream(*redisAddr, qOpts)
	if err != nil {
		logger.Error("could not configure the queue", "err", err)
		os.Exit(1)
	}
	defer q.Close()

	locOpts := locations.DefaultOptions()
	locOpts.TenantID = *tenant
	drivers, err := locations.NewRedis(*redisAddr, locOpts)
	if err != nil {
		logger.Error("could not configure the driver store", "err", err)
		os.Exit(1)
	}
	defer drivers.Close()

	pingCtx, cancelPing := context.WithTimeout(context.Background(), 5*time.Second)
	if err := drivers.Ping(pingCtx); err != nil {
		cancelPing()
		logger.Error("cannot reach Redis", "addr", *redisAddr, "err", err)
		os.Exit(1)
	}
	cancelPing()

	eng, err := engine.Dial(*engineAddr)
	if err != nil {
		logger.Error("could not configure the engine client", "err", err)
		os.Exit(1)
	}
	defer eng.Close()

	// Probe the engine BEFORE consuming anything. This does two jobs, and the
	// second one was found by running the system rather than by reading it:
	//
	//  1. It surfaces a wrong address or a dead engine immediately, instead of
	//     one failed batch at a time.
	//  2. It WARMS THE CONNECTION. grpc.NewClient dials lazily, so without this
	//     the first RPC pays TCP setup plus the HTTP/2 handshake — measured at
	//     over a second on a loaded machine, against a SolveTimeout of
	//     window/2. The first batch after every start therefore timed out and
	//     was requeued, delaying those riders by a full reclaim cycle. The
	//     solve itself takes ~6 ms.
	//
	// Not fatal if it fails: the engine may simply be starting up alongside us,
	// and the retry path handles that correctly. The point is to pay the setup
	// cost here rather than inside a rider's batch.
	warmCtx, cancelWarm := context.WithTimeout(context.Background(), 10*time.Second)
	health, warmErr := eng.Health(warmCtx)
	cancelWarm()
	if warmErr != nil {
		logger.Warn("could not reach the matching engine at startup; "+
			"continuing and will retry per batch", "addr", *engineAddr, "err", warmErr)
	} else {
		logger.Info("matching engine reachable",
			"addr", *engineAddr, "version", health.GetVersion(),
			"graphs", len(health.GetLoadedGraphs()))
	}

	metric := matchingv1.CostMetric_COST_METRIC_EUCLIDEAN
	if *useRoads {
		metric = matchingv1.CostMetric_COST_METRIC_TRAVEL_TIME

		// A missing graph is fatal: every travel-time batch would fail
		// FAILED_PRECONDITION, which is not retryable, so every request would
		// be dead-lettered. Better to refuse to start.
		loaded := false
		for _, g := range health.GetLoadedGraphs() {
			if g.GetRoadGraphId() == *graphID {
				loaded = true
				break
			}
		}
		if warmErr == nil && !loaded {
			logger.Error("the engine does not have this road graph loaded",
				"graph", *graphID,
				"hint", "start matching_server with --graph "+*graphID+"=<path.osm>")
			os.Exit(1)
		}
	}

	// ---- Batcher ----------------------------------------------------------
	cfg := batcher.DefaultConfig()
	cfg.Window = *window
	cfg.MaxBatchSize = *maxBatch
	cfg.SearchRadiusMeters = *radius
	cfg.MaxCandidatesPerRider = *candidates
	cfg.CostMetric = metric
	cfg.RoadGraphID = *graphID
	cfg.SolveTimeout = *window / 2
	cfg.Logger = logger

	b, err := batcher.New(q, drivers, eng, cfg)
	if err != nil {
		logger.Error("could not configure the batcher", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := b.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("batcher stopped", "err", err)
		}
	}()

	// ---- Observability ----------------------------------------------------
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		reqCtx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()

		st := b.Stats()
		depth, _ := q.Depth(reqCtx)
		pending, _ := q.Pending(reqCtx)
		dead, _ := q.DeadLetterDepth(reqCtx)

		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w,
			"batches %d\nrequests %d\nmatched %d\nunmatched %d\nrequeued %d\n"+
				"dead_lettered %d\nsolve_errors %d\nqueue_depth %d\nqueue_pending %d\n"+
				"queue_dead_letter %d\n",
			st.Batches, st.Requests, st.Matched, st.Unmatched, st.Requeued,
			st.DeadLettered, st.SolveErrors, depth, pending, dead)
	})

	metricsSrv := &http.Server{
		Addr:              *metricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("batcherd running",
			"consumer", name, "window", *window, "max_batch", *maxBatch,
			"metric", metric.String(), "metrics_addr", *metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("metrics server failed", "err", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = metricsSrv.Shutdown(shutdownCtx)

	// Wait for the batcher's final batch. Anything it does not finish stays
	// unacked and is reclaimed by another instance, so nothing is lost either
	// way — but solving it now spares those riders a reclaim cycle.
	select {
	case <-b.Done():
	case <-shutdownCtx.Done():
		logger.Warn("batcher did not finish its final batch in time")
	}
	logger.Info("stopped cleanly", "stats", b.Stats())
}
