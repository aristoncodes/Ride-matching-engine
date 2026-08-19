// loadtest turns the project's performance goals into measured evidence.
//
// Two modes, because they answer different questions:
//
//	--mode=pipeline  End-to-end. N drivers streaming over WebSockets while
//	                 riders submit over REST. Reports p50/p95/p99 of what a
//	                 rider's client actually experiences.
//
//	--mode=sweep     Algorithmic. Talks straight to the C++ engine over gRPC,
//	                 sweeping rider count N and driver count M, to test the
//	                 complexity claim without network noise in the way.
//
// # Percentiles, never averages
//
// An average hides the tail, and the tail is the entire point of surviving a
// spike. A mean of 5 ms with a p99 of 4 s is a system where one rider in a
// hundred waits four seconds — and that rider is the one who uninstalls the app.
//
// # What the sweep is actually testing
//
// The claim is that matching is near O(N log M), not O(N·M):
//
//	each rider's k nearest drivers  -> quadtree query, ~O(log M)
//	solve over N·k edges            -> grows with N, barely with M
//
// So the falsifiable predictions are: DOUBLING N roughly doubles the time,
// while DOUBLING M barely moves it. If instead time scales with N·M, the
// shortlist is not working and the claim is false.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	matchingv1 "github.com/aditya/ride-matching/gen/matching/v1"
	"github.com/aditya/ride-matching/internal/engine"
)

func main() {
	var (
		mode       = flag.String("mode", "pipeline", "pipeline | sweep")
		wsURL      = flag.String("ws", "ws://localhost:8080/v1/drivers/stream", "ingestd WebSocket URL")
		restURL    = flag.String("rest", "http://localhost:8081", "requestd base URL")
		engineAddr = flag.String("engine", "localhost:50051", "C++ engine gRPC address")
		drivers    = flag.Int("drivers", 10000, "concurrent drivers (pipeline mode)")
		riders     = flag.Int("riders", 2000, "ride requests to submit (pipeline mode)")
		rate       = flag.Int("rate", 500, "ride requests per second (pipeline mode)")
		pingEvery  = flag.Duration("ping-every", 3*time.Second, "driver ping interval")
		out        = flag.String("out", "", "write a Markdown report to this path")
		apiKey     = flag.String("key", os.Getenv("RIDEMATCH_API_KEY"),
			"API key (or RIDEMATCH_API_KEY). Required unless the services run with --allow-anonymous")
	)
	flag.Parse()

	switch *mode {
	case "pipeline":
		runPipeline(*wsURL, *restURL, *apiKey, *drivers, *riders, *rate, *pingEvery, *out)
	case "sweep":
		runSweep(*engineAddr, *out)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", *mode)
		os.Exit(2)
	}
}

// ---------------------------------------------------------------------------
// Latency recording
// ---------------------------------------------------------------------------

// recorder collects raw samples rather than a running mean, because
// percentiles cannot be computed incrementally without either an approximation
// structure (t-digest, HDR histogram) or the full set. At these volumes the
// full set costs a few MB and is exact, which is the right trade for a report
// that is meant to be believed.
type recorder struct {
	mu      sync.Mutex
	samples []time.Duration
}

func (r *recorder) add(d time.Duration) {
	r.mu.Lock()
	r.samples = append(r.samples, d)
	r.mu.Unlock()
}

type stats struct {
	Count              int
	Min, P50, P95, P99 time.Duration
	P999, Max          time.Duration
	Mean               time.Duration
}

func (r *recorder) stats() stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.samples) == 0 {
		return stats{}
	}
	s := append([]time.Duration(nil), r.samples...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })

	// Nearest-rank percentile: the smallest value at or above which p% of
	// samples fall. Simple, exact, and does not interpolate between two
	// measurements that never happened.
	at := func(p float64) time.Duration {
		idx := int(p * float64(len(s)))
		if idx >= len(s) {
			idx = len(s) - 1
		}
		return s[idx]
	}

	var total time.Duration
	for _, d := range s {
		total += d
	}

	return stats{
		Count: len(s),
		Min:   s[0],
		P50:   at(0.50),
		P95:   at(0.95),
		P99:   at(0.99),
		P999:  at(0.999),
		Max:   s[len(s)-1],
		Mean:  total / time.Duration(len(s)),
	}
}

func (s stats) String() string {
	return fmt.Sprintf("n=%d  min=%v  p50=%v  p95=%v  p99=%v  p99.9=%v  max=%v  (mean=%v)",
		s.Count, round(s.Min), round(s.P50), round(s.P95), round(s.P99),
		round(s.P999), round(s.Max), round(s.Mean))
}

func round(d time.Duration) time.Duration {
	switch {
	case d > time.Second:
		return d.Round(time.Millisecond)
	case d > time.Millisecond:
		return d.Round(10 * time.Microsecond)
	default:
		return d.Round(time.Microsecond)
	}
}

// ---------------------------------------------------------------------------
// Pipeline mode
// ---------------------------------------------------------------------------

func runPipeline(wsURL, restURL, apiKey string, numDrivers, numRiders, rate int,
	pingEvery time.Duration, outPath string) {

	fmt.Printf("=== pipeline load test ===\n")
	fmt.Printf("drivers=%d  riders=%d  rate=%d/s  ping=%v\n\n", numDrivers, numRiders, rate, pingEvery)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var connected, connectFailed, refused atomic.Int64
	connectLatency := &recorder{}

	// ---- Ramp drivers ----------------------------------------------------
	fmt.Printf("connecting %d drivers...\n", numDrivers)
	rampStart := time.Now()

	var wg sync.WaitGroup
	// A semaphore on the DIAL only. Opening 10k sockets simultaneously measures
	// the kernel's accept queue rather than the server, and on macOS routinely
	// exhausts ephemeral ports.
	dialSem := make(chan struct{}, 200)

	for i := 0; i < numDrivers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			dialSem <- struct{}{}
			start := time.Now()
			conn, resp, err := websocket.DefaultDialer.Dial(wsURL, authHeader(apiKey))
			<-dialSem

			if err != nil {
				if resp != nil && resp.StatusCode == http.StatusServiceUnavailable {
					refused.Add(1) // the server correctly enforcing its limit
				} else {
					connectFailed.Add(1)
				}
				return
			}
			connectLatency.add(time.Since(start))
			connected.Add(1)
			defer conn.Close()

			// A reader is required: gorilla only answers pings from inside a
			// read call, so without this every driver fails its heartbeat and
			// the server correctly hangs up on all of them.
			go func() {
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
				}
			}()

			rng := rand.New(rand.NewSource(int64(id)*7919 + 3))
			lat := 12.9716 + (rng.Float64()-0.5)*0.09
			lng := 77.5946 + (rng.Float64()-0.5)*0.09

			// Stagger, or every driver fires on the same millisecond and the
			// load arrives as a spike per interval rather than as a stream.
			select {
			case <-time.After(time.Duration(rng.Int63n(int64(pingEvery)))):
			case <-ctx.Done():
				return
			}

			ticker := time.NewTicker(pingEvery)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					lat += (rng.Float64() - 0.5) * 0.0002
					lng += (rng.Float64() - 0.5) * 0.0002
					msg, _ := json.Marshal(map[string]interface{}{
						"driver_id": fmt.Sprintf("D-%06d", id),
						"lat":       lat, "lng": lng,
					})
					_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
					if conn.WriteMessage(websocket.TextMessage, msg) != nil {
						return
					}
				}
			}
		}(i)
	}

	// Wait for the ramp to settle rather than for every goroutine (they run
	// until cancel).
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		done := connected.Load() + connectFailed.Load() + refused.Load()
		if done >= int64(numDrivers) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	rampDuration := time.Since(rampStart)

	fmt.Printf("  connected=%d failed=%d refused=%d in %v\n",
		connected.Load(), connectFailed.Load(), refused.Load(), rampDuration.Round(time.Millisecond))
	fmt.Printf("  connect latency: %s\n\n", connectLatency.stats())

	// Let a couple of ping cycles land so the driver pool is populated before
	// riders start arriving — otherwise this measures an empty city.
	fmt.Printf("warming driver locations (%v)...\n", 2*pingEvery)
	time.Sleep(2 * pingEvery)

	// ---- Submit ride requests --------------------------------------------
	fmt.Printf("submitting %d ride requests at %d/s...\n", numRiders, rate)
	requestLatency := &recorder{}
	var accepted, rejected, failed atomic.Int64

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			// Without raising these, Go's default of 2 idle connections per
			// host forces a new TCP handshake for almost every request, and the
			// test measures connection setup instead of the service.
			MaxIdleConns:        512,
			MaxIdleConnsPerHost: 512,
			MaxConnsPerHost:     512,
		},
	}

	submitStart := time.Now()
	interval := time.Second / time.Duration(rate)
	limiter := time.NewTicker(interval)
	defer limiter.Stop()

	var submitWG sync.WaitGroup
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < numRiders; i++ {
		<-limiter.C
		lat := 12.9716 + (rng.Float64()-0.5)*0.09
		lng := 77.5946 + (rng.Float64()-0.5)*0.09

		submitWG.Add(1)
		go func(i int, lat, lng float64) {
			defer submitWG.Done()
			body := fmt.Sprintf(`{"rider_id":"R-%06d","pickup":{"lat":%f,"lng":%f}}`, i, lat, lng)

			req, err := http.NewRequest(http.MethodPost, restURL+"/v1/ride-requests",
				strings.NewReader(body))
			if err != nil {
				failed.Add(1)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			if apiKey != "" {
				req.Header.Set("X-API-Key", apiKey)
			}

			start := time.Now()
			resp, err := client.Do(req)
			elapsed := time.Since(start)

			if err != nil {
				failed.Add(1)
				return
			}
			defer resp.Body.Close()
			_, _ = resp.Body.Read(make([]byte, 256))

			requestLatency.add(elapsed)
			switch {
			case resp.StatusCode == http.StatusAccepted:
				accepted.Add(1)
			case resp.StatusCode >= 500:
				failed.Add(1)
			default:
				rejected.Add(1)
			}
		}(i, lat, lng)
	}
	submitWG.Wait()
	submitDuration := time.Since(submitStart)

	reqStats := requestLatency.stats()
	fmt.Printf("  accepted=%d rejected=%d failed=%d in %v (%.0f req/s achieved)\n",
		accepted.Load(), rejected.Load(), failed.Load(),
		submitDuration.Round(time.Millisecond),
		float64(accepted.Load())/submitDuration.Seconds())
	fmt.Printf("  request latency: %s\n\n", reqStats)

	cancel()

	if outPath != "" {
		writePipelineReport(outPath, numDrivers, numRiders, rate,
			connected.Load(), refused.Load(), connectLatency.stats(), reqStats,
			rampDuration, submitDuration)
		fmt.Printf("report written to %s\n", outPath)
	}
}

// ---------------------------------------------------------------------------
// Sweep mode — the complexity claim
// ---------------------------------------------------------------------------

type sweepPoint struct {
	Series             string // "N", "M" or "dense" — tagged explicitly
	Riders, Drivers, K int
	Solve              stats
}

// authHeader carries the key on the WebSocket handshake, or nothing at all
// when the services are running anonymously.
func authHeader(apiKey string) http.Header {
	if apiKey == "" {
		return nil
	}
	return http.Header{"X-API-Key": []string{apiKey}}
}

func runSweep(engineAddr string, outPath string) {
	fmt.Printf("=== complexity sweep (direct gRPC to the engine) ===\n\n")

	client, err := engine.Dial(engineAddr, engine.WithTimeout(120*time.Second))
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	if _, err := client.Health(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "engine unreachable at %s: %v\n", engineAddr, err)
		os.Exit(1)
	}

	const k = 8
	const repeats = 7 // odd, so the median is an actual measurement

	var points []sweepPoint

	// --- Vary N with M fixed. Prediction: roughly linear. ---
	fmt.Println("A. riders (N) varying, drivers (M) fixed at 2000, k=8")
	for _, n := range []int{100, 200, 400, 800, 1600, 3200} {
		p := measure(ctx, client, n, 2000, k, repeats)
		p.Series = "N"
		points = append(points, p)
		fmt.Printf("   N=%-5d M=2000  %s\n", n, p.Solve)
	}

	// --- Vary M with N fixed. Prediction: barely moves (the log term). ---
	fmt.Println("\nB. drivers (M) varying, riders (N) fixed at 500, k=8")
	for _, m := range []int{500, 1000, 2000, 4000, 8000, 16000} {
		p := measure(ctx, client, 500, m, k, repeats)
		p.Series = "M"
		points = append(points, p)
		fmt.Printf("   N=500  M=%-6d %s\n", m, p.Solve)
	}

	// --- The control: dense, O(N*M). Prediction: blows up. ---
	fmt.Println("\nC. control — DENSE matrix (k=0, every pair), which is O(N*M)")
	var densePoints []sweepPoint
	for _, n := range []int{100, 200, 400, 800} {
		p := measure(ctx, client, n, n, 0, 3)
		p.Series = "dense"
		densePoints = append(densePoints, p)
		fmt.Printf("   N=M=%-5d dense  %s\n", n, p.Solve)
	}

	fmt.Println()
	analyse(points, densePoints)

	if outPath != "" {
		writeSweepReport(outPath, points, densePoints)
		fmt.Printf("\nreport written to %s\n", outPath)
	}
}

func measure(ctx context.Context, client *engine.Client, n, m, k, repeats int) sweepPoint {
	req := buildBatch(n, m, k)
	rec := &recorder{}

	// One untimed warm-up: the first call after a change in batch shape has a
	// different allocation profile, and including it would report a number no
	// steady-state request ever sees.
	if _, err := client.SolveBatch(ctx, req); err != nil {
		fmt.Fprintf(os.Stderr, "warm-up N=%d M=%d: %v\n", n, m, err)
	}

	for i := 0; i < repeats; i++ {
		start := time.Now()
		resp, err := client.SolveBatch(ctx, req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "solve N=%d M=%d: %v\n", n, m, err)
			continue
		}
		// Prefer the engine's OWN measurement where available: it excludes
		// serialization and the loopback round trip, isolating the algorithm.
		if resp.GetComputeMicros() > 0 {
			rec.add(time.Duration(resp.GetComputeMicros()) * time.Microsecond)
		} else {
			rec.add(time.Since(start))
		}
	}
	st := rec.stats()
	if st.Count == 0 {
		// Every attempt failed. Reported as a zero-count point and skipped by
		// the analysis rather than folded in as "0s", which would look like the
		// fastest result in the table.
		fmt.Fprintf(os.Stderr, "  (no successful measurements at N=%d M=%d)\n", n, m)
	}
	return sweepPoint{Riders: n, Drivers: m, K: k, Solve: st}
}

func buildBatch(n, m, k int) *matchingv1.MatchBatchRequest {
	rng := rand.New(rand.NewSource(int64(n)*1000003 + int64(m)))
	req := &matchingv1.MatchBatchRequest{
		TenantId:              "loadtest",
		BatchId:               fmt.Sprintf("sweep-%d-%d-%d", n, m, k),
		CostMetric:            matchingv1.CostMetric_COST_METRIC_EUCLIDEAN,
		MaxCandidatesPerRider: int32(k),
	}
	// A 20 km box, roughly a metropolitan service area.
	for i := 0; i < n; i++ {
		req.Riders = append(req.Riders, &matchingv1.Rider{
			Id: fmt.Sprintf("R-%06d", i),
			Pickup: &matchingv1.LatLng{
				Lat: 12.9716 + (rng.Float64()-0.5)*0.18,
				Lng: 77.5946 + (rng.Float64()-0.5)*0.18,
			},
		})
	}
	for j := 0; j < m; j++ {
		req.CandidateDrivers = append(req.CandidateDrivers, &matchingv1.Driver{
			Id: fmt.Sprintf("D-%06d", j),
			Location: &matchingv1.LatLng{
				Lat: 12.9716 + (rng.Float64()-0.5)*0.18,
				Lng: 77.5946 + (rng.Float64()-0.5)*0.18,
			},
		})
	}
	return req
}

// analyse turns the raw points into the falsifiable statement the week asks
// for: does measured growth track the claim, or O(N*M)?
func analyse(sparse, dense []sweepPoint) {
	fmt.Println("=== analysis ===")

	// Growth in N: doubling N should roughly double the time.
	var nPoints []sweepPoint
	for _, p := range sparse {
		if p.Series == "N" && p.Solve.Count > 0 {
			nPoints = append(nPoints, p)
		}
	}
	fmt.Println("\nDoubling N (M fixed) — linear would be a ratio near 2.0:")
	for i := 1; i < len(nPoints); i++ {
		prev, cur := nPoints[i-1], nPoints[i]
		if prev.Solve.P50 > 0 {
			fmt.Printf("   N %5d -> %-5d  time x%.2f\n",
				prev.Riders, cur.Riders,
				float64(cur.Solve.P50)/float64(prev.Solve.P50))
		}
	}

	// Growth in M: doubling M should barely move the time.
	var mPoints []sweepPoint
	for _, p := range sparse {
		if p.Series == "M" && p.Solve.Count > 0 {
			mPoints = append(mPoints, p)
		}
	}
	fmt.Println("\nDoubling M (N fixed) — O(N log M) predicts a ratio near 1.0;")
	fmt.Println("                       O(N*M) would predict 2.0:")
	for i := 1; i < len(mPoints); i++ {
		prev, cur := mPoints[i-1], mPoints[i]
		if prev.Solve.P50 > 0 {
			fmt.Printf("   M %6d -> %-6d time x%.2f\n",
				prev.Drivers, cur.Drivers,
				float64(cur.Solve.P50)/float64(prev.Solve.P50))
		}
	}

	fmt.Println("\nControl — dense O(N*M), doubling BOTH N and M (so N*M x4):")
	for i := 1; i < len(dense); i++ {
		prev, cur := dense[i-1], dense[i]
		if prev.Solve.P50 > 0 {
			fmt.Printf("   N=M %4d -> %-5d time x%.2f\n",
				prev.Riders, cur.Riders,
				float64(cur.Solve.P50)/float64(prev.Solve.P50))
		}
	}
}
