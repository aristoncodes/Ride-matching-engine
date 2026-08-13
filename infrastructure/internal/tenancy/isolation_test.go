// Package tenancy_test is the Week 19 checkpoint.
//
//	"An automated test attempts cross-tenant access and is denied at EVERY
//	 layer."
//
// It lives in its own package on purpose. Isolation is not a property of any
// one component — it is a property of the composition, and each package's own
// tests can only ever check its own layer. A leak between two correct layers is
// exactly the kind nobody notices.
//
// The layers, and what would go wrong at each:
//
//	AUTH      a client naming its own tenant could address any fleet
//	QUEUE     a shared stream would deliver tenant A's riders to tenant B
//	CACHE     a shared geo index would dispatch A's drivers to B's riders
//	PIPELINE  a driver-keyed buffer would let A's ping overwrite B's position
//	KEYS      one tenant's revocation or rate limit would affect another
//
// The tests below are written from the ATTACKER's side: each one tries to reach
// tenant B's data while holding only tenant A's credentials, and asserts it
// cannot. "Isolation you haven't tested is isolation you don't have."
package tenancy_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aditya/ride-matching/internal/api"
	"github.com/aditya/ride-matching/internal/auth"
	"github.com/aditya/ride-matching/internal/ingest"
	"github.com/aditya/ride-matching/internal/locations"
	"github.com/aditya/ride-matching/internal/pipeline"
	"github.com/aditya/ride-matching/internal/queue"
	"github.com/aditya/ride-matching/internal/testutil"
)

const (
	tenantA = "acme-taxis"
	tenantB = "rival-cabs"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// twoTenants sets up one Redis with an API key for each of two tenants.
func twoTenants(t *testing.T) (addr string, store *auth.RedisStore, keyA, keyB string) {
	t.Helper()
	proc := testutil.StartRedis(t)

	store, err := auth.NewRedisStore(proc.Addr, nil)
	if err != nil {
		t.Fatalf("auth store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	keyA, _, err = store.Create(ctx, tenantA, "acme prod", 0, 100000)
	if err != nil {
		t.Fatalf("create key A: %v", err)
	}
	keyB, _, err = store.Create(ctx, tenantB, "rival prod", 0, 100000)
	if err != nil {
		t.Fatalf("create key B: %v", err)
	}
	return proc.Addr, store, keyA, keyB
}

func newQueue(t *testing.T, addr, tenant, consumer string) *queue.RedisStream {
	t.Helper()
	opts := queue.DefaultStreamOptions()
	opts.TenantID = tenant
	opts.Consumer = consumer
	q, err := queue.NewRedisStream(addr, opts)
	if err != nil {
		t.Fatalf("queue for %s: %v", tenant, err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func newLocations(t *testing.T, addr, tenant string) *locations.RedisRepository {
	t.Helper()
	opts := locations.DefaultOptions()
	opts.TenantID = tenant
	repo, err := locations.NewRedis(addr, opts)
	if err != nil {
		t.Fatalf("locations for %s: %v", tenant, err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// ---------------------------------------------------------------------------
// LAYER 1 — AUTH: a client cannot choose its own tenant
// ---------------------------------------------------------------------------

func TestTenantComesFromTheKeyNotTheRequest(t *testing.T) {
	// The foundation everything else rests on. If a client can name its tenant,
	// every downstream key prefix is decoration.
	addr, store, keyA, _ := twoTenants(t)

	q := newQueue(t, addr, tenantA, "c1")
	cfg := api.DefaultConfig()
	cfg.TenantID = "SHOULD-BE-IGNORED"
	cfg.Keys = store
	cfg.Logger = quiet()

	srv, err := api.NewServer(q, cfg)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	h := srv.Routes()

	// Tenant A's key, with a body and headers claiming to be tenant B.
	req := httptest.NewRequest(http.MethodPost, "/v1/ride-requests",
		strings.NewReader(`{"rider_id":"R-1","pickup":{"lat":12.97,"lng":77.59}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", keyA)
	req.Header.Set("X-Tenant-ID", tenantB) // ignored — not a real input
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body: %s", rec.Code, rec.Body.String())
	}

	// The request must have landed in A's stream, and B's must be untouched.
	ctx := context.Background()
	qb := newQueue(t, addr, tenantB, "c-b")

	msgsA, err := q.Consume(ctx, 10, time.Second)
	if err != nil {
		t.Fatalf("consume A: %v", err)
	}
	if len(msgsA) != 1 {
		t.Fatalf("tenant A's queue has %d messages, want 1", len(msgsA))
	}
	if got := msgsA[0].Request.TenantID; got != tenantA {
		t.Errorf("stored tenant = %q, want %q — the header was trusted", got, tenantA)
	}

	msgsB, err := qb.Consume(ctx, 10, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("consume B: %v", err)
	}
	if len(msgsB) != 0 {
		t.Fatalf("CROSS-TENANT LEAK: %d of tenant A's requests reached tenant B's queue",
			len(msgsB))
	}
}

func TestRequestWithoutAKeyIsRejected(t *testing.T) {
	addr, store, _, _ := twoTenants(t)
	q := newQueue(t, addr, tenantA, "c1")

	cfg := api.DefaultConfig()
	cfg.Keys = store
	cfg.Logger = quiet()
	srv, _ := api.NewServer(q, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/ride-requests",
		strings.NewReader(`{"rider_id":"R-1","pickup":{"lat":12.97,"lng":77.59}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a key", rec.Code)
	}
	// And nothing was written.
	if n, _ := q.Depth(context.Background()); n != 0 {
		t.Errorf("an unauthenticated request reached the queue (depth=%d)", n)
	}
}

// ---------------------------------------------------------------------------
// LAYER 2 — QUEUE: streams are per tenant
// ---------------------------------------------------------------------------

func TestOneTenantsConsumerNeverSeesAnothersRequests(t *testing.T) {
	addr, _, _, _ := twoTenants(t)
	ctx := context.Background()

	qa := newQueue(t, addr, tenantA, "batcher-a")
	qb := newQueue(t, addr, tenantB, "batcher-b")

	for i := 0; i < 5; i++ {
		if _, err := qa.Publish(ctx, queue.RideRequest{
			RequestID: fmt.Sprintf("A-%d", i), TenantID: tenantA,
			RiderID: "RA", Lat: 12.97, Lng: 77.59,
		}); err != nil {
			t.Fatalf("publish A: %v", err)
		}
	}
	if _, err := qb.Publish(ctx, queue.RideRequest{
		RequestID: "B-0", TenantID: tenantB, RiderID: "RB", Lat: 19.07, Lng: 72.87,
	}); err != nil {
		t.Fatalf("publish B: %v", err)
	}

	msgsB, err := qb.Consume(ctx, 100, time.Second)
	if err != nil {
		t.Fatalf("consume B: %v", err)
	}
	for _, m := range msgsB {
		if m.Request.TenantID != tenantB || strings.HasPrefix(m.Request.RequestID, "A-") {
			t.Fatalf("CROSS-TENANT LEAK: tenant B consumed %s (tenant %q)",
				m.Request.RequestID, m.Request.TenantID)
		}
	}
	if len(msgsB) != 1 {
		t.Errorf("tenant B got %d messages, want exactly its own 1", len(msgsB))
	}

	// And A still has all five — B's consumption did not steal them.
	msgsA, err := qa.Consume(ctx, 100, time.Second)
	if err != nil {
		t.Fatalf("consume A: %v", err)
	}
	if len(msgsA) != 5 {
		t.Errorf("tenant A got %d of its 5 requests", len(msgsA))
	}
}

func TestDeadLetterIsAlsoPerTenant(t *testing.T) {
	// An easy layer to forget: the main stream is scoped but the dead-letter
	// queue is shared, so one tenant can read another's failed requests —
	// which contain rider ids and pickup coordinates.
	addr, _, _, _ := twoTenants(t)
	ctx := context.Background()

	qa := newQueue(t, addr, tenantA, "a")
	qb := newQueue(t, addr, tenantB, "b")

	if err := qa.DeadLetter(ctx, queue.Message{
		ID: "1-0",
		Request: queue.RideRequest{
			RequestID: "A-secret", TenantID: tenantA, RiderID: "RA-private",
		},
	}, "test"); err != nil {
		t.Fatalf("dead-letter: %v", err)
	}

	depthA, _ := qa.DeadLetterDepth(ctx)
	depthB, _ := qb.DeadLetterDepth(ctx)
	if depthA != 1 {
		t.Errorf("tenant A dead-letter depth = %d, want 1", depthA)
	}
	if depthB != 0 {
		t.Fatalf("CROSS-TENANT LEAK: tenant B sees %d of tenant A's dead-lettered "+
			"requests, which contain rider ids and pickup locations", depthB)
	}
}

// ---------------------------------------------------------------------------
// LAYER 3 — CACHE: driver locations are per tenant
// ---------------------------------------------------------------------------

func TestRadiusQueryNeverReturnsAnotherTenantsDrivers(t *testing.T) {
	// The most damaging leak of the set: it would dispatch one operator's
	// drivers to a competitor's riders.
	addr, _, _, _ := twoTenants(t)
	ctx := context.Background()

	la := newLocations(t, addr, tenantA)
	lb := newLocations(t, addr, tenantB)

	// Both fleets in the SAME place, so only the tenant scoping can separate
	// them — geography cannot.
	for i := 0; i < 5; i++ {
		if err := la.UpsertDriver(ctx, fmt.Sprintf("A-%d", i), 12.9716, 77.5946); err != nil {
			t.Fatalf("upsert A: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := lb.UpsertDriver(ctx, fmt.Sprintf("B-%d", i), 12.9716, 77.5946); err != nil {
			t.Fatalf("upsert B: %v", err)
		}
	}

	q := locations.Query{Lat: 12.9716, Lng: 77.5946, Radius: 5000, Limit: 100}

	gotA, err := la.Nearby(ctx, q)
	if err != nil {
		t.Fatalf("nearby A: %v", err)
	}
	if len(gotA) != 5 {
		t.Errorf("tenant A sees %d drivers, want its own 5", len(gotA))
	}
	for _, d := range gotA {
		if strings.HasPrefix(d.DriverID, "B-") {
			t.Fatalf("CROSS-TENANT LEAK: tenant A's radius query returned %s, "+
				"a competitor's driver", d.DriverID)
		}
	}

	gotB, err := lb.Nearby(ctx, q)
	if err != nil {
		t.Fatalf("nearby B: %v", err)
	}
	if len(gotB) != 3 {
		t.Errorf("tenant B sees %d drivers, want its own 3", len(gotB))
	}
	for _, d := range gotB {
		if strings.HasPrefix(d.DriverID, "A-") {
			t.Fatalf("CROSS-TENANT LEAK: tenant B's radius query returned %s", d.DriverID)
		}
	}
}

func TestSameDriverIDInTwoTenantsStaysSeparate(t *testing.T) {
	// Two operators can legitimately both have a driver called "D-001". If ids
	// collide in storage, one tenant's ping silently relocates the other's
	// driver — corruption that looks like a flapping GPS bug.
	addr, _, _, _ := twoTenants(t)
	ctx := context.Background()

	la := newLocations(t, addr, tenantA)
	lb := newLocations(t, addr, tenantB)

	if err := la.UpsertDriver(ctx, "D-001", 12.9716, 77.5946); err != nil { // Bengaluru
		t.Fatalf("upsert A: %v", err)
	}
	if err := lb.UpsertDriver(ctx, "D-001", 19.0760, 72.8777); err != nil { // Mumbai
		t.Fatalf("upsert B: %v", err)
	}

	nearBengaluru := locations.Query{Lat: 12.9716, Lng: 77.5946, Radius: 5000, Limit: 10}
	gotA, err := la.Nearby(ctx, nearBengaluru)
	if err != nil {
		t.Fatalf("nearby A: %v", err)
	}
	if len(gotA) != 1 {
		t.Fatalf("tenant A sees %d drivers near Bengaluru, want 1", len(gotA))
	}

	// If the ids had collided, A's driver would have been moved to Mumbai.
	if gotA[0].Lat < 12 || gotA[0].Lat > 13 {
		t.Fatalf("CROSS-TENANT CORRUPTION: tenant A's D-001 is at %f,%f — "+
			"tenant B's write overwrote it", gotA[0].Lat, gotA[0].Lng)
	}

	nearMumbai := locations.Query{Lat: 19.0760, Lng: 72.8777, Radius: 5000, Limit: 10}
	gotB, err := lb.Nearby(ctx, nearMumbai)
	if err != nil {
		t.Fatalf("nearby B: %v", err)
	}
	if len(gotB) != 1 || gotB[0].Lat < 18 {
		t.Fatalf("tenant B's D-001 is wrong: %+v", gotB)
	}
}

// ---------------------------------------------------------------------------
// LAYER 4 — PIPELINE: the in-memory buffer is per tenant too
// ---------------------------------------------------------------------------

func TestPipelineRoutesEachPingToItsOwnTenant(t *testing.T) {
	// The buffer is in MEMORY and shared across tenants, so it is the one layer
	// a Redis key prefix cannot protect. Keyed by driver id alone, two tenants
	// using "D-001" would collide before the write ever happened.
	addr, _, _, _ := twoTenants(t)
	ctx := context.Background()

	la := newLocations(t, addr, tenantA)
	lb := newLocations(t, addr, tenantB)

	stores := map[string]locations.Repository{tenantA: la, tenantB: lb}
	p, err := pipeline.NewMultiTenant(func(tenant string) locations.Repository {
		return stores[tenant]
	}, pipeline.Config{Window: time.Hour, Logger: quiet()})
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}

	// The SAME driver id from two different tenants, in different cities.
	if err := p.Accept(ctx, ingest.Ping{TenantID: tenantA, DriverID: "D-001", Lat: 12.9716, Lng: 77.5946}); err != nil {
		t.Fatalf("accept A: %v", err)
	}
	if err := p.Accept(ctx, ingest.Ping{TenantID: tenantB, DriverID: "D-001", Lat: 19.0760, Lng: 72.8777}); err != nil {
		t.Fatalf("accept B: %v", err)
	}

	// Two buffered entries, not one — the collision would show up here first.
	if n := p.Stats().Buffered; n != 2 {
		t.Fatalf("buffered = %d, want 2 — the same driver id from two tenants "+
			"collided in the buffer", n)
	}

	if err := p.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	gotA, _ := la.Nearby(ctx, locations.Query{Lat: 12.9716, Lng: 77.5946, Radius: 5000, Limit: 10})
	gotB, _ := lb.Nearby(ctx, locations.Query{Lat: 19.0760, Lng: 72.8777, Radius: 5000, Limit: 10})

	if len(gotA) != 1 {
		t.Errorf("tenant A has %d drivers in Bengaluru, want 1", len(gotA))
	}
	if len(gotB) != 1 {
		t.Errorf("tenant B has %d drivers in Mumbai, want 1", len(gotB))
	}
	if len(gotA) == 1 && (gotA[0].Lat < 12 || gotA[0].Lat > 13) {
		t.Error("CROSS-TENANT LEAK: tenant A's driver was written with tenant B's position")
	}
}

// ---------------------------------------------------------------------------
// LAYER 5 — KEYS: one tenant's key lifecycle cannot affect another
// ---------------------------------------------------------------------------

func TestRevokingOneTenantsKeyDoesNotAffectAnother(t *testing.T) {
	addr, store, keyA, keyB := twoTenants(t)
	_ = addr
	ctx := context.Background()

	idA, _, err := auth.ParseKey(keyA)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := store.Revoke(ctx, idA); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// time.Now() is read AFTER the revocation, not before.
	//
	// Active() compares the supplied clock against RevokedAt, so a timestamp
	// captured before the revoke is legitimately still inside the key's valid
	// window — the first version of this test captured `now` at the top and
	// then reported "a revoked key still works". The middleware calls now() per
	// request, so this is a property of the test, not of revocation. Worth
	// knowing, though: revocation is instant with respect to REQUEST time, not
	// to whatever clock a caller happens to hold.
	if _, err := auth.Verify(ctx, store, keyA, time.Now()); err == nil {
		t.Error("tenant A's revoked key still works")
	}
	if _, err := auth.Verify(ctx, store, keyB, time.Now()); err != nil {
		t.Fatalf("revoking tenant A's key broke tenant B: %v", err)
	}
}

func TestRateLimitingOneTenantDoesNotStarveAnother(t *testing.T) {
	// The noisy-neighbour case. A shared limiter would let one B2B customer
	// consume another's paid capacity.
	proc := testutil.StartRedis(t)
	store, err := auth.NewRedisStore(proc.Addr, nil)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	noisy, _, err := store.Create(ctx, tenantA, "noisy", 0, 3)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	quietKey, _, err := store.Create(ctx, tenantB, "quiet", 0, 3)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now()
	for i := 0; i < 20; i++ {
		_, _ = auth.Verify(ctx, store, noisy, now)
	}

	if _, err := auth.Verify(ctx, store, quietKey, now); err != nil {
		t.Fatalf("tenant B was rate limited by tenant A's traffic: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The composed claim
// ---------------------------------------------------------------------------

// TestCrossTenantAccessIsDeniedAtEveryLayer is the Week 19 checkpoint proper:
// one attacker, holding only tenant A's key, tries every route to tenant B's
// data in sequence.
func TestCrossTenantAccessIsDeniedAtEveryLayer(t *testing.T) {
	addr, store, keyA, _ := twoTenants(t)
	ctx := context.Background()

	// Tenant B has real data: a driver and a queued request.
	lb := newLocations(t, addr, tenantB)
	if err := lb.UpsertDriver(ctx, "B-driver", 12.9716, 77.5946); err != nil {
		t.Fatalf("seed B driver: %v", err)
	}
	qb := newQueue(t, addr, tenantB, "b")
	if _, err := qb.Publish(ctx, queue.RideRequest{
		RequestID: "B-request", TenantID: tenantB, RiderID: "B-rider",
		Lat: 12.9716, Lng: 77.5946,
	}); err != nil {
		t.Fatalf("seed B request: %v", err)
	}

	qa := newQueue(t, addr, tenantA, "a")
	cfg := api.DefaultConfig()
	cfg.Keys = store
	cfg.Logger = quiet()
	srv, _ := api.NewServer(qa, cfg)
	h := srv.Routes()

	attacks := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"claim tenant B in a header", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/ride-requests",
				strings.NewReader(`{"rider_id":"R","pickup":{"lat":12.97,"lng":77.59}}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-API-Key", keyA)
			req.Header.Set("X-Tenant-ID", tenantB)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			msgs, _ := qb.Consume(ctx, 10, 300*time.Millisecond)
			for _, m := range msgs {
				if m.Request.RiderID == "R" {
					t.Fatal("DENIED-CHECK FAILED: wrote into tenant B's queue via a header")
				}
			}
		}},
		{"read tenant B's drivers with tenant A's store", func(t *testing.T) {
			la := newLocations(t, addr, tenantA)
			got, err := la.Nearby(ctx, locations.Query{
				Lat: 12.9716, Lng: 77.5946, Radius: 50000, Limit: 100,
			})
			if err != nil {
				t.Fatalf("nearby: %v", err)
			}
			for _, d := range got {
				if d.DriverID == "B-driver" {
					t.Fatal("DENIED-CHECK FAILED: read a competitor's driver")
				}
			}
		}},
		{"consume tenant B's requests with tenant A's consumer", func(t *testing.T) {
			msgs, err := qa.Consume(ctx, 100, 300*time.Millisecond)
			if err != nil {
				t.Fatalf("consume: %v", err)
			}
			for _, m := range msgs {
				if m.Request.TenantID != tenantA {
					t.Fatalf("DENIED-CHECK FAILED: consumed a %s request", m.Request.TenantID)
				}
			}
		}},
		{"read tenant B's dead-letter queue", func(t *testing.T) {
			if err := qb.DeadLetter(ctx, queue.Message{
				ID: "9-0", Request: queue.RideRequest{RequestID: "B-dead", TenantID: tenantB},
			}, "seed"); err != nil {
				t.Fatalf("seed: %v", err)
			}
			depth, err := qa.DeadLetterDepth(ctx)
			if err != nil {
				t.Fatalf("depth: %v", err)
			}
			if depth != 0 {
				t.Fatalf("DENIED-CHECK FAILED: tenant A sees %d of tenant B's "+
					"dead-lettered requests", depth)
			}
		}},
		{"unauthenticated access", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/ride-requests",
				strings.NewReader(`{"rider_id":"R","pickup":{"lat":12.97,"lng":77.59}}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("DENIED-CHECK FAILED: unauthenticated request got %d", rec.Code)
			}
		}},
	}

	for _, a := range attacks {
		t.Run(a.name, a.run)
	}
}
