package ingest_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/aditya/ride-matching/internal/auth"
	"github.com/aditya/ride-matching/internal/ingest"
)

// blockingStore parks a request inside the upgrade handler, on demand.
//
// The bug below lives in a window a few instructions wide, so hoping the
// scheduler lands in it is not a test — it is a coin toss that passed on my
// laptop and failed on CI. An auth store that blocks until released puts a
// request in that window deliberately and holds it there.
type blockingStore struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingStore) Lookup(ctx context.Context, keyID string) (*auth.APIKey, error) {
	select {
	case b.entered <- struct{}{}:
	default: // only the first caller needs to be observed
	}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil, auth.ErrInvalidKey
}

func (b *blockingStore) Create(context.Context, string, string, time.Duration, int) (string, *auth.APIKey, error) {
	return "", nil, auth.ErrInvalidKey
}
func (b *blockingStore) Revoke(context.Context, string) error { return nil }
func (b *blockingStore) Rotate(context.Context, string, time.Duration) (string, *auth.APIKey, error) {
	return "", nil, auth.ErrInvalidKey
}
func (b *blockingStore) Allow(context.Context, string, int) (bool, int, error) { return true, 1, nil }
func (b *blockingStore) Close() error                                          { return nil }

// TestShutdownWaitsForRequestsNotYetUpgraded pins down a bug CI caught and a
// fast laptop did not.
//
// The original Handler checked the shutdown channel, incremented activeConns,
// authenticated, upgraded, and only THEN called wg.Add. A request anywhere in
// that window when Shutdown ran produced two failures:
//
//  1. wg.Wait saw a zero counter and returned, so Shutdown reported a clean
//     exit while a request was still being served — and activeConns was
//     non-zero afterwards; and
//  2. sync.WaitGroup forbids an Add that raises the counter from zero while a
//     Wait is in flight, which the race detector reports as a data race.
//
// The fix takes the wait-group token at the TOP of the handler, under the same
// lock Shutdown uses to stop admitting. Authentication is the widest part of
// that window in production (it talks to Redis), which is what this test
// stands in for.
func TestShutdownWaitsForRequestsNotYetUpgraded(t *testing.T) {
	store := &blockingStore{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	srv, url := newServer(t, &collectingSink{}, ingest.Config{Keys: store})

	go func() {
		hdr := http.Header{}
		hdr.Set("Authorization", "Bearer rmk_someid_somesecret")
		if conn, _, err := websocket.DefaultDialer.Dial(url, hdr); err == nil {
			_ = conn.Close()
		}
	}()

	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached authentication")
	}

	// A request is now parked mid-handler: past the shutdown check, past the
	// activeConns increment, not yet upgraded.
	shutdownReturned := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownReturned <- srv.Shutdown(ctx)
	}()

	// Shutdown MUST still be blocked. Returning here is the bug: it would be
	// promising that nothing is in flight while a request is mid-flight.
	select {
	case err := <-shutdownReturned:
		t.Fatalf("Shutdown returned (%v) while a request was still in the handler; "+
			"active = %d", err, srv.Stats().ActiveConnections)
	case <-time.After(250 * time.Millisecond):
	}

	close(store.release)

	select {
	case err := <-shutdownReturned:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the request finished")
	}

	if n := srv.Stats().ActiveConnections; n != 0 {
		t.Errorf("active = %d after Shutdown returned, want 0", n)
	}
}
