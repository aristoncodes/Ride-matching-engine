package queue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStream implements Queue on Redis Streams (ADR-0006).
//
// # The three commands that make durability real
//
//	XADD                        append; the request is now the broker's problem
//	XREADGROUP  ... NOACK=false claim into THIS consumer's pending list
//	XACK                        done — remove from pending
//
// A claimed-but-unacked message stays in the consumer's Pending Entries List
// (PEL) forever. That is the whole mechanism: if the consumer dies between
// claiming and acking, the message is still sitting in its PEL, and
// XPENDING/XCLAIM let another consumer take it over.
//
// # The failure mode this design has to avoid
//
// Durability without reclaim is worthless. A crashed consumer's messages are
// safely stored and delivered to nobody, which from the rider's point of view
// is identical to having lost them. So `Reclaim` is not an optimisation — it is
// the other half of the guarantee, and something must call it on a schedule.
type RedisStream struct {
	client *redis.Client
	opts   StreamOptions
}

// StreamOptions configures the stream.
type StreamOptions struct {
	// TenantID scopes the keys, as everywhere else in this system.
	TenantID string

	// Group is the consumer group name. All batcher instances share one group,
	// so each message goes to exactly one of them.
	Group string

	// Consumer identifies THIS instance within the group. Must be unique per
	// process — two consumers sharing a name share a pending list, and one's
	// crash-recovery would silently reclaim the other's in-flight work.
	Consumer string

	// MaxLen caps the stream length. NOT optional: an untrimmed stream grows
	// forever, which is the same slow-motion OOM as an unbounded buffer.
	// Applied approximately (`MAXLEN ~`), which lets Redis trim on whole-node
	// boundaries and is dramatically cheaper than exact trimming.
	MaxLen int64

	// MaxDeliveries is how many times a message may be delivered before it is
	// treated as poison and dead-lettered. 1 would dead-letter on the first
	// transient blip; too high and a poison message wastes the consumer's
	// capacity for a long time.
	MaxDeliveries int64

	// Now is injectable for tests.
	Now func() time.Time
}

// DefaultStreamOptions returns production-shaped defaults.
func DefaultStreamOptions() StreamOptions {
	return StreamOptions{
		TenantID:      "default",
		Group:         "batchers",
		MaxLen:        1_000_000,
		MaxDeliveries: 5,
		Now:           time.Now,
	}
}

// NewRedisStream connects and creates the consumer group if needed.
func NewRedisStream(addr string, opts StreamOptions) (*RedisStream, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.TenantID == "" {
		return nil, errors.New("queue: TenantID must be set")
	}
	if opts.Group == "" {
		return nil, errors.New("queue: Group must be set")
	}
	if opts.Consumer == "" {
		return nil, errors.New("queue: Consumer must be set (unique per process)")
	}
	if opts.MaxLen <= 0 {
		return nil, errors.New("queue: MaxLen must be positive — an untrimmed stream grows forever")
	}
	if opts.MaxDeliveries <= 0 {
		return nil, errors.New("queue: MaxDeliveries must be positive")
	}

	network := "tcp"
	if len(addr) > 0 && addr[0] == '/' {
		network = "unix"
	}

	client := redis.NewClient(&redis.Options{
		Network:      network,
		Addr:         addr,
		DialTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		// Deliberately 0 = no read timeout. XREADGROUP with BLOCK holds the
		// connection open on purpose; a read timeout shorter than the block
		// duration would tear down a perfectly healthy blocking read. The
		// caller's context bounds the wait instead.
		ReadTimeout: 0,
	})

	q := &RedisStream{client: client, opts: opts}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.ensureGroup(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return q, nil
}

func (q *RedisStream) streamKey() string { return "requests:stream:" + q.opts.TenantID }
func (q *RedisStream) deadKey() string   { return "requests:dead:" + q.opts.TenantID }

// ensureGroup creates the consumer group, tolerating the case where it exists.
func (q *RedisStream) ensureGroup(ctx context.Context) error {
	// MKSTREAM creates the stream if absent, so a fresh deployment does not
	// have to publish before it can consume.
	//
	// "$" would start the group at the CURRENT end, silently skipping every
	// message already waiting. "0" starts at the beginning, which is the only
	// choice that cannot lose requests that arrived before the consumer booted.
	err := q.client.XGroupCreateMkStream(ctx, q.streamKey(), q.opts.Group, "0").Err()
	if err != nil && !isGroupExists(err) {
		return fmt.Errorf("queue: create consumer group: %w", err)
	}
	return nil
}

// isGroupExists reports whether the error is Redis's "the group is already
// there" response, which is the normal case for every process after the first
// and must not be fatal.
//
// Matched with HasPrefix rather than a hand-rolled slice: the first version
// compared err.Error()[:8] against "BUSYGROUP", which is nine characters, so it
// never matched and every second consumer failed to start.
func isGroupExists(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "BUSYGROUP")
}

// Ping verifies connectivity.
func (q *RedisStream) Ping(ctx context.Context) error { return q.client.Ping(ctx).Err() }

// Publish appends a ride request to the stream.
func (q *RedisStream) Publish(ctx context.Context, req RideRequest) (string, error) {
	if req.RequestID == "" {
		return "", ErrEmptyRequestID
	}
	requestedAt := req.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = q.opts.Now()
	}

	// Flat field/value pairs rather than a JSON blob: Redis Streams are
	// natively field-based, and keeping them flat means a human debugging a
	// stuck queue can read an entry with XRANGE without decoding anything.
	id, err := q.client.XAdd(ctx, &redis.XAddArgs{
		Stream: q.streamKey(),
		MaxLen: q.opts.MaxLen,
		Approx: true, // `MAXLEN ~`: trim on node boundaries, far cheaper
		Values: map[string]interface{}{
			"request_id":   req.RequestID,
			"tenant_id":    req.TenantID,
			"rider_id":     req.RiderID,
			"lat":          strconv.FormatFloat(req.Lat, 'f', -1, 64),
			"lng":          strconv.FormatFloat(req.Lng, 'f', -1, 64),
			"requested_at": strconv.FormatInt(requestedAt.UnixMilli(), 10),
		},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("queue: publish: %w", err)
	}
	return id, nil
}

// Consume claims up to max new messages for this consumer.
func (q *RedisStream) Consume(ctx context.Context, max int, block time.Duration) ([]Message, error) {
	if max <= 0 {
		max = 1
	}

	streams, err := q.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    q.opts.Group,
		Consumer: q.opts.Consumer,
		Streams:  []string{q.streamKey(), ">"}, // ">" = messages never delivered to anyone
		Count:    int64(max),
		Block:    block,
		NoAck:    false, // the entire point: claimed messages stay pending until acked
	}).Result()

	if errors.Is(err, redis.Nil) {
		return nil, nil // blocked and nothing arrived; not an error
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("queue: consume: %w", err)
	}

	var out []Message
	for _, stream := range streams {
		for _, entry := range stream.Messages {
			msg, decodeErr := decode(entry)
			if decodeErr != nil {
				// A payload that will not parse now will not parse on any
				// redelivery. Dead-letter immediately rather than looping.
				_ = q.DeadLetter(ctx, Message{ID: entry.ID}, decodeErr.Error())
				continue
			}
			msg.Deliveries = 1 // fresh from ">" — never delivered before
			out = append(out, msg)
		}
	}
	return out, nil
}

// Reclaim takes over messages abandoned by a consumer that never acked them.
func (q *RedisStream) Reclaim(ctx context.Context, minIdle time.Duration, max int) ([]Message, error) {
	if max <= 0 {
		max = 1
	}

	// XPENDING first, because it reports the DELIVERY COUNT, which XAUTOCLAIM
	// alone does not surface. Without that count there is no way to tell a
	// message that failed once from one that has failed fifty times, and no
	// basis for a dead-letter decision.
	pending, err := q.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: q.streamKey(),
		Group:  q.opts.Group,
		Idle:   minIdle,
		Start:  "-",
		End:    "+",
		Count:  int64(max),
	}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("queue: xpending: %w", err)
	}
	if len(pending) == 0 {
		return nil, nil
	}

	var toClaim []string
	deliveries := make(map[string]int64, len(pending))
	for _, p := range pending {
		deliveries[p.ID] = p.RetryCount
		toClaim = append(toClaim, p.ID)
	}

	// XCLAIM transfers ownership to this consumer and increments the delivery
	// count. The message is now in OUR pending list, so if we die too, the next
	// reclaimer picks it up — the recovery mechanism is itself recoverable.
	entries, err := q.client.XClaim(ctx, &redis.XClaimArgs{
		Stream:   q.streamKey(),
		Group:    q.opts.Group,
		Consumer: q.opts.Consumer,
		MinIdle:  minIdle,
		Messages: toClaim,
	}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("queue: xclaim: %w", err)
	}

	var out []Message
	for _, entry := range entries {
		msg, decodeErr := decode(entry)
		if decodeErr != nil {
			_ = q.DeadLetter(ctx, Message{ID: entry.ID}, decodeErr.Error())
			continue
		}
		// +1 because XPENDING reported the count BEFORE this claim.
		msg.Deliveries = deliveries[entry.ID] + 1

		if msg.Deliveries > q.opts.MaxDeliveries {
			// Poison: it has come back too many times. Set it aside so it
			// cannot occupy a consumer slot forever. This is the decision the
			// Week 6 error taxonomy was designed to feed.
			if err := q.DeadLetter(ctx, msg,
				fmt.Sprintf("exceeded MaxDeliveries (%d)", q.opts.MaxDeliveries)); err != nil {
				return out, err
			}
			continue
		}
		out = append(out, msg)
	}
	return out, nil
}

// Ack removes messages from the pending list.
func (q *RedisStream) Ack(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := q.client.XAck(ctx, q.streamKey(), q.opts.Group, ids...).Err(); err != nil {
		return fmt.Errorf("queue: ack: %w", err)
	}
	return nil
}

// DeadLetter moves a message aside and acks it.
func (q *RedisStream) DeadLetter(ctx context.Context, msg Message, reason string) error {
	// Pipelined so the write-aside and the ack go together. They are not
	// atomic, and the ordering is chosen so the surviving failure is safe: if
	// the ack fails after the dead-letter write, the message is reclaimed once
	// more and dead-lettered again — a duplicate in the dead-letter stream,
	// which is inspectable and harmless. The reverse ordering would ack first
	// and could lose the message entirely.
	pipe := q.client.Pipeline()
	pipe.XAdd(ctx, &redis.XAddArgs{
		Stream: q.deadKey(),
		MaxLen: q.opts.MaxLen,
		Approx: true,
		Values: map[string]interface{}{
			"original_id": msg.ID,
			"request_id":  msg.Request.RequestID,
			"tenant_id":   msg.Request.TenantID,
			"rider_id":    msg.Request.RiderID,
			"lat":         strconv.FormatFloat(msg.Request.Lat, 'f', -1, 64),
			"lng":         strconv.FormatFloat(msg.Request.Lng, 'f', -1, 64),
			"deliveries":  strconv.FormatInt(msg.Deliveries, 10),
			"reason":      reason,
			"dead_at":     strconv.FormatInt(q.opts.Now().UnixMilli(), 10),
		},
	})
	if msg.ID != "" {
		pipe.XAck(ctx, q.streamKey(), q.opts.Group, msg.ID)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("queue: dead-letter: %w", err)
	}
	return nil
}

// Depth is the number of undelivered messages.
func (q *RedisStream) Depth(ctx context.Context) (int64, error) {
	groups, err := q.client.XInfoGroups(ctx, q.streamKey()).Result()
	if err != nil {
		return 0, fmt.Errorf("queue: depth: %w", err)
	}
	for _, g := range groups {
		if g.Name == q.opts.Group {
			return g.Lag, nil
		}
	}
	return 0, nil
}

// Pending is the number of claimed-but-unacked messages across the group.
func (q *RedisStream) Pending(ctx context.Context) (int64, error) {
	res, err := q.client.XPending(ctx, q.streamKey(), q.opts.Group).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("queue: pending: %w", err)
	}
	return res.Count, nil
}

// DeadLetterDepth is how many messages have been set aside. A non-zero value
// here should page someone: it means requests are being dropped on the floor,
// which is exactly what this whole package exists to prevent.
func (q *RedisStream) DeadLetterDepth(ctx context.Context) (int64, error) {
	n, err := q.client.XLen(ctx, q.deadKey()).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("queue: dead-letter depth: %w", err)
	}
	return n, nil
}

// Close releases the connection pool.
func (q *RedisStream) Close() error { return q.client.Close() }

func decode(entry redis.XMessage) (Message, error) {
	get := func(k string) string {
		if v, ok := entry.Values[k].(string); ok {
			return v
		}
		return ""
	}

	requestID := get("request_id")
	if requestID == "" {
		return Message{}, fmt.Errorf("%w: entry %s has no request_id", ErrMalformedMessage, entry.ID)
	}

	lat, err := strconv.ParseFloat(get("lat"), 64)
	if err != nil {
		return Message{}, fmt.Errorf("%w: entry %s has a bad lat", ErrMalformedMessage, entry.ID)
	}
	lng, err := strconv.ParseFloat(get("lng"), 64)
	if err != nil {
		return Message{}, fmt.Errorf("%w: entry %s has a bad lng", ErrMalformedMessage, entry.ID)
	}

	var requestedAt time.Time
	if ms, err := strconv.ParseInt(get("requested_at"), 10, 64); err == nil {
		requestedAt = time.UnixMilli(ms)
	}

	return Message{
		ID: entry.ID,
		Request: RideRequest{
			RequestID:   requestID,
			TenantID:    get("tenant_id"),
			RiderID:     get("rider_id"),
			Lat:         lat,
			Lng:         lng,
			RequestedAt: requestedAt,
		},
	}, nil
}

var _ Queue = (*RedisStream)(nil)
