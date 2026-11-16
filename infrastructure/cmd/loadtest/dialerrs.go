package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// errBuckets counts distinct dial error messages. Messages are redacted and
// truncated before bucketing so the map stays small and never holds a key.
type errBuckets struct {
	mu     sync.Mutex
	counts map[string]int
	secret string
}

type errCount struct {
	Msg   string
	Count int
}

func newErrBuckets(secret string) *errBuckets {
	return &errBuckets{counts: map[string]int{}, secret: secret}
}

// bucketKey redacts the secret, then keeps the first 120 characters.
func bucketKey(msg, secret string) string {
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, "<redacted>")
	}
	if r := []rune(msg); len(r) > 120 {
		msg = string(r[:120])
	}
	return msg
}

func (b *errBuckets) add(err error) {
	if err == nil {
		return
	}
	k := bucketKey(err.Error(), b.secret)
	b.mu.Lock()
	b.counts[k]++
	b.mu.Unlock()
}

// top returns up to n buckets, most frequent first (ties by message).
func (b *errBuckets) top(n int) []errCount {
	b.mu.Lock()
	out := make([]errCount, 0, len(b.counts))
	for m, c := range b.counts {
		out = append(out, errCount{m, c})
	}
	b.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Msg < out[j].Msg
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func (b *errBuckets) print(n int) {
	for _, e := range b.top(n) {
		fmt.Printf("  dial error x%d: %s\n", e.Count, e.Msg)
	}
}
