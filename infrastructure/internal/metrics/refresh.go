package metrics

import (
	"context"
	"log/slog"
	"time"
)

// DefaultRefreshInterval is the default period for gauge refresh loops.
const DefaultRefreshInterval = 15 * time.Second

// StreamGauges is satisfied by queue.RedisStream. Each method sets its own
// gauge as a side effect; the refresh loop only calls them.
type StreamGauges interface {
	Depth(ctx context.Context) (int64, error)
	Pending(ctx context.Context) (int64, error)
	DeadLetterDepth(ctx context.Context) (int64, error)
}

// DriverCounter is satisfied by locations.RedisRepository.
type DriverCounter interface {
	Count(ctx context.Context) (int, error)
}

// RefreshStreamGauges calls Depth, Pending and DeadLetterDepth every interval
// until ctx is cancelled, so the queue gauges stay current without a scrape.
// A failing call is logged at warn and the loop continues.
func RefreshStreamGauges(ctx context.Context, s StreamGauges, interval time.Duration, logger *slog.Logger) {
	runLoop(ctx, interval, func() {
		if _, err := s.Depth(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("queue depth refresh failed", "err", err)
		}
		if _, err := s.Pending(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("queue pending refresh failed", "err", err)
		}
		if _, err := s.DeadLetterDepth(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("queue dead-letter refresh failed", "err", err)
		}
	})
}

// RefreshDriverGauge calls Count every interval until ctx is cancelled.
func RefreshDriverGauge(ctx context.Context, c DriverCounter, interval time.Duration, logger *slog.Logger) {
	runLoop(ctx, interval, func() {
		if _, err := c.Count(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("drivers indexed refresh failed", "err", err)
		}
	})
}

func runLoop(ctx context.Context, interval time.Duration, fn func()) {
	if interval <= 0 {
		interval = DefaultRefreshInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}
