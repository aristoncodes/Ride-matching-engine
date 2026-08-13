package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aditya/ride-matching/internal/auth"
	"github.com/aditya/ride-matching/internal/testutil"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)} }
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newStore(t *testing.T, clk *clock) *auth.RedisStore {
	t.Helper()
	proc := testutil.StartRedis(t)
	var now func() time.Time
	if clk != nil {
		now = clk.Now
	}
	s, err := auth.NewRedisStore(proc.Addr, now)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return s
}

func TestKeyIsHashedAtRestAndNeverRecoverable(t *testing.T) {
	// The single most important property: a database dump must not contain
	// working credentials.
	store := newStore(t, nil)
	ctx := context.Background()

	raw, key, err := store.Create(ctx, "acme", "prod", 0, 600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if !strings.HasPrefix(raw, "rmk_") {
		t.Errorf("key %q lacks the scannable prefix — secret scanners match on it", raw)
	}

	_, secret, err := auth.ParseKey(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// The stored record must not contain the secret in any form.
	if strings.Contains(key.SecretHash, secret) {
		t.Fatal("the stored hash contains the raw secret")
	}
	if key.SecretHash == secret {
		t.Fatal("the secret was stored verbatim rather than hashed")
	}
	if key.SecretHash != auth.HashSecret(secret) {
		t.Error("stored hash does not match the hash of the secret")
	}

	// Re-reading gives the hash and nothing else — there is no code path that
	// can reproduce the raw key.
	fetched, err := store.Lookup(ctx, key.KeyID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%+v", fetched), secret) {
		t.Fatal("the raw secret is recoverable from the stored record")
	}
}

func TestVerifyAcceptsOnlyTheCorrectSecret(t *testing.T) {
	store := newStore(t, nil)
	ctx := context.Background()
	now := time.Now()

	raw, key, err := store.Create(ctx, "acme", "prod", 0, 600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := auth.Verify(ctx, store, raw, now)
	if err != nil {
		t.Fatalf("verify a valid key: %v", err)
	}
	if got.TenantID != "acme" {
		t.Errorf("tenant = %q, want acme", got.TenantID)
	}

	cases := []struct{ name, key string }{
		{"wrong secret", "rmk_" + key.KeyID + "_" + strings.Repeat("0", 64)},
		{"unknown key id", "rmk_" + strings.Repeat("a", 16) + "_deadbeef"},
		{"no prefix", key.KeyID + "_secret"},
		{"wrong prefix", "sk_" + key.KeyID + "_secret"},
		{"empty", ""},
		{"garbage", "not-a-key-at-all"},
		{"missing secret", "rmk_" + key.KeyID + "_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := auth.Verify(ctx, store, tc.key, now); !errors.Is(err, auth.ErrInvalidKey) {
				t.Errorf("err = %v, want ErrInvalidKey", err)
			}
		})
	}
}

// TestRevokedKeyIsRejectedInstantly is half the Week 18 checkpoint.
func TestRevokedKeyIsRejectedInstantly(t *testing.T) {
	store := newStore(t, nil)
	ctx := context.Background()
	now := time.Now()

	raw, key, err := store.Create(ctx, "acme", "prod", 0, 600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := auth.Verify(ctx, store, raw, now); err != nil {
		t.Fatalf("key should work before revocation: %v", err)
	}

	if err := store.Revoke(ctx, key.KeyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// INSTANTLY — no cache to expire, no propagation delay. Every request
	// re-reads the store, which is what makes revocation a security control
	// rather than a suggestion. A cache here would be a real tradeoff and would
	// need an explicit invalidation story.
	if _, err := auth.Verify(ctx, store, raw, time.Now()); !errors.Is(err, auth.ErrInvalidKey) {
		t.Fatalf("a revoked key was accepted (err = %v)", err)
	}

	// The record survives for audit rather than being deleted.
	revoked, err := store.Lookup(ctx, key.KeyID)
	if err != nil {
		t.Fatalf("a revoked key's record should remain for audit: %v", err)
	}
	if revoked.RevokedAt.IsZero() {
		t.Error("RevokedAt was not recorded")
	}
}

func TestExpiredKeyIsRejected(t *testing.T) {
	clk := newClock()
	store := newStore(t, clk)
	ctx := context.Background()

	raw, _, err := store.Create(ctx, "acme", "temporary", time.Hour, 600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := auth.Verify(ctx, store, raw, clk.Now()); err != nil {
		t.Fatalf("key should work before expiry: %v", err)
	}

	clk.Advance(2 * time.Hour)
	if _, err := auth.Verify(ctx, store, raw, clk.Now()); !errors.Is(err, auth.ErrInvalidKey) {
		t.Errorf("an expired key was accepted (err = %v)", err)
	}
}

func TestRotationKeepsTheOldKeyWorkingDuringOverlap(t *testing.T) {
	// Rotation without an overlap breaks every client that has not yet picked
	// up the new key — which is exactly why people avoid rotating, and
	// unrotated keys are the real security problem.
	clk := newClock()
	store := newStore(t, clk)
	ctx := context.Background()

	oldRaw, oldKey, err := store.Create(ctx, "acme", "prod", 0, 600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	newRaw, newKey, err := store.Rotate(ctx, oldKey.KeyID, 24*time.Hour)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if newKey.KeyID == oldKey.KeyID {
		t.Fatal("rotation reused the key id")
	}
	if newRaw == oldRaw {
		t.Fatal("rotation reused the secret")
	}
	if newKey.TenantID != oldKey.TenantID {
		t.Errorf("rotation changed the tenant: %q -> %q", oldKey.TenantID, newKey.TenantID)
	}
	if newKey.RotatedFrom != oldKey.KeyID {
		t.Errorf("RotatedFrom = %q, want %q — the link is what shows an operator "+
			"a rotation happened", newKey.RotatedFrom, oldKey.KeyID)
	}

	// BOTH work during the overlap.
	if _, err := auth.Verify(ctx, store, oldRaw, clk.Now()); err != nil {
		t.Errorf("the old key stopped working immediately: %v", err)
	}
	if _, err := auth.Verify(ctx, store, newRaw, clk.Now()); err != nil {
		t.Errorf("the new key does not work: %v", err)
	}

	// After the overlap, only the new one.
	clk.Advance(25 * time.Hour)
	if _, err := auth.Verify(ctx, store, oldRaw, clk.Now()); !errors.Is(err, auth.ErrInvalidKey) {
		t.Error("the old key still works after the overlap expired")
	}
	if _, err := auth.Verify(ctx, store, newRaw, clk.Now()); err != nil {
		t.Errorf("the new key stopped working: %v", err)
	}
}

func TestRateLimitIsPerKey(t *testing.T) {
	// Per-key, so one noisy tenant cannot consume another's capacity.
	store := newStore(t, nil)
	ctx := context.Background()

	noisyRaw, _, err := store.Create(ctx, "noisy", "prod", 0, 5)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	quietRaw, _, err := store.Create(ctx, "quiet", "prod", 0, 5)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now()
	var limited int
	for i := 0; i < 12; i++ {
		if _, err := auth.Verify(ctx, store, noisyRaw, now); errors.Is(err, auth.ErrRateLimited) {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("the noisy key was never rate limited")
	}

	// The quiet tenant is untouched by the noisy one's behaviour.
	if _, err := auth.Verify(ctx, store, quietRaw, now); err != nil {
		t.Errorf("a quiet tenant was affected by another tenant's traffic: %v", err)
	}
}

func TestRateLimitWindowResets(t *testing.T) {
	clk := newClock()
	store := newStore(t, clk)
	ctx := context.Background()

	raw, _, err := store.Create(ctx, "acme", "prod", 0, 3)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for i := 0; i < 6; i++ {
		_, _ = auth.Verify(ctx, store, raw, clk.Now())
	}
	if _, err := auth.Verify(ctx, store, raw, clk.Now()); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatal("expected to be rate limited")
	}

	// A new minute is a new window.
	clk.Advance(61 * time.Second)
	if _, err := auth.Verify(ctx, store, raw, clk.Now()); err != nil {
		t.Errorf("the rate limit did not reset in the next window: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

func testWriteError(w http.ResponseWriter, status int, code, message string, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"code":%q,"message":%q}}`, code, message)
}

func newHandler(t *testing.T, store auth.Store) http.Handler {
	t.Helper()
	mw := auth.Middleware(auth.Options{
		Store:      store,
		WriteError: testWriteError,
		SkipPaths:  []string{"/healthz"},
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/thing", func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := auth.TenantFromContext(r.Context())
		if !ok {
			t.Error("handler reached with no tenant in context")
		}
		fmt.Fprintf(w, `{"tenant":%q}`, tenant)
	})
	return mw(mux)
}

func TestMiddlewareRejectsAndAccepts(t *testing.T) {
	store := newStore(t, nil)
	ctx := context.Background()
	raw, key, err := store.Create(ctx, "acme", "prod", 0, 600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	h := newHandler(t, store)

	t.Run("no key is 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/thing", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("X-API-Key works", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
		req.Header.Set("X-API-Key", raw)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"acme"`) {
			t.Errorf("handler did not see the tenant: %s", rec.Body.String())
		}
	})

	t.Run("Authorization Bearer works", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("health probes bypass auth", func(t *testing.T) {
		// A kubelet has no API key. Requiring one would make every pod
		// permanently unready — an outage caused by the auth layer.
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("healthz = %d, want 200 without a key", rec.Code)
		}
	})

	t.Run("revoked key is 401 immediately", func(t *testing.T) {
		if err := store.Revoke(ctx, key.KeyID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
		req.Header.Set("X-API-Key", raw)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 for a revoked key", rec.Code)
		}
	})
}

func TestMiddlewareRateLimitIs429WithRetryAfter(t *testing.T) {
	store := newStore(t, nil)
	ctx := context.Background()
	raw, _, err := store.Create(ctx, "acme", "prod", 0, 3)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	h := newHandler(t, store)

	var got429 bool
	for i := 0; i < 8; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
		req.Header.Set("X-API-Key", raw)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			// 429, not 401: the key is fine, the client just needs to slow
			// down. Returning 401 would send them rotating credentials.
			if rec.Header().Get("Retry-After") == "" {
				t.Error("a 429 must tell the client when to retry")
			}
			break
		}
	}
	if !got429 {
		t.Fatal("never got a 429 despite exceeding the limit")
	}
}

// storeFailure simulates the auth store being down.
type failingStore struct{ auth.Store }

func (failingStore) Lookup(context.Context, string) (*auth.APIKey, error) {
	return nil, errors.New("redis is down")
}

func TestStoreFailureIs503Not401(t *testing.T) {
	// Telling a customer their valid key is invalid during OUR outage sends
	// them rotating credentials that were never the problem.
	h := newHandler(t, failingStore{})
	req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
	req.Header.Set("X-API-Key", "rmk_abc_def")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the auth store is down", rec.Code)
	}
}

func TestErrorsDoNotLeakWhetherAKeyExists(t *testing.T) {
	// Unknown, revoked and wrong-secret must be indistinguishable to a caller,
	// or a key id becomes enumerable.
	store := newStore(t, nil)
	ctx := context.Background()
	_, key, err := store.Create(ctx, "acme", "prod", 0, 600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Revoke(ctx, key.KeyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	h := newHandler(t, store)

	bodies := map[string]string{}
	for name, k := range map[string]string{
		"revoked key": "rmk_" + key.KeyID + "_" + strings.Repeat("0", 64),
		"unknown key": "rmk_" + strings.Repeat("f", 16) + "_" + strings.Repeat("0", 64),
		"malformed":   "totally-invalid",
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
		req.Header.Set("X-API-Key", k)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
		bodies[name] = rec.Body.String()
	}

	var prev string
	for name, body := range bodies {
		if prev != "" && body != prev {
			t.Errorf("responses differ between failure modes, which lets a caller "+
				"enumerate valid key ids (%s: %s)", name, body)
		}
		prev = body
	}
}

func TestWebSocketKeyFallsBackToSubprotocol(t *testing.T) {
	// Browsers cannot set headers on a WebSocket handshake, so the subprotocol
	// is the standard carrier.
	req := httptest.NewRequest(http.MethodGet, "/v1/drivers/stream", nil)
	req.Header.Set("Sec-WebSocket-Protocol", "apikey.rmk_abc_def")
	if got := auth.WebSocketKey(req); got != "rmk_abc_def" {
		t.Errorf("WebSocketKey = %q, want rmk_abc_def", got)
	}

	// A header still wins when present (native apps can set one).
	req2 := httptest.NewRequest(http.MethodGet, "/v1/drivers/stream", nil)
	req2.Header.Set("X-API-Key", "rmk_from_header")
	req2.Header.Set("Sec-WebSocket-Protocol", "apikey.rmk_from_proto")
	if got := auth.WebSocketKey(req2); got != "rmk_from_header" {
		t.Errorf("WebSocketKey = %q, want the header to win", got)
	}
}

func TestConcurrentVerificationIsSafe(t *testing.T) {
	store := newStore(t, nil)
	ctx := context.Background()
	raw, _, err := store.Create(ctx, "acme", "prod", 0, 100000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := auth.Verify(ctx, store, raw, time.Now()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent verify: %v", err)
	}
}
