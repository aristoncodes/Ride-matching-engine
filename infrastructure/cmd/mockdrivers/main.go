// mockdrivers is a load generator: N simulated drivers streaming GPS pings to
// ingestd over WebSockets.
//
// It exists because the Week 9 checkpoint is not "the code compiles" — it is
// "pings flow in every 3 seconds, Redis reflects them, and the system stays
// bounded under overload". You cannot observe any of that without traffic.
//
// Usage:
//
//	mockdrivers --url ws://localhost:8080/v1/drivers/stream --drivers 500 --interval 3s
//	mockdrivers --drivers 20000 --interval 100ms      # overload: watch shedding
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

type ping struct {
	DriverID     string  `json:"driver_id"`
	Lat          float64 `json:"lat"`
	Lng          float64 `json:"lng"`
	SentAtUnixMs int64   `json:"sent_at_ms"`
}

func main() {
	var (
		url       = flag.String("url", "ws://localhost:8080/v1/drivers/stream", "ingestd WebSocket URL")
		drivers   = flag.Int("drivers", 100, "number of simulated drivers")
		interval  = flag.Duration("interval", 3*time.Second, "ping interval per driver")
		duration  = flag.Duration("duration", 0, "stop after this long (0 = run until interrupted)")
		centreLat = flag.Float64("lat", 12.9716, "centre latitude")
		centreLng = flag.Float64("lng", 77.5946, "centre longitude")
		spreadKm  = flag.Float64("spread-km", 5.0, "how far drivers wander from the centre")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	var (
		connected atomic.Int64
		sent      atomic.Int64
		failed    atomic.Int64
		refused   atomic.Int64
	)

	var wg sync.WaitGroup
	for i := 0; i < *drivers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			runDriver(ctx, *url, id, *centreLat, *centreLng, *spreadKm, *interval,
				&connected, &sent, &failed, &refused)
		}(i)

		// Ramp up rather than opening every socket at once: a thundering herd
		// of connections measures the accept queue, not the pipeline.
		if i%50 == 49 {
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Progress, so an operator can see it working without reading ingestd's log.
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		var last int64
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := sent.Load()
				log.Printf("connected=%d sent=%d (%.0f/s) failed=%d refused=%d",
					connected.Load(), now, float64(now-last)/2.0, failed.Load(), refused.Load())
				last = now
			}
		}
	}()

	wg.Wait()
	fmt.Printf("\ntotal: sent=%d failed=%d refused=%d\n",
		sent.Load(), failed.Load(), refused.Load())
}

func runDriver(ctx context.Context, url string, id int,
	centreLat, centreLng, spreadKm float64, interval time.Duration,
	connected, sent, failed, refused *atomic.Int64) {

	// Independent RNG per goroutine. Sharing the global one would serialise
	// every driver on its internal lock and measure that instead of the server.
	rng := rand.New(rand.NewSource(int64(id)*7919 + 13))

	driverID := fmt.Sprintf("D-%05d", id)
	const kmPerDegree = 111.0
	lat := centreLat + (rng.Float64()-0.5)*2*spreadKm/kmPerDegree
	lng := centreLng + (rng.Float64()-0.5)*2*spreadKm/kmPerDegree

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		// A 503 is the server correctly enforcing its connection limit, which
		// is a successful test of backpressure rather than a failure of this
		// tool — so it is counted separately.
		if resp != nil && resp.StatusCode == 503 {
			refused.Add(1)
		} else {
			failed.Add(1)
		}
		return
	}
	defer conn.Close()

	connected.Add(1)
	defer connected.Add(-1)

	// A reader is required even though the server sends no application
	// messages: gorilla only replies to pings from inside a read call, so
	// without this the driver never pongs and the server correctly hangs up.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Stagger the first ping. Without it every driver fires on the same
	// millisecond and the load arrives as a spike per interval rather than a
	// stream, which is not what a real fleet looks like.
	select {
	case <-time.After(time.Duration(rng.Int63n(int64(interval)))):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(time.Second))
			return

		case <-ticker.C:
			// Drift ~10 m per tick, so positions actually change and the geo
			// index is exercised rather than rewriting the same point.
			lat += (rng.Float64() - 0.5) * 0.0002
			lng += (rng.Float64() - 0.5) * 0.0002

			data, err := json.Marshal(ping{
				DriverID: driverID, Lat: lat, Lng: lng, SentAtUnixMs: time.Now().UnixMilli(),
			})
			if err != nil {
				failed.Add(1)
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				failed.Add(1)
				return
			}
			sent.Add(1)
		}
	}
}
