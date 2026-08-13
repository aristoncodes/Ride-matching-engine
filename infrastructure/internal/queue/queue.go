// Package queue is the durable buffer between the rider-facing API and the
// match batcher.
//
// The asymmetry with driver locations is the point. GPS pings are STATE: only
// the latest matters, so the Week 9 pipeline coalesces and sheds them freely.
// Ride requests are EVENTS: every one is a customer waiting on a street corner,
// so none may be silently dropped. Getting these two backwards is a serious
// design error in either direction.
//
// Delivery is AT-LEAST-ONCE, and that is a deliberate choice rather than a
// limitation we tolerate. Exactly-once across a process boundary is not
// available without distributed transactions; what is available is
// at-least-once plus idempotent consumers, which is why every message carries
// an id the consumer can deduplicate on.
package queue

import (
	"context"
	"errors"
	"time"
)

// RideRequest is one rider asking for a car.
type RideRequest struct {
	// RequestID is the idempotency key. Set by the API (Week 11) and carried
	// end to end, so a redelivered message is recognisable as the same request
	// rather than a second customer.
	RequestID string

	TenantID string
	RiderID  string
	Lat      float64
	Lng      float64

	// RequestedAt is when the API accepted it — server clock, never the
	// client's. Used to measure queue latency and to expire requests a rider
	// has long since given up on.
	RequestedAt time.Time

	// MatchAttempts counts how many batches have TRIED and failed to find this
	// rider a driver.
	//
	// Deliberately separate from Message.Deliveries, and the distinction is a
	// correctness one that a chaos test caught the hard way:
	//
	//	Deliveries    = infrastructure retries. "the consumer died holding this"
	//	MatchAttempts = product outcome.       "we looked and there was no car"
	//
	// Both look identical to a broker — an un-acked message either way — so
	// without this field a rider who simply cannot find a driver accumulates
	// deliveries and is eventually dead-lettered AS POISON. That silently drops
	// a real customer, which is exactly what the durability work exists to
	// prevent. See Republish.
	MatchAttempts int
}

// Message is a RideRequest plus its delivery metadata.
type Message struct {
	// ID is the broker's own id, distinct from RequestID. Acking needs the
	// broker id; deduplication needs the business id. Conflating them breaks
	// the moment a message is redelivered with a new broker id.
	ID string

	Request RideRequest

	// Deliveries is how many times this message has been handed out, including
	// the current attempt. A first delivery is 1. This is what makes the
	// dead-letter decision possible: a message that keeps coming back is
	// poison, whatever its payload claims.
	Deliveries int64
}

// Queue is a durable, at-least-once ride-request queue.
//
// Defined before any implementation so the batcher (Week 12) can be tested
// against an in-memory fake with no Redis at all — the same discipline that let
// the Week 9 pipeline be tested without a location store.
type Queue interface {
	// Publish appends a request. It returns once the broker has it durably
	// enough that a consumer crash cannot lose it.
	Publish(ctx context.Context, req RideRequest) (messageID string, err error)

	// Consume claims up to `max` messages for this consumer, blocking up to
	// `block` for at least one to arrive.
	//
	// Claimed messages are NOT removed. They sit in a pending list belonging to
	// this consumer until acked — which is exactly what makes redelivery
	// possible when the consumer dies mid-work.
	Consume(ctx context.Context, max int, block time.Duration) ([]Message, error)

	// Ack marks messages as successfully processed and removes them from the
	// pending list. Not acking is how work survives a crash, so acking must
	// happen only after the work is genuinely done.
	Ack(ctx context.Context, messageIDs ...string) error

	// Reclaim takes over messages that another consumer claimed and never
	// acked for longer than minIdle, and returns them for reprocessing.
	//
	// This is the actual crash-recovery mechanism. Without someone calling it,
	// a dead consumer's in-flight messages sit pending forever: durably stored,
	// and never delivered to anyone. Durability without reclaim is just a
	// slower way to lose the request.
	Reclaim(ctx context.Context, minIdle time.Duration, max int) ([]Message, error)

	// Republish returns a request to the queue for a LATER window, with its
	// match-attempt count incremented, and acks the original.
	//
	// This is how "we could not match you yet" is expressed, as distinct from
	// "the consumer crashed". Leaving the message pending instead would work
	// once and then accumulate delivery counts until the poison detector threw
	// a perfectly valid rider away.
	//
	// Ordering is publish-then-ack, deliberately: a crash in between duplicates
	// the request, which RequestID makes recoverable, whereas ack-then-publish
	// would lose it outright.
	Republish(ctx context.Context, msg Message) error

	// DeadLetter moves a message aside permanently, with a reason.
	// Acknowledged as part of the same operation, so a poison message cannot be
	// both dead-lettered and left pending.
	DeadLetter(ctx context.Context, msg Message, reason string) error

	// Depth is the number of messages not yet delivered. Pending returns the
	// number claimed but not yet acked. The two together are the queue's
	// health: a growing Depth means consumers are too slow; a growing Pending
	// means they are failing or dying.
	Depth(ctx context.Context) (int64, error)
	Pending(ctx context.Context) (int64, error)
	DeadLetterDepth(ctx context.Context) (int64, error)

	Close() error
}

var (
	// ErrEmptyRequestID is returned by Publish when the idempotency key is
	// missing. Rejected at the boundary because a request with no id cannot be
	// deduplicated on redelivery, and at-least-once delivery makes that
	// mandatory rather than nice to have.
	ErrEmptyRequestID = errors.New("queue: RequestID must be set")

	// ErrMalformedMessage indicates a message that cannot be decoded. It is
	// dead-lettered immediately rather than retried — a payload that failed to
	// parse will fail to parse identically on every redelivery, and retrying it
	// is how a queue stalls.
	ErrMalformedMessage = errors.New("queue: malformed message")
)
