package results

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is an in-process Store for tests and for the fake stacks the
// batcher and API suites already run against.
//
// It exists for the same reason locations and queue have in-memory doubles:
// the tests that matter here are about WHICH outcome gets recorded and when,
// not about Redis, and a test that needs a server to prove "an unmatched
// rider ends up UNMATCHED" is a slower test that proves less.
type MemoryStore struct {
	mu  sync.RWMutex
	out map[string]Outcome

	// Now is injectable, matching RedisStore.
	Now func() time.Time
}

// NewMemory creates an empty store.
func NewMemory() *MemoryStore {
	return &MemoryStore{out: make(map[string]Outcome), Now: time.Now}
}

// Put records an outcome, replacing any previous one.
func (m *MemoryStore) Put(_ context.Context, out Outcome) error {
	if out.RequestID == "" {
		return ErrEmptyRequestID
	}
	if out.TenantID == "" {
		return ErrEmptyTenantID
	}
	if out.UpdatedAt.IsZero() {
		now := time.Now
		if m.Now != nil {
			now = m.Now
		}
		out.UpdatedAt = now()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Copy the pointer's value rather than sharing it: a caller that reuses an
	// Outcome across writes would otherwise mutate what is already stored.
	if out.ETASeconds != nil {
		eta := *out.ETASeconds
		out.ETASeconds = &eta
	}
	m.out[key(out.TenantID, out.RequestID)] = out
	return nil
}

// Get reads one outcome, scoped to the tenant.
func (m *MemoryStore) Get(_ context.Context, tenantID, requestID string) (Outcome, error) {
	if requestID == "" {
		return Outcome{}, ErrEmptyRequestID
	}
	if tenantID == "" {
		return Outcome{}, ErrEmptyTenantID
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	out, ok := m.out[key(tenantID, requestID)]
	if !ok {
		return Outcome{}, ErrNotFound
	}
	if out.ETASeconds != nil {
		eta := *out.ETASeconds
		out.ETASeconds = &eta
	}
	return out, nil
}

// Len is the number of recorded outcomes. Tests only.
func (m *MemoryStore) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.out)
}

// Close is a no-op.
func (m *MemoryStore) Close() error { return nil }
