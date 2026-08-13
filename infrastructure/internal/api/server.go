// Package api is the rider-facing front door: the HTTP service that accepts
// ride requests and puts them on the durable queue.
//
// It is deliberately thin. It validates, assigns a request id, publishes, and
// returns 202 — it does not match, route, or talk to the C++ engine. That
// separation is what lets the matcher be restarted, scaled, or crash without
// riders seeing anything worse than a slightly longer wait.
//
// "Defensive from day one" is the week's requirement, and the shape it takes
// here is: every input is validated before it can reach the queue, every
// response has one documented envelope, every request carries a traceable id,
// and no client input can make the process panic.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aditya/ride-matching/internal/auth"
	"github.com/aditya/ride-matching/internal/metrics"
	"github.com/aditya/ride-matching/internal/queue"
)

// Config tunes the server.
type Config struct {
	// TenantID is the FALLBACK used only when no auth store is configured
	// (local development, and the Week 9-15 demos). When Keys is set, the
	// tenant comes from the authenticated API key instead and this is ignored.
	//
	// Week 19's isolation depends on never trusting a client-supplied tenant,
	// so there is deliberately no way to override it per request.
	TenantID string

	// Keys enables API-key authentication (Week 18). Nil disables auth
	// entirely, which is acceptable for local development and is logged loudly
	// at startup so it cannot ship unnoticed.
	Keys auth.Store

	// MaxBodyBytes caps a request body. A ride request is ~150 bytes; without
	// a cap a client can stream gigabytes into the decoder and the process
	// dies with no bug of its own.
	MaxBodyBytes int64

	// PublishTimeout bounds the queue write, so a slow Redis becomes a 503
	// rather than a hung request holding a connection open.
	PublishTimeout time.Duration

	Logger *slog.Logger

	// Now is injectable for tests.
	Now func() time.Time
}

// DefaultConfig returns production-shaped defaults.
func DefaultConfig() Config {
	return Config{
		TenantID:       "default",
		MaxBodyBytes:   16 * 1024,
		PublishTimeout: 2 * time.Second,
		Logger:         slog.Default(),
		Now:            time.Now,
	}
}

func (c *Config) applyDefaults() {
	d := DefaultConfig()
	if c.TenantID == "" {
		c.TenantID = d.TenantID
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = d.MaxBodyBytes
	}
	if c.PublishTimeout <= 0 {
		c.PublishTimeout = d.PublishTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Stats are cumulative counters.
type Stats struct {
	Accepted int64
	Rejected int64
	Failed   int64
	Panics   int64
}

// Server is the ride-request HTTP API.
type Server struct {
	cfg   Config
	queue queue.Queue

	accepted atomic.Int64
	rejected atomic.Int64
	failed   atomic.Int64
	panics   atomic.Int64
}

// NewServer creates the API. It does not listen; mount Routes() on a listener.
func NewServer(q queue.Queue, cfg Config) (*Server, error) {
	if q == nil {
		return nil, errors.New("api: queue must not be nil")
	}
	cfg.applyDefaults()
	return &Server{cfg: cfg, queue: q}, nil
}

// Stats snapshots the counters.
func (s *Server) Stats() Stats {
	return Stats{
		Accepted: s.accepted.Load(),
		Rejected: s.rejected.Load(),
		Failed:   s.failed.Load(),
		Panics:   s.panics.Load(),
	}
}

// ---- Wire types --------------------------------------------------------

type latLng struct {
	// Pointers, not bare float64s, so a MISSING field is distinguishable from
	// a legitimate 0. Without this, `{"lat": 0}` and `{}` decode identically —
	// and (0,0) is a real coordinate in the Gulf of Guinea, so the difference
	// between "the client forgot a field" and "the client is at Null Island"
	// would be silently lost.
	Lat *float64 `json:"lat"`
	Lng *float64 `json:"lng"`
}

type rideRequestInput struct {
	RiderID string  `json:"rider_id"`
	Pickup  *latLng `json:"pickup"`
}

type rideRequestAccepted struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
}

// ---- Routing -----------------------------------------------------------

// Routes returns the mounted handler, middleware included.
//
// Order matters and is chosen deliberately: recovery is outermost so it catches
// panics from everything inside it, including the request-id middleware itself.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Go 1.22+ patterns include the method, so a GET to a POST-only route
	// yields 405 from the router rather than falling through to a handler that
	// has to check the method itself.
	mux.HandleFunc("POST /v1/ride-requests", s.handleCreateRideRequest)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /stats", s.handleStats)

	// Anything unmatched gets the same structured envelope as everything else,
	// instead of Go's default plain-text 404.
	mux.HandleFunc("/", s.handleNotFound)

	handler := http.Handler(mux)

	// Auth sits INSIDE request-id and logging so that a rejected request still
	// gets an id and a log line — an operator debugging "my key stopped
	// working" needs both. It sits OUTSIDE the routes so no handler can ever
	// run unauthenticated.
	if s.cfg.Keys != nil {
		handler = auth.Middleware(auth.Options{
			Store:  s.cfg.Keys,
			Logger: s.cfg.Logger,
			Now:    s.cfg.Now,
			WriteError: func(w http.ResponseWriter, status int, code, message string, r *http.Request) {
				writeError(w, status, code, message, "", RequestIDFrom(r.Context()))
			},
			// Probes have no API key. Requiring one would make every pod
			// permanently unready — an outage caused by the auth layer.
			SkipPaths: []string{"/healthz", "/readyz"},
		})(handler)
	}

	return s.recoverPanics(s.withRequestID(s.logRequests(handler)))
}

// ---- Middleware --------------------------------------------------------

type contextKey string

const requestIDKey contextKey = "request_id"

// RequestIDFrom returns the request id attached to a request's context.
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// withRequestID attaches a trace id to every request.
//
// An inbound X-Request-ID is honoured so a trace can span services — the
// batcher's log line for a batch should be findable from the rider's original
// request. If absent, one is generated. Inbound ids are length-capped and
// sanitised, because they end up in logs and headers and an unbounded
// client-controlled string in a log line is a log-injection vector.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitiseRequestID(r.Header.Get("X-Request-ID"))
		if id == "" {
			id = newRequestID()
		}
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Even a failed CSPRNG must not break request handling; a timestamp is
		// a poor id but an unusable API is worse.
		return fmt.Sprintf("req_ts_%d", time.Now().UnixNano())
	}
	return "req_" + hex.EncodeToString(b[:])
}

func sanitiseRequestID(raw string) string {
	if raw == "" {
		return ""
	}
	if len(raw) > 64 {
		raw = raw[:64]
	}
	// Keep only characters that are safe in a header and a log line. Newlines
	// in particular would let a client forge log entries.
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// logRequests emits one structured line per request.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Route, not raw path: a path label would create one series per
		// request id and blow up cardinality.
		metrics.HTTPRequestsTotal.WithLabelValues(routeLabel(r), metrics.StatusClass(rec.status)).Inc()

		s.cfg.Logger.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(started).Milliseconds(),
			"request_id", RequestIDFrom(r.Context()))
	})
}

// statusRecorder captures the status code, which net/http does not expose.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// recoverPanics turns a panic into a 500 with a request id.
//
// Without this, a panic in a handler kills the whole process — net/http
// recovers the goroutine but the connection dies abruptly and the client sees a
// dropped connection rather than an error. The week's checkpoint is "a
// malformed request gets a clear 4xx, not a 500"; this is the backstop that
// makes even an unforeseen 500 diagnosable rather than silent.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.panics.Add(1)
				requestID := RequestIDFrom(r.Context())
				s.cfg.Logger.Error("panic in handler",
					"panic", rec, "path", r.URL.Path, "request_id", requestID)
				writeError(w, http.StatusInternalServerError, CodeInternal,
					"internal error", "", requestID)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---- Handlers ----------------------------------------------------------

func (s *Server) handleCreateRideRequest(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := RequestIDFrom(r.Context())

	// Bound the body BEFORE decoding. MaxBytesReader makes the decoder itself
	// fail at the limit, so a huge body is never fully buffered.
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)

	var input rideRequestInput
	dec := json.NewDecoder(r.Body)
	// Unknown fields are an error rather than silently ignored: a client that
	// sends `{"pickupp": ...}` has a bug, and failing loudly finds it during
	// their integration instead of in production as a mysteriously absent field.
	dec.DisallowUnknownFields()

	if err := dec.Decode(&input); err != nil {
		s.rejected.Add(1)
		code, message, field := describeDecodeError(err, s.cfg.MaxBodyBytes)
		status := http.StatusBadRequest
		if code == CodePayloadTooLarge {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, code, message, field, requestID)
		return
	}

	// Exactly one JSON value per request. Trailing content usually means a
	// client is concatenating objects and expecting them all to be accepted.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		s.rejected.Add(1)
		writeError(w, http.StatusBadRequest, CodeMalformedJSON,
			"body must contain exactly one JSON object", "", requestID)
		return
	}

	if field, message := validate(input); field != "" {
		s.rejected.Add(1)
		writeError(w, http.StatusBadRequest, CodeInvalidArgument, message, field, requestID)
		return
	}

	// The tenant comes from the AUTHENTICATED key, never from the request.
	// This single line is where Week 19's isolation is actually enforced for
	// the write path: a client cannot address another tenant's queue because it
	// has no way to say which tenant it is.
	tenantID := s.cfg.TenantID
	if authenticated, ok := auth.TenantFromContext(r.Context()); ok {
		tenantID = authenticated
	}

	// The request id doubles as the queue's idempotency key, so a redelivered
	// message is traceable all the way back to this HTTP call.
	req := queue.RideRequest{
		RequestID:   requestID,
		TenantID:    tenantID,
		RiderID:     input.RiderID,
		Lat:         *input.Pickup.Lat,
		Lng:         *input.Pickup.Lng,
		RequestedAt: s.cfg.Now(),
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.PublishTimeout)
	defer cancel()

	if _, err := s.queue.Publish(ctx, req); err != nil {
		// 503, not 500: the request was valid and the failure is ours and
		// probably transient, so the client should retry rather than fix
		// anything. Retry-After makes that machine-readable.
		s.failed.Add(1)
		s.cfg.Logger.Error("could not enqueue ride request",
			"err", err, "request_id", requestID)
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable,
			"could not accept the request right now; please retry", "", requestID)
		return
	}

	// 202, not 201: nothing has been created yet. The request is queued and
	// will be matched in a later window, and claiming otherwise would be a lie
	// the client might act on.
	s.accepted.Add(1)
	metrics.RideRequestsTotal.WithLabelValues(tenantID, "accepted").Inc()
	metrics.RequestAcceptSeconds.WithLabelValues(tenantID).Observe(time.Since(started).Seconds())
	writeJSON(w, http.StatusAccepted, requestID, rideRequestAccepted{
		RequestID: requestID,
		Status:    "PENDING",
	})
}

// describeDecodeError turns a decoder error into an actionable message.
//
// Go's raw JSON errors ("json: cannot unmarshal string into Go struct field
// ... of type float64") leak internal type names and read as gibberish to
// someone integrating against an HTTP API.
func describeDecodeError(err error, maxBytes int64) (code, message, field string) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return CodePayloadTooLarge,
			fmt.Sprintf("request body must not exceed %d bytes", maxBytes), ""
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return CodeInvalidArgument,
			fmt.Sprintf("field %q must be of type %s", typeErr.Field, typeErr.Type.String()),
			typeErr.Field
	}

	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return CodeMalformedJSON,
			fmt.Sprintf("invalid JSON at byte offset %d", syntaxErr.Offset), ""
	}

	if errors.Is(err, io.EOF) {
		return CodeMalformedJSON, "request body is empty", ""
	}

	// DisallowUnknownFields reports as a plain error; surface the field name.
	if msg := err.Error(); strings.HasPrefix(msg, "json: unknown field ") {
		name := strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`)
		return CodeInvalidArgument, fmt.Sprintf("unknown field %q", name), name
	}

	return CodeMalformedJSON, "request body is not valid JSON", ""
}

// validate returns the offending field and a message, or "" if the input is
// acceptable. Every branch names what was expected, not merely what was wrong.
func validate(in rideRequestInput) (field, message string) {
	if in.RiderID == "" {
		return "rider_id", "rider_id is required"
	}
	if len(in.RiderID) > 128 {
		return "rider_id", "rider_id must be at most 128 characters"
	}
	if in.Pickup == nil {
		return "pickup", "pickup is required"
	}
	if in.Pickup.Lat == nil {
		return "pickup.lat", "pickup.lat is required"
	}
	if in.Pickup.Lng == nil {
		return "pickup.lng", "pickup.lng is required"
	}

	lat, lng := *in.Pickup.Lat, *in.Pickup.Lng
	// NaN and ±Inf are valid JSON numbers to some encoders and would otherwise
	// flow all the way into the geo index and the solver, where they produce
	// nonsense rather than an error.
	if math.IsNaN(lat) || math.IsInf(lat, 0) || math.IsNaN(lng) || math.IsInf(lng, 0) {
		return "pickup", "pickup coordinates must be finite numbers"
	}
	if lat < -90 || lat > 90 {
		return "pickup.lat", "pickup.lat must be between -90 and 90"
	}
	if lng < -180 || lng > 180 {
		return "pickup.lng", "pickup.lng must be between -180 and 180"
	}
	return "", ""
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Liveness: is this process running? Deliberately does NOT check Redis —
	// a liveness probe that fails on a dependency outage gets the process
	// killed and restarted, which fixes nothing and removes capacity exactly
	// when the system is already degraded.
	writeJSON(w, http.StatusOK, RequestIDFrom(r.Context()),
		map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	requestID := RequestIDFrom(r.Context())

	// Readiness: can this instance do its job? It cannot accept requests
	// without a reachable queue, so this DOES check the dependency — a failing
	// readiness probe removes the instance from the load balancer without
	// killing it, which is the correct response to a dependency outage.
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()

	if _, err := s.queue.Depth(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable,
			"queue is not reachable", "", requestID)
		return
	}
	writeJSON(w, http.StatusOK, requestID, map[string]string{"status": "ready"})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()

	depth, _ := s.queue.Depth(ctx)
	pending, _ := s.queue.Pending(ctx)
	dead, _ := s.queue.DeadLetterDepth(ctx)
	st := s.Stats()

	writeJSON(w, http.StatusOK, RequestIDFrom(r.Context()), map[string]int64{
		"accepted":          st.Accepted,
		"rejected":          st.Rejected,
		"failed":            st.Failed,
		"panics":            st.Panics,
		"queue_depth":       depth,
		"queue_pending":     pending,
		"queue_dead_letter": dead,
	})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	requestID := RequestIDFrom(r.Context())
	writeError(w, http.StatusNotFound, CodeNotFound,
		fmt.Sprintf("no route for %s %s", r.Method, r.URL.Path), "", requestID)
}

// routeLabel maps a request to a BOUNDED label.
//
// Using r.URL.Path directly would be a cardinality bomb the moment a path
// contains an id: every distinct URL becomes its own Prometheus time series,
// and a monitoring system dies of that long before it helps you.
func routeLabel(r *http.Request) string {
	switch r.URL.Path {
	case "/v1/ride-requests", "/healthz", "/readyz", "/stats":
		return r.Method + " " + r.URL.Path
	default:
		return r.Method + " other"
	}
}
