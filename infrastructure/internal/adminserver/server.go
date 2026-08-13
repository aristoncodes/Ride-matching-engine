// Package adminserver exposes profiling and operational endpoints on a
// SEPARATE port from the public API.
//
// # Why a separate port
//
// pprof endpoints are a remote code-execution-adjacent liability and an
// information leak: /debug/pprof/heap dumps your memory layout,
// /debug/pprof/goroutine names every function you are executing, and
// /debug/pprof/profile will happily burn 30 seconds of CPU on request — a
// free denial-of-service for anyone who can reach it.
//
// Go's net/http/pprof registers itself on http.DefaultServeMux as an import
// side effect, so simply importing it in a service that also uses
// DefaultServeMux publishes all of that on the public port. That is one of the
// most common accidental exposures in Go services, and it is why this package
// builds its own mux and never touches the default one.
//
// The admin port is bound separately so it can be firewalled, bound to
// localhost, or exposed only inside the cluster — a decision the deployment
// makes, not the code.
package adminserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"runtime"
	"time"
)

// Config tunes the admin server.
type Config struct {
	// Addr is the listen address. Default ":6060", the conventional pprof port.
	Addr string

	// DisableProfiling hides /debug/pprof/*.
	//
	// INVERTED deliberately, so the zero value does the thing this package
	// exists to do. The first version was `EnableProfiling bool` documented as
	// "on by default" — but callers build Config{Addr: ..., ServiceName: ...}
	// without it, the zero value is false, and profiling was silently OFF. The
	// symptom was a 47-byte "profile" that turned out to be the index page,
	// which pprof then rejected as an unrecognised format.
	//
	// A bool whose zero value contradicts its documented default is a trap.
	// Either invert it, or make it a pointer so "unset" is distinguishable —
	// inverting is simpler here, and turning profiling off becomes an explicit
	// act rather than an omission.
	DisableProfiling bool

	Logger *slog.Logger

	// ServiceName labels the index page, so an operator who has port-forwarded
	// three admin ports can tell which is which.
	ServiceName string
}

// DefaultConfig returns sensible defaults.
func DefaultConfig(service string) Config {
	return Config{
		Addr:        ":6060",
		Logger:      slog.Default(),
		ServiceName: service,
	}
}

// Server is the admin HTTP server.
type Server struct {
	cfg  Config
	mux  *http.ServeMux
	http *http.Server
}

// New builds the server. Register extra handlers with Handle before Start.
func New(cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = DefaultConfig(cfg.ServiceName).Addr
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	// Our OWN mux, never http.DefaultServeMux — see the package doc. Importing
	// net/http/pprof registers on the default mux as a side effect, so a
	// service using the default mux for its API would publish pprof publicly
	// without a single line of code saying so.
	mux := http.NewServeMux()

	s := &Server{cfg: cfg, mux: mux}

	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// A cheap always-on snapshot. Reading it costs nothing, unlike a profile,
	// so it is the first thing to look at when something is wrong: a goroutine
	// count that climbs monotonically is a leak, and heap growth with a flat
	// object count is fragmentation rather than a leak.
	mux.HandleFunc("GET /debug/runtime", s.runtimeStats)

	if !cfg.DisableProfiling {
		// Registered EXPLICITLY rather than relying on the import side effect,
		// so it is visible in the code that these routes exist and on which mux.
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}

	s.http = &http.Server{
		Addr:    cfg.Addr,
		Handler: mux,
		// Deliberately long. A CPU profile is a 30-second streaming response by
		// default, and the usual 15s write timeout would truncate it — which
		// presents as a corrupt profile rather than a timeout, and wastes an
		// afternoon.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      6 * time.Minute,
	}
	return s
}

// Handle registers an extra endpoint (Week 23 mounts /metrics here).
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// Start begins serving in the background.
func (s *Server) Start() {
	go func() {
		s.cfg.Logger.Info("admin server listening",
			"addr", s.cfg.Addr, "service", s.cfg.ServiceName,
			"profiling", !s.cfg.DisableProfiling)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Never fatal. Losing the admin port must not take down the
			// service it exists to observe.
			s.cfg.Logger.Error("admin server stopped", "err", err)
		}
	}()
}

// Shutdown stops the server.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "%s — admin\n\n", s.cfg.ServiceName)
	fmt.Fprintf(w, "  /healthz\n  /debug/runtime\n")
	if !s.cfg.DisableProfiling {
		fmt.Fprintf(w, "  /debug/pprof/\n")
		fmt.Fprintf(w, "  /debug/pprof/profile?seconds=30   CPU\n")
		fmt.Fprintf(w, "  /debug/pprof/heap                 in-use memory\n")
		fmt.Fprintf(w, "  /debug/pprof/allocs               total allocations\n")
		fmt.Fprintf(w, "  /debug/pprof/goroutine            stacks (leak hunting)\n")
		fmt.Fprintf(w, "  /debug/pprof/block                blocking profile*\n")
		fmt.Fprintf(w, "  /debug/pprof/mutex                contention profile*\n\n")
		fmt.Fprintf(w, "  * block and mutex need SetBlockProfileRate / "+
			"SetMutexProfileFraction; see EnableContentionProfiling.\n")
	}
}

func (s *Server) runtimeStats(w http.ResponseWriter, _ *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "heap_alloc_bytes %d\n", m.HeapAlloc)
	fmt.Fprintf(w, "heap_objects %d\n", m.HeapObjects)
	fmt.Fprintf(w, "heap_sys_bytes %d\n", m.HeapSys)
	fmt.Fprintf(w, "stack_sys_bytes %d\n", m.StackSys)
	// TotalAlloc only ever grows: it is CUMULATIVE allocation, not current
	// usage. Its rate of change is the allocation pressure that drives GC, and
	// it is the number Week 22 tries to reduce.
	fmt.Fprintf(w, "total_alloc_bytes %d\n", m.TotalAlloc)
	fmt.Fprintf(w, "mallocs %d\n", m.Mallocs)
	fmt.Fprintf(w, "frees %d\n", m.Frees)
	fmt.Fprintf(w, "gc_cycles %d\n", m.NumGC)
	fmt.Fprintf(w, "gc_pause_total_ns %d\n", m.PauseTotalNs)
	fmt.Fprintf(w, "gc_cpu_fraction %f\n", m.GCCPUFraction)
	fmt.Fprintf(w, "next_gc_bytes %d\n", m.NextGC)
}

// EnableContentionProfiling turns on the block and mutex profilers.
//
// OFF by default, and that is not laziness: both add measurable overhead to
// every blocking operation and every mutex acquisition, which is exactly the
// hot path in this codebase. Turning them on changes the thing being measured,
// so they are enabled deliberately for a profiling run and left off otherwise.
//
// rate=1 records every event (accurate, slowest); larger values sample.
func EnableContentionProfiling(rate int) {
	if rate <= 0 {
		rate = 1
	}
	runtime.SetBlockProfileRate(rate)
	runtime.SetMutexProfileFraction(rate)
}
