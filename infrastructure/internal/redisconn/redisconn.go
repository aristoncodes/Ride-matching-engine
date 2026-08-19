// Package redisconn holds the connection settings every Redis client in this
// process shares.
//
// # Why the password comes from the environment and not a flag
//
// Command-line arguments are readable by any process on the host via `ps`, they
// are captured in `docker inspect`, and they end up in shell history and in
// crash reports. A password in argv is a password you have published to
// everyone with a login on that box.
//
// The environment is not perfect either — it is inherited by children and
// visible in /proc/<pid>/environ to the same user — but it stays out of `ps`
// and out of process listings, which is the exposure that actually bites. The
// step beyond this is a mounted secret file or a secrets manager, and that is
// the right move the moment this stops being a single VM.
package redisconn

import "os"

// Password returns the Redis password, or "" when Redis needs none.
//
// go-redis sends no AUTH when this is empty, so an unset variable keeps local
// development and the test suite working against a password-less Redis with no
// special casing anywhere.
func Password() string {
	return os.Getenv("REDIS_PASSWORD")
}
