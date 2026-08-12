package testutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisProcess is a redis-server started just for one test.
type RedisProcess struct {
	cmd  *exec.Cmd
	Addr string // unix socket path
}

// StartRedis launches a private redis-server on a UNIX SOCKET in the test's
// temp directory.
//
// A socket rather than a TCP port on purpose: picking a "free" port is always a
// race (the port can be taken between the check and the bind), and tests that
// run in parallel would fight over it. A socket path in t.TempDir() is unique
// by construction.
//
// The instance is also told to keep nothing on disk, so tests cannot leak state
// into each other or onto the developer's machine.
func StartRedis(t *testing.T) *RedisProcess {
	t.Helper()

	if _, err := exec.LookPath("redis-server"); err != nil {
		t.Skip("redis-server not installed; brew install redis")
	}

	// The socket must live under a SHORT path. A unix socket address is a
	// fixed-size char array in the kernel -- sun_path, 104 bytes on macOS, 108
	// on Linux -- and the address is silently unusable beyond it. Go's
	// t.TempDir() embeds the test's name, so
	//   /var/folders/.../T/TestStaleDriversAreNotReturned2114060021/001/redis.sock
	// is exactly 104 characters and fails, while a shorter test name passes.
	// That is why only some tests broke: the bug was a function of test NAME
	// length, which is about as misleading a symptom as it gets.
	dir, err := os.MkdirTemp("", "rm")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "r.sock")
	if len(socket) > 100 {
		t.Skipf("temp path too long for a unix socket (%d chars): %s", len(socket), socket)
	}

	cmd := exec.Command("redis-server",
		"--port", "0", // no TCP listener at all
		"--unixsocket", socket,
		"--save", "", // no RDB snapshots
		"--appendonly", "no", // no AOF
		"--dir", dir,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start redis: %v", err)
	}

	proc := &RedisProcess{cmd: cmd, Addr: socket}
	t.Cleanup(proc.Stop)

	// Wait for the socket to appear rather than sleeping a fixed amount.
	if !WaitForFile(socket, 10*time.Second) {
		proc.Stop()
		t.Fatalf("redis did not create %s in time", socket)
	}
	return proc
}

// Stop terminates the server. Safe to call more than once.
func (p *RedisProcess) Stop() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_, _ = p.cmd.Process.Wait()
	p.cmd = nil
}

// Kill terminates Redis abruptly, for tests that assert on connection failures.
func (p *RedisProcess) Kill(t *testing.T) {
	t.Helper()
	p.Stop()
}

// WaitForFile polls until path exists or the timeout expires.
func WaitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fileExists(path) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fileExists(path)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// RedisCmd runs a raw Redis command against addr. Used by tests that need to
// write malformed data directly, bypassing the production code's validation —
// there is otherwise no way to exercise the "what if the stream contains
// garbage" path, because the writer refuses to create it.
func RedisCmd(ctx context.Context, addr string, args ...interface{}) error {
	network := "tcp"
	if len(addr) > 0 && addr[0] == '/' {
		network = "unix"
	}
	client := redis.NewClient(&redis.Options{Network: network, Addr: addr})
	defer client.Close()
	return client.Do(ctx, args...).Err()
}
