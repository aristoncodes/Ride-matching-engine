package results

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/aditya/ride-matching/internal/redisconn"
)

// RedisStore keeps outcomes in one Redis hash per request.
//
// # Key layout
//
//	results:<tenant>:<request_id>   HASH, with a TTL
//
// The tenant is IN THE KEY rather than a field checked after reading. That is
// the isolation boundary: a cross-tenant read addresses a key that does not
// exist, so it cannot leak a driver id, a coordinate, or even the fact that
// the request is real. Storing one flat `results:<request_id>` hash with a
// tenant field would work right up until a handler forgot the comparison —
// and the whole point of internal/tenancy is that isolation must not depend on
// every future handler remembering.
//
// # Why a TTL and not forever
//
// An outcome is a receipt a client polls for within seconds or minutes, not a
// trip history — there is no reporting, billing, or audit feature that reads
// it later. Without an expiry this set grows without bound in a Redis that is
// also capped at 512mb by REDIS_MAXMEMORY, and the eviction that follows would
// take the API keys and the durable queue down with it. A day is generous for
// polling and bounded for capacity.
type RedisStore struct {
	client *redis.Client
	opts   Options
}

// Options configures the store.
type Options struct {
	// TTL is how long a recorded outcome stays readable.
	TTL time.Duration

	// PoolSize caps concurrent connections.
	PoolSize int

	// Now is injectable so expiry tests need not sleep in real time.
	Now func() time.Time
}

// DefaultOptions are production-shaped defaults.
func DefaultOptions() Options {
	return Options{
		TTL:      24 * time.Hour,
		PoolSize: 20,
		Now:      time.Now,
	}
}

func (o *Options) applyDefaults() {
	d := DefaultOptions()
	if o.TTL <= 0 {
		o.TTL = d.TTL
	}
	if o.PoolSize <= 0 {
		o.PoolSize = d.PoolSize
	}
	if o.Now == nil {
		o.Now = d.Now
	}
}

// NewRedis connects to Redis. `addr` may be "host:port" or a unix socket path.
func NewRedis(addr string, opts Options) (*RedisStore, error) {
	opts.applyDefaults()

	network := "tcp"
	if len(addr) > 0 && addr[0] == '/' {
		network = "unix"
	}

	client := redis.NewClient(&redis.Options{
		Network:      network,
		Addr:         addr,
		Password:     redisconn.Password(),
		PoolSize:     opts.PoolSize,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
	})

	return &RedisStore{client: client, opts: opts}, nil
}

// Ping verifies connectivity, so a bad address is one clear startup error
// rather than a trickle of failures under load.
func (s *RedisStore) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }

func key(tenantID, requestID string) string {
	return "results:" + tenantID + ":" + requestID
}

// Put writes the outcome and refreshes its TTL.
func (s *RedisStore) Put(ctx context.Context, out Outcome) error {
	if out.RequestID == "" {
		return ErrEmptyRequestID
	}
	if out.TenantID == "" {
		return ErrEmptyTenantID
	}
	if out.UpdatedAt.IsZero() {
		out.UpdatedAt = s.opts.Now()
	}

	fields := []any{
		"status", string(out.Status),
		"straight_line_meters", strconv.FormatFloat(out.StraightLineMeters, 'f', -1, 64),
		"reason", out.Reason,
		"attempts", strconv.Itoa(out.Attempts),
		"updated_at", strconv.FormatInt(out.UpdatedAt.UnixMilli(), 10),
	}
	if out.DriverID != "" {
		fields = append(fields, "driver_id", out.DriverID)
	}
	if out.ETASeconds != nil {
		fields = append(fields, "eta_seconds", strconv.Itoa(int(*out.ETASeconds)))
	}

	k := key(out.TenantID, out.RequestID)

	// HSET and EXPIRE in one pipeline. Two round trips would leave a window in
	// which a crash between them stores an outcome with no expiry — a key that
	// then lives forever, which is precisely what the TTL exists to prevent.
	//
	// HSET rather than DEL+HSET, so an UNMATCHED record that had no driver_id
	// cannot resurrect a stale one: the only transition that sets driver_id is
	// into MATCHED, and MATCHED is terminal.
	// PExpire rather than Expire: EXPIRE truncates to whole seconds, so a
	// sub-second TTL would be silently rounded up to 1s. Production TTLs are
	// hours and would never notice, which is exactly why the rounding would
	// go unnoticed until something short-lived depended on it.
	pipe := s.client.TxPipeline()
	pipe.HSet(ctx, k, fields...)
	pipe.PExpire(ctx, k, s.opts.TTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("results: put %s: %w", out.RequestID, err)
	}
	return nil
}

// Get reads one outcome, scoped to the tenant.
func (s *RedisStore) Get(ctx context.Context, tenantID, requestID string) (Outcome, error) {
	if requestID == "" {
		return Outcome{}, ErrEmptyRequestID
	}
	if tenantID == "" {
		return Outcome{}, ErrEmptyTenantID
	}

	raw, err := s.client.HGetAll(ctx, key(tenantID, requestID)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return Outcome{}, fmt.Errorf("results: get %s: %w", requestID, err)
	}
	// A missing key and an empty hash are the same thing in Redis: HGETALL
	// returns an empty map rather than redis.Nil, so absence must be checked
	// on the map, not on the error.
	if len(raw) == 0 {
		return Outcome{}, ErrNotFound
	}

	out := Outcome{
		RequestID: requestID,
		TenantID:  tenantID,
		Status:    Status(raw["status"]),
		DriverID:  raw["driver_id"],
		Reason:    raw["reason"],
	}
	// Parse errors are deliberately not fatal. These fields are informational;
	// failing the whole read because one of them is unparseable would turn a
	// cosmetic problem into "the rider cannot see their driver".
	out.StraightLineMeters, _ = strconv.ParseFloat(raw["straight_line_meters"], 64)
	out.Attempts, _ = strconv.Atoi(raw["attempts"])
	if ms, convErr := strconv.ParseInt(raw["updated_at"], 10, 64); convErr == nil {
		out.UpdatedAt = time.UnixMilli(ms)
	}
	if v, ok := raw["eta_seconds"]; ok {
		if n, convErr := strconv.Atoi(v); convErr == nil {
			eta := int32(n)
			out.ETASeconds = &eta
		}
	}
	return out, nil
}

// Close releases the connection pool.
func (s *RedisStore) Close() error { return s.client.Close() }
