package engine

import (
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Failure taxonomy for calls into the C++ engine.
//
// The only question the service layer ever actually asks about a failure is
// "do I retry this batch, or is it poison?". gRPC status codes answer that,
// but only if you know the mapping — so it is encoded once, here, instead of
// being re-derived (differently, and eventually wrongly) at each call site.
//
// This taxonomy is what Week 9's backpressure and Week 10's dead-letter path
// are built on: Retryable batches go back on the queue, non-retryable ones go
// aside so they cannot block it forever.
var (
	// ErrEngineUnavailable — the engine process is down, restarting, or
	// unreachable. THE case ADR-0002 chose gRPC-over-cgo for: a C++ segfault
	// arrives here as an error instead of taking this process down with it.
	// Retryable: nothing about the batch is wrong.
	ErrEngineUnavailable = errors.New("matching engine unavailable")

	// ErrTimeout — the engine did not answer inside the deadline. Retryable,
	// because SolveBatch is stateless and side-effect free, so a retry cannot
	// double-book anyone.
	ErrTimeout = errors.New("matching engine timed out")

	// ErrInvalidBatch — malformed input: duplicate ids, bad coordinates.
	// NOT retryable. Retrying a poison batch forever is how a queue stalls.
	ErrInvalidBatch = errors.New("invalid batch")

	// ErrGraphNotLoaded — travel-time pricing was requested against a graph the
	// engine does not have. Not retryable by the caller; it needs an operator
	// or a different cost metric.
	ErrGraphNotLoaded = errors.New("road graph not loaded on engine")

	// ErrBatchTooLarge — above the engine's configured limit. Not retryable as
	// is; the batch must be split.
	ErrBatchTooLarge = errors.New("batch too large for engine")

	// ErrCancelled — the caller's context was cancelled. Not an engine fault.
	ErrCancelled = errors.New("call cancelled")
)

// classify converts a gRPC error into one of the sentinels above, keeping the
// server's message as context. Callers match with errors.Is.
func classify(err error) error {
	if err == nil {
		return nil
	}

	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%w: %v", ErrEngineUnavailable, err)
	}

	switch st.Code() {
	case codes.OK:
		return nil
	case codes.Unavailable:
		// Connection refused, connection reset, server shutting down — and,
		// importantly, the engine having crashed mid-call.
		return fmt.Errorf("%w: %s", ErrEngineUnavailable, st.Message())
	case codes.DeadlineExceeded:
		return fmt.Errorf("%w: %s", ErrTimeout, st.Message())
	case codes.InvalidArgument:
		return fmt.Errorf("%w: %s", ErrInvalidBatch, st.Message())
	case codes.FailedPrecondition:
		return fmt.Errorf("%w: %s", ErrGraphNotLoaded, st.Message())
	case codes.ResourceExhausted:
		return fmt.Errorf("%w: %s", ErrBatchTooLarge, st.Message())
	case codes.Canceled:
		return fmt.Errorf("%w: %s", ErrCancelled, st.Message())
	default:
		// Unknown/Internal most often means the engine died mid-handler, so it
		// is treated as unavailable — retryable, which is the safe default when
		// the batch itself is not known to be at fault.
		return fmt.Errorf("%w: %s (code %s)", ErrEngineUnavailable, st.Message(), st.Code())
	}
}

// Retryable reports whether re-submitting the same batch could plausibly
// succeed. The batcher uses this to decide ack-vs-requeue.
//
// Deliberately conservative in one direction only: an unrecognised error is
// treated as retryable, because dropping a real ride request is worse than
// attempting it twice on a stateless, side-effect-free call.
func Retryable(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrInvalidBatch),
		errors.Is(err, ErrGraphNotLoaded),
		errors.Is(err, ErrBatchTooLarge),
		errors.Is(err, ErrCancelled):
		return false
	default:
		return true
	}
}
