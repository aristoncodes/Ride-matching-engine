package ingest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/aditya/ride-matching/internal/ingest"
)

// collectingSink records everything the server accepts.
type collectingSink struct {
	mu    sync.Mutex
	pings []ingest.Ping
	err   error
}

func (s *collectingSink) Accept(_ context.Context, p ingest.Ping) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.pings = append(s.pings, p)
	return nil
}

func (s *collectingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pings)
}

func (s *collectingSink) all() []ingest.Ping {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ingest.Ping(nil), s.pings...)
}

// newServer starts an httptest server with the WebSocket handler mounted.
func newServer(t *testing.T, sink ingest.Sink, cfg ingest.Config) (*ingest.Server, string) {
	t.Helper()
	srv := ingest.NewServer(sink, cfg)
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	return srv, "ws" + strings.TrimPrefix(httpSrv.URL, "http")
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func sendPing(t *testing.T, conn *websocket.Conn, p ingest.Ping) {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// waitFor polls until cond holds or the timeout expires. Polling beats sleeping
// a fixed amount: it is both faster when things go well and more reliable when
// the machine is loaded.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func TestAcceptsPings(t *testing.T) {
	sink := &collectingSink{}
	srv, url := newServer(t, sink, ingest.Config{})

	conn := dial(t, url)
	sendPing(t, conn, ingest.Ping{DriverID: "D-1", Lat: 12.97, Lng: 77.59})
	sendPing(t, conn, ingest.Ping{DriverID: "D-2", Lat: 12.98, Lng: 77.60})

	if !waitFor(2*time.Second, func() bool { return sink.count() == 2 }) {
		t.Fatalf("got %d pings, want 2", sink.count())
	}

	got := sink.all()
	if got[0].DriverID != "D-1" || got[0].Lat != 12.97 {
		t.Errorf("unexpected first ping: %+v", got[0])
	}
	if s := srv.Stats(); s.PingsReceived != 2 || s.TotalConnections != 1 {
		t.Errorf("stats = %+v", s)
	}
}

// TestNoGoroutineLeakOnClientDisconnect is the Week 8 checkpoint.
//
// Each connection owns two goroutines (a reader and a writer). If either
// survives its connection, the process leaks one goroutine per driver who ever
// connected — invisible for hours, then fatal. The check is a real
// before/after goroutine count across many connect/disconnect cycles, because
// a leak of one is easy to miss in noise and a leak of 200 is not.
func TestNoGoroutineLeakOnClientDisconnect(t *testing.T) {
	sink := &collectingSink{}
	_, url := newServer(t, sink, ingest.Config{})

	// Warm up: the first connection allocates HTTP-server internals that would
	// otherwise be counted as a "leak".
	warm := dial(t, url)
	sendPing(t, warm, ingest.Ping{DriverID: "D-warm", Lat: 12.97, Lng: 77.59})
	waitFor(time.Second, func() bool { return sink.count() >= 1 })
	_ = warm.Close()
	time.Sleep(200 * time.Millisecond)

	runtime.GC()
	before := runtime.NumGoroutine()

	const cycles = 100
	for i := 0; i < cycles; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		sendPing(t, conn, ingest.Ping{
			DriverID: fmt.Sprintf("D-%d", i), Lat: 12.97, Lng: 77.59,
		})
		// Close abruptly, without a closing handshake — what a force-quit or a
		// dead battery looks like from the server's side.
		_ = conn.Close()
	}

	leaked := waitFor(10*time.Second, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= before+5
	})

	after := runtime.NumGoroutine()
	if !leaked {
		t.Fatalf("goroutine leak: %d before, %d after %d connections (+%d)",
			before, after, cycles, after-before)
	}
	t.Logf("goroutines: %d before, %d after %d connect/disconnect cycles", before, after, cycles)
}

func TestDeadClientIsDisconnectedByReadDeadline(t *testing.T) {
	// The core dead-client argument: a client that stops answering pings is
	// dropped even though TCP still believes the socket is perfectly fine. To
	// prove it, the client must go silent WITHOUT closing.
	sink := &collectingSink{}
	srv, url := newServer(t, sink, ingest.Config{
		PongWait:     300 * time.Millisecond,
		PingInterval: 100 * time.Millisecond,
	})

	conn := dial(t, url)

	// Suppress the automatic pong reply, simulating a client that is present at
	// the TCP level and dead at the application level.
	conn.SetPingHandler(func(string) error { return nil })

	if !waitFor(2*time.Second, func() bool { return srv.Stats().ActiveConnections == 1 }) {
		t.Fatal("connection never registered")
	}

	if !waitFor(5*time.Second, func() bool { return srv.Stats().ActiveConnections == 0 }) {
		t.Fatal("silent client was never disconnected — the read deadline is not working")
	}
}

func TestHealthyClientIsNotDisconnected(t *testing.T) {
	// The other half of the same claim, and the one that actually catches a bad
	// PingInterval/PongWait ratio: a client that DOES answer must survive well
	// past PongWait.
	sink := &collectingSink{}
	srv, url := newServer(t, sink, ingest.Config{
		PongWait:     300 * time.Millisecond,
		PingInterval: 50 * time.Millisecond,
	})

	conn := dial(t, url)

	// gorilla replies to pings automatically, but only while something is
	// reading the connection. This goroutine is the client's read loop.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Three times PongWait: a broken heartbeat would have dropped it long ago.
	time.Sleep(900 * time.Millisecond)
	if n := srv.Stats().ActiveConnections; n != 1 {
		t.Fatalf("active connections = %d, want 1 — a responsive client was dropped", n)
	}

	_ = conn.Close()
	<-done
}

func TestConnectionLimitIsEnforced(t *testing.T) {
	sink := &collectingSink{}
	srv, url := newServer(t, sink, ingest.Config{MaxConnections: 3})

	var conns []*websocket.Conn
	for i := 0; i < 3; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		conns = append(conns, conn)
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	if !waitFor(2*time.Second, func() bool { return srv.Stats().ActiveConnections == 3 }) {
		t.Fatalf("active = %d, want 3", srv.Stats().ActiveConnections)
	}

	// The fourth must be refused at the HTTP layer, BEFORE the upgrade — a 503
	// the client can back off from, not an accepted connection that is then
	// torn down.
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("expected the 4th connection to be rejected")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %v, want 503", resp)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("a 503 should tell the client when to come back")
	}
	if srv.Stats().RejectedConnections != 1 {
		t.Errorf("rejected = %d, want 1", srv.Stats().RejectedConnections)
	}

	// Freeing a slot must let a new client in — the limit is a live gauge, not
	// a one-way counter.
	_ = conns[0].Close()
	conns = conns[1:]
	if !waitFor(2*time.Second, func() bool { return srv.Stats().ActiveConnections == 2 }) {
		t.Fatalf("active = %d after a close, want 2", srv.Stats().ActiveConnections)
	}
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial after a slot freed: %v", err)
	}
	conns = append(conns, conn)
}

func TestOversizedMessageIsRejected(t *testing.T) {
	sink := &collectingSink{}
	srv, url := newServer(t, sink, ingest.Config{MaxMessageBytes: 256})

	conn := dial(t, url)

	// A frame far above the limit. gorilla closes the connection on a read-limit
	// breach, which is the correct response: the client is not speaking the
	// protocol we agreed on.
	huge := make([]byte, 64*1024)
	for i := range huge {
		huge[i] = 'x'
	}
	_ = conn.WriteMessage(websocket.TextMessage, huge)

	if !waitFor(3*time.Second, func() bool { return srv.Stats().ActiveConnections == 0 }) {
		t.Fatal("oversized message did not close the connection")
	}
	if sink.count() != 0 {
		t.Errorf("sink received %d pings from an oversized frame", sink.count())
	}
}

func TestMalformedPingsAreCountedNotFatal(t *testing.T) {
	// A single bad frame must not cost a driver their GPS stream — but it must
	// be visible, or a permanently broken client looks like a quiet one.
	sink := &collectingSink{}
	srv, url := newServer(t, sink, ingest.Config{})

	conn := dial(t, url)

	if err := conn.WriteMessage(websocket.TextMessage, []byte("{not json")); err != nil {
		t.Fatalf("write: %v", err)
	}
	sendPing(t, conn, ingest.Ping{DriverID: "", Lat: 12.97, Lng: 77.59})    // no id
	sendPing(t, conn, ingest.Ping{DriverID: "D-1", Lat: 999, Lng: 77.59})   // bad lat
	sendPing(t, conn, ingest.Ping{DriverID: "D-1", Lat: 12.97, Lng: 77.59}) // valid

	if !waitFor(2*time.Second, func() bool { return sink.count() == 1 }) {
		t.Fatalf("sink got %d pings, want exactly the 1 valid one", sink.count())
	}
	if !waitFor(2*time.Second, func() bool { return srv.Stats().PingsRejected == 3 }) {
		t.Errorf("rejected = %d, want 3", srv.Stats().PingsRejected)
	}
	if srv.Stats().ActiveConnections != 1 {
		t.Error("connection was closed by a malformed frame; it should survive")
	}
}

func TestSinkErrorsDoNotKillTheConnection(t *testing.T) {
	// Redis being down must not disconnect every driver in the city. The pings
	// are lost, which is bad; dropping the connections too would turn a
	// recoverable dependency failure into a reconnect storm.
	sink := &collectingSink{err: fmt.Errorf("redis is down")}
	srv, url := newServer(t, sink, ingest.Config{})

	conn := dial(t, url)
	sendPing(t, conn, ingest.Ping{DriverID: "D-1", Lat: 12.97, Lng: 77.59})

	if !waitFor(2*time.Second, func() bool { return srv.Stats().PingsReceived == 1 }) {
		t.Fatal("ping was not counted")
	}
	time.Sleep(100 * time.Millisecond)
	if srv.Stats().ActiveConnections != 1 {
		t.Error("a sink error closed the connection")
	}
}

func TestShutdownClosesConnectionsAndWaits(t *testing.T) {
	sink := &collectingSink{}
	srv, url := newServer(t, sink, ingest.Config{})

	for i := 0; i < 5; i++ {
		conn := dial(t, url)
		sendPing(t, conn, ingest.Ping{DriverID: fmt.Sprintf("D-%d", i), Lat: 12.97, Lng: 77.59})
	}
	if !waitFor(2*time.Second, func() bool { return srv.Stats().ActiveConnections == 5 }) {
		t.Fatalf("active = %d, want 5", srv.Stats().ActiveConnections)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shutdown must WAIT for the goroutines, not merely signal them. Returning
	// early would report a clean exit while connections were still writing.
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if n := srv.Stats().ActiveConnections; n != 0 {
		t.Fatalf("active = %d after Shutdown returned, want 0", n)
	}

	// And it must refuse new connections afterwards.
	if _, _, err := websocket.DefaultDialer.Dial(url, nil); err == nil {
		t.Error("server accepted a connection after shutdown")
	}

	// Idempotent: a second call must not panic on a closed channel.
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("second shutdown: %v", err)
	}
}

func TestConcurrentClientsAreIsolated(t *testing.T) {
	// Run under -race, this is what proves the counters and the sink are
	// actually safe to share across one goroutine per connection.
	sink := &collectingSink{}
	srv, url := newServer(t, sink, ingest.Config{MaxConnections: 100})

	const clients, perClient = 25, 20
	var wg sync.WaitGroup
	var failures atomic.Int64

	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			conn, _, err := websocket.DefaultDialer.Dial(url, nil)
			if err != nil {
				failures.Add(1)
				return
			}
			defer conn.Close()
			for i := 0; i < perClient; i++ {
				data, _ := json.Marshal(ingest.Ping{
					DriverID: fmt.Sprintf("D-%d", c),
					Lat:      12.97 + float64(i)*0.0001,
					Lng:      77.59,
				})
				if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
					failures.Add(1)
					return
				}
			}
			// Give the server a moment to drain before closing, or the last
			// writes race the close and the count is legitimately short.
			time.Sleep(200 * time.Millisecond)
		}(c)
	}
	wg.Wait()

	if n := failures.Load(); n != 0 {
		t.Fatalf("%d client failures", n)
	}
	want := clients * perClient
	if !waitFor(5*time.Second, func() bool { return sink.count() == want }) {
		t.Fatalf("sink got %d pings, want %d", sink.count(), want)
	}
	if s := srv.Stats(); s.PingsReceived != int64(want) {
		t.Errorf("PingsReceived = %d, want %d", s.PingsReceived, want)
	}
}

func TestConfigDefaultsAreSane(t *testing.T) {
	cfg := ingest.DefaultConfig()
	// The ratio that matters: pinging slower than the read deadline means the
	// server times out clients it never gave a chance to answer.
	if cfg.PingInterval >= cfg.PongWait {
		t.Fatalf("PingInterval (%v) must be shorter than PongWait (%v)",
			cfg.PingInterval, cfg.PongWait)
	}
	if cfg.MaxMessageBytes <= 0 || cfg.MaxConnections <= 0 {
		t.Error("limits must be positive by default; unbounded is not a default")
	}
}
