// Package results is the terminal-state store for ride requests: the answer a
// rider eventually gets back.
//
// # Why this exists separately from the queue
//
// The queue is about WORK — a message lives there while something still has to
// be done about it, and it is deleted the moment that work completes. That is
// exactly the right lifecycle for a task and exactly the wrong one for an
// answer: acking a matched request removes the only record that it was ever
// matched, so the rider who asked "did I get a car?" has nothing to read.
//
// Before this package, the batcher computed an optimal assignment, acked the
// request, and dropped the assignment on the floor. The REST contract in
// docs/api/rest-openapi.yaml documented `GET /ride-requests/{id}` returning a
// match, and it could not be implemented, because the data no longer existed
// anywhere by the time anyone could ask for it.
//
// # What this is NOT
//
// This is not a dispatch or offer-negotiation protocol (offer, 15s accept
// window, first-accept-wins, fall through to the next driver). That remains
// deliberately out of scope — see docs/Scope_and_Production_Comparison.md.
// This is the strictly smaller thing: the assignment the engine already
// produced, durably readable by the client that asked for it.
package results

import (
	"context"
	"errors"
	"time"
)

// Status is where a ride request ended up.
//
// Deliberately only the states this system can actually OBSERVE. An earlier
// draft of the OpenAPI schema also listed BATCHED and CANCELLED; neither was
// ever writable — BATCHED is a sub-second transient nobody can usefully poll
// for, and cancellation was never built. Publishing a status a client can
// branch on but never receive is a contract that lies.
type Status string

const (
	// StatusPending means accepted and queued, not yet resolved.
	StatusPending Status = "PENDING"

	// StatusMatched means a driver was assigned. Terminal.
	StatusMatched Status = "MATCHED"

	// StatusUnmatched means the system looked, repeatedly, and there was no
	// car. Terminal, and a legitimate product answer rather than a fault.
	StatusUnmatched Status = "UNMATCHED"

	// StatusFailed means the request could not be processed at all — the
	// poison path. Terminal, and an operational fault rather than an answer.
	StatusFailed Status = "FAILED"
)

// Outcome is one request's current state, plus the match if there is one.
type Outcome struct {
	RequestID string
	TenantID  string
	Status    Status

	// DriverID is set only when Status is MATCHED.
	DriverID string

	// ETASeconds is how long the driver takes to reach the rider. Set only
	// under TRAVEL_TIME pricing, mirroring the engine's `optional` on the
	// wire: under EUCLIDEAN the engine has no basis for an ETA, and reporting
	// a default 0 would read as "arrives immediately".
	ETASeconds *int32

	// StraightLineMeters is always set on a match. It is what makes the
	// response auditable — an ETA of 40s across a 6km gap is visibly wrong.
	StraightLineMeters float64

	// Reason explains a non-MATCHED terminal state in words, e.g. "no driver
	// found after 20 match attempts".
	Reason string

	// Attempts is how many match windows this request went through.
	Attempts int

	// UpdatedAt is the server clock at the last write.
	UpdatedAt time.Time
}

// Store records and retrieves request outcomes.
//
// Defined as an interface first, like locations.Repository and queue.Queue, so
// the API and batcher can be tested against the in-memory implementation with
// no Redis in the loop.
//
// Implementations must be safe for concurrent use.
type Store interface {
	// Put writes an outcome, replacing any previous one for the same request.
	//
	// Last-write-wins is correct here rather than merely convenient: delivery
	// is at-least-once, so a request whose ack was lost is re-matched, and the
	// newer assignment is the true one. It also means Put is idempotent, which
	// is what lets it sit on a retry path safely.
	Put(ctx context.Context, out Outcome) error

	// Get returns the outcome for a request WITHIN a tenant.
	//
	// The tenant is a parameter rather than a filter applied afterwards: it is
	// part of the key, so a caller holding tenant A's key cannot construct a
	// read of tenant B's request even by guessing its id. A cross-tenant read
	// is indistinguishable from a request that does not exist, which is the
	// intended answer — 404, never 403, because 403 confirms the id is real.
	Get(ctx context.Context, tenantID, requestID string) (Outcome, error)

	Close() error
}

var (
	// ErrNotFound means no outcome is recorded for that tenant and id.
	ErrNotFound = errors.New("results: no such request")

	// ErrEmptyRequestID rejects a write with no key, at the boundary.
	ErrEmptyRequestID = errors.New("results: RequestID must be set")

	// ErrEmptyTenantID rejects an unscoped write. An outcome with no tenant
	// would be unreadable by design — every read is tenant-scoped — so this is
	// a programming error worth failing loudly rather than storing.
	ErrEmptyTenantID = errors.New("results: TenantID must be set")
)
