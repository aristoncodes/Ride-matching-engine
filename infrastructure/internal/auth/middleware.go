package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// contextKey is unexported so no other package can collide with it or forge a
// value. Threading the authenticated key through the CONTEXT rather than a
// header is what makes Week 19's tenant isolation trustworthy: a handler reads
// the tenant from something the middleware proved, not from something the
// client sent.
type contextKey string

const apiKeyContextKey contextKey = "auth.apikey"

// FromContext returns the authenticated key, if the request passed auth.
func FromContext(ctx context.Context) (*APIKey, bool) {
	key, ok := ctx.Value(apiKeyContextKey).(*APIKey)
	return key, ok
}

// TenantFromContext returns the authenticated tenant id.
//
// The ONLY sanctioned way for a handler to learn its tenant. Anything reading a
// tenant from a header, a query parameter, or a request body is trusting the
// caller to tell the truth about who they are.
func TenantFromContext(ctx context.Context) (string, bool) {
	key, ok := FromContext(ctx)
	if !ok || key == nil {
		return "", false
	}
	return key.TenantID, true
}

// ErrorWriter renders an auth failure in the caller's error format, so auth
// responses match every other error the API produces (Week 11's envelope).
type ErrorWriter func(w http.ResponseWriter, status int, code, message string, r *http.Request)

// Options configures the middleware.
type Options struct {
	Store  Store
	Logger *slog.Logger
	Now    func() time.Time

	// WriteError renders failures. Required — defaulting to http.Error would
	// emit plain text and break the single-envelope contract.
	WriteError ErrorWriter

	// SkipPaths bypass authentication entirely. Health and readiness probes
	// must be here: a kubelet has no API key, and requiring one would make
	// every pod permanently unready.
	SkipPaths []string
}

// Middleware authenticates every request by API key.
func Middleware(opts Options) func(http.Handler) http.Handler {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	skip := make(map[string]bool, len(opts.SkipPaths))
	for _, p := range opts.SkipPaths {
		skip[p] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skip[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}

			raw := presentedKey(r)
			if raw == "" {
				opts.WriteError(w, http.StatusUnauthorized, "unauthorized",
					"an API key is required (X-API-Key header or Authorization: Bearer)", r)
				return
			}

			key, err := Verify(r.Context(), opts.Store, raw, now())
			switch {
			case err == nil:
				// Attach to the context and continue.
				ctx := context.WithValue(r.Context(), apiKeyContextKey, key)
				next.ServeHTTP(w, r.WithContext(ctx))

			case errors.Is(err, ErrRateLimited):
				// 429 with Retry-After. The key is valid, so the client should
				// slow down rather than re-authenticate.
				w.Header().Set("Retry-After", "60")
				if key != nil {
					w.Header().Set("X-RateLimit-Limit", strconv.Itoa(key.RateLimitPerMinute))
					w.Header().Set("X-RateLimit-Remaining", "0")
				}
				opts.WriteError(w, http.StatusTooManyRequests, "rate_limited",
					"rate limit exceeded for this API key", r)

			case errors.Is(err, ErrInvalidKey):
				// One message for malformed, unknown, revoked and expired. The
				// distinction is logged, not returned — telling a caller "that
				// key exists but is revoked" hands them an enumeration oracle.
				logger.Warn("rejected API key", "path", r.URL.Path,
					"remote", r.RemoteAddr, "reason", err)
				opts.WriteError(w, http.StatusUnauthorized, "unauthorized",
					"invalid API key", r)

			default:
				// The STORE failed — Redis is down, say. That is our fault, not
				// the caller's, so it must not be a 401: telling a customer
				// their valid key is invalid during an outage sends them
				// rotating credentials that were never the problem.
				logger.Error("auth store failure", "err", err)
				w.Header().Set("Retry-After", "1")
				opts.WriteError(w, http.StatusServiceUnavailable, "unavailable",
					"could not verify credentials right now; please retry", r)
			}
		})
	}
}

// presentedKey extracts the key from the request.
//
// Two accepted forms. `Authorization: Bearer` is the convention most HTTP
// clients and proxies already understand; `X-API-Key` is what many B2B
// integrations expect. Supporting both costs three lines and removes a
// pointless integration obstacle.
//
// Never from a query parameter: URLs end up in access logs, browser history,
// and Referer headers, which is how keys leak.
func presentedKey(r *http.Request) string {
	if v := r.Header.Get("X-API-Key"); v != "" {
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("Authorization"); v != "" {
		if after, found := strings.CutPrefix(v, "Bearer "); found {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// WebSocketKey extracts a key from a WebSocket upgrade request.
//
// Browsers cannot set headers on a WebSocket handshake, so the subprotocol
// field is the usual carrier. Native driver apps can and do set headers, so
// headers are tried first and the subprotocol is the fallback.
func WebSocketKey(r *http.Request) string {
	if k := presentedKey(r); k != "" {
		return k
	}
	for _, proto := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		proto = strings.TrimSpace(proto)
		if after, found := strings.CutPrefix(proto, "apikey."); found {
			return after
		}
	}
	return ""
}
