package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type fakeStream struct{ depth, pending, dead atomic.Int64 }

func (f *fakeStream) Depth(context.Context) (int64, error) { f.depth.Add(1); return 0, nil }
func (f *fakeStream) Pending(context.Context) (int64, error) {
	f.pending.Add(1)
	return 0, errors.New("boom") // must not stop the loop
}
func (f *fakeStream) DeadLetterDepth(context.Context) (int64, error) { f.dead.Add(1); return 0, nil }

func TestRefreshStreamGauges(t *testing.T) {
	f := &fakeStream{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RefreshStreamGauges(ctx, f, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for f.pending.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not return after cancel")
	}
	if f.depth.Load() < 2 || f.pending.Load() < 2 || f.dead.Load() < 2 {
		t.Fatalf("calls depth=%d pending=%d dead=%d, want >=2 each", f.depth.Load(), f.pending.Load(), f.dead.Load())
	}
}

type fakeCounter struct{ n atomic.Int64 }

func (f *fakeCounter) Count(context.Context) (int, error) { f.n.Add(1); return 0, nil }

func TestRefreshDriverGauge(t *testing.T) {
	f := &fakeCounter{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RefreshDriverGauge(ctx, f, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for f.n.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not return after cancel")
	}
	if f.n.Load() < 2 {
		t.Fatalf("Count called %d times, want >=2", f.n.Load())
	}
}
