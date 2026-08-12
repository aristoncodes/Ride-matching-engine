// Package locks provides distributed leases over Redis, so two batcher
// instances can never assign the same driver to two riders at once.
//
// # Why the solver is not enough
//
// The C++ engine guarantees that WITHIN one batch no driver is used twice —
// that is what the unit-capacity flow model is for. It says nothing about two
// batchers solving two different batches concurrently, both of which contain
// the same nearby driver. Each solve is internally correct and the combination
// dispatches one car to two riders.
//
// # Leases, not locks
//
// Every acquisition carries a TTL. A lock without one is a deadlock waiting for
// a crash: the holder dies mid-batch and that driver becomes unmatchable
// forever, with nothing in the system able to tell the difference between "in
// use" and "abandoned". A lease expires on its own, so the worst case of a
// crash is a few seconds of unavailability rather than a permanently lost
// driver.
//
// # Fencing
//
// Release is guarded by a token, so a lease holder that stalls past its TTL
// cannot release a lease that has since been granted to someone else. Without
// that check, a slow holder waking up after expiry deletes the *new* owner's
// lease, and the driver is double-booked by the very mechanism meant to prevent
// it.
package locks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrNotAcquired means another holder currently owns the lease.
var ErrNotAcquired = errors.New("locks: lease is held by someone else")

// Lease is a granted, time-limited claim on a driver.
type Lease struct {
	DriverID string

	// Token proves ownership. Release and Extend both check it, which is what
	// stops a stalled holder from releasing someone else's lease.
	Token string

	// ExpiresAt is when the lease self-releases if not extended. Advisory on
	// the client side — Redis is the authority — but useful for a holder that
	// wants to check whether it still plausibly owns the lease before doing
	// something expensive.
	ExpiresAt time.Time
}

// Options configures the lease manager.
type Options struct {
	TenantID string

	// TTL is how long a lease survives without being extended.
	//
	// Sized against the work it protects: a batch solve takes milliseconds and
	// dispatch confirmation takes seconds, so 10s covers the normal path with
	// room to spare. Too short and a slow-but-alive holder loses its driver
	// mid-dispatch; too long and a crashed holder strands that driver for the
	// remainder of the TTL.
	TTL time.Duration

	// Partitions is how many geohash-derived lock shards exist. See
	// PartitionFor: this is what turns one global contention point into many
	// independent ones.
	Partitions int

	Now func() time.Time
}

// DefaultOptions returns production-shaped defaults.
func DefaultOptions() Options {
	return Options{
		TenantID:   "default",
		TTL:        10 * time.Second,
		Partitions: 256,
		Now:        time.Now,
	}
}

// Manager grants and releases driver leases.
type Manager struct {
	client *redis.Client
	opts   Options
}

// NewManager connects to Redis.
func NewManager(addr string, opts Options) (*Manager, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.TenantID == "" {
		return nil, errors.New("locks: TenantID must be set")
	}
	if opts.TTL <= 0 {
		return nil, errors.New("locks: TTL must be positive — a lease without one is a deadlock")
	}
	if opts.Partitions <= 0 {
		opts.Partitions = DefaultOptions().Partitions
	}

	network := "tcp"
	if len(addr) > 0 && addr[0] == '/' {
		network = "unix"
	}
	client := redis.NewClient(&redis.Options{
		Network:      network,
		Addr:         addr,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
	})
	return &Manager{client: client, opts: opts}, nil
}

// Ping verifies connectivity.
func (m *Manager) Ping(ctx context.Context) error { return m.client.Ping(ctx).Err() }

func (m *Manager) key(driverID string) string {
	return "lock:driver:" + m.opts.TenantID + ":" + driverID
}

// Acquire claims a driver, or returns ErrNotAcquired if someone else holds it.
//
// Implemented as `SET key token NX PX ttl` — a single atomic command. The
// tempting GET-then-SET version has a race between the two calls wide enough
// for two batchers to both observe "free" and both write, which is exactly the
// double-booking this package exists to prevent.
func (m *Manager) Acquire(ctx context.Context, driverID string) (Lease, error) {
	if driverID == "" {
		return Lease{}, errors.New("locks: driverID must not be empty")
	}
	token, err := newToken()
	if err != nil {
		return Lease{}, err
	}

	ok, err := m.client.SetNX(ctx, m.key(driverID), token, m.opts.TTL).Result()
	if err != nil {
		return Lease{}, fmt.Errorf("locks: acquire: %w", err)
	}
	if !ok {
		return Lease{}, ErrNotAcquired
	}
	return Lease{
		DriverID:  driverID,
		Token:     token,
		ExpiresAt: m.opts.Now().Add(m.opts.TTL),
	}, nil
}

// AcquireMany claims as many of the given drivers as it can, returning the
// leases obtained and releasing nothing on partial failure.
//
// Partial success is the right semantics here rather than all-or-nothing: the
// batch can still match the drivers it did win, and a rider matched to an
// available driver should not be denied because a different driver in the same
// batch was taken. All-or-nothing would also need a distributed transaction to
// be correct, and would live-lock under contention as competing batchers
// repeatedly grabbed overlapping subsets and rolled back.
func (m *Manager) AcquireMany(ctx context.Context, driverIDs []string) ([]Lease, error) {
	var leases []Lease
	for _, id := range driverIDs {
		lease, err := m.Acquire(ctx, id)
		if errors.Is(err, ErrNotAcquired) {
			continue
		}
		if err != nil {
			return leases, err // leases so far are the caller's to release
		}
		leases = append(leases, lease)
	}
	return leases, nil
}

// releaseScript deletes the key only if the token still matches.
//
// A Lua script because the check and the delete must be ATOMIC. As two
// commands, this sequence is possible:
//
//	holder A: GET  -> token matches
//	          ...A stalls, its lease expires, B acquires...
//	holder A: DEL  -> deletes B's lease. Driver now double-bookable.
//
// Redis runs a script as a single unit, closing that window entirely.
var releaseScript = redis.NewScript(`
	if redis.call("GET", KEYS[1]) == ARGV[1] then
		return redis.call("DEL", KEYS[1])
	end
	return 0
`)

// Release gives up a lease. Safe to call on an expired one — it simply reports
// that nothing was released.
func (m *Manager) Release(ctx context.Context, lease Lease) (bool, error) {
	if lease.DriverID == "" || lease.Token == "" {
		return false, errors.New("locks: release needs a driver id and a token")
	}
	res, err := releaseScript.Run(ctx, m.client, []string{m.key(lease.DriverID)}, lease.Token).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, fmt.Errorf("locks: release: %w", err)
	}
	return res == 1, nil
}

// ReleaseMany gives up several leases, continuing past individual failures so
// one bad release cannot strand the rest until their TTLs expire.
func (m *Manager) ReleaseMany(ctx context.Context, leases []Lease) error {
	var firstErr error
	for _, l := range leases {
		if _, err := m.Release(ctx, l); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// extendScript refreshes a TTL only for the current owner. Same atomicity
// argument as release: extending someone else's lease is just as wrong as
// deleting it.
var extendScript = redis.NewScript(`
	if redis.call("GET", KEYS[1]) == ARGV[1] then
		return redis.call("PEXPIRE", KEYS[1], ARGV[2])
	end
	return 0
`)

// Extend renews a lease the caller still holds.
//
// Needed whenever work can legitimately outlast the TTL: rather than choosing a
// TTL long enough for the worst case (which strands drivers after a crash for
// exactly that long), keep it short and have a live holder renew it. A dead
// holder stops renewing, and the lease expires quickly.
func (m *Manager) Extend(ctx context.Context, lease Lease) (Lease, error) {
	ms := m.opts.TTL.Milliseconds()
	res, err := extendScript.Run(ctx, m.client,
		[]string{m.key(lease.DriverID)}, lease.Token, ms).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return lease, fmt.Errorf("locks: extend: %w", err)
	}
	if res != 1 {
		return lease, ErrNotAcquired // no longer ours: expired or taken
	}
	lease.ExpiresAt = m.opts.Now().Add(m.opts.TTL)
	return lease, nil
}

// IsHeld reports whether anyone currently holds a driver's lease. Advisory
// only — the answer can change immediately after it returns, so never branch on
// it to decide whether to Acquire. Acquire's atomicity is the real check.
func (m *Manager) IsHeld(ctx context.Context, driverID string) (bool, error) {
	n, err := m.client.Exists(ctx, m.key(driverID)).Result()
	if err != nil {
		return false, fmt.Errorf("locks: exists: %w", err)
	}
	return n == 1, nil
}

// Close releases the connection pool.
func (m *Manager) Close() error { return m.client.Close() }

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("locks: token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ---- Geohash partitioning ----------------------------------------------

const geohashAlphabet = "0123456789bcdefghjkmnpqrstuvwxyz"

// Geohash encodes a coordinate to `precision` characters.
//
// A geohash is a Z-order curve over interleaved latitude/longitude bits, so a
// shared prefix means physical proximity. That property is the whole point
// here: riders and drivers in the same neighbourhood land in the same
// partition, and neighbourhoods are exactly the unit within which contention
// actually happens.
func Geohash(lat, lng float64, precision int) string {
	if precision <= 0 {
		precision = 6
	}
	latRange := [2]float64{-90, 90}
	lngRange := [2]float64{-180, 180}

	var out strings.Builder
	var bit, ch int
	evenBit := true // longitude bits first, then alternating

	for out.Len() < precision {
		if evenBit {
			mid := (lngRange[0] + lngRange[1]) / 2
			if lng > mid {
				ch = ch<<1 | 1
				lngRange[0] = mid
			} else {
				ch <<= 1
				lngRange[1] = mid
			}
		} else {
			mid := (latRange[0] + latRange[1]) / 2
			if lat > mid {
				ch = ch<<1 | 1
				latRange[0] = mid
			} else {
				ch <<= 1
				latRange[1] = mid
			}
		}
		evenBit = !evenBit

		if bit++; bit == 5 {
			out.WriteByte(geohashAlphabet[ch])
			bit, ch = 0, 0
		}
	}
	return out.String()
}

// PartitionFor maps a coordinate to one of `Partitions` lock shards.
//
// # Why partition at all
//
// The named top risk in the TDD is contention on the matching path. A single
// global lock serialises every batcher in the fleet: throughput becomes one
// batch at a time no matter how many instances run, and adding instances makes
// it worse rather than better.
//
// # Why geohash rather than, say, driver id
//
// Contention is SPATIAL. Two batchers collide precisely when they are working
// the same neighbourhood, because that is when their candidate sets overlap.
// Hashing by driver id would spread one neighbourhood's drivers across every
// partition, so a busy area would touch all of them and re-serialise everything
// — statistically random, and useless for the problem.
//
// Partitioning by geohash means a concert letting out in one district contends
// only with itself, while the rest of the city proceeds untouched.
func (m *Manager) PartitionFor(lat, lng float64) int {
	// Precision 5 is roughly a 5 km x 5 km cell — about the scale of a
	// candidate search radius, so a batch usually touches one or two cells.
	hash := Geohash(lat, lng, 5)

	// FNV-1a: cheap, no allocation, and well spread across buckets.
	var h uint32 = 2166136261
	for i := 0; i < len(hash); i++ {
		h ^= uint32(hash[i])
		h *= 16777619
	}
	return int(h % uint32(m.opts.Partitions))
}

// PartitionKey is the Redis key for a partition-level lock, used when a caller
// wants to serialise a whole neighbourhood rather than individual drivers.
func (m *Manager) PartitionKey(partition int) string {
	return fmt.Sprintf("lock:partition:%s:%d", m.opts.TenantID, partition)
}

// AcquirePartition takes a coarse lock over a whole geohash cell.
//
// Deliberately offered alongside per-driver leases rather than instead of them.
// Per-driver locking is finer and allows more parallelism; partition locking is
// coarser but bounds the number of round trips when a batch touches hundreds of
// drivers. Which is faster depends on batch size and overlap, so the Week 13
// benchmark measures both rather than assuming.
func (m *Manager) AcquirePartition(ctx context.Context, lat, lng float64) (Lease, error) {
	partition := m.PartitionFor(lat, lng)
	token, err := newToken()
	if err != nil {
		return Lease{}, err
	}
	key := m.PartitionKey(partition)

	ok, err := m.client.SetNX(ctx, key, token, m.opts.TTL).Result()
	if err != nil {
		return Lease{}, fmt.Errorf("locks: acquire partition: %w", err)
	}
	if !ok {
		return Lease{}, ErrNotAcquired
	}
	// DriverID carries the key here so Release works unchanged; the token check
	// is what actually matters and is identical either way.
	return Lease{
		DriverID:  fmt.Sprintf("__partition_%d", partition),
		Token:     token,
		ExpiresAt: m.opts.Now().Add(m.opts.TTL),
	}, nil
}

// ReleasePartition gives up a partition lock.
func (m *Manager) ReleasePartition(ctx context.Context, lease Lease) (bool, error) {
	var partition int
	if _, err := fmt.Sscanf(lease.DriverID, "__partition_%d", &partition); err != nil {
		return false, fmt.Errorf("locks: %q is not a partition lease", lease.DriverID)
	}
	res, err := releaseScript.Run(ctx, m.client,
		[]string{m.PartitionKey(partition)}, lease.Token).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, fmt.Errorf("locks: release partition: %w", err)
	}
	return res == 1, nil
}

// SpreadScore reports how evenly a set of coordinates maps across partitions,
// as the ratio of distinct partitions used to the theoretical maximum. Used by
// the benchmark to show that partitioning genuinely spreads load rather than
// piling everything into one shard.
func (m *Manager) SpreadScore(coords [][2]float64) float64 {
	if len(coords) == 0 {
		return 0
	}
	seen := map[int]bool{}
	for _, c := range coords {
		seen[m.PartitionFor(c[0], c[1])] = true
	}
	maxPossible := math.Min(float64(len(coords)), float64(m.opts.Partitions))
	return float64(len(seen)) / maxPossible
}
