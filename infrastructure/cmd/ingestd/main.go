// ingestd is the Week 9 pipeline as a runnable service:
//
//	drivers --WebSocket--> ingest --coalesce--> pipeline --3s window--> Redis
//
// It is the first process in this project that a person can actually start,
// point a load generator at, and watch. Run it alongside cmd/mockdrivers.
//
// Usage:
//
//	ingestd [--addr :8080] [--redis localhost:6379] [--window 3s]
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aditya/ride-matching/internal/ingest"
	"github.com/aditya/ride-matching/internal/locations"
	"github.com/aditya/ride-matching/internal/pipeline"
)

func main() {
	var (
		addr        = flag.String("addr", ":8080", "HTTP listen address")
		redisAddr   = flag.String("redis", "localhost:6379", "Redis address or unix socket path")
		tenant      = flag.String("tenant", "default", "tenant id (scopes the Redis keys)")
		window      = flag.Duration("window", 3*time.Second, "batch window (ADR-0005)")
		ttl         = flag.Duration("ttl", 30*time.Second, "how long a driver stays matchable after its last ping")
		maxConns    = flag.Int("max-conns", 10000, "maximum concurrent WebSocket connections")
		maxBuffered = flag.Int("max-buffered", 100000, "maximum distinct drivers buffered per window")
		reapEvery   = flag.Duration("reap-every", 30*time.Second, "how often to delete stale drivers")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// ---- Redis ----------------------------------------------------------
	storeOpts := locations.DefaultOptions()
	storeOpts.TenantID = *tenant
	storeOpts.TTL = *ttl

	store, err := locations.NewRedis(*redisAddr, storeOpts)
	if err != nil {
		logger.Error("could not configure the location store", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	// Verified at startup rather than discovered on the first ping: a
	// misconfigured address should be one clear message now, not a slow trickle
	// of failures under load.
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 5*time.Second)
	if err := store.Ping(pingCtx); err != nil {
		cancelPing()
		logger.Error("cannot reach Redis", "addr", *redisAddr, "err", err)
		os.Exit(1)
	}
	cancelPing()
	logger.Info("connected to Redis", "addr", *redisAddr, "tenant", *tenant, "ttl", *ttl)

	// ---- Wiring ---------------------------------------------------------
	// Signal-driven shutdown: SIGINT/SIGTERM cancel one root context, and every
	// component below stops from that single source rather than each inventing
	// its own shutdown path.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pipe, err := pipeline.New(store, pipeline.Config{
		Window:             *window,
		MaxBufferedDrivers: *maxBuffered,
		// Comfortably inside the window, so a slow Redis cannot let one flush
		// run into the next.
		FlushTimeout: *window / 2,
		Logger:       logger,
	})
	if err != nil {
		logger.Error("could not configure the pipeline", "err", err)
		os.Exit(1)
	}

	go func() {
		if err := pipe.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("pipeline stopped", "err", err)
		}
	}()

	reaperDone := store.StartReaper(ctx, *reapEvery, func(removed int, err error) {
		switch {
		case err != nil:
			logger.Warn("reap failed", "err", err)
		case removed > 0:
			logger.Info("reaped stale drivers", "count", removed)
		}
	})

	wsServer := ingest.NewServer(pipe, ingest.Config{
		MaxConnections: *maxConns,
		Logger:         logger,
	})

	mux := http.NewServeMux()
	mux.Handle("/v1/drivers/stream", wsServer.Handler())

	// Liveness only. Readiness belongs with the matcher, which is the component
	// that actually needs a loaded road graph.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// Plain-text counters. Real Prometheus wiring is Week 23; this exists so
	// the pipeline can be watched today rather than in four months.
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		ws := wsServer.Stats()
		ps := pipe.Stats()
		count, _ := store.Count(context.Background())
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(
			"connections_active " + itoa(ws.ActiveConnections) + "\n" +
				"connections_total " + itoa(ws.TotalConnections) + "\n" +
				"connections_rejected " + itoa(ws.RejectedConnections) + "\n" +
				"pings_received " + itoa(ws.PingsReceived) + "\n" +
				"pings_malformed " + itoa(ws.PingsRejected) + "\n" +
				"pipeline_shed " + itoa(ps.TotalShed) + "\n" +
				"pipeline_flushed " + itoa(ps.TotalFlushed) + "\n" +
				"pipeline_windows " + itoa(ps.TotalWindows) + "\n" +
				"pipeline_flush_failures " + itoa(ps.FlushFailures) + "\n" +
				"pipeline_buffered " + itoa(int64(ps.Buffered)) + "\n" +
				"drivers_indexed " + itoa(int64(count)) + "\n"))
	})

	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: mux,
		// A slow-loris client must not be able to hold a connection open
		// through the handshake forever. The WebSocket's own deadlines take
		// over after the upgrade.
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("ingestd listening",
			"addr", *addr, "window", *window, "max_conns", *maxConns)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	// Order matters, and it is the whole point of a graceful shutdown:
	//   1. stop accepting new connections;
	//   2. close the existing ones and WAIT for their goroutines;
	//   3. let the pipeline perform its final flush.
	// Draining before the last flush is what stops a clean exit from silently
	// discarding the final window of driver positions.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown", "err", err)
	}
	if err := wsServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("websocket shutdown", "err", err)
	}

	select {
	case <-pipe.Done():
	case <-shutdownCtx.Done():
		logger.Warn("pipeline did not finish its final flush in time")
	}
	<-reaperDone

	logger.Info("stopped cleanly", "stats", pipe.Stats())
}

// itoa avoids pulling strconv into the metrics path for one call site.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
