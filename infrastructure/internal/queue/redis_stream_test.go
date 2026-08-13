package queue_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aditya/ride-matching/internal/queue"
	"github.com/aditya/ride-matching/internal/testutil"
)

func newQueue(t *testing.T, addr, consumer string) *queue.RedisStream {
	t.Helper()
	opts := queue.DefaultStreamOptions()
	opts.TenantID = "test"
	opts.Consumer = consumer
	opts.MaxDeliveries = 3

	q, err := queue.NewRedisStream(addr, opts)
	if err != nil {
		t.Fatalf("new queue (%s): %v", consumer, err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func request(id string) queue.RideRequest {
	return queue.RideRequest{
		RequestID: id,
		TenantID:  "test",
		RiderID:   "R-" + id,
		Lat:       12.9716,
		Lng:       77.5946,
	}
}

func TestPublishAndConsume(t *testing.T) {
	proc := testutil.StartRedis(t)
	q := newQueue(t, proc.Addr, "c1")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := q.Publish(ctx, request(fmt.Sprintf("req-%d", i))); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	msgs, err := q.Consume(ctx, 10, time.Second)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}

	// The payload must survive the round trip intact — a queue that mangles
	// coordinates dispatches cars to the wrong place.
	if got := msgs[0].Request.RequestID; got != "req-0" {
		t.Errorf("request_id = %q, want req-0", got)
	}
	if got := msgs[0].Request.Lat; got != 12.9716 {
		t.Errorf("lat = %v, want 12.9716", got)
	}
	if msgs[0].Deliveries != 1 {
		t.Errorf("deliveries = %d, want 1 on first delivery", msgs[0].Deliveries)
	}

	// Consumed but unacked: still pending, which is what makes recovery possible.
	if n, _ := q.Pending(ctx); n != 3 {
		t.Fatalf("pending = %d, want 3 before acking", n)
	}

	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	if err := q.Ack(ctx, ids...); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if n, _ := q.Pending(ctx); n != 0 {
		t.Errorf("pending = %d after ack, want 0", n)
	}
}

// TestUnackedMessageIsRedelivered is the Week 10 checkpoint.
//
// The "Fail-Safe Orchestration" tenet says a crash must not lose a ride
// request. This simulates a consumer that claims work and dies before acking —
// the message must be recoverable by ANOTHER consumer, with its payload intact.
//
// Note what is NOT being tested: that Redis stored the bytes. That is trivially
// true and useless on its own. Durability without a reclaim path is just a
// slower way to lose the request, because the message sits in a dead consumer's
// pending list, safely stored and delivered to nobody.
func TestUnackedMessageIsRedelivered(t *testing.T) {
	proc := testutil.StartRedis(t)
	ctx := context.Background()

	// Consumer A claims the work...
	consumerA := newQueue(t, proc.Addr, "consumer-A")
	if _, err := consumerA.Publish(ctx, request("req-survives")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	claimed, err := consumerA.Consume(ctx, 10, time.Second)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("consume: got %d (err %v), want 1", len(claimed), err)
	}

	// ...and dies here. No ack. This is the crash.
	_ = consumerA.Close()

	// The message is NOT gone — it is pending, owned by a consumer that no
	// longer exists.
	consumerB := newQueue(t, proc.Addr, "consumer-B")
	if n, _ := consumerB.Pending(ctx); n != 1 {
		t.Fatalf("pending = %d, want 1 — the message must survive the crash", n)
	}

	// A fresh Consume finds nothing: ">" only returns messages never delivered
	// to anyone, and this one was delivered to A. Reclaim is the only way back.
	fresh, err := consumerB.Consume(ctx, 10, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(fresh) != 0 {
		t.Fatalf("a plain Consume returned %d messages; it must not see another "+
			"consumer's pending work", len(fresh))
	}

	// Reclaim recovers it, intact.
	reclaimed, err := consumerB.Reclaim(ctx, 0, 10)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("reclaimed %d, want 1 — THE REQUEST WAS LOST", len(reclaimed))
	}
	if got := reclaimed[0].Request.RequestID; got != "req-survives" {
		t.Errorf("request_id = %q, want req-survives", got)
	}
	if reclaimed[0].Deliveries < 2 {
		t.Errorf("deliveries = %d, want >= 2 after a redelivery", reclaimed[0].Deliveries)
	}

	// And once B acks, it is finally done.
	if err := consumerB.Ack(ctx, reclaimed[0].ID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if n, _ := consumerB.Pending(ctx); n != 0 {
		t.Errorf("pending = %d after the reclaiming consumer acked, want 0", n)
	}
}

func TestMinIdleProtectsWorkInProgress(t *testing.T) {
	// Reclaim must not steal from a consumer that is merely SLOW. minIdle is
	// the entire distinction between "crashed" and "still working" — set it too
	// low and two consumers process the same request concurrently, which for a
	// ride request means dispatching two cars.
	proc := testutil.StartRedis(t)
	ctx := context.Background()

	slow := newQueue(t, proc.Addr, "slow-but-alive")
	if _, err := slow.Publish(ctx, request("req-inflight")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if msgs, err := slow.Consume(ctx, 10, time.Second); err != nil || len(msgs) != 1 {
		t.Fatalf("consume: got %d (err %v)", len(msgs), err)
	}

	other := newQueue(t, proc.Addr, "other")

	// A generous idle threshold: the work is only milliseconds old.
	stolen, err := other.Reclaim(ctx, 30*time.Second, 10)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(stolen) != 0 {
		t.Fatalf("reclaimed %d messages from a live consumer — minIdle is not "+
			"protecting work in progress", len(stolen))
	}

	// Once the threshold is met, it becomes reclaimable.
	if got, err := other.Reclaim(ctx, 0, 10); err != nil || len(got) != 1 {
		t.Fatalf("reclaim after idle: got %d (err %v), want 1", len(got), err)
	}
}

// TestPoisonMessageIsDeadLettered proves a repeatedly-failing message is set
// aside rather than blocking the queue forever.
func TestPoisonMessageIsDeadLettered(t *testing.T) {
	proc := testutil.StartRedis(t)
	ctx := context.Background()

	q := newQueue(t, proc.Addr, "poison-handler") // MaxDeliveries = 3
	if _, err := q.Publish(ctx, request("req-poison")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Delivery 1: claimed and "processing fails", so never acked.
	if msgs, err := q.Consume(ctx, 10, time.Second); err != nil || len(msgs) != 1 {
		t.Fatalf("consume: got %d (err %v)", len(msgs), err)
	}

	// Reclaim repeatedly, simulating a consumer that keeps failing on it.
	var lastCount int
	for attempt := 0; attempt < 6; attempt++ {
		msgs, err := q.Reclaim(ctx, 0, 10)
		if err != nil {
			t.Fatalf("reclaim %d: %v", attempt, err)
		}
		lastCount = len(msgs)
		if lastCount == 0 {
			break // dead-lettered: it stops coming back
		}
	}

	if lastCount != 0 {
		t.Fatal("a poison message is still being redelivered; it must be dead-lettered")
	}

	dead, err := q.DeadLetterDepth(ctx)
	if err != nil {
		t.Fatalf("dead-letter depth: %v", err)
	}
	if dead != 1 {
		t.Fatalf("dead-letter depth = %d, want 1", dead)
	}

	// Critically: it must no longer occupy a consumer slot.
	if n, _ := q.Pending(ctx); n != 0 {
		t.Errorf("pending = %d, want 0 — a dead-lettered message must be acked "+
			"so it stops blocking the queue", n)
	}
}

func TestMalformedMessageIsDeadLetteredNotRetried(t *testing.T) {
	// A payload that fails to decode will fail identically on every redelivery.
	// Retrying it is how a queue stalls on a single bad entry.
	proc := testutil.StartRedis(t)
	ctx := context.Background()

	q := newQueue(t, proc.Addr, "c1")

	// Write an entry directly, bypassing Publish's validation.
	if err := testutil.RedisCmd(ctx, proc.Addr,
		"XADD", "requests:stream:test", "*", "garbage", "yes"); err != nil {
		t.Fatalf("xadd: %v", err)
	}

	msgs, err := q.Consume(ctx, 10, time.Second)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("got %d messages, want 0 — an undecodable entry must not be "+
			"handed to the consumer", len(msgs))
	}

	if dead, _ := q.DeadLetterDepth(ctx); dead != 1 {
		t.Errorf("dead-letter depth = %d, want 1", dead)
	}
	if n, _ := q.Pending(ctx); n != 0 {
		t.Errorf("pending = %d, want 0 — the bad entry must not stay pending", n)
	}
}

func TestConsumerGroupSplitsWorkWithoutDuplication(t *testing.T) {
	// The scaling property: N batcher instances share one stream, and each
	// message goes to exactly ONE of them. If this were wrong, two batchers
	// would match the same rider twice.
	proc := testutil.StartRedis(t)
	ctx := context.Background()

	a := newQueue(t, proc.Addr, "batcher-A")
	b := newQueue(t, proc.Addr, "batcher-B")

	const total = 50
	for i := 0; i < total; i++ {
		if _, err := a.Publish(ctx, request(fmt.Sprintf("req-%02d", i))); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	seen := map[string]string{} // requestID -> which consumer got it
	for i := 0; i < 10 && len(seen) < total; i++ {
		for name, q := range map[string]*queue.RedisStream{"A": a, "B": b} {
			msgs, err := q.Consume(ctx, 7, 200*time.Millisecond)
			if err != nil {
				t.Fatalf("consume %s: %v", name, err)
			}
			for _, m := range msgs {
				if prev, dup := seen[m.Request.RequestID]; dup {
					t.Fatalf("%s was delivered to both %s and %s — a consumer "+
						"group must not duplicate", m.Request.RequestID, prev, name)
				}
				seen[m.Request.RequestID] = name
			}
		}
	}

	if len(seen) != total {
		t.Fatalf("saw %d distinct requests, want %d", len(seen), total)
	}
}

func TestPublishRejectsMissingRequestID(t *testing.T) {
	// At-least-once delivery makes an idempotency key mandatory: without one, a
	// redelivered message is indistinguishable from a second customer.
	proc := testutil.StartRedis(t)
	q := newQueue(t, proc.Addr, "c1")

	if _, err := q.Publish(context.Background(), queue.RideRequest{RiderID: "R-1"}); err == nil {
		t.Fatal("expected an error when RequestID is empty")
	}
}

func TestOptionsValidation(t *testing.T) {
	// Every one of these is a real foot-gun, so each is refused at construction
	// rather than discovered in production.
	cases := []struct {
		name   string
		mutate func(*queue.StreamOptions)
	}{
		{"no consumer name", func(o *queue.StreamOptions) { o.Consumer = "" }},
		{"no tenant", func(o *queue.StreamOptions) { o.TenantID = "" }},
		{"no group", func(o *queue.StreamOptions) { o.Group = "" }},
		{"unbounded stream", func(o *queue.StreamOptions) { o.MaxLen = 0 }},
		{"zero max deliveries", func(o *queue.StreamOptions) { o.MaxDeliveries = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := queue.DefaultStreamOptions()
			opts.Consumer = "c1"
			tc.mutate(&opts)
			if _, err := queue.NewRedisStream("localhost:6379", opts); err == nil {
				t.Errorf("expected an error for %s", tc.name)
			}
		})
	}
}

func TestConsumerGroupStartsAtZeroNotEnd(t *testing.T) {
	// Requests published BEFORE any consumer existed must still be delivered.
	// Creating the group at "$" instead of "0" would silently skip them —
	// a data-loss bug that only shows up on a cold start.
	proc := testutil.StartRedis(t)
	ctx := context.Background()

	publisher := newQueue(t, proc.Addr, "publisher")
	if _, err := publisher.Publish(ctx, request("req-before-anyone-listened")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	consumer := newQueue(t, proc.Addr, "late-arriver")
	msgs, err := consumer.Consume(ctx, 10, time.Second)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1 — requests published before the "+
			"consumer started must not be skipped", len(msgs))
	}
}

func TestDepthAndPendingReportQueueHealth(t *testing.T) {
	proc := testutil.StartRedis(t)
	ctx := context.Background()
	q := newQueue(t, proc.Addr, "c1")

	for i := 0; i < 5; i++ {
		if _, err := q.Publish(ctx, request(fmt.Sprintf("req-%d", i))); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	// Undelivered work shows up as depth.
	if d, err := q.Depth(ctx); err != nil || d != 5 {
		t.Fatalf("depth = %d (err %v), want 5", d, err)
	}

	msgs, err := q.Consume(ctx, 2, time.Second)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("consume: got %d (err %v)", len(msgs), err)
	}

	// Claimed work moves from depth to pending.
	if d, _ := q.Depth(ctx); d != 3 {
		t.Errorf("depth = %d after consuming 2 of 5, want 3", d)
	}
	if p, _ := q.Pending(ctx); p != 2 {
		t.Errorf("pending = %d, want 2", p)
	}
}

// TestUnmatchedRiderIsNotTreatedAsPoison pins the bug the Week 16 chaos test
// found.
//
// Two mechanisms collided:
//   - Week 10 counts DELIVERIES to detect poison messages.
//   - Week 12 deliberately leaves an unmatched rider un-acked so a later window
//     can try again.
//
// Both look identical to the broker, so a rider who simply could not find a car
// accumulated deliveries until the poison detector dead-lettered them. Observed
// live: delivery count 4 of a maximum 5, on perfectly valid requests.
//
// Republish is the fix: it resets the infrastructure counter and advances a
// separate, business-level MatchAttempts instead.
func TestUnmatchedRiderIsNotTreatedAsPoison(t *testing.T) {
	proc := testutil.StartRedis(t)
	ctx := context.Background()
	q := newQueue(t, proc.Addr, "matcher") // MaxDeliveries = 3

	if _, err := q.Publish(ctx, request("req-unlucky")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Ten windows go by with no driver available — comfortably past
	// MaxDeliveries. Before the fix this dead-lettered on the 4th.
	for window := 0; window < 10; window++ {
		msgs, err := q.Consume(ctx, 10, time.Second)
		if err != nil {
			t.Fatalf("window %d consume: %v", window, err)
		}
		if len(msgs) != 1 {
			t.Fatalf("window %d: got %d messages, want 1 — the request vanished",
				window, len(msgs))
		}
		if got := msgs[0].Request.RequestID; got != "req-unlucky" {
			t.Fatalf("window %d: request_id = %q", window, got)
		}
		// Still unmatched, so it goes back for another window.
		if err := q.Republish(ctx, msgs[0]); err != nil {
			t.Fatalf("window %d republish: %v", window, err)
		}
	}

	// Never discarded.
	if dead, err := q.DeadLetterDepth(ctx); err != nil || dead != 0 {
		t.Fatalf("dead-letter depth = %d (err %v), want 0 — an unmatched rider "+
			"is not poison", dead, err)
	}

	// And the business-level counter is what actually advanced, so a caller can
	// still decide to give up on an informed basis.
	msgs, err := q.Consume(ctx, 10, time.Second)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("final consume: got %d (err %v)", len(msgs), err)
	}
	if got := msgs[0].Request.MatchAttempts; got != 10 {
		t.Errorf("MatchAttempts = %d, want 10", got)
	}
	if msgs[0].Deliveries > 1 {
		t.Errorf("Deliveries = %d — republishing must reset the infrastructure "+
			"retry counter, not accumulate it", msgs[0].Deliveries)
	}
}

func TestRepublishPreservesThePayload(t *testing.T) {
	proc := testutil.StartRedis(t)
	ctx := context.Background()
	q := newQueue(t, proc.Addr, "c1")

	original := request("req-keep")
	original.Lat, original.Lng = 12.9716, 77.5946
	if _, err := q.Publish(ctx, original); err != nil {
		t.Fatalf("publish: %v", err)
	}

	msgs, err := q.Consume(ctx, 1, time.Second)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("consume: got %d (err %v)", len(msgs), err)
	}
	if err := q.Republish(ctx, msgs[0]); err != nil {
		t.Fatalf("republish: %v", err)
	}

	// The original must be acked, or it would be redelivered as well and the
	// rider would be matched twice.
	if pending, _ := q.Pending(ctx); pending != 0 {
		t.Errorf("pending = %d after republish, want 0 — the original must be acked", pending)
	}

	again, err := q.Consume(ctx, 1, time.Second)
	if err != nil || len(again) != 1 {
		t.Fatalf("re-consume: got %d (err %v)", len(again), err)
	}
	got := again[0].Request
	if got.RequestID != "req-keep" || got.Lat != 12.9716 || got.Lng != 77.5946 {
		t.Errorf("payload mangled by republish: %+v", got)
	}
	if got.RiderID != original.RiderID {
		t.Errorf("rider id = %q, want %q", got.RiderID, original.RiderID)
	}
}
