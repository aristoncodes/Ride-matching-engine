package main

import (
	"errors"
	"strings"
	"testing"
)

func TestBucketKeyRedactsAndTruncates(t *testing.T) {
	const key = "sekret-key-123"
	got := bucketKey("dial failed with header "+key+" rejected", key)
	if strings.Contains(got, key) || !strings.Contains(got, "<redacted>") {
		t.Fatalf("not redacted: %q", got)
	}
	if n := len([]rune(bucketKey(strings.Repeat("x", 500), key))); n != 120 {
		t.Fatalf("len=%d, want 120", n)
	}
	if bucketKey("abc", "") != "abc" {
		t.Fatal("empty secret must be a no-op")
	}
}

func TestErrBucketsTop(t *testing.T) {
	b := newErrBuckets("k3y")
	for i := 0; i < 3; i++ {
		b.add(errors.New("connection refused"))
	}
	b.add(errors.New("bad handshake k3y"))
	b.add(errors.New("bad handshake k3y"))
	for _, m := range []string{"a", "b", "c", "d"} {
		b.add(errors.New(m))
	}
	b.add(nil)
	top := b.top(5)
	if len(top) != 5 {
		t.Fatalf("len=%d, want 5", len(top))
	}
	if top[0].Msg != "connection refused" || top[0].Count != 3 {
		t.Fatalf("top[0]=%+v", top[0])
	}
	if top[1].Msg != "bad handshake <redacted>" || top[1].Count != 2 {
		t.Fatalf("top[1]=%+v", top[1])
	}
}
