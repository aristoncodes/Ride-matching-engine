package locations

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisRepository stores live driver locations in Redis.
//
// # Why two keys, not one
//
// Redis geo sets are sorted sets whose score encodes a geohash, and Redis has
// no per-MEMBER expiry — only per-key. So `EXPIRE` on the geo set would drop
// every driver in the city at once, which is useless.
//
// The fix is a companion sorted set holding each driver's last-ping timestamp
// as its score:
//
//	drivers:geo:<tenant>   ZSET, score = geohash   (written by GEOADD)
//	drivers:seen:<tenant>  ZSET, score = unix ms   (written by ZADD)
//
// Freshness is then a score range on the second key, which Redis answers in
// O(log N). Reads filter by it; a background Reap deletes from both. Filtering
// alone would leave dead drivers in memory forever; reaping alone would serve
// stale drivers in the window between sweeps. Both are needed.
type RedisRepository struct {
	client *redis.Client
	opts   Options
}

// Options configures the store.
type Options struct {
	// TenantID scopes the keys. Multi-tenancy is a Week 19 concern, but the
	// key layout has to allow for it now — retrofitting a prefix onto live
	// keys means a migration.
	TenantID string

	// TTL is how long after its last ping a driver stays matchable.
	//
	// Sized against the ping interval, not guessed: pings arrive every ~3 s, so
	// 30 s tolerates about ten consecutive losses. Too short and a driver in a
	// tunnel is dropped from the pool; too long and the matcher dispatches cars
	// that left minutes ago.
	TTL time.Duration

	// MaxRetries bounds retry-with-backoff on transient failures.
	MaxRetries int

	// PoolSize caps concurrent connections. go-redis pools by default; the
	// value matters because the ingestion layer can have thousands of
	// goroutines and each would otherwise be happy to open its own socket.
	PoolSize int

	// Now is injectable so freshness tests do not have to sleep in real time.
	Now func() time.Time
}

// DefaultOptions are sensible values for a single-tenant dev setup.
func DefaultOptions() Options {
	return Options{
		TenantID:   "default",
		TTL:        30 * time.Second,
		MaxRetries: 3,
		PoolSize:   50,
		Now:        time.Now,
	}
}

// NewRedis connects to Redis. `addr` may be "host:port" or a unix socket path.
func NewRedis(addr string, opts Options) (*RedisRepository, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.TTL <= 0 {
		return nil, errors.New("locations: TTL must be positive")
	}
	if opts.TenantID == "" {
		return nil, errors.New("locations: TenantID must be set")
	}

	network := "tcp"
	if len(addr) > 0 && addr[0] == '/' {
		network = "unix"
	}

	client := redis.NewClient(&redis.Options{
		Network:  network,
		Addr:     addr,
		PoolSize: opts.PoolSize,

		// go-redis retries internally too, but only for connection setup. The
		// command-level retry lives in withRetry below, where it can be
		// restricted to errors that are actually transient.
		MaxRetries: 0,

		// Without these a network partition leaves a goroutine blocked on a
		// socket read until the OS gives up, which can be minutes. The whole
		// point of a live location store is that it answers now or not at all.
		DialTimeout:  2 * time.Second,
		ReadTimeout:  1 * time.Second,
		WriteTimeout: 1 * time.Second,
	})

	return &RedisRepository{client: client, opts: opts}, nil
}

// Ping verifies connectivity. Called at startup so a misconfigured address is
// one clear error rather than a slow trickle of failures under load.
func (r *RedisRepository) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

func (r *RedisRepository) geoKey() string  { return "drivers:geo:" + r.opts.TenantID }
func (r *RedisRepository) seenKey() string { return "drivers:seen:" + r.opts.TenantID }

// retryable reports whether an error is worth another attempt.
//
// The distinction that matters: a dropped connection is transient, a WRONGTYPE
// is a bug. Retrying a bug just means failing three times as slowly, and
// retrying a context cancellation ignores the caller who already gave up.
func retryable(err error) bool {
	if err == nil || errors.Is(err, redis.Nil) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// go-redis reports server-side errors (WRONGTYPE, NOSCRIPT, OOM) as
	// RedisError. Those will not fix themselves on a retry.
	var redisErr redis.Error
	if errors.As(err, &redisErr) {
		return false
	}
	return true // network, timeout, pool exhaustion
}

// withRetry runs op with capped exponential backoff plus jitter.
//
// The jitter is not decoration. Without it, every goroutine that failed at the
// same instant — which is what a Redis restart produces — retries at the same
// instant, and the recovering server is hit by a synchronised thundering herd
// exactly when it is least able to cope.
func (r *RedisRepository) withRetry(ctx context.Context, op func() error) error {
	var err error
	backoff := 10 * time.Millisecond

	for attempt := 0; attempt <= r.opts.MaxRetries; attempt++ {
		if err = op(); !retryable(err) {
			return err
		}
		if attempt == r.opts.MaxRetries {
			break
		}

		jitter := time.Duration(rand.Int63n(int64(backoff)))
		select {
		case <-ctx.Done():
			// Honour the caller's deadline instead of burning the rest of it
			// on sleeps for a result nobody is waiting for any more.
			return ctx.Err()
		case <-time.After(backoff + jitter):
		}
		if backoff *= 2; backoff > 500*time.Millisecond {
			backoff = 500 * time.Millisecond
		}
	}
	return fmt.Errorf("after %d retries: %w", r.opts.MaxRetries, err)
}

// UpsertDriver records one ping.
func (r *RedisRepository) UpsertDriver(ctx context.Context, driverID string, lat, lng float64) error {
	return r.UpsertMany(ctx, []DriverLocation{{DriverID: driverID, Lat: lat, Lng: lng}})
}

// UpsertMany records a batch of pings in a single round trip.
func (r *RedisRepository) UpsertMany(ctx context.Context, locs []DriverLocation) error {
	if len(locs) == 0 {
		return nil
	}

	now := r.opts.Now()
	geoLocs := make([]*redis.GeoLocation, 0, len(locs))
	seen := make([]redis.Z, 0, len(locs))
	for _, l := range locs {
		if l.DriverID == "" {
			return errors.New("locations: empty driver id")
		}
		if lat, lng := l.Lat, l.Lng; lat < -85.05112878 || lat > 85.05112878 ||
			lng < -180 || lng > 180 {
			// Redis rejects these itself, but with an opaque message. Catching
			// it here names the driver, which is what an operator needs.
			return fmt.Errorf("locations: driver %s has out-of-range coordinates (%f, %f)",
				l.DriverID, lat, lng)
		}
		ts := l.LastSeen
		if ts.IsZero() {
			ts = now
		}
		geoLocs = append(geoLocs, &redis.GeoLocation{Name: l.DriverID, Latitude: l.Lat, Longitude: l.Lng})
		seen = append(seen, redis.Z{Score: float64(ts.UnixMilli()), Member: l.DriverID})
	}

	return r.withRetry(ctx, func() error {
		// Pipelined so both keys are written in ONE round trip. They are not
		// atomic with respect to each other, and that is a deliberate, benign
		// choice: the only interleaving is a driver briefly present in the geo
		// index without a freshness score, which reads treat as stale and skip.
		// Failing closed like that is the right side to err on.
		pipe := r.client.Pipeline()
		pipe.GeoAdd(ctx, r.geoKey(), geoLocs...)
		pipe.ZAdd(ctx, r.seenKey(), seen...)
		_, err := pipe.Exec(ctx)
		return err
	})
}

// Nearby returns fresh drivers within the radius, nearest first.
func (r *RedisRepository) Nearby(ctx context.Context, q Query) ([]DriverLocation, error) {
	if q.Radius <= 0 {
		return nil, errors.New("locations: radius must be positive")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	// Over-fetch, because some of what comes back will be stale and dropped.
	// Asking for exactly `limit` would silently return fewer than requested
	// whenever the index holds dead drivers — worst precisely when the fleet is
	// churning and the caller most needs a full shortlist.
	fetch := limit * 3
	if fetch > 1000 {
		fetch = 1000
	}

	var raw []redis.GeoLocation
	err := r.withRetry(ctx, func() error {
		var innerErr error
		raw, innerErr = r.client.GeoSearchLocation(ctx, r.geoKey(), &redis.GeoSearchLocationQuery{
			GeoSearchQuery: redis.GeoSearchQuery{
				Longitude:  q.Lng,
				Latitude:   q.Lat,
				Radius:     q.Radius,
				RadiusUnit: "m",
				Sort:       "ASC", // nearest first
				Count:      fetch,
			},
			WithCoord: true,
			WithDist:  true,
		}).Result()
		return innerErr
	})
	if err != nil {
		return nil, fmt.Errorf("geosearch: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}

	// One ZMSCORE for every candidate, rather than a ZSCORE each. N round trips
	// inside a 3-second batch window is how a fast store becomes a slow one.
	members := make([]string, len(raw))
	for i, loc := range raw {
		members[i] = loc.Name
	}

	// ZMScore reports a missing member as 0 rather than distinguishing it.
	// That is harmless here and in fact the behaviour we want: a driver in the
	// geo index with no recorded ping is exactly as untrustworthy as one whose
	// ping is ancient, and 0 is before every cutoff, so both fail closed.
	var scores []float64
	err = r.withRetry(ctx, func() error {
		var innerErr error
		scores, innerErr = r.client.ZMScore(ctx, r.seenKey(), members...).Result()
		return innerErr
	})
	if err != nil {
		return nil, fmt.Errorf("zmscore: %w", err)
	}

	cutoff := r.opts.Now().Add(-r.opts.TTL).UnixMilli()
	out := make([]DriverLocation, 0, limit)
	for i, loc := range raw {
		if i >= len(scores) {
			continue
		}
		lastSeen := int64(scores[i])
		if lastSeen < cutoff {
			continue // stopped pinging: still on the map, not actually there
		}
		out = append(out, DriverLocation{
			DriverID:       loc.Name,
			Lat:            loc.Latitude,
			Lng:            loc.Longitude,
			DistanceMeters: loc.Dist,
			LastSeen:       time.UnixMilli(lastSeen),
		})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// RemoveDriver deletes a driver from both keys.
func (r *RedisRepository) RemoveDriver(ctx context.Context, driverID string) error {
	return r.withRetry(ctx, func() error {
		pipe := r.client.Pipeline()
		// A geo set IS a sorted set, so ZRem is how you delete from it — there
		// is no GEOREM command, which surprises most people once.
		pipe.ZRem(ctx, r.geoKey(), driverID)
		pipe.ZRem(ctx, r.seenKey(), driverID)
		_, err := pipe.Exec(ctx)
		return err
	})
}

// Reap deletes drivers whose last ping predates the TTL.
func (r *RedisRepository) Reap(ctx context.Context) (int, error) {
	cutoff := r.opts.Now().Add(-r.opts.TTL).UnixMilli()

	var stale []string
	err := r.withRetry(ctx, func() error {
		var innerErr error
		stale, innerErr = r.client.ZRangeByScore(ctx, r.seenKey(), &redis.ZRangeBy{
			Min: "-inf",
			Max: fmt.Sprintf("(%d", cutoff), // exclusive: equal to the cutoff is still fresh
			// Bounded per sweep. An unbounded reap after an outage could pull
			// millions of ids into one command and stall Redis, which is
			// single-threaded — the reaper would cause the outage it exists to
			// clean up after. Whatever is left goes on the next sweep.
			Count: 10000,
		}).Result()
		return innerErr
	})
	if err != nil {
		return 0, fmt.Errorf("reap scan: %w", err)
	}
	if len(stale) == 0 {
		return 0, nil
	}

	members := make([]interface{}, len(stale))
	for i, id := range stale {
		members[i] = id
	}
	err = r.withRetry(ctx, func() error {
		pipe := r.client.Pipeline()
		pipe.ZRem(ctx, r.geoKey(), members...)
		pipe.ZRem(ctx, r.seenKey(), members...)
		_, err := pipe.Exec(ctx)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("reap delete: %w", err)
	}
	return len(stale), nil
}

// Count returns how many drivers are indexed, fresh or not.
func (r *RedisRepository) Count(ctx context.Context) (int, error) {
	var n int64
	err := r.withRetry(ctx, func() error {
		var innerErr error
		n, innerErr = r.client.ZCard(ctx, r.geoKey()).Result()
		return innerErr
	})
	return int(n), err
}

// Close releases the connection pool.
func (r *RedisRepository) Close() error { return r.client.Close() }

// StartReaper runs Reap on a ticker until ctx is cancelled.
//
// Returns a channel closed on exit, so a caller shutting down can wait for the
// goroutine to actually finish rather than assume it did — an unjoined
// background goroutine is how "clean shutdown" quietly becomes a leak.
func (r *RedisRepository) StartReaper(ctx context.Context, every time.Duration,
	onReap func(removed int, err error)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				removed, err := r.Reap(ctx)
				if onReap != nil {
					onReap(removed, err)
				}
			}
		}
	}()
	return done
}

// Compile-time proof that the Redis implementation satisfies the interface.
// Cheaper than discovering a signature drift at the call site.
var _ Repository = (*RedisRepository)(nil)
