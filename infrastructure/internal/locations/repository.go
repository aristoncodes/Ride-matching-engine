// Package locations is the live store of where drivers are right now.
//
// It is defined as an INTERFACE first and a Redis implementation second, and
// that ordering is the point: everything upstream (the WebSocket ingestor, the
// batcher) depends on the interface, so it can be tested against an in-memory
// fake with no Redis at all, and the store can be swapped later without those
// callers noticing.
package locations

import (
	"context"
	"errors"
	"time"
)

// DriverLocation is one driver's last known position.
type DriverLocation struct {
	// TenantID identifies which operator's fleet this driver belongs to. Set by
	// the ingestion layer from the authenticated API key; used to route the
	// write to the right tenant's keys.
	TenantID string

	DriverID string
	Lat      float64
	Lng      float64

	// How far this driver is from the query point. Set by Nearby, meaningless
	// otherwise — the store does not track distance, it computes it per query.
	DistanceMeters float64

	// When this position was recorded. Exposed because "how stale is this?" is
	// a question the caller often needs to answer differently than the store's
	// single TTL allows.
	LastSeen time.Time
}

// Query bounds a radius search.
type Query struct {
	Lat    float64
	Lng    float64
	Radius float64 // metres

	// Cap on returned drivers, nearest first. Required in practice: an
	// unbounded radius query in a dense city returns thousands of drivers,
	// and the matcher only ever wanted the closest handful anyway.
	Limit int
}

// Repository is the live driver-location store.
//
// Implementations must be safe for concurrent use — the ingestion layer writes
// from one goroutine per connection while the batcher reads from another.
type Repository interface {
	// UpsertDriver records a driver's current position, refreshing its
	// freshness clock. Called on every GPS ping, so it is the hottest write in
	// the system.
	UpsertDriver(ctx context.Context, driverID string, lat, lng float64) error

	// UpsertMany records a batch of pings in one round trip. The ingestion
	// layer batches pings before writing; doing them one at a time would make
	// network latency, not Redis, the bottleneck.
	UpsertMany(ctx context.Context, locations []DriverLocation) error

	// Nearby returns drivers within the radius, nearest first, EXCLUDING any
	// whose last ping is older than the configured TTL.
	//
	// That exclusion is the whole reason this interface exists rather than raw
	// GEOSEARCH: a driver who stopped pinging ten minutes ago is still sitting
	// in the geo index at their last known spot, and matching a rider to them
	// means dispatching a car that is not there.
	Nearby(ctx context.Context, q Query) ([]DriverLocation, error)

	// RemoveDriver takes a driver out of the index entirely — going off shift,
	// or accepting a ride and becoming unavailable.
	RemoveDriver(ctx context.Context, driverID string) error

	// Reap deletes entries older than the TTL and returns how many went.
	// Filtering on read keeps stale drivers out of ANSWERS; reaping keeps them
	// out of MEMORY. Both are needed: without the reaper the index grows
	// forever with drivers who never come back.
	Reap(ctx context.Context) (int, error)

	// Count is the number of drivers currently indexed, fresh or not. Used by
	// the reaper's metrics and by tests.
	Count(ctx context.Context) (int, error)

	Close() error
}

// ErrNotFound is returned when a specific driver is asked for and is absent.
var ErrNotFound = errors.New("driver not found")
