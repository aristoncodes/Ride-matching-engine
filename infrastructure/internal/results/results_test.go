package results_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aditya/ride-matching/internal/results"
	"github.com/aditya/ride-matching/internal/testutil"
)

// Every test runs against BOTH implementations. The in-memory store is what
// the API and batcher suites use as a double, so a behavioural difference
// between it and Redis would make those suites prove something that is not
// true of the deployed system — which is the exact failure a test double is
// supposed to avoid.
func eachStore(t *testing.T, fn func(t *testing.T, s results.Store)) {
	t.Helper()

	t.Run("memory", func(t *testing.T) {
		fn(t, results.NewMemory())
	})

	t.Run("redis", func(t *testing.T) {
		proc := testutil.StartRedis(t)
		s, err := results.NewRedis(proc.Addr, results.Options{TTL: time.Hour})
		if err != nil {
			t.Fatalf("new redis store: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		fn(t, s)
	})
}

func TestPutThenGetRoundTrips(t *testing.T) {
	eachStore(t, func(t *testing.T, s results.Store) {
		ctx := context.Background()
		eta := int32(275)

		want := results.Outcome{
			RequestID:          "req_abc",
			TenantID:           "acme",
			Status:             results.StatusMatched,
			DriverID:           "D-7",
			ETASeconds:         &eta,
			StraightLineMeters: 1432.5,
			Attempts:           2,
		}
		if err := s.Put(ctx, want); err != nil {
			t.Fatalf("put: %v", err)
		}

		got, err := s.Get(ctx, "acme", "req_abc")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != results.StatusMatched {
			t.Errorf("status = %q, want MATCHED", got.Status)
		}
		if got.DriverID != "D-7" {
			t.Errorf("driver_id = %q, want D-7", got.DriverID)
		}
		if got.ETASeconds == nil || *got.ETASeconds != 275 {
			t.Errorf("eta = %v, want 275", got.ETASeconds)
		}
		if got.StraightLineMeters != 1432.5 {
			t.Errorf("straight_line_meters = %v, want 1432.5", got.StraightLineMeters)
		}
		if got.Attempts != 2 {
			t.Errorf("attempts = %d, want 2", got.Attempts)
		}
		if got.UpdatedAt.IsZero() {
			t.Error("updated_at was not stamped")
		}
	})
}

// The isolation property, stated as a test: the tenant is part of the key, so
// knowing another tenant's request id exactly is still not enough to read it.
// This is the store-level half of what internal/tenancy asserts end to end.
func TestGetIsScopedToTenant(t *testing.T) {
	eachStore(t, func(t *testing.T, s results.Store) {
		ctx := context.Background()

		if err := s.Put(ctx, results.Outcome{
			RequestID: "req_shared_id",
			TenantID:  "tenant-a",
			Status:    results.StatusMatched,
			DriverID:  "A-DRIVER",
		}); err != nil {
			t.Fatalf("put: %v", err)
		}

		_, err := s.Get(ctx, "tenant-b", "req_shared_id")
		if !errors.Is(err, results.ErrNotFound) {
			t.Fatalf("cross-tenant read returned %v, want ErrNotFound — a "+
				"competitor's driver assignment must not be readable", err)
		}

		// And the owner is unaffected: the miss above is isolation, not a
		// collision that broke the real record.
		got, err := s.Get(ctx, "tenant-a", "req_shared_id")
		if err != nil {
			t.Fatalf("owner read: %v", err)
		}
		if got.DriverID != "A-DRIVER" {
			t.Errorf("driver_id = %q, want A-DRIVER", got.DriverID)
		}
	})
}

func TestGetUnknownRequestIsNotFound(t *testing.T) {
	eachStore(t, func(t *testing.T, s results.Store) {
		_, err := s.Get(context.Background(), "acme", "req_never_seen")
		if !errors.Is(err, results.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// Last write wins. At-least-once delivery means a request whose ack was lost
// gets re-matched, and the newer assignment is the true one.
func TestPutOverwrites(t *testing.T) {
	eachStore(t, func(t *testing.T, s results.Store) {
		ctx := context.Background()

		if err := s.Put(ctx, results.Outcome{
			RequestID: "req_1", TenantID: "acme",
			Status: results.StatusPending,
		}); err != nil {
			t.Fatalf("put pending: %v", err)
		}
		if err := s.Put(ctx, results.Outcome{
			RequestID: "req_1", TenantID: "acme",
			Status: results.StatusMatched, DriverID: "D-9",
		}); err != nil {
			t.Fatalf("put matched: %v", err)
		}

		got, err := s.Get(ctx, "acme", "req_1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != results.StatusMatched || got.DriverID != "D-9" {
			t.Errorf("got %q/%q, want MATCHED/D-9", got.Status, got.DriverID)
		}
	})
}

// A missing ETA must stay missing. Under EUCLIDEAN pricing the engine has no
// basis for one, and a zero would read to a client as "the car is already
// here" — the reason the field is a pointer rather than a bare int32.
func TestAbsentETAStaysAbsent(t *testing.T) {
	eachStore(t, func(t *testing.T, s results.Store) {
		ctx := context.Background()

		if err := s.Put(ctx, results.Outcome{
			RequestID: "req_euclid", TenantID: "acme",
			Status: results.StatusMatched, DriverID: "D-1",
		}); err != nil {
			t.Fatalf("put: %v", err)
		}

		got, err := s.Get(ctx, "acme", "req_euclid")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.ETASeconds != nil {
			t.Errorf("eta = %d, want nil under euclidean pricing", *got.ETASeconds)
		}
	})
}

func TestPutRejectsUnkeyedOutcomes(t *testing.T) {
	eachStore(t, func(t *testing.T, s results.Store) {
		ctx := context.Background()

		if err := s.Put(ctx, results.Outcome{TenantID: "acme"}); !errors.Is(err, results.ErrEmptyRequestID) {
			t.Errorf("missing request id: err = %v, want ErrEmptyRequestID", err)
		}
		// An outcome with no tenant would be unreadable by construction, since
		// every read is tenant-scoped. Better to fail than to store it.
		if err := s.Put(ctx, results.Outcome{RequestID: "req_1"}); !errors.Is(err, results.ErrEmptyTenantID) {
			t.Errorf("missing tenant id: err = %v, want ErrEmptyTenantID", err)
		}
	})
}

// An outcome must not outlive its TTL, or the keyspace grows without bound in
// a Redis that is also holding the API keys and the durable queue.
func TestRedisOutcomeExpires(t *testing.T) {
	proc := testutil.StartRedis(t)
	s, err := results.NewRedis(proc.Addr, results.Options{TTL: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("new redis store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	if err := s.Put(ctx, results.Outcome{
		RequestID: "req_ttl", TenantID: "acme", Status: results.StatusMatched,
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := s.Get(ctx, "acme", "req_ttl"); err != nil {
		t.Fatalf("expected readable before expiry: %v", err)
	}

	// A real sleep, because this asserts REDIS's expiry rather than our own
	// clock arithmetic; an injected time source would skip the thing under
	// test. Kept short enough to stay cheap.
	time.Sleep(600 * time.Millisecond)

	if _, err := s.Get(ctx, "acme", "req_ttl"); !errors.Is(err, results.ErrNotFound) {
		t.Fatalf("after TTL: err = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreIsConcurrencySafe(t *testing.T) {
	s := results.NewMemory()
	ctx := context.Background()
	done := make(chan struct{})

	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 200; j++ {
				_ = s.Put(ctx, results.Outcome{
					RequestID: "req_hot", TenantID: "acme",
					Status: results.StatusMatched, DriverID: "D-1",
				})
				_, _ = s.Get(ctx, "acme", "req_hot")
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
