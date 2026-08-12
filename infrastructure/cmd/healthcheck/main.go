// healthcheck is a container HEALTHCHECK probe for images that have no shell.
//
// The runtime images are built `FROM scratch`, which is the point — no libc, no
// package manager, nothing for an attacker to use. But it also means there is no
// curl, no wget, and no `sh` to run them from, so the usual
//
//	HEALTHCHECK CMD curl -f http://localhost:8080/healthz
//
// cannot work: Docker's shell form needs /bin/sh, and the exec form needs a
// binary that exists in the image.
//
// So the probe is a Go binary compiled into the image alongside the service.
// It is a few hundred KB, needs nothing at runtime, and is invoked in exec form:
//
//	HEALTHCHECK CMD ["/healthcheck", "http://127.0.0.1:8080/healthz"]
//
// Exit 0 = healthy, non-zero = unhealthy, which is the whole contract.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: healthcheck <http://host:port/path | host:port>")
		os.Exit(2)
	}
	target := os.Args[1]

	// Deliberately shorter than Docker's --timeout. A probe that outlives its
	// own timeout gets killed with no diagnostic; failing first at least logs
	// something an operator can read.
	const timeout = 2 * time.Second

	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		os.Exit(probeHTTP(target, timeout))
	}
	os.Exit(probeTCP(target, timeout))
}

func probeHTTP(url string, timeout time.Duration) int {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	// Any 2xx is healthy. Notably 503 is NOT — that is how a readiness endpoint
	// reports a dependency outage, and treating it as healthy would defeat the
	// entire purpose of having one.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %d\n", url, resp.StatusCode)
		return 1
	}
	return 0
}

// probeTCP is for services with no HTTP surface — the gRPC engine, for example.
// It proves only that something is listening, which is weaker than an
// application-level check but is exactly what compose needs to order startup.
func probeTCP(addr string, timeout time.Duration) int {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	_ = conn.Close()
	return 0
}
