package ingest_test

import (
	"net/http"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/aditya/ride-matching/internal/ingest"
)

// TestOriginPolicy pins the browser-facing half of the upgrade check.
//
// WebSockets are not covered by the same-origin policy: a page on any site can
// open a ws:// connection to us with the user's cookies attached, and there is
// no CORS preflight to stop it. CheckOrigin is the only place to refuse, and
// gorilla's default accepts everything — which is what this used to do.
func TestOriginPolicy(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		origin  string
		wantOK  bool
	}{
		{
			// The important default. A native driver app sends no Origin, so
			// the strict-looking empty allowlist does not break a single real
			// client — and rejecting a missing Origin would, while stopping no
			// attack, since anything that can omit a header can forge one.
			name:   "no origin is allowed with an empty allowlist",
			origin: "",
			wantOK: true,
		},
		{
			name:   "a browser origin is refused by default",
			origin: "https://evil.example",
			wantOK: false,
		},
		{
			name:    "an allowlisted origin is accepted",
			allowed: []string{"https://ops.acme.example"},
			origin:  "https://ops.acme.example",
			wantOK:  true,
		},
		{
			name:    "a non-listed origin is still refused",
			allowed: []string{"https://ops.acme.example"},
			origin:  "https://evil.example",
			wantOK:  false,
		},
		{
			// Exact match, not a prefix or suffix test. "ops.acme.example.evil.com"
			// is a domain an attacker can register, and substring matching is
			// how that gets accepted.
			name:    "a lookalike domain is refused",
			allowed: []string{"https://ops.acme.example"},
			origin:  "https://ops.acme.example.evil.com",
			wantOK:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, url := newServer(t, &collectingSink{}, ingest.Config{
				AllowedOrigins: tc.allowed,
			})
			t.Cleanup(func() { _ = srv })

			var hdr http.Header
			if tc.origin != "" {
				hdr = http.Header{"Origin": []string{tc.origin}}
			}

			conn, resp, err := websocket.DefaultDialer.Dial(url, hdr)
			if conn != nil {
				_ = conn.Close()
			}

			gotOK := err == nil
			if gotOK != tc.wantOK {
				status := 0
				if resp != nil {
					status = resp.StatusCode
				}
				t.Fatalf("upgrade accepted = %v (status %d, err %v), want accepted = %v",
					gotOK, status, err, tc.wantOK)
			}
		})
	}
}
