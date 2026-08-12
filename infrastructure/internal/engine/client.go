// Package engine is the Go side of the bridge to the C++ matching engine.
//
// The engine runs as a SEPARATE PROCESS behind gRPC (ADR-0002), so this package
// is where "the brain might die" becomes an ordinary error value rather than a
// crashed Go process. Everything here exists to make that boundary safe:
// every call is bounded, every failure is classified, and no call can block the
// caller indefinitely.
package engine

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	matchingv1 "github.com/aditya/ride-matching/gen/matching/v1"
)

// DefaultTimeout bounds a single SolveBatch call.
//
// Sized against the batch window, not against the engine's speed: batches
// arrive every 3 seconds, so a call that has not returned in 2 has already lost
// its window and is now competing with the next one. Failing fast and shedding
// is better than a queue of calls that are each individually "nearly done".
//
// For scale: the engine solves 100x100 in ~0.4 ms euclidean, so this is roughly
// 5000x the expected cost. It is a runaway detector, not a performance budget.
const DefaultTimeout = 2 * time.Second

// Client is a bounded, classified wrapper over the generated gRPC stub.
//
// Safe for concurrent use: grpc.ClientConn multiplexes over one HTTP/2
// connection and is explicitly designed to be shared, so the batcher can call
// SolveBatch from many goroutines without a pool of its own.
type Client struct {
	conn    *grpc.ClientConn
	api     matchingv1.MatchingEngineClient
	timeout time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithTimeout overrides the per-call deadline.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// Dial connects to the engine.
//
// Deliberately does NOT wait for the connection to be ready. gRPC connects
// lazily and reconnects on its own, so blocking here would only move the
// failure from a call site (which has a deadline and a retry policy) into
// startup (which usually has neither). A dead engine surfaces as
// ErrEngineUnavailable on the first call — which is exactly where the caller is
// already prepared to handle it.
func Dial(target string, opts ...Option) (*Client, error) {
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// Keepalives are how a HALF-OPEN connection is discovered. If the
		// engine's host vanishes (killed container, severed network) TCP can
		// hold the socket open indefinitely, and calls would hang until their
		// deadline instead of failing immediately.
		grpc.WithDefaultServiceConfig(`{
			"methodConfig": [{
				"name": [{"service": "matching.v1.MatchingEngine"}],
				"waitForReady": false
			}]
		}`),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", target, err)
	}

	c := &Client{conn: conn, api: matchingv1.NewMatchingEngineClient(conn), timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Close releases the connection.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// SolveBatch sends one batch and returns the optimal assignment.
//
// The passed context is honoured for cancellation, and a deadline is imposed on
// top of it. Both matter, and they are not the same thing:
//   - the DEADLINE stops a hung engine from pinning this goroutine forever;
//   - CANCELLATION lets the caller abandon work it no longer needs (its own
//     request was dropped, the process is shutting down) without waiting out
//     the deadline.
//
// If the caller's context already has an earlier deadline, that one wins —
// context.WithTimeout never extends an existing deadline.
func (c *Client) SolveBatch(ctx context.Context, req *matchingv1.MatchBatchRequest) (*matchingv1.MatchBatchResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil request", ErrInvalidBatch)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.api.SolveBatch(ctx, req)
	if err != nil {
		return nil, classify(err)
	}
	return resp, nil
}

// Health probes the engine. Used by readiness checks, which need to know not
// just that the process answers but WHICH road graphs it can actually serve.
func (c *Client) Health(ctx context.Context) (*matchingv1.HealthResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.api.Health(ctx, &matchingv1.HealthRequest{})
	if err != nil {
		return nil, classify(err)
	}
	return resp, nil
}

// HasGraph reports whether the engine currently serves a given road graph.
// A travel-time batch sent without this check fails one request at a time;
// checking once at startup turns that into a single clear message.
func (c *Client) HasGraph(ctx context.Context, graphID string) (bool, error) {
	resp, err := c.Health(ctx)
	if err != nil {
		return false, err
	}
	for _, g := range resp.GetLoadedGraphs() {
		if g.GetRoadGraphId() == graphID {
			return true, nil
		}
	}
	return false, nil
}
