// Package ingest accepts live GPS streams from drivers over WebSockets.
//
// This is the part of the system that faces thousands of phones on flaky
// mobile networks, so almost all of the code here is about what happens when a
// client misbehaves or vanishes rather than about the happy path. The three
// things that actually matter:
//
//	DEAD CLIENTS   TCP will happily hold a "connection" open long after the
//	               phone is in a tunnel or the app was force-quit. Only an
//	               application-level heartbeat plus read deadlines detect it.
//	LIMITS         One abusive client must not be able to exhaust the process.
//	               Message size and connection count are both bounded.
//	NO LEAKS       Every connection owns exactly two goroutines, and both must
//	               exit when the connection does. A leak here is invisible for
//	               hours and then fatal.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/aditya/ride-matching/internal/auth"
)

// Ping is one GPS report from a driver.
type Ping struct {
	// TenantID is set by the SERVER from the authenticated API key, never read
	// from the client's message. A driver app that could name its own tenant
	// could write into another operator's fleet.
	TenantID string `json:"-"`

	DriverID string  `json:"driver_id"`
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
	// Client clock, optional. Never trusted for freshness decisions: phones
	// have wrong clocks, and a driver whose clock is an hour fast would
	// otherwise look permanently fresh. The server stamps its own arrival time.
	SentAtUnixMs int64 `json:"sent_at_ms,omitempty"`
}

// Sink receives validated pings. Week 9 wires this to the Redis location store;
// keeping it an interface means the WebSocket layer can be tested with no
// storage at all, and neither layer knows how the other works.
type Sink interface {
	Accept(ctx context.Context, p Ping) error
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(ctx context.Context, p Ping) error

func (f SinkFunc) Accept(ctx context.Context, p Ping) error { return f(ctx, p) }

// Config tunes the server. Zero values are replaced by the defaults below.
type Config struct {
	// MaxConnections caps concurrent clients. Beyond it the server refuses with
	// 503 rather than accepting a connection it cannot serve — degrading
	// honestly beats accepting everyone and slowing down for everyone.
	MaxConnections int

	// MaxMessageBytes bounds a single frame. Without it a client can announce a
	// 2 GB message and the server will dutifully try to buffer it.
	MaxMessageBytes int64

	// PongWait is how long a connection may go silent before it is closed.
	// This is the actual dead-client detector: the read deadline is pushed
	// forward on every pong, so a client that stops answering is dropped even
	// though TCP still believes the socket is fine.
	PongWait time.Duration

	// PingInterval must be meaningfully shorter than PongWait, or the server
	// times out a healthy client between its own pings. The conventional ratio
	// is ~9/10, which tolerates losing one ping.
	PingInterval time.Duration

	// WriteWait bounds a single write. A client that stops reading applies TCP
	// backpressure, and without a deadline the write blocks forever, pinning
	// its goroutine and the buffer it holds.
	WriteWait time.Duration

	Logger *slog.Logger

	// Keys enables API-key authentication on the upgrade (Week 18). Nil
	// disables auth, which is fine for local development and is logged loudly.
	Keys auth.Store

	// TenantID is the fallback when Keys is nil.
	TenantID string

	// Now is injectable for tests.
	Now func() time.Time
}

// DefaultConfig returns production-shaped defaults.
func DefaultConfig() Config {
	return Config{
		MaxConnections:  10000,
		MaxMessageBytes: 4096, // a GPS ping is ~100 bytes; 4 KB is generous
		PongWait:        60 * time.Second,
		PingInterval:    54 * time.Second, // 90% of PongWait
		WriteWait:       10 * time.Second,
		Logger:          slog.Default(),
	}
}

func (c *Config) applyDefaults() {
	d := DefaultConfig()
	if c.MaxConnections <= 0 {
		c.MaxConnections = d.MaxConnections
	}
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = d.MaxMessageBytes
	}
	if c.PongWait <= 0 {
		c.PongWait = d.PongWait
	}
	if c.PingInterval <= 0 {
		c.PingInterval = c.PongWait * 9 / 10
	}
	if c.WriteWait <= 0 {
		c.WriteWait = d.WriteWait
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.TenantID == "" {
		c.TenantID = "default"
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Stats are cumulative counters for observability.
type Stats struct {
	ActiveConnections   int64
	TotalConnections    int64
	RejectedConnections int64
	PingsReceived       int64
	PingsRejected       int64
}

// Server accepts driver GPS streams.
type Server struct {
	cfg      Config
	sink     Sink
	upgrader websocket.Upgrader

	// Counters are atomic rather than mutex-guarded: they are incremented on
	// every message from every connection, so this is the hottest shared state
	// in the process and the one place a lock would actually be felt.
	activeConns atomic.Int64
	totalConns  atomic.Int64
	rejected    atomic.Int64
	pings       atomic.Int64
	pingsBad    atomic.Int64

	// Closed to signal shutdown. Every connection selects on it, which is what
	// makes shutdown reach goroutines that are otherwise blocked on a channel.
	shutdown  chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// NewServer creates a server. It does not listen; mount Handler() on a mux.
func NewServer(sink Sink, cfg Config) *Server {
	cfg.applyDefaults()
	return &Server{
		cfg:  cfg,
		sink: sink,
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			ReadBufferSize:   1024,
			WriteBufferSize:  1024,
			// Origin checking is deliberately permissive here and MUST be
			// tightened before this is exposed publicly. Drivers connect from a
			// native app with an API key (Week 18), not from a browser, so the
			// real defence is authentication rather than the Origin header —
			// but leaving this unremarked is how it silently ships.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		shutdown: make(chan struct{}),
	}
}

// Stats snapshots the counters.
func (s *Server) Stats() Stats {
	return Stats{
		ActiveConnections:   s.activeConns.Load(),
		TotalConnections:    s.totalConns.Load(),
		RejectedConnections: s.rejected.Load(),
		PingsReceived:       s.pings.Load(),
		PingsRejected:       s.pingsBad.Load(),
	}
}

// Handler upgrades HTTP requests to WebSocket connections.
func (s *Server) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-s.shutdown:
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		default:
		}

		// Reserve a slot BEFORE upgrading. Checking after the upgrade means the
		// connection is already established and must then be torn down, which
		// is both wasteful and a worse client experience than a plain 503.
		if n := s.activeConns.Add(1); n > int64(s.cfg.MaxConnections) {
			s.activeConns.Add(-1)
			s.rejected.Add(1)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "connection limit reached", http.StatusServiceUnavailable)
			return
		}

		// Authenticate BEFORE upgrading. After the handshake the only way to
		// reject a client is a WebSocket close frame, which many clients
		// surface as a generic disconnect — a plain 401 is far easier for an
		// integrator to debug.
		tenantID := s.cfg.TenantID
		if s.cfg.Keys != nil {
			raw := auth.WebSocketKey(r)
			if raw == "" {
				s.activeConns.Add(-1)
				s.rejected.Add(1)
				http.Error(w, "an API key is required", http.StatusUnauthorized)
				return
			}
			key, err := auth.Verify(r.Context(), s.cfg.Keys, raw, s.cfg.Now())
			if err != nil {
				s.activeConns.Add(-1)
				s.rejected.Add(1)
				switch {
				case errors.Is(err, auth.ErrRateLimited):
					w.Header().Set("Retry-After", "60")
					http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				case errors.Is(err, auth.ErrInvalidKey):
					s.cfg.Logger.Warn("rejected driver stream", "remote", r.RemoteAddr)
					http.Error(w, "invalid API key", http.StatusUnauthorized)
				default:
					// The auth store is down — our fault, not the driver's.
					http.Error(w, "cannot verify credentials", http.StatusServiceUnavailable)
				}
				return
			}
			// The tenant is taken from the KEY. A driver app cannot name its
			// own tenant, so it cannot write into another operator's fleet.
			tenantID = key.TenantID
		}

		conn, err := s.upgrader.Upgrade(w, r, nil)
		if err != nil {
			// Upgrade already wrote a response; just release the slot.
			s.activeConns.Add(-1)
			return
		}

		s.totalConns.Add(1)
		s.wg.Add(1)
		go s.serve(conn, tenantID)
	}
}

// serve owns one connection for its entire lifetime.
func (s *Server) serve(conn *websocket.Conn, tenantID string) {
	defer s.wg.Done()
	defer s.activeConns.Add(-1)
	defer conn.Close()

	// Per-connection context, cancelled when either pump exits. This is what
	// couples the two goroutines' lifetimes: whichever notices the connection
	// is finished takes the other down with it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A blocked ReadMessage does NOT return when a context is cancelled --
	// gorilla/websocket has no context-aware read, and the goroutine sits in a
	// socket read until its deadline (up to PongWait, 60s) or until the
	// connection is closed under it.
	//
	// So closing the connection is the only thing that actually unblocks the
	// reader, and this goroutine is what does it. Without it, Shutdown() waits
	// a full PongWait per idle connection and times out -- which is exactly how
	// the shutdown test failed before this existed.
	//
	// It cannot leak: ctx is always cancelled by serve's deferred cancel(), on
	// every exit path.
	closer := make(chan struct{})
	go func() {
		defer close(closer)
		<-ctx.Done()
		_ = conn.Close()
	}()

	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() { defer pumps.Done(); s.writePump(ctx, cancel, conn) }()
	go func() { defer pumps.Done(); s.readPump(ctx, cancel, conn, tenantID) }()

	// Waiting here, rather than returning immediately, is what makes
	// Shutdown() able to guarantee that no goroutines remain.
	pumps.Wait()
	cancel()
	<-closer
}

// readPump is the ONLY goroutine that reads from conn. gorilla/websocket
// permits one concurrent reader and one concurrent writer, and no more —
// violating that is a data race, not a queue.
func (s *Server) readPump(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, tenantID string) {
	defer cancel()

	conn.SetReadLimit(s.cfg.MaxMessageBytes)

	// The deadline is absolute, so it must be pushed forward on every sign of
	// life. Setting it once would disconnect every client after PongWait
	// regardless of how healthy they are.
	_ = conn.SetReadDeadline(time.Now().Add(s.cfg.PongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(s.cfg.PongWait))
	})

	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			// Normal closes are not worth logging; everything else might be.
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseNormalClosure, websocket.CloseGoingAway,
				websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure) {
				s.cfg.Logger.Debug("websocket read ended", "err", err)
			}
			return
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}

		// Any traffic proves the client is alive, not only pongs.
		_ = conn.SetReadDeadline(time.Now().Add(s.cfg.PongWait))

		var p Ping
		if err := json.Unmarshal(data, &p); err != nil {
			// Counted and dropped, NOT fatal to the connection. A driver app
			// that emits one malformed frame during an upgrade should not lose
			// its GPS stream; a client that only ever sends garbage shows up in
			// the PingsRejected counter instead.
			s.pingsBad.Add(1)
			continue
		}
		if err := validate(p); err != nil {
			s.pingsBad.Add(1)
			continue
		}

		// Stamped from the AUTHENTICATED connection, discarding whatever the
		// client may have put in the payload.
		p.TenantID = tenantID

		s.pings.Add(1)
		if s.sink != nil {
			// The sink gets the CONNECTION's context, so a shutdown or a dead
			// client cancels in-flight storage work instead of letting it run on.
			if err := s.sink.Accept(ctx, p); err != nil {
				s.cfg.Logger.Warn("sink rejected ping", "driver", p.DriverID, "err", err)
			}
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// writePump is the ONLY goroutine that writes to conn.
//
// Today it writes nothing but heartbeats and the closing handshake — ingestion
// is one-directional. It exists as a separate goroutine anyway because
// gorilla/websocket allows exactly one concurrent writer, so the moment
// anything else needs to send (pushing a match to a driver, a later week),
// there is one obvious place for it to go rather than a race waiting to happen.
func (s *Server) writePump(ctx context.Context, cancel context.CancelFunc,
	conn *websocket.Conn) {
	defer cancel()

	ticker := time.NewTicker(s.cfg.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Try to close politely, but never block on it: a client that has
			// stopped reading would otherwise hold this goroutine open for as
			// long as it likes, which is the leak we are here to prevent.
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return

		case <-s.shutdown:
			// Tell the client why, so a driver app can reconnect immediately
			// instead of treating it as an error and backing off.
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseServiceRestart, "shutting down"))
			return

		case <-ticker.C:
			// The heartbeat. If this write fails, or the client never pongs,
			// readPump's deadline fires and the connection is torn down.
			_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func validate(p Ping) error {
	if p.DriverID == "" {
		return errors.New("empty driver_id")
	}
	if len(p.DriverID) > 128 {
		return errors.New("driver_id too long")
	}
	if p.Lat < -90 || p.Lat > 90 || p.Lng < -180 || p.Lng > 180 {
		return fmt.Errorf("coordinates out of range: %f, %f", p.Lat, p.Lng)
	}
	return nil
}

// Shutdown stops accepting connections, closes the existing ones, and waits
// for every connection goroutine to exit or for ctx to expire.
//
// Waiting is the whole point. Returning as soon as the listener closes would
// report "shut down" while goroutines were still running and still writing to
// Redis — which is how a process that "exited cleanly" corrupts its last batch.
func (s *Server) Shutdown(ctx context.Context) error {
	s.closeOnce.Do(func() { close(s.shutdown) })

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown timed out with %d connections still active: %w",
			s.activeConns.Load(), ctx.Err())
	}
}
