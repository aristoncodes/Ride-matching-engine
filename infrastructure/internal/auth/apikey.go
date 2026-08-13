// Package auth authenticates B2B clients by API key and enforces per-key
// rate limits.
//
// # Key format
//
//	rmk_<key_id>_<secret>
//	     ^^^^^^  ^^^^^^^^
//	     lookup  the part that is actually secret
//
// Splitting the id from the secret matters: it makes verification an O(1)
// lookup by id. The alternative — hashing the whole key and scanning for a
// match — is either a full table scan per request or forces a reversible
// (i.e. useless) hash.
//
// # Why SHA-256 and not bcrypt/argon2
//
// This looks wrong at first glance, so it is worth stating plainly. Password
// hashing is deliberately SLOW because passwords are low-entropy and
// guessable; the slowness is what makes brute force impractical.
//
// An API key here is 32 bytes from crypto/rand — 256 bits of entropy. Brute
// force is not a threat model at that size, so the slowness buys nothing and
// costs a great deal: bcrypt at ~100 ms per verification would cap the API at
// roughly ten authenticated requests per second per core.
//
// SHA-256 over a high-entropy secret is the correct primitive, and it is what
// Stripe, GitHub and AWS use for the same reason. What matters far more is
// that the raw key is NEVER stored — only its hash — so a database leak does
// not hand over working credentials.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// KeyPrefix makes a leaked key greppable. Secret scanners (GitHub's, and
	// most commercial ones) match on known prefixes, so a distinctive one is
	// what lets a key committed by accident be found and revoked automatically.
	KeyPrefix = "rmk"

	keyIDBytes  = 8  // 64 bits — only needs to be unique, not unguessable
	secretBytes = 32 // 256 bits — this is the part that must resist guessing
)

var (
	// ErrInvalidKey covers malformed, unknown, revoked, and expired keys.
	//
	// Deliberately ONE error for all four. Distinguishing them tells an
	// attacker which key ids exist, turning a guessing problem into an
	// enumeration problem. The specific reason is logged server-side, where the
	// operator can see it and the attacker cannot.
	ErrInvalidKey = errors.New("auth: invalid API key")

	// ErrRateLimited means the key is valid but is over its quota.
	ErrRateLimited = errors.New("auth: rate limit exceeded")

	ErrKeyNotFound = errors.New("auth: key not found")
)

// APIKey is the stored record. It never contains the secret.
type APIKey struct {
	KeyID    string
	TenantID string
	Name     string // human label, e.g. "acme-corp prod"

	// SecretHash is hex-encoded SHA-256 of the secret. The secret itself is
	// returned exactly once, at creation, and is unrecoverable afterwards —
	// which is the entire point of hashing at rest.
	SecretHash string

	CreatedAt time.Time
	ExpiresAt time.Time // zero = no expiry
	RevokedAt time.Time // zero = active

	// RateLimitPerMinute is per-key rather than global, so one noisy tenant
	// cannot consume another tenant's capacity.
	RateLimitPerMinute int

	// RotatedFrom links a key to the one it replaces, so an operator can see
	// that a rotation happened rather than two unrelated keys appearing.
	RotatedFrom string
}

// Active reports whether the key may be used right now.
func (k *APIKey) Active(now time.Time) bool {
	if !k.RevokedAt.IsZero() && !now.Before(k.RevokedAt) {
		return false
	}
	if !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt) {
		return false
	}
	return true
}

// Store persists API keys.
//
// An interface, as everywhere else in this project, so the middleware can be
// tested against an in-memory implementation with no Redis at all.
type Store interface {
	// Create mints a key. The RAW key is returned once and never again; only
	// its hash is persisted.
	Create(ctx context.Context, tenantID, name string, ttl time.Duration, rateLimit int) (raw string, key *APIKey, err error)

	// Lookup fetches by key id. It does NOT verify the secret — Verify does.
	Lookup(ctx context.Context, keyID string) (*APIKey, error)

	// Revoke disables a key immediately.
	Revoke(ctx context.Context, keyID string) error

	// Rotate mints a replacement and schedules the old key's revocation after
	// `overlap`. The overlap is what makes rotation possible without downtime:
	// revoking instantly would break every in-flight client that has not yet
	// picked up the new key.
	Rotate(ctx context.Context, keyID string, overlap time.Duration) (raw string, key *APIKey, err error)

	// Allow consumes one unit of the key's rate budget.
	Allow(ctx context.Context, keyID string, limitPerMinute int) (allowed bool, remaining int, err error)

	Close() error
}

// GenerateKey mints a new id and secret, returning the raw key and its hash.
func GenerateKey() (raw, keyID, secretHash string, err error) {
	idBytes := make([]byte, keyIDBytes)
	if _, err = rand.Read(idBytes); err != nil {
		return "", "", "", fmt.Errorf("auth: generate key id: %w", err)
	}
	secret := make([]byte, secretBytes)
	if _, err = rand.Read(secret); err != nil {
		return "", "", "", fmt.Errorf("auth: generate secret: %w", err)
	}

	keyID = hex.EncodeToString(idBytes)
	secretHex := hex.EncodeToString(secret)
	raw = fmt.Sprintf("%s_%s_%s", KeyPrefix, keyID, secretHex)
	secretHash = HashSecret(secretHex)
	return raw, keyID, secretHash, nil
}

// HashSecret hashes the secret portion for storage.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// ParseKey splits a presented key into its id and secret.
func ParseKey(raw string) (keyID, secret string, err error) {
	parts := strings.Split(raw, "_")
	if len(parts) != 3 || parts[0] != KeyPrefix || parts[1] == "" || parts[2] == "" {
		return "", "", ErrInvalidKey
	}
	return parts[1], parts[2], nil
}

// Verify authenticates a presented key and consumes one unit of its rate
// budget. It returns the key so the caller can read the tenant off it — which
// is how Week 19's isolation gets its tenant id, rather than trusting anything
// the client sent.
func Verify(ctx context.Context, store Store, raw string, now time.Time) (*APIKey, error) {
	keyID, secret, err := ParseKey(raw)
	if err != nil {
		return nil, ErrInvalidKey
	}

	key, err := store.Lookup(ctx, keyID)
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			return nil, ErrInvalidKey
		}
		return nil, err // a real store failure: 503, not 401
	}

	// Constant-time comparison. A byte-by-byte `==` returns faster on an early
	// mismatch, and that timing difference is enough to recover a secret one
	// byte at a time over enough requests. The hashes are equal-length hex, so
	// this is a clean fit.
	expected := []byte(key.SecretHash)
	presented := []byte(HashSecret(secret))
	if subtle.ConstantTimeCompare(expected, presented) != 1 {
		return nil, ErrInvalidKey
	}

	// Checked AFTER the secret comparison, on purpose. Checking first would let
	// an attacker learn that a key id exists and is revoked without knowing the
	// secret — the enumeration leak ErrInvalidKey exists to prevent.
	if !key.Active(now) {
		return nil, ErrInvalidKey
	}

	allowed, _, err := store.Allow(ctx, keyID, key.RateLimitPerMinute)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return key, ErrRateLimited
	}
	return key, nil
}
