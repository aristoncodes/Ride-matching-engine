// Package authsetup wires API-key authentication into a service binary.
//
// It exists because the alternative is the same twenty lines pasted into every
// main(), which is three chances to get the fail-closed rule subtly wrong.
//
// # The default is ON
//
// The flag is --allow-anonymous, not --auth. That asymmetry is the whole point:
// a boolean whose zero value is the INSECURE state will eventually ship
// disabled, because forgetting a flag is a normal thing to do and nothing
// visibly breaks. This project already made that exact mistake once — Week 20's
// EnableProfiling defaulted to false, so pprof was silently off and a 47-byte
// response looked like a working profile.
//
// So: not passing the flag gets you authentication. Turning it off requires
// saying so out loud, on the command line, where a reviewer can see it.
package authsetup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aditya/ride-matching/internal/auth"
)

// Open returns the auth store a service should use, and a close function.
//
// A nil store means authentication is disabled — which happens ONLY when
// allowAnonymous is true. When authentication is required, an unreachable key
// store is a startup failure, never a downgrade: a service that cannot check
// credentials and serves traffic anyway is worse than one that refuses to
// start, because the second failure is loud and the first is invisible.
func Open(ctx context.Context, redisAddr string, allowAnonymous bool, logger *slog.Logger) (auth.Store, func(), error) {
	if allowAnonymous {
		// Logged at WARN with the consequence spelled out, not a tidy
		// "auth=false". This line is the last chance to notice before a
		// wide-open service starts taking traffic.
		logger.Warn("AUTHENTICATION IS DISABLED: every client is treated as the " +
			"fallback tenant, no rate limits apply, and any client that can reach " +
			"this port can read and write fleet data. Acceptable for local " +
			"development only — never pass --allow-anonymous in production")
		return nil, func() {}, nil
	}

	store, err := auth.NewRedisStore(redisAddr, time.Now)
	if err != nil {
		return nil, nil, fmt.Errorf("configuring the key store: %w", err)
	}

	// Ping at startup rather than discovering the store is unreachable on the
	// first request. A service that fails readiness immediately is rolled back
	// by the orchestrator; one that starts and 503s every request looks healthy
	// and is not.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := store.Ping(pingCtx); err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("cannot reach the key store at %s: %w", redisAddr, err)
	}

	logger.Info("authentication enabled", "key_store", redisAddr)
	return store, func() { _ = store.Close() }, nil
}
