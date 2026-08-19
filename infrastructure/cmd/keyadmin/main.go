// keyadmin manages API keys.
//
// Turning authentication on is only half the job: without a way to mint a key,
// "secure" and "unusable" are the same state. This is the operator's side of
// internal/auth.
//
// The raw key is printed EXACTLY ONCE, at creation, and is unrecoverable
// afterwards — only its SHA-256 hash is stored. That is deliberate and is the
// whole reason a compromised key store does not hand an attacker working
// credentials. If a key is lost, rotate it; there is no "show me the key again".
//
// Usage:
//
//	keyadmin create --tenant acme --name "acme prod" [--ttl 8760h] [--rate 600]
//	keyadmin list   --tenant acme
//	keyadmin rotate --key-id rmk_abc123 [--overlap 24h]
//	keyadmin revoke --key-id rmk_abc123
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/aditya/ride-matching/internal/auth"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	redisAddr := fs.String("redis", envOr("REDIS_ADDR", "localhost:6379"), "Redis address")
	tenant := fs.String("tenant", "", "tenant id")
	name := fs.String("name", "", "human label for the key, e.g. \"acme prod\"")
	keyID := fs.String("key-id", "", "key id to act on")
	ttl := fs.Duration("ttl", 0, "how long the key stays valid (0 = no expiry)")
	rate := fs.Int("rate", 600, "rate limit, requests per minute per key")
	overlap := fs.Duration("overlap", 24*time.Hour,
		"how long the old key keeps working after a rotation")

	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}

	store, err := auth.NewRedisStore(*redisAddr, time.Now)
	if err != nil {
		fail("cannot configure the key store: %v", err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := store.Ping(ctx); err != nil {
		fail("cannot reach Redis at %s: %v", *redisAddr, err)
	}

	switch cmd {
	case "create":
		if *tenant == "" || *name == "" {
			fail("create needs --tenant and --name")
		}
		raw, key, err := store.Create(ctx, *tenant, *name, *ttl, *rate)
		if err != nil {
			fail("create: %v", err)
		}
		fmt.Printf("key id    : %s\n", key.KeyID)
		fmt.Printf("tenant    : %s\n", key.TenantID)
		fmt.Printf("rate limit: %d/min\n", key.RateLimitPerMinute)
		if key.ExpiresAt.IsZero() {
			fmt.Printf("expires   : never\n")
		} else {
			fmt.Printf("expires   : %s\n", key.ExpiresAt.Format(time.RFC3339))
		}
		fmt.Printf("\n  %s\n\n", raw)
		fmt.Println("This is the only time the key is shown. Store it now — only its")
		fmt.Println("hash is kept, so it cannot be recovered, only rotated.")

	case "list":
		if *tenant == "" {
			fail("list needs --tenant")
		}
		ids, err := store.ListForTenant(ctx, *tenant)
		if err != nil {
			fail("list: %v", err)
		}
		if len(ids) == 0 {
			fmt.Printf("no keys for tenant %q\n", *tenant)
			return
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "KEY ID\tNAME\tSTATUS\tCREATED\tEXPIRES")
		for _, id := range ids {
			k, err := store.Lookup(ctx, id)
			if err != nil {
				fmt.Fprintf(w, "%s\t?\t%v\t\t\n", id, err)
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				k.KeyID, k.Name, status(k), k.CreatedAt.Format(time.DateOnly), expiry(k))
		}
		_ = w.Flush()

	case "rotate":
		if *keyID == "" {
			fail("rotate needs --key-id")
		}
		raw, key, err := store.Rotate(ctx, *keyID, *overlap)
		if err != nil {
			fail("rotate: %v", err)
		}
		fmt.Printf("new key id: %s\n", key.KeyID)
		fmt.Printf("replaces  : %s (keeps working for %s)\n", key.RotatedFrom, *overlap)
		fmt.Printf("\n  %s\n\n", raw)
		// The overlap is the entire point of rotation. Revoking instantly would
		// break every in-flight client that has not picked up the new key yet,
		// which turns a routine hygiene task into an outage.
		fmt.Println("Roll clients onto the new key before the overlap expires.")

	case "revoke":
		if *keyID == "" {
			fail("revoke needs --key-id")
		}
		if err := store.Revoke(ctx, *keyID); err != nil {
			fail("revoke: %v", err)
		}
		fmt.Printf("revoked %s — it stops working immediately\n", *keyID)

	default:
		usage()
		os.Exit(2)
	}
}

func status(k *auth.APIKey) string {
	switch {
	case !k.RevokedAt.IsZero():
		return "revoked"
	case !k.ExpiresAt.IsZero() && k.ExpiresAt.Before(time.Now()):
		return "expired"
	default:
		return "active"
	}
}

func expiry(k *auth.APIKey) string {
	if k.ExpiresAt.IsZero() {
		return "never"
	}
	return k.ExpiresAt.Format(time.DateOnly)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "keyadmin: "+format+"\n", args...)
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, `keyadmin manages API keys.

  keyadmin create --tenant acme --name "acme prod" [--ttl 8760h] [--rate 600]
  keyadmin list   --tenant acme
  keyadmin rotate --key-id rmk_abc123 [--overlap 24h]
  keyadmin revoke --key-id rmk_abc123

Common flags: --redis (or REDIS_ADDR), default localhost:6379.
`)
}
