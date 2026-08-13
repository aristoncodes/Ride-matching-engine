package locations_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aditya/ride-matching/internal/locations"
)

// TestNearbyManyIsFasterThanSequential measures the Week 22 optimisation
// directly, rather than inferring it from production logs where batch sizes
// differ between runs.
//
// This is the isolated claim: for a batch of N riders, one pipelined call beats
// N sequential ones. It is a TEST rather than a Benchmark so it runs in CI and
// keeps the two paths honest with each other, but the numbers it logs are the
// measurement.
func TestNearbyManyIsFasterThanSequential(t *testing.T) {
	repo := newRepo(t, nil)
	ctx := context.Background()

	// A realistic driver pool.
	locs := make([]locations.DriverLocation, 0, 2000)
	for i := 0; i < 2000; i++ {
		locs = append(locs, locations.DriverLocation{
			DriverID: fmt.Sprintf("D-%04d", i),
			Lat:      baseLat + float64(i%50)*0.001,
			Lng:      baseLng + float64(i/50)*0.001,
		})
	}
	if err := repo.UpsertMany(ctx, locs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, riders := range []int{50, 200, 500} {
		queries := make([]locations.Query, riders)
		for i := range queries {
			queries[i] = locations.Query{
				Lat:    baseLat + float64(i%50)*0.001,
				Lng:    baseLng + float64(i/50)*0.001,
				Radius: 5000,
				Limit:  32,
			}
		}

		// Warm both paths so neither pays first-call costs in the measurement.
		_, _ = repo.NearbyMany(ctx, queries[:1])
		_, _ = repo.Nearby(ctx, queries[0])

		start := time.Now()
		for _, q := range queries {
			if _, err := repo.Nearby(ctx, q); err != nil {
				t.Fatalf("sequential: %v", err)
			}
		}
		sequential := time.Since(start)

		start = time.Now()
		if _, err := repo.NearbyMany(ctx, queries); err != nil {
			t.Fatalf("pipelined: %v", err)
		}
		pipelined := time.Since(start)

		speedup := float64(sequential) / float64(pipelined)
		t.Logf("riders=%-4d sequential=%-10v pipelined=%-10v speedup=%.1fx",
			riders, sequential.Round(time.Microsecond),
			pipelined.Round(time.Microsecond), speedup)

		// A REGRESSION guard, not a performance claim.
		//
		// The first version asserted >1.5x and failed at 1.2x, which taught me
		// something the profile alone had not: round trips were never the
		// dominant cost here. Measured loopback RTT is ~0.10 ms, so 500
		// sequential queries spend ~50 ms waiting on the network out of ~131 ms
		// total. The remaining ~80 ms is Redis EXECUTING 500 GEOSEARCHes
		// (single-threaded, so pipelining cannot overlap them) plus the client
		// parsing 500 result sets. Pipelining removes the round trips and
		// nothing else, which caps the win near 1.4x on loopback.
		//
		// Across a real network the same change is worth far more: at 0.5 ms
		// RTT those 500 round trips cost 250 ms rather than 50 ms, and removing
		// them dominates. The ratio is therefore a property of the DEPLOYMENT,
		// not of the code, so the bound stays loose and only catches a revert to
		// per-query round trips.
		if riders >= 200 && speedup < 1.05 {
			t.Errorf("riders=%d: pipelined (%.2fx) is no faster than sequential — "+
				"the batched path may have reverted to per-query round trips",
				riders, speedup)
		}
	}
}
