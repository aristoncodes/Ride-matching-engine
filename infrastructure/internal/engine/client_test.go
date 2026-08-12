package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	matchingv1 "github.com/aditya/ride-matching/gen/matching/v1"
	"github.com/aditya/ride-matching/internal/engine"
	"github.com/aditya/ride-matching/internal/testutil"
)

func batch(id string, riders, drivers int) *matchingv1.MatchBatchRequest {
	req := &matchingv1.MatchBatchRequest{
		TenantId:   "t-test",
		BatchId:    id,
		CostMetric: matchingv1.CostMetric_COST_METRIC_EUCLIDEAN,
	}
	for i := 0; i < riders; i++ {
		req.Riders = append(req.Riders, &matchingv1.Rider{
			Id:     "R-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Pickup: &matchingv1.LatLng{Lat: 12.97 + float64(i)*0.001, Lng: 77.59},
		})
	}
	for j := 0; j < drivers; j++ {
		// Offset off the riders' latitude on purpose: placing a driver at the
		// exact coordinate of a rider is legal and correctly yields a zero
		// distance, which would make a "> 0" assertion below meaningless.
		req.CandidateDrivers = append(req.CandidateDrivers, &matchingv1.Driver{
			Id:       "D-" + string(rune('a'+j%26)) + string(rune('0'+j/26)),
			Location: &matchingv1.LatLng{Lat: 12.9705, Lng: 77.59 + float64(j)*0.001},
		})
	}
	return req
}

func TestSolveBatchRoundTrip(t *testing.T) {
	proc := testutil.StartEngine(t, false)

	client, err := engine.Dial(proc.Addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	resp, err := client.SolveBatch(context.Background(), batch("b-1", 5, 5))
	if err != nil {
		t.Fatalf("solve: %v", err)
	}

	if got := len(resp.GetMatches()); got != 5 {
		t.Fatalf("matches = %d, want 5", got)
	}
	if resp.GetBatchId() != "b-1" {
		t.Errorf("batch_id = %q, want b-1", resp.GetBatchId())
	}
	if resp.GetComputeMicros() <= 0 {
		t.Errorf("compute_micros = %d, want > 0", resp.GetComputeMicros())
	}

	// No driver may appear twice — the invariant the whole flow model exists
	// for, verified here across the wire rather than only in C++.
	seen := map[string]bool{}
	for _, m := range resp.GetMatches() {
		if seen[m.GetDriverId()] {
			t.Fatalf("driver %s matched twice", m.GetDriverId())
		}
		seen[m.GetDriverId()] = true
	}
}

// TestSurvivesEngineCrash is the Week 6 checkpoint.
//
// The claim in ADR-0002 is that running C++ as a separate process converts a
// crash into an error value. This kills the engine with SIGKILL — no cleanup,
// no goodbye, the way a segfault or an OOM kill arrives — and asserts that:
//
//  1. the Go process is still running and able to make decisions;
//  2. the failure is a typed, classified error, not a panic or a hang;
//  3. the batch is marked RETRYABLE, so the request is never silently lost;
//  4. the client recovers on its own once the engine comes back.
//
// (4) matters as much as the rest: an isolation story that requires restarting
// the Go layer to recover is not isolation.
func TestSurvivesEngineCrash(t *testing.T) {
	proc := testutil.StartEngine(t, false)

	client, err := engine.Dial(proc.Addr, engine.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// Healthy to begin with, so a later failure is attributable to the kill.
	if _, err := client.SolveBatch(context.Background(), batch("b-before", 3, 3)); err != nil {
		t.Fatalf("pre-crash solve: %v", err)
	}

	proc.Kill(t)

	// The Go process is alive here at all, which is most of the point.
	_, err = client.SolveBatch(context.Background(), batch("b-after", 3, 3))
	if err == nil {
		t.Fatal("expected an error after the engine was killed, got nil")
	}
	if !errors.Is(err, engine.ErrEngineUnavailable) {
		t.Fatalf("error = %v, want ErrEngineUnavailable", err)
	}
	if !engine.Retryable(err) {
		t.Fatal("a crashed engine must be retryable — the batch is not at fault")
	}

	// And it recovers by itself when the engine returns. gRPC reconnects
	// underneath; no code in the service layer has to know a crash happened.
	restarted := testutil.StartEngineAt(t, proc.Addr)
	defer restarted.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	recovered := testutil.WaitFor(ctx, 15*time.Second, func() bool {
		_, err := client.SolveBatch(ctx, batch("b-recovered", 3, 3))
		return err == nil
	})
	if !recovered {
		t.Fatal("client did not recover after the engine restarted")
	}
}

func TestDeadlineIsEnforced(t *testing.T) {
	proc := testutil.StartEngine(t, false)

	// A deadline so short the call cannot possibly complete. The point is that
	// the caller regains control at the deadline rather than whenever the
	// engine feels like answering.
	client, err := engine.Dial(proc.Addr, engine.WithTimeout(1*time.Nanosecond))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	start := time.Now()
	_, err = client.SolveBatch(context.Background(), batch("b-slow", 200, 200))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout")
	}
	if !errors.Is(err, engine.ErrTimeout) && !errors.Is(err, engine.ErrEngineUnavailable) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %v to honour a 1ns deadline", elapsed)
	}
}

func TestCallerCancellationIsHonoured(t *testing.T) {
	proc := testutil.StartEngine(t, false)

	client, err := engine.Dial(proc.Addr, engine.WithTimeout(30*time.Second))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// Cancellation is not the same as a deadline: it lets a caller abandon work
	// it no longer needs without waiting the deadline out.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if _, err := client.SolveBatch(ctx, batch("b-cancelled", 50, 50)); err == nil {
		t.Fatal("expected an error from a cancelled context")
	} else if !errors.Is(err, engine.ErrCancelled) {
		t.Fatalf("error = %v, want ErrCancelled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancellation took %v to take effect", elapsed)
	}
}

func TestInvalidBatchIsNotRetryable(t *testing.T) {
	proc := testutil.StartEngine(t, false)

	client, err := engine.Dial(proc.Addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// Duplicate rider ids: the response would be ambiguous, so the engine
	// refuses. Retrying this forever is how a queue stalls on poison.
	req := batch("b-bad", 1, 1)
	req.Riders = append(req.Riders, &matchingv1.Rider{
		Id:     req.Riders[0].Id,
		Pickup: &matchingv1.LatLng{Lat: 12.98, Lng: 77.59},
	})

	_, err = client.SolveBatch(context.Background(), req)
	if !errors.Is(err, engine.ErrInvalidBatch) {
		t.Fatalf("error = %v, want ErrInvalidBatch", err)
	}
	if engine.Retryable(err) {
		t.Fatal("a malformed batch must not be retryable")
	}
}

func TestTravelTimeWithoutGraphIsNotRetryable(t *testing.T) {
	proc := testutil.StartEngine(t, false) // started WITHOUT a graph

	client, err := engine.Dial(proc.Addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	req := batch("b-tt", 2, 2)
	req.CostMetric = matchingv1.CostMetric_COST_METRIC_TRAVEL_TIME
	req.RoadGraphId = "blr-central"

	_, err = client.SolveBatch(context.Background(), req)
	if !errors.Is(err, engine.ErrGraphNotLoaded) {
		t.Fatalf("error = %v, want ErrGraphNotLoaded", err)
	}
	if engine.Retryable(err) {
		t.Fatal("a missing graph needs an operator, not a retry")
	}
}

func TestHealthReportsLoadedGraphs(t *testing.T) {
	proc := testutil.StartEngine(t, true) // WITH the Bengaluru extract

	client, err := engine.Dial(proc.Addr, engine.WithTimeout(10*time.Second))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	ok, err := client.HasGraph(context.Background(), proc.GraphID)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !ok {
		t.Fatalf("engine does not report graph %q as loaded", proc.GraphID)
	}

	if ok, _ := client.HasGraph(context.Background(), "atlantis"); ok {
		t.Error("reported a graph that was never loaded")
	}
}

func TestTravelTimeMatchingOverTheWire(t *testing.T) {
	proc := testutil.StartEngine(t, true)

	client, err := engine.Dial(proc.Addr, engine.WithTimeout(20*time.Second))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	req := batch("b-road", 5, 5)
	req.CostMetric = matchingv1.CostMetric_COST_METRIC_TRAVEL_TIME
	req.RoadGraphId = proc.GraphID
	req.MaxCandidatesPerRider = 4

	resp, err := client.SolveBatch(context.Background(), req)
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	if resp.GetCostMetricUsed() != matchingv1.CostMetric_COST_METRIC_TRAVEL_TIME {
		t.Errorf("cost_metric_used = %v", resp.GetCostMetricUsed())
	}
	if resp.GetRoadGraphId() != proc.GraphID {
		t.Errorf("road_graph_id = %q", resp.GetRoadGraphId())
	}
	for _, m := range resp.GetMatches() {
		// Presence, not value: the whole reason eta_seconds is optional is so
		// "absent" cannot be confused with "arrives immediately".
		if m.EtaSeconds == nil {
			t.Errorf("match %s has no ETA under travel-time pricing", m.GetRiderId())
		}
		if m.GetStraightLineMeters() <= 0 {
			t.Errorf("match %s has no straight-line distance", m.GetRiderId())
		}
	}
}

func TestRetryableClassification(t *testing.T) {
	// Pure table test, no engine needed — pins the ack-vs-requeue policy that
	// Week 9's backpressure and Week 10's dead-letter path both depend on.
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"engine down", engine.ErrEngineUnavailable, true},
		{"timeout", engine.ErrTimeout, true},
		{"malformed", engine.ErrInvalidBatch, false},
		{"missing graph", engine.ErrGraphNotLoaded, false},
		{"too large", engine.ErrBatchTooLarge, false},
		{"cancelled", engine.ErrCancelled, false},
		{"unknown defaults to retryable", errors.New("mystery"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := engine.Retryable(tc.err); got != tc.want {
				t.Errorf("Retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
