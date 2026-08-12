// Package testutil starts and stops real dependencies for integration tests.
//
// These tests use the ACTUAL C++ binary and the ACTUAL Redis, not mocks. Mocks
// are the right tool for testing this package's logic; they are the wrong tool
// for testing the boundary, because the whole claim being tested is about what
// happens when a real process really dies. A mock that returns
// codes.Unavailable proves only that the mock was written to.
package testutil

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// EngineProcess is a running matching_server.
type EngineProcess struct {
	cmd     *exec.Cmd
	Addr    string
	GraphID string
}

// repoRoot walks up from the working directory looking for the marker that only
// the repository root has. Tests run with their package as the CWD, so a
// relative path to the C++ binary would break the moment a test moves.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "matching_engine", "CMakeLists.txt")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate repo root (no matching_engine/CMakeLists.txt above cwd)")
	return ""
}

// StartEngine launches matching_server on an OS-assigned port and waits until
// it is actually listening.
//
// Skips (rather than fails) when the C++ binary has not been built, so
// `go test ./...` stays useful on a machine that has not run cmake. A test that
// cannot run is not the same as a test that failed.
func StartEngine(t *testing.T, withGraph bool) *EngineProcess {
	t.Helper()

	root := repoRoot(t)
	bin := filepath.Join(root, "matching_engine", "build", "matching_server")
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("matching_server not built (%s); run: make build", bin)
	}

	// Port 0 asks the OS for a free port, and the server prints the one it got.
	// Hardcoding a port makes tests fail when run in parallel or when something
	// else on the machine happens to hold it.
	args := []string{"--port", "0"}
	graphID := ""
	if withGraph {
		graphPath := filepath.Join(root, "matching_engine", "data", "bengaluru_roads.osm")
		if _, err := os.Stat(graphPath); err != nil {
			t.Skipf("road graph extract missing (%s)", graphPath)
		}
		graphID = "blr-central"
		args = append(args, "--graph", graphID+"="+graphPath)
	}

	cmd := exec.Command(bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start engine: %v", err)
	}

	proc := &EngineProcess{cmd: cmd, GraphID: graphID}
	t.Cleanup(proc.Stop)

	// Wait for the readiness line rather than sleeping. A fixed sleep is a race
	// that passes on a fast machine and goes red in CI.
	type result struct {
		addr string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "matching_server listening on ") {
				port := strings.TrimPrefix(line, "matching_server listening on ")
				done <- result{addr: "127.0.0.1:" + strings.TrimSpace(port)}
				// Keep draining so the pipe buffer cannot fill and block the
				// server on its own stdout.
				for scanner.Scan() {
				}
				return
			}
		}
		done <- result{err: fmt.Errorf("engine exited before listening")}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("engine did not start: %v", r.err)
		}
		proc.Addr = r.addr
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for engine to listen")
	}
	return proc
}

// StartEngineAt launches an engine bound to a SPECIFIC address, used to bring
// one back on the port a killed engine held. Recovering on the same address is
// what a container restart or a Kubernetes reschedule behind a stable Service
// looks like from the client's side.
//
// The port can briefly still be held by the OS after a kill, so binding is
// retried rather than assumed.
func StartEngineAt(t *testing.T, addr string) *EngineProcess {
	t.Helper()

	root := repoRoot(t)
	bin := filepath.Join(root, "matching_engine", "build", "matching_server")
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("matching_server not built (%s)", bin)
	}

	port := addr[strings.LastIndex(addr, ":")+1:]

	var lastErr error
	for attempt := 0; attempt < 40; attempt++ {
		cmd := exec.Command(bin, "--port", port)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("stdout pipe: %v", err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}

		listening := make(chan bool, 1)
		go func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				if strings.HasPrefix(scanner.Text(), "matching_server listening on ") {
					listening <- true
					for scanner.Scan() {
					}
					return
				}
			}
			listening <- false
		}()

		select {
		case ok := <-listening:
			if ok {
				proc := &EngineProcess{cmd: cmd, Addr: addr}
				t.Cleanup(proc.Stop)
				return proc
			}
			lastErr = fmt.Errorf("engine exited before listening (port %s still held?)", port)
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			lastErr = fmt.Errorf("timed out waiting for engine on %s", addr)
		}
		_, _ = cmd.Process.Wait()
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("could not restart engine on %s: %v", addr, lastErr)
	return nil
}

// Kill terminates the engine abruptly with SIGKILL — no graceful shutdown, no
// chance to close connections. This simulates a segfault or an OOM kill, which
// is the failure ADR-0002 exists to survive.
func (p *EngineProcess) Kill(t *testing.T) {
	t.Helper()
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill engine: %v", err)
	}
	_, _ = p.cmd.Process.Wait()
	p.cmd = nil
}

// Stop shuts the engine down if it is still running. Safe to call twice.
func (p *EngineProcess) Stop() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_, _ = p.cmd.Process.Wait()
	p.cmd = nil
}

// WaitFor polls until cond returns true or the timeout expires.
func WaitFor(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
	return cond()
}
