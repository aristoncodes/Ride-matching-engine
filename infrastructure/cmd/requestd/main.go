// requestd is the rider-facing front door (Week 11): it accepts ride requests
// over REST and puts them on the durable queue.
//
// Deliberately a separate process from the batcher. This one must stay
// available and fast even when matching is degraded — a rider should be able to
// submit a request while the engine is restarting, and find out later that it
// took a moment longer than usual. Coupling them would mean an engine outage
// became a front-door outage.
//
// Usage:
//
//	requestd [--addr :8081] [--redis localhost:6379] [--tenant default]
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

	"github.com/aditya/ride-matching/internal/adminserver"
	"github.com/aditya/ride-matching/internal/api"
	"github.com/aditya/ride-matching/internal/queue"
)

func main() {
	var (
		addr      = flag.String("addr", ":8081", "HTTP listen address")
		redisAddr = flag.String("redis", "localhost:6379", "Redis address or unix socket path")
		tenant    = flag.String("tenant", "default", "tenant id")
		maxLen    = flag.Int64("stream-maxlen", 1_000_000, "max ride requests retained in the stream")
	)
	adminAddr := flag.String("admin-addr", ":6061",
		"admin/pprof listen address (NEVER expose this publicly)")
	contentionProfile := flag.Bool("contention-profile", false,
		"enable block+mutex profiling; adds overhead, use only for a profiling run")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Profiling lives on its own port. See internal/adminserver: importing
	// net/http/pprof publishes it on http.DefaultServeMux as a side effect, so
	// keeping it on a separate mux AND a separate listener is what stops heap
	// dumps and 30-second CPU burns being reachable from the public API.
	if *contentionProfile {
		adminserver.EnableContentionProfiling(1)
		logger.Warn("contention profiling enabled; this adds overhead to every " +
			"block and mutex operation and changes what you are measuring")
	}
	admin := adminserver.New(adminserver.Config{
		Addr:        *adminAddr,
		ServiceName: "requestd",
		Logger:      logger,
	})
	admin.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = admin.Shutdown(ctx)
	}()

	qOpts := queue.DefaultStreamOptions()
	qOpts.TenantID = *tenant
	qOpts.MaxLen = *maxLen
	// The API only publishes, but the shared constructor requires a consumer
	// name. Naming it explicitly beats a blank that later looks like a bug.
	qOpts.Consumer = "requestd-producer"

	q, err := queue.NewRedisStream(*redisAddr, qOpts)
	if err != nil {
		logger.Error("could not configure the queue", "err", err)
		os.Exit(1)
	}
	defer q.Close()

	pingCtx, cancelPing := context.WithTimeout(context.Background(), 5*time.Second)
	if err := q.Ping(pingCtx); err != nil {
		cancelPing()
		logger.Error("cannot reach Redis", "addr", *redisAddr, "err", err)
		os.Exit(1)
	}
	cancelPing()

	cfg := api.DefaultConfig()
	cfg.TenantID = *tenant
	cfg.Logger = logger

	srv, err := api.NewServer(q, cfg)
	if err != nil {
		logger.Error("could not configure the API", "err", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: srv.Routes(),
		// A slow-loris client must not be able to hold a connection through the
		// header phase indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("requestd listening", "addr", *addr, "tenant", *tenant)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	// Graceful: in-flight requests finish, so a rider mid-submit is not dropped
	// on a deploy. The requests are already durable once published, so there is
	// nothing else to drain.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown", "err", err)
	}
	logger.Info("stopped cleanly", "stats", srv.Stats())
}
