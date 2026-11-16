package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aditya/ride-matching/internal/api"
	"github.com/aditya/ride-matching/internal/queue"
	"github.com/aditya/ride-matching/internal/results"
)

// fakeQueue is an in-memory Queue. The API's job is validation and error
// shape, not durability, so a real Redis here would only add latency to
// assertions about HTTP status codes.
type fakeQueue struct {
	mu        sync.Mutex
	published []queue.RideRequest
	failWith  error
	delay     time.Duration
}

func (f *fakeQueue) Publish(ctx context.Context, req queue.RideRequest) (string, error) {
	f.mu.Lock()
	delay, failWith := f.delay, f.failWith
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if failWith != nil {
		return "", failWith
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, req)
	return "1-0", nil
}

func (f *fakeQueue) Consume(context.Context, int, time.Duration) ([]queue.Message, error) {
	return nil, nil
}
func (f *fakeQueue) Ack(context.Context, ...string) error { return nil }
func (f *fakeQueue) Reclaim(context.Context, time.Duration, int) ([]queue.Message, error) {
	return nil, nil
}
func (f *fakeQueue) DeadLetter(context.Context, queue.Message, string) error { return nil }
func (f *fakeQueue) Republish(context.Context, queue.Message) error          { return nil }
func (f *fakeQueue) Pending(context.Context) (int64, error)                  { return 0, nil }
func (f *fakeQueue) DeadLetterDepth(context.Context) (int64, error)          { return 0, nil }
func (f *fakeQueue) Close() error                                            { return nil }

func (f *fakeQueue) Depth(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return 0, f.failWith
	}
	return int64(len(f.published)), nil
}

func (f *fakeQueue) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.published)
}

func (f *fakeQueue) last() queue.RideRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.published[len(f.published)-1]
}

func (f *fakeQueue) setFailure(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = err
}

func newTestServer(t *testing.T) (*api.Server, *fakeQueue, http.Handler) {
	t.Helper()
	srv, q, _, h := newTestServerWithResults(t)
	return srv, q, h
}

// newTestServerWithResults is the same server plus a handle on its outcome
// store. A results store is wired in for EVERY test, not just the ones that
// read it back, so the pending-write the POST path now performs is exercised
// by the whole suite rather than only where it is asserted.
func newTestServerWithResults(t *testing.T) (*api.Server, *fakeQueue, *results.MemoryStore, http.Handler) {
	t.Helper()
	q := &fakeQueue{}
	store := results.NewMemory()

	cfg := api.DefaultConfig()
	cfg.TenantID = "test"
	cfg.Results = store
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := api.NewServer(q, cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, q, store, srv.Routes()
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func post(t *testing.T, h http.Handler, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeError parses the error envelope, failing the test if the response is
// not the documented shape. This is itself an assertion: every non-2xx must be
// parseable the same way.
func decodeError(t *testing.T, rec *httptest.ResponseRecorder) api.ErrorBody {
	t.Helper()
	var body api.APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a structured error (%q): %v", rec.Body.String(), err)
	}
	if body.Error.Code == "" {
		t.Errorf("error has no machine-readable code: %s", rec.Body.String())
	}
	if body.Error.RequestID == "" {
		t.Errorf("error has no request_id — untraceable: %s", rec.Body.String())
	}
	return body.Error
}

func TestAcceptsValidRideRequest(t *testing.T) {
	_, q, h := newTestServer(t)

	rec := post(t, h, "/v1/ride-requests",
		`{"rider_id":"R-1","pickup":{"lat":12.9716,"lng":77.5946}}`, nil)

	// 202, not 201: nothing is created yet, it is queued for a later window.
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		RequestID string `json:"request_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.RequestID == "" {
		t.Error("no request_id returned")
	}
	if body.Status != "PENDING" {
		t.Errorf("status = %q, want PENDING", body.Status)
	}
	if rec.Header().Get("X-Request-ID") != body.RequestID {
		t.Error("X-Request-ID header and body request_id disagree")
	}

	if q.count() != 1 {
		t.Fatalf("published %d requests, want 1", q.count())
	}
	published := q.last()
	if published.RiderID != "R-1" || published.Lat != 12.9716 {
		t.Errorf("published payload is wrong: %+v", published)
	}
	// The HTTP request id IS the queue idempotency key, so a redelivered
	// message is traceable back to this call.
	if published.RequestID != body.RequestID {
		t.Error("queue RequestID does not match the HTTP request_id")
	}
	if published.RequestedAt.IsZero() {
		t.Error("RequestedAt was not stamped by the server")
	}
}

// TestMalformedRequestsGetClear4xxWithRequestID is the Week 11 checkpoint.
//
// Every one of these must be a 4xx with a machine-readable code and a request
// id — never a 500, never a bare string, never a Go type name leaked from the
// JSON decoder.
func TestMalformedRequestsGetClear4xxWithRequestID(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
		wantField  string
	}{
		{"empty body", ``, 400, api.CodeMalformedJSON, ""},
		{"not json", `hello`, 400, api.CodeMalformedJSON, ""},
		{"truncated json", `{"rider_id":`, 400, api.CodeMalformedJSON, ""},
		{"missing rider_id", `{"pickup":{"lat":12.9,"lng":77.5}}`, 400, api.CodeInvalidArgument, "rider_id"},
		{"missing pickup", `{"rider_id":"R-1"}`, 400, api.CodeInvalidArgument, "pickup"},
		{"missing lat", `{"rider_id":"R-1","pickup":{"lng":77.5}}`, 400, api.CodeInvalidArgument, "pickup.lat"},
		{"missing lng", `{"rider_id":"R-1","pickup":{"lat":12.9}}`, 400, api.CodeInvalidArgument, "pickup.lng"},
		{"lat out of range", `{"rider_id":"R-1","pickup":{"lat":91,"lng":77.5}}`, 400, api.CodeInvalidArgument, "pickup.lat"},
		{"lng out of range", `{"rider_id":"R-1","pickup":{"lat":12.9,"lng":181}}`, 400, api.CodeInvalidArgument, "pickup.lng"},
		{"wrong type", `{"rider_id":"R-1","pickup":{"lat":"north","lng":77.5}}`, 400, api.CodeInvalidArgument, ""},
		{"unknown field", `{"rider_id":"R-1","pickupp":{"lat":12.9,"lng":77.5}}`, 400, api.CodeInvalidArgument, ""},
		{"two objects", `{"rider_id":"R-1","pickup":{"lat":12.9,"lng":77.5}}{"x":1}`, 400, api.CodeMalformedJSON, ""},
		{"empty rider_id", `{"rider_id":"","pickup":{"lat":12.9,"lng":77.5}}`, 400, api.CodeInvalidArgument, "rider_id"},
		{"oversized rider_id", `{"rider_id":"` + strings.Repeat("x", 200) + `","pickup":{"lat":12.9,"lng":77.5}}`, 400, api.CodeInvalidArgument, "rider_id"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, q, h := newTestServer(t)
			rec := post(t, h, "/v1/ride-requests", tc.body, nil)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d. body: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			// The heart of the checkpoint: a bad request must never be a 500.
			if rec.Code >= 500 {
				t.Fatalf("a malformed request produced a %d — client errors must be 4xx", rec.Code)
			}

			errBody := decodeError(t, rec)
			if tc.wantCode != "" && errBody.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", errBody.Code, tc.wantCode)
			}
			if tc.wantField != "" && errBody.Field != tc.wantField {
				t.Errorf("field = %q, want %q", errBody.Field, tc.wantField)
			}
			if errBody.Message == "" {
				t.Error("error message is empty")
			}
			// Nothing invalid may reach the queue.
			if q.count() != 0 {
				t.Errorf("an invalid request was published to the queue")
			}
		})
	}
}

func TestNonFiniteCoordinatesRejected(t *testing.T) {
	// Go's encoding/json rejects bare NaN, but a client can send a huge float
	// literal that decodes to +Inf. Left unchecked it flows into the geo index
	// and the solver, where it produces nonsense rather than an error.
	_, q, h := newTestServer(t)

	rec := post(t, h, "/v1/ride-requests",
		`{"rider_id":"R-1","pickup":{"lat":1e400,"lng":77.5}}`, nil)

	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("status = %d, want a 4xx. body: %s", rec.Code, rec.Body.String())
	}
	if q.count() != 0 {
		t.Error("a non-finite coordinate reached the queue")
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	_, q, h := newTestServer(t)

	// Well past the 16 KB cap. MaxBytesReader makes the decoder fail at the
	// limit, so this is never fully buffered.
	huge := `{"rider_id":"` + strings.Repeat("x", 64*1024) + `","pickup":{"lat":12.9,"lng":77.5}}`
	rec := post(t, h, "/v1/ride-requests", huge, nil)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413. body: %s", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Code; got != api.CodePayloadTooLarge {
		t.Errorf("code = %q, want %q", got, api.CodePayloadTooLarge)
	}
	if q.count() != 0 {
		t.Error("an oversized request was published")
	}
}

func TestInboundRequestIDIsHonouredAndSanitised(t *testing.T) {
	_, _, h := newTestServer(t)

	t.Run("a clean id is propagated so traces span services", func(t *testing.T) {
		rec := post(t, h, "/v1/ride-requests",
			`{"rider_id":"R-1","pickup":{"lat":12.9,"lng":77.5}}`,
			map[string]string{"X-Request-ID": "trace-abc-123"})
		if got := rec.Header().Get("X-Request-ID"); got != "trace-abc-123" {
			t.Errorf("X-Request-ID = %q, want the inbound value", got)
		}
	})

	t.Run("a hostile id cannot inject into logs or headers", func(t *testing.T) {
		rec := post(t, h, "/v1/ride-requests",
			`{"rider_id":"R-1","pickup":{"lat":12.9,"lng":77.5}}`,
			map[string]string{"X-Request-ID": "evil\r\nX-Injected: yes"})

		got := rec.Header().Get("X-Request-ID")
		if strings.ContainsAny(got, "\r\n") {
			t.Fatalf("request id %q still contains CRLF — header/log injection", got)
		}
		if rec.Header().Get("X-Injected") != "" {
			t.Fatal("a header was injected via X-Request-ID")
		}
	})

	t.Run("an over-long id is truncated", func(t *testing.T) {
		rec := post(t, h, "/v1/ride-requests",
			`{"rider_id":"R-1","pickup":{"lat":12.9,"lng":77.5}}`,
			map[string]string{"X-Request-ID": strings.Repeat("a", 500)})
		if got := len(rec.Header().Get("X-Request-ID")); got > 64 {
			t.Errorf("request id length = %d, want <= 64", got)
		}
	})
}

func TestRequestIDsAreUniquePerRequest(t *testing.T) {
	_, _, h := newTestServer(t)

	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		rec := post(t, h, "/v1/ride-requests",
			`{"rider_id":"R-1","pickup":{"lat":12.9,"lng":77.5}}`, nil)
		id := rec.Header().Get("X-Request-ID")
		if seen[id] {
			t.Fatalf("duplicate request id %q — ids must be unique to be traceable", id)
		}
		seen[id] = true
	}
}

func TestQueueFailureIs503NotCrashOr500(t *testing.T) {
	// The request was valid; the failure is ours and probably transient. A 503
	// with Retry-After tells the client to retry rather than to fix its input.
	_, q, h := newTestServer(t)
	q.setFailure(errors.New("redis is down"))

	rec := post(t, h, "/v1/ride-requests",
		`{"rider_id":"R-1","pickup":{"lat":12.9,"lng":77.5}}`, nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503. body: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 503 should tell the client when to retry")
	}

	errBody := decodeError(t, rec)
	if errBody.Code != api.CodeUnavailable {
		t.Errorf("code = %q, want %q", errBody.Code, api.CodeUnavailable)
	}
	// The internal error string must not leak to the client.
	if strings.Contains(strings.ToLower(errBody.Message), "redis") {
		t.Errorf("internal detail leaked to the client: %q", errBody.Message)
	}
}

func TestWrongMethodAndUnknownRouteAreStructured(t *testing.T) {
	_, _, h := newTestServer(t)

	t.Run("unknown route", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/nope", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		// Go's default 404 is plain text; ours must be the same envelope as
		// every other error so clients need only one parser.
		if got := decodeError(t, rec).Code; got != api.CodeNotFound {
			t.Errorf("code = %q, want %q", got, api.CodeNotFound)
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/ride-requests", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 405 or 404", rec.Code)
		}
	})
}

func TestHealthAndReadinessDifferOnDependencyFailure(t *testing.T) {
	// The distinction matters operationally: a failing LIVENESS probe kills and
	// restarts the process, which does nothing for a Redis outage except remove
	// capacity. A failing READINESS probe pulls the instance out of the load
	// balancer, which is the correct response.
	_, q, h := newTestServer(t)

	healthy := httptest.NewRecorder()
	h.ServeHTTP(healthy, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if healthy.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", healthy.Code)
	}

	ready := httptest.NewRecorder()
	h.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("readyz = %d, want 200 while healthy", ready.Code)
	}

	q.setFailure(errors.New("redis is down"))

	stillLive := httptest.NewRecorder()
	h.ServeHTTP(stillLive, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if stillLive.Code != http.StatusOK {
		t.Error("liveness must NOT fail on a dependency outage — restarting fixes nothing")
	}

	notReady := httptest.NewRecorder()
	h.ServeHTTP(notReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if notReady.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz = %d, want 503 when the queue is unreachable", notReady.Code)
	}
}

func TestPublishTimeoutBecomes503(t *testing.T) {
	q := &fakeQueue{delay: 2 * time.Second}
	cfg := api.DefaultConfig()
	cfg.PublishTimeout = 50 * time.Millisecond
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := api.NewServer(q, cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	start := time.Now()
	rec := post(t, srv.Routes(), "/v1/ride-requests",
		`{"rider_id":"R-1","pickup":{"lat":12.9,"lng":77.5}}`, nil)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if elapsed > time.Second {
		t.Fatalf("took %v — the publish timeout is not being enforced", elapsed)
	}
}

func TestConcurrentRequestsAreSafe(t *testing.T) {
	_, q, h := newTestServer(t)

	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(t, h, "/v1/ride-requests",
				`{"rider_id":"R-1","pickup":{"lat":12.9,"lng":77.5}}`, nil)
		}()
	}
	wg.Wait()

	if q.count() != n {
		t.Errorf("published %d, want %d", q.count(), n)
	}
}

func TestNilQueueIsRejected(t *testing.T) {
	if _, err := api.NewServer(nil, api.DefaultConfig()); err == nil {
		t.Error("expected an error for a nil queue")
	}
}

// ---- GET /v1/ride-requests/{id} ----------------------------------------

// decodeStatus parses the status envelope as a client would.
func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON (%q): %v", rec.Body.String(), err)
	}
	return body
}

// The contract's whole point: submit, then read the outcome back. Before the
// results store this was impossible — the assignment was discarded on ack —
// even though rest-openapi.yaml has always documented the endpoint.
func TestSubmittedRequestIsImmediatelyReadableAsPending(t *testing.T) {
	_, _, _, h := newTestServerWithResults(t)

	rec := post(t, h, "/v1/ride-requests",
		`{"rider_id":"R-1","pickup":{"lat":12.9716,"lng":77.5946}}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, want 202", rec.Code)
	}
	var accepted struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode accept: %v", err)
	}

	got := get(t, h, "/v1/ride-requests/"+accepted.RequestID)
	if got.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200 (body %s)", got.Code, got.Body.String())
	}

	body := decodeStatus(t, got)
	if body["status"] != "PENDING" {
		t.Errorf("status = %v, want PENDING", body["status"])
	}
	if body["request_id"] != accepted.RequestID {
		t.Errorf("request_id = %v, want %s", body["request_id"], accepted.RequestID)
	}
	// `match` must be present and null, not absent. A client checking
	// `"match" in response` should get a definite "no driver yet".
	if m, ok := body["match"]; !ok || m != nil {
		t.Errorf("match = %v (present=%v), want an explicit null while pending", m, ok)
	}
}

func TestMatchedRequestReturnsTheDriver(t *testing.T) {
	_, _, store, h := newTestServerWithResults(t)

	eta := int32(240)
	if err := store.Put(context.Background(), results.Outcome{
		RequestID:          "req_matched",
		TenantID:           "test",
		Status:             results.StatusMatched,
		DriverID:           "D-42",
		ETASeconds:         &eta,
		StraightLineMeters: 812.5,
		Attempts:           1,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := get(t, h, "/v1/ride-requests/req_matched")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := decodeStatus(t, rec)
	if body["status"] != "MATCHED" {
		t.Fatalf("status = %v, want MATCHED", body["status"])
	}
	match, ok := body["match"].(map[string]any)
	if !ok {
		t.Fatalf("match is not an object: %s", rec.Body.String())
	}
	if match["driver_id"] != "D-42" {
		t.Errorf("driver_id = %v, want D-42", match["driver_id"])
	}
	if match["eta_seconds"] != float64(240) {
		t.Errorf("eta_seconds = %v, want 240", match["eta_seconds"])
	}
	if match["straight_line_meters"] != 812.5 {
		t.Errorf("straight_line_meters = %v, want 812.5", match["straight_line_meters"])
	}
}

// Under EUCLIDEAN pricing the engine reports no ETA, and the field must be
// OMITTED rather than sent as 0 — which a client would read as "already here".
func TestMatchWithoutETAOmitsTheField(t *testing.T) {
	_, _, store, h := newTestServerWithResults(t)

	if err := store.Put(context.Background(), results.Outcome{
		RequestID: "req_euclid", TenantID: "test",
		Status: results.StatusMatched, DriverID: "D-1", StraightLineMeters: 300,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := get(t, h, "/v1/ride-requests/req_euclid")
	match := decodeStatus(t, rec)["match"].(map[string]any)
	if v, present := match["eta_seconds"]; present {
		t.Errorf("eta_seconds present as %v; it must be omitted when unknown", v)
	}
}

// "No car available" is an ANSWER, and the rider has to be able to receive it.
// Without this the client polls PENDING forever on a request the system has
// already given up on.
func TestUnmatchedRequestReportsWhy(t *testing.T) {
	_, _, store, h := newTestServerWithResults(t)

	if err := store.Put(context.Background(), results.Outcome{
		RequestID: "req_nocar", TenantID: "test",
		Status:   results.StatusUnmatched,
		Reason:   "no driver found after 20 match attempts",
		Attempts: 20,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := get(t, h, "/v1/ride-requests/req_nocar")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — 'no car' is an answer, not an error", rec.Code)
	}
	body := decodeStatus(t, rec)
	if body["status"] != "UNMATCHED" {
		t.Errorf("status = %v, want UNMATCHED", body["status"])
	}
	if body["reason"] == "" || body["reason"] == nil {
		t.Error("no reason given for an unmatched request")
	}
	if body["match"] != nil {
		t.Errorf("match = %v, want null on an unmatched request", body["match"])
	}
}

// 404 must mean "never accepted", which is only true because the POST path
// writes a PENDING record. Otherwise a real, queued request would 404 too and
// the status code would be meaningless.
func TestUnknownRequestIs404(t *testing.T) {
	_, _, _, h := newTestServerWithResults(t)

	rec := get(t, h, "/v1/ride-requests/req_neverseen")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if body := decodeError(t, rec); body.Code != api.CodeNotFound {
		t.Errorf("code = %q, want %q", body.Code, api.CodeNotFound)
	}
}

// Another tenant's request must be indistinguishable from one that does not
// exist. A 403 here would confirm the id is real, which is itself the leak.
func TestAnotherTenantsRequestIs404NotForbidden(t *testing.T) {
	_, _, store, h := newTestServerWithResults(t)

	if err := store.Put(context.Background(), results.Outcome{
		RequestID: "req_competitor", TenantID: "some-other-tenant",
		Status: results.StatusMatched, DriverID: "THEIR-DRIVER",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := get(t, h, "/v1/ride-requests/req_competitor")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — a cross-tenant read must not be "+
			"distinguishable from a missing request", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "THEIR-DRIVER") {
		t.Fatal("the response leaked another tenant's driver assignment")
	}
}

func TestMalformedRequestIDRejected(t *testing.T) {
	_, _, _, h := newTestServerWithResults(t)

	for _, id := range []string{
		"req%20with%20spaces",
		"req_" + strings.Repeat("x", 100), // over the 64-char cap
		"req%0anewline",                   // log injection attempt
	} {
		rec := get(t, h, "/v1/ride-requests/"+id)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id %q: status = %d, want 400", id, rec.Code)
		}
	}
}

// A results-store outage must not be reported as "your request does not
// exist". 503 says the truth: we cannot look right now.
func TestResultsOutageIs503NotFalse404(t *testing.T) {
	q := &fakeQueue{}
	cfg := api.DefaultConfig()
	cfg.TenantID = "test"
	cfg.Results = brokenResults{}
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := api.NewServer(q, cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	rec := get(t, srv.Routes(), "/v1/ride-requests/req_anything")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if body := decodeError(t, rec); body.Code != api.CodeUnavailable {
		t.Errorf("code = %q, want %q", body.Code, api.CodeUnavailable)
	}
}

// A results-store outage must NOT stop rides being accepted. The queue is what
// makes a request durable; the outcome record is a convenience on top of it,
// and losing the convenience must not lose the ride.
func TestResultsOutageDoesNotBlockSubmission(t *testing.T) {
	q := &fakeQueue{}
	cfg := api.DefaultConfig()
	cfg.TenantID = "test"
	cfg.Results = brokenResults{}
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := api.NewServer(q, cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	rec := post(t, srv.Routes(), "/v1/ride-requests",
		`{"rider_id":"R-1","pickup":{"lat":12.97,"lng":77.59}}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 — a results outage must not reject a ride", rec.Code)
	}
	if q.count() != 1 {
		t.Errorf("published %d requests, want 1", q.count())
	}
}

// brokenResults fails every operation, standing in for a Redis outage.
type brokenResults struct{}

func (brokenResults) Put(context.Context, results.Outcome) error { return errors.New("redis is down") }
func (brokenResults) Get(context.Context, string, string) (results.Outcome, error) {
	return results.Outcome{}, errors.New("redis is down")
}
func (brokenResults) Close() error { return nil }
