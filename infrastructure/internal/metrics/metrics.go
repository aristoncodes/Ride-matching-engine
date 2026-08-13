// Package metrics defines the system's Prometheus instrumentation and the
// SLOs it is held to.
//
// # Why the SLOs live here, next to the metrics
//
// An SLO written in a document drifts from the system within a month. Defined
// beside the metric that measures it, the two cannot disagree — and the
// alerting rules in docs/Observability.md are written from these same names.
//
// # What is deliberately NOT measured
//
// There is no per-rider or per-driver label anywhere. Prometheus creates one
// time series per label combination, so a driver_id label on a fleet of 10,000
// is 10,000 series per metric — a cardinality explosion that kills the
// monitoring system long before it helps you. Tenant is bounded (tens of B2B
// customers) and is therefore safe; entity ids are not.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// The SLOs. These are the promises, and every number is chosen against
// something real rather than picked because it is round.
const (
	// SLOIngestP99 — a driver's GPS ping is accepted within this. Measured at
	// p50 405us / p99 8.35ms in Week 15, so 50ms is generous headroom; the ping
	// path must never be the thing that falls over under a spike.
	SLOIngestP99 = 50 * time.Millisecond

	// SLORequestAcceptP99 — the rider-facing front door. Validate, assign an
	// id, publish, return 202. Week 15 measured p99 8.35ms at 10k concurrent
	// drivers; 100ms leaves room for a loaded cluster without being meaningless.
	SLORequestAcceptP99 = 100 * time.Millisecond

	// SLOMatchLatencyP99 — from accepting a ride request to matching it.
	//
	// Bounded by the BATCH WINDOW, not by compute: a request arriving just
	// after a flush waits nearly a full window before it is even considered.
	// So the floor is ~3s (ADR-0005) and the SLO must exceed it. Quoting a
	// sub-millisecond number here would be quoting the solve time and calling
	// it the rider's experience.
	SLOMatchLatencyP99 = 5 * time.Second

	// SLOMatchRate — the fraction of requests that get a driver. This is the
	// one that says whether the system is doing its JOB, as distinct from
	// doing it quickly. It is easy to build something with beautiful latency
	// that matches nobody.
	SLOMatchRate = 0.95

	// SLOAvailability — successful (non-5xx) responses. 99.9% is ~43 minutes
	// of error budget per month.
	SLOAvailability = 0.999
)

var (
	// ---- Ingestion ------------------------------------------------------
	DriverPingsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ridematch_driver_pings_total",
		Help: "GPS pings received from drivers.",
	}, []string{"tenant", "result"}) // result: accepted | malformed | shed

	ActiveConnections = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ridematch_websocket_connections",
		Help: "Currently open driver WebSocket connections.",
	}, []string{"tenant"})

	// ---- Ride requests ---------------------------------------------------
	RideRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ridematch_ride_requests_total",
		Help: "Ride requests received at the API.",
	}, []string{"tenant", "result"}) // result: accepted | rejected | failed

	RequestAcceptSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "ridematch_request_accept_seconds",
		Help: "Time to validate and enqueue a ride request.",
		// Buckets straddle the SLO (0.1s) rather than being evenly spaced.
		// Prometheus computes quantiles by interpolating WITHIN a bucket, so a
		// bucket boundary at the SLO is what makes "are we meeting it?"
		// answerable rather than approximate.
		Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"tenant"})

	// ---- Matching --------------------------------------------------------
	BatchesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ridematch_batches_total",
		Help: "Batches processed, by flush reason.",
	}, []string{"tenant", "reason"}) // reason: timer | size | shutdown

	BatchSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ridematch_batch_size_riders",
		Help:    "Riders per batch.",
		Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500},
	}, []string{"tenant"})

	SolveSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "ridematch_solve_seconds",
		Help: "Time inside the C++ engine for one batch.",
		// Fine-grained at the bottom: Week 15 measured 0.4ms at N=M=100 and
		// 328ms at N=3200, so the interesting range spans three orders of
		// magnitude.
		Buckets: []float64{.0005, .001, .005, .01, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"tenant", "metric"}) // metric: euclidean | travel_time

	MatchLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "ridematch_match_latency_seconds",
		Help: "Accepted-to-matched latency, as the rider experiences it.",
		// Starts at 1s because the batch window makes anything faster
		// impossible; a bucket at 3s and 5s brackets the window and the SLO.
		Buckets: []float64{.5, 1, 2, 3, 4, 5, 7.5, 10, 20, 30},
	}, []string{"tenant"})

	RidersTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ridematch_riders_total",
		Help: "Rider outcomes.",
	}, []string{"tenant", "outcome"}) // outcome: matched | requeued | exhausted | dead_lettered

	// ---- Queue health ----------------------------------------------------
	QueueDepth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ridematch_queue_depth",
		Help: "Ride requests waiting to be delivered to a batcher.",
	}, []string{"tenant"})

	QueuePending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ridematch_queue_pending",
		Help: "Requests claimed by a consumer but not yet acked.",
	}, []string{"tenant"})

	QueueDeadLettered = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ridematch_queue_dead_lettered",
		Help: "Requests set aside permanently. Any sustained increase should page someone.",
	}, []string{"tenant"})

	// ---- Dependencies ----------------------------------------------------
	DependencyErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ridematch_dependency_errors_total",
		Help: "Errors talking to a dependency, by whether a retry can help.",
	}, []string{"dependency", "retryable"}) // dependency: redis | engine

	DriversIndexed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ridematch_drivers_indexed",
		Help: "Drivers currently in the live location index.",
	}, []string{"tenant"})

	// ---- HTTP ------------------------------------------------------------
	HTTPRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ridematch_http_requests_total",
		Help: "HTTP requests by route and status class.",
		// STATUS CLASS (2xx/4xx/5xx), not the exact code. Exact codes multiply
		// series for no operational gain — nobody alerts on 418 specifically,
		// and the class is what an availability SLO is computed from.
	}, []string{"route", "status_class"})
)

// Registry holds this process's collectors.
//
// A custom registry rather than the global default, for the same reason the
// admin server uses its own mux: the default registry collects whatever any
// imported library decided to register, which is neither predictable nor
// reviewable.
func Registry() *prometheus.Registry {
	reg := prometheus.NewRegistry()

	// Go runtime and process collectors. These give GC pause, goroutine count
	// and RSS for free, and they are how the Week 22 GC work is observed in
	// production rather than only in a profile.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	reg.MustRegister(
		DriverPingsTotal, ActiveConnections,
		RideRequestsTotal, RequestAcceptSeconds,
		BatchesTotal, BatchSize, SolveSeconds, MatchLatencySeconds, RidersTotal,
		QueueDepth, QueuePending, QueueDeadLettered,
		DependencyErrorsTotal, DriversIndexed,
		HTTPRequestsTotal,
	)
	return reg
}

// Handler returns the /metrics endpoint for a registry.
func Handler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		// One scrape failing should not fail the whole endpoint — partial
		// metrics beat none when you are trying to diagnose an incident.
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// StatusClass reduces a status code to its class, keeping cardinality bounded.
func StatusClass(code int) string {
	switch {
	case code < 200:
		return "1xx"
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}
