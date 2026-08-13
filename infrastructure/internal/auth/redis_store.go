package auth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore persists API keys and enforces rate limits in Redis.
//
// Keys are NOT tenant-scoped in their Redis key path, unlike everything else in
// this system. That is deliberate and worth being explicit about: the whole
// point of a lookup is to DISCOVER which tenant a request belongs to, so
// scoping the lookup by tenant would require knowing the answer first.
//
//	apikey:<key_id>        -> the key record (hash, tenant, limits, status)
//	apikey:tenant:<id>     -> set of key ids, for listing and bulk revocation
//	ratelimit:<key_id>     -> the current minute's request counter
type RedisStore struct {
	client *redis.Client
	now    func() time.Time
}

// NewRedisStore connects to Redis.
func NewRedisStore(addr string, now func() time.Time) (*RedisStore, error) {
	if now == nil {
		now = time.Now
	}
	network := "tcp"
	if len(addr) > 0 && addr[0] == '/' {
		network = "unix"
	}
	return &RedisStore{
		client: redis.NewClient(&redis.Options{
			Network:      network,
			Addr:         addr,
			DialTimeout:  2 * time.Second,
			ReadTimeout:  time.Second,
			WriteTimeout: time.Second,
		}),
		now: now,
	}, nil
}

func (s *RedisStore) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }
func (s *RedisStore) Close() error                   { return s.client.Close() }

func keyPath(keyID string) string     { return "apikey:" + keyID }
func tenantPath(tenant string) string { return "apikey:tenant:" + tenant }
func ratePath(keyID string) string    { return "ratelimit:" + keyID }

// Create mints a key and stores only its hash.
func (s *RedisStore) Create(ctx context.Context, tenantID, name string,
	ttl time.Duration, rateLimit int) (string, *APIKey, error) {

	if tenantID == "" {
		return "", nil, errors.New("auth: tenantID must be set")
	}
	if rateLimit <= 0 {
		rateLimit = 600 // 10/s sustained
	}

	raw, keyID, secretHash, err := GenerateKey()
	if err != nil {
		return "", nil, err
	}

	now := s.now()
	key := &APIKey{
		KeyID:              keyID,
		TenantID:           tenantID,
		Name:               name,
		SecretHash:         secretHash,
		CreatedAt:          now,
		RateLimitPerMinute: rateLimit,
	}
	if ttl > 0 {
		key.ExpiresAt = now.Add(ttl)
	}

	if err := s.save(ctx, key); err != nil {
		return "", nil, err
	}
	// The raw key is returned HERE and nowhere else, ever. There is no endpoint
	// that can show it again, because the server does not have it.
	return raw, key, nil
}

func (s *RedisStore) save(ctx context.Context, key *APIKey) error {
	fields := map[string]interface{}{
		"tenant_id":   key.TenantID,
		"name":        key.Name,
		"secret_hash": key.SecretHash,
		"created_at":  strconv.FormatInt(key.CreatedAt.UnixMilli(), 10),
		"rate_limit":  strconv.Itoa(key.RateLimitPerMinute),
	}
	if !key.ExpiresAt.IsZero() {
		fields["expires_at"] = strconv.FormatInt(key.ExpiresAt.UnixMilli(), 10)
	}
	if !key.RevokedAt.IsZero() {
		fields["revoked_at"] = strconv.FormatInt(key.RevokedAt.UnixMilli(), 10)
	}
	if key.RotatedFrom != "" {
		fields["rotated_from"] = key.RotatedFrom
	}

	pipe := s.client.Pipeline()
	pipe.HSet(ctx, keyPath(key.KeyID), fields)
	pipe.SAdd(ctx, tenantPath(key.TenantID), key.KeyID)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("auth: save key: %w", err)
	}
	return nil
}

// Lookup fetches a key record by id.
func (s *RedisStore) Lookup(ctx context.Context, keyID string) (*APIKey, error) {
	fields, err := s.client.HGetAll(ctx, keyPath(keyID)).Result()
	if err != nil {
		return nil, fmt.Errorf("auth: lookup: %w", err)
	}
	if len(fields) == 0 {
		return nil, ErrKeyNotFound
	}

	ms := func(k string) time.Time {
		if v, ok := fields[k]; ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return time.UnixMilli(n)
			}
		}
		return time.Time{}
	}
	limit, _ := strconv.Atoi(fields["rate_limit"])

	return &APIKey{
		KeyID:              keyID,
		TenantID:           fields["tenant_id"],
		Name:               fields["name"],
		SecretHash:         fields["secret_hash"],
		CreatedAt:          ms("created_at"),
		ExpiresAt:          ms("expires_at"),
		RevokedAt:          ms("revoked_at"),
		RateLimitPerMinute: limit,
		RotatedFrom:        fields["rotated_from"],
	}, nil
}

// Revoke disables a key immediately.
//
// A field write rather than a delete, so the record survives for audit: "this
// key was revoked at 14:02" is a question incident response actually asks, and
// a deleted key answers it with silence.
func (s *RedisStore) Revoke(ctx context.Context, keyID string) error {
	exists, err := s.client.Exists(ctx, keyPath(keyID)).Result()
	if err != nil {
		return fmt.Errorf("auth: revoke: %w", err)
	}
	if exists == 0 {
		return ErrKeyNotFound
	}
	if err := s.client.HSet(ctx, keyPath(keyID),
		"revoked_at", strconv.FormatInt(s.now().UnixMilli(), 10)).Err(); err != nil {
		return fmt.Errorf("auth: revoke: %w", err)
	}
	return nil
}

// Rotate mints a replacement and revokes the old key after an overlap window.
func (s *RedisStore) Rotate(ctx context.Context, keyID string, overlap time.Duration) (string, *APIKey, error) {
	old, err := s.Lookup(ctx, keyID)
	if err != nil {
		return "", nil, err
	}

	var ttl time.Duration
	if !old.ExpiresAt.IsZero() {
		ttl = old.ExpiresAt.Sub(s.now())
	}
	raw, fresh, err := s.Create(ctx, old.TenantID, old.Name+" (rotated)", ttl, old.RateLimitPerMinute)
	if err != nil {
		return "", nil, err
	}
	fresh.RotatedFrom = keyID
	if err := s.save(ctx, fresh); err != nil {
		return "", nil, err
	}

	// The old key keeps working for `overlap`. Revoking it the instant a new
	// one is minted would break every client that has not yet redeployed —
	// which is what makes people avoid rotating at all, and unrotated keys are
	// the actual security problem.
	old.RevokedAt = s.now().Add(overlap)
	if err := s.save(ctx, old); err != nil {
		return "", nil, err
	}
	return raw, fresh, nil
}

// ListForTenant returns a tenant's key ids.
func (s *RedisStore) ListForTenant(ctx context.Context, tenantID string) ([]string, error) {
	ids, err := s.client.SMembers(ctx, tenantPath(tenantID)).Result()
	if err != nil {
		return nil, fmt.Errorf("auth: list: %w", err)
	}
	return ids, nil
}

// rateLimitScript is a fixed-window counter: INCR, and set the expiry only on
// the first request of the window.
//
// Lua because the increment and the expiry must be ATOMIC. As two commands, a
// crash between them leaves a counter with no TTL — which never resets, and
// silently locks that key out forever. That failure is invisible until a
// customer complains.
var rateLimitScript = redis.NewScript(`
	local current = redis.call("INCR", KEYS[1])
	if current == 1 then
		redis.call("EXPIRE", KEYS[1], ARGV[1])
	end
	return current
`)

// Allow consumes one unit of a key's per-minute budget.
//
// A fixed window, chosen knowing its flaw: a client can send 2x the limit
// across a window boundary (all of minute 1's budget at 0:59, all of minute 2's
// at 1:00). A sliding-window log or token bucket fixes that at the cost of more
// state per key.
//
// Fixed window is the right trade here because this limit exists to stop one
// tenant starving another, not to enforce a billing quota to the request. If it
// ever backs billing, this must be revisited.
func (s *RedisStore) Allow(ctx context.Context, keyID string, limitPerMinute int) (bool, int, error) {
	if limitPerMinute <= 0 {
		return true, 0, nil // unlimited
	}

	// The window is part of the Redis key, so expiry and rollover need no
	// separate bookkeeping.
	window := s.now().Unix() / 60
	key := fmt.Sprintf("%s:%d", ratePath(keyID), window)

	count, err := rateLimitScript.Run(ctx, s.client, []string{key}, 70).Int64()
	if err != nil {
		// Fail OPEN on a rate-limiter failure. A Redis blip should not take the
		// whole API down; the limiter protects against noisy neighbours, and
		// briefly not enforcing it is far less harmful than refusing every
		// authenticated request. A limiter that fails closed turns a dependency
		// blip into a total outage.
		return true, 0, nil
	}

	remaining := limitPerMinute - int(count)
	if remaining < 0 {
		remaining = 0
	}
	return int(count) <= limitPerMinute, remaining, nil
}

var _ Store = (*RedisStore)(nil)
