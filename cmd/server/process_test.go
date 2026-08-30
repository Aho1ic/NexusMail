//go:build sqlite_fts5

package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// main itself cannot be covered in-process: it installs signal handlers for the whole
// test binary and calls os.Exit on failure, which would take the test runner with it.
// Re-executing this binary as a child process is what reaches it, and it is worth
// reaching because main owns two things run does not — the signal-to-context wiring
// that makes SIGTERM a clean shutdown rather than an abrupt kill, and the non-zero
// exit status that tells an init system or container runtime the process failed.
const reexecEnv = "NEXUSMAIL_TEST_REEXEC_MAIN"

// TestMain runs main instead of the test suite when the guard variable is set, so the
// child process below is the real entry point rather than an imitation of it.
func TestMain(m *testing.M) {
	if os.Getenv(reexecEnv) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// spawnMain re-executes this test binary as the server. The returned reader carries
// the child's merged output so a caller can wait for a specific log line.
func spawnMain(t *testing.T, env map[string]string) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable)
	// A clean environment, not the developer's: an inherited NEXUSMAIL_ variable would
	// change what the child is configured with and make this test pass or fail for
	// reasons that have nothing to do with the code.
	command.Env = []string{reexecEnv + "=1", "PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	// GOCOVERDIR is forwarded when present so the child's counters are attributed
	// rather than discarded. Without it main reads as uncovered even though these
	// tests are the only thing that executes it, because the counters live in the
	// child's memory and the parent's profile never sees them. Set it up with
	// `go test -cover -args` style tooling: go build -cover, then GOCOVERDIR=dir.
	if dir := os.Getenv("GOCOVERDIR"); dir != "" {
		command.Env = append(command.Env, "GOCOVERDIR="+dir)
	}
	for key, value := range env {
		command.Env = append(command.Env, key+"="+value)
	}
	pipe, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})
	return command, bufio.NewScanner(pipe)
}

func serverEnv(t *testing.T) (map[string]string, string) {
	t.Helper()
	dir := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	return map[string]string{
		"NEXUSMAIL_DATA_DIR":      dir,
		"NEXUSMAIL_DATABASE_PATH": filepath.Join(dir, "mail.db"),
		"NEXUSMAIL_LISTEN_ADDR":   addr,
		"NEXUSMAIL_PUBLIC_URL":    "http://" + addr,
		"NEXUSMAIL_API_KEY":       strings.Repeat("k", 48),
		"NEXUSMAIL_MASTER_KEY":    base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"NEXUSMAIL_LOG_LEVEL":     "info",
	}, addr
}

func waitForHealthy(t *testing.T, addr string, command *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if command.ProcessState != nil && command.ProcessState.Exited() {
			t.Fatalf("server exited before becoming healthy: %v", command.ProcessState)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server at %s never became healthy", addr)
}

// TestMainShutsDownOnSIGTERM is the property that matters in a container: the
// orchestrator sends SIGTERM and expects the process to close its listener, stop its
// background loops and exit zero. An exit status other than zero here would be read as
// a crash and, under a restart policy, as a reason to restart the container.
func TestMainShutsDownOnSIGTERM(t *testing.T) {
	env, addr := serverEnv(t)
	command, _ := spawnMain(t, env)
	waitForHealthy(t, addr, command)

	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("server exited with %v after SIGTERM, want a clean exit", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("server did not exit within 30s of SIGTERM")
	}

	// The listener must be gone, not merely idle: a socket still held by a lingering
	// goroutine would keep the next start from binding.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Errorf("port still held after shutdown: %v", err)
	} else {
		_ = listener.Close()
	}
}

// TestMainShutsDownOnSIGINT covers the other signal main registers. Ctrl-C in a
// terminal is the developer-facing half of the same path.
func TestMainShutsDownOnSIGINT(t *testing.T) {
	env, addr := serverEnv(t)
	command, _ := spawnMain(t, env)
	waitForHealthy(t, addr, command)

	if err := command.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("server exited with %v after SIGINT, want a clean exit", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("server did not exit within 30s of SIGINT")
	}
}

// TestMainExitsNonZeroOnAFailedStart covers the os.Exit(1) branch. Exiting zero on a
// failed start is worse than the failure: a supervisor that reads the status sees a
// successful run and does not restart or alert.
func TestMainExitsNonZeroOnAFailedStart(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"api key too short", func(env map[string]string) { env["NEXUSMAIL_API_KEY"] = "short" }},
		{"master key wrong size", func(env map[string]string) {
			env["NEXUSMAIL_MASTER_KEY"] = base64.StdEncoding.EncodeToString(make([]byte, 16))
		}},
		{"master key not base64", func(env map[string]string) { env["NEXUSMAIL_MASTER_KEY"] = "!!!not base64!!!" }},
		{"database path unusable", func(env map[string]string) {
			// A directory where the file is expected: sqlite cannot open it either way,
			// and this needs no permission bits, so it behaves the same for root. A
			// merely missing parent directory would not do — that path gets created.
			occupied := filepath.Join(env["NEXUSMAIL_DATA_DIR"], "occupied.db")
			if err := os.Mkdir(occupied, 0o700); err != nil {
				panic(err)
			}
			env["NEXUSMAIL_DATABASE_PATH"] = occupied
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			env, _ := serverEnv(t)
			testCase.mutate(env)
			command, _ := spawnMain(t, env)

			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case err := <-done:
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("server exited with %v, want a non-zero status", err)
				}
				if exitErr.ExitCode() != 1 {
					t.Errorf("exit status is %d, want 1", exitErr.ExitCode())
				}
			case <-time.After(30 * time.Second):
				t.Fatal("server did not exit within 30s of a failed start")
			}
		})
	}
}

// TestMainServesRealRequests is the end-to-end check that the re-executed process is
// a working server and not just one that opens a port: an unauthenticated request must
// be refused and an API-keyed one answered, which together exercise config, the
// repository, the router and the auth middleware in the real binary.
func TestMainServesRealRequests(t *testing.T) {
	env, addr := serverEnv(t)
	command, _ := spawnMain(t, env)
	waitForHealthy(t, addr, command)

	base := "http://" + addr

	for _, path := range []string{"/healthz", "/readyz"} {
		response, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, response.StatusCode)
		}
	}

	response, err := http.Get(base + "/api/v1/accounts")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /api/v1/accounts = %d, want 401", response.StatusCode)
	}

	request, err := http.NewRequest(http.MethodGet, base+"/api/v1/accounts", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-API-Key", env["NEXUSMAIL_API_KEY"])
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("authenticated GET /api/v1/accounts = %d, want 200", response.StatusCode)
	}
}
