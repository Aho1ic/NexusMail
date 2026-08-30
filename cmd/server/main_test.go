//go:build sqlite_fts5

package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This is the assembly point: the only place where every component is wired to
// every other one, and the only place where the order of construction and the
// order of shutdown matter. A unit test of any one service cannot see a
// dependency wired to the wrong instance, a background loop that ignores the
// root context, or a defer ordering that stops the supervisor before the drafts
// service has flushed. Standing the whole process up and taking it down again is
// what covers that.

const testAPIKey = "an-api-key-that-is-at-least-32-characters-long"

// freePort returns a port nothing is listening on. There is an unavoidable gap
// between releasing it and run binding it; the alternative is threading a
// listener through run, which would stop the test from covering the bind.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// configure points the process at a scratch directory and a free port.
func configure(t *testing.T) (dir string, addr string) {
	t.Helper()
	dir = t.TempDir()
	port := freePort(t)
	addr = fmt.Sprintf("127.0.0.1:%d", port)
	t.Setenv("NEXUSMAIL_DATA_DIR", dir)
	t.Setenv("NEXUSMAIL_DATABASE_PATH", filepath.Join(dir, "mail.db"))
	t.Setenv("NEXUSMAIL_LISTEN_ADDR", addr)
	t.Setenv("NEXUSMAIL_PUBLIC_URL", "http://"+addr)
	t.Setenv("NEXUSMAIL_API_KEY", testAPIKey)
	t.Setenv("NEXUSMAIL_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("NEXUSMAIL_LOG_LEVEL", "debug")
	// Clear anything the developer's own environment might contribute, so the test
	// asserts against a known configuration rather than the machine it runs on.
	for _, key := range []string{
		"NEXUSMAIL_GOOGLE_CLIENT_ID", "NEXUSMAIL_GOOGLE_CLIENT_SECRET",
		"NEXUSMAIL_MICROSOFT_CLIENT_ID", "NEXUSMAIL_MICROSOFT_CLIENT_SECRET",
		"NEXUSMAIL_TRUSTED_PROXIES", "NEXUSMAIL_BLOB_CACHE_BYTES", "NEXUSMAIL_MAX_OUTBOUND_BYTES",
	} {
		t.Setenv(key, "")
	}
	return dir, addr
}

func waitForServer(t *testing.T, addr string, failed <-chan error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-failed:
			t.Fatalf("the server exited before it was reachable: %v", err)
		default:
		}
		response, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the server was not reachable on %s within 30s", addr)
}

// The whole process: assembled, serving, and shut down cleanly when the root
// context ends. A shutdown that returns an error, hangs, or leaves the port bound
// is what a container restart would surface as a stuck pod.
func TestRunServesAndShutsDownCleanly(t *testing.T) {
	dir, addr := configure(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exited := make(chan error, 1)
	go func() { exited <- run(ctx) }()
	waitForServer(t, addr, exited)

	// Serving means the router, the config and the embedded assets are all wired:
	// /readyz reports on the database the assembly opened.
	response, err := http.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	var ready map[string]any
	if err := json.NewDecoder(response.Body).Decode(&ready); err != nil {
		t.Fatalf("decode readyz: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("readyz returned %d with %v, want 200", response.StatusCode, ready)
	}

	// The database and the blob directory are created by the assembly, not by the
	// first request: a deployment that cannot write them must fail at startup.
	if _, err := os.Stat(filepath.Join(dir, "mail.db")); err != nil {
		t.Errorf("the database was not created: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, "blobs")); err != nil || !info.IsDir() {
		t.Errorf("the blob directory was not created: %v", err)
	}

	cancel()
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("shutdown returned %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not return after the root context was cancelled")
	}

	// The listener has to be released, or the next start on the same port fails.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port was still bound after shutdown: %v", err)
	}
	_ = listener.Close()

	// And nothing may still be answering.
	if response, err := http.Get("http://" + addr + "/healthz"); err == nil {
		_ = response.Body.Close()
		t.Error("the server still answered after shutdown")
	}
}

// An API key the deployment got wrong must stop the process at startup rather
// than bring up an endpoint that accepts a short key.
func TestRunRejectsAnInvalidConfiguration(t *testing.T) {
	configure(t)
	t.Setenv("NEXUSMAIL_API_KEY", "too-short")
	if err := run(context.Background()); err == nil {
		t.Fatal("run accepted an API key below the minimum length")
	}
}

func TestRunRejectsAnUnusableMasterKey(t *testing.T) {
	configure(t)
	t.Setenv("NEXUSMAIL_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	err := run(context.Background())
	if err == nil {
		t.Fatal("run accepted a 16-byte master key")
	}
	if !strings.Contains(err.Error(), "MASTER_KEY") {
		t.Errorf("error is %q, want it to name the master key", err)
	}
}

// A data directory that cannot be written is the ordinary container
// misconfiguration: the wrong volume mount or the wrong uid. It has to fail at
// startup, before the port is bound.
func TestRunFailsWhenTheDataDirectoryIsUnwritable(t *testing.T) {
	dir, addr := configure(t)
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	t.Setenv("NEXUSMAIL_DATA_DIR", filepath.Join(blocked, "data"))
	t.Setenv("NEXUSMAIL_DATABASE_PATH", filepath.Join(dir, "mail.db"))

	if err := run(context.Background()); err == nil {
		t.Fatal("run started with an unwritable data directory")
	}
	if response, err := http.Get("http://" + addr + "/healthz"); err == nil {
		_ = response.Body.Close()
		t.Error("the port was bound despite the startup failure")
	}
}

// A database path that cannot be opened must also fail before the port is bound.
func TestRunFailsWhenTheDatabaseCannotBeOpened(t *testing.T) {
	dir, _ := configure(t)
	// A directory where the file is expected: sqlite cannot open it either way, and
	// this needs no permission bits, so it behaves the same for root.
	occupied := filepath.Join(dir, "occupied.db")
	if err := os.Mkdir(occupied, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXUSMAIL_DATABASE_PATH", occupied)

	if err := run(context.Background()); err == nil {
		t.Fatal("run started with an unopenable database")
	}
}

// A run that is cancelled before it ever serves still has to return, and has to
// return without the shutdown erroring: this is the path a container takes when
// it is stopped during startup.
func TestRunReturnsWhenCancelledImmediately(t *testing.T) {
	configure(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	exited := make(chan error, 1)
	go func() { exited <- run(ctx) }()
	select {
	case err := <-exited:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("run returned %v, want nil or context.Canceled", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("run ignored a context that was already cancelled")
	}
}

// Two runs in sequence against the same data directory: the second must find the
// schema already migrated and start anyway. A migration runner that re-applied
// its steps would fail here.
func TestRunStartsAgainstAnExistingDatabase(t *testing.T) {
	dir, addr := configure(t)
	for round := range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		exited := make(chan error, 1)
		go func() { exited <- run(ctx) }()
		waitForServer(t, addr, exited)
		cancel()
		select {
		case err := <-exited:
			if err != nil {
				t.Fatalf("round %d: shutdown returned %v", round, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("round %d: run did not return", round)
		}
		// The same directory is reused deliberately, so the second round opens the
		// database the first one migrated.
		_ = dir
	}
}

// The supervisor is the component every other one shares, and it is started from
// the root context here. With no accounts configured its Start is a no-op, so a
// startup test alone cannot tell a wired supervisor from an unwired one: this adds
// an account, restarts, and waits for the supervisor to move its status. The
// account points at a closed loopback port, so the connection attempt is local,
// immediate and needs no server.
func TestRunStartsTheSupervisorForStoredAccounts(t *testing.T) {
	dir, addr := configure(t)
	databasePath := filepath.Join(dir, "mail.db")

	// First run: create the account over the API, which is also how a real
	// deployment gets one.
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan error, 1)
	go func() { exited <- run(ctx) }()
	waitForServer(t, addr, exited)

	body := strings.NewReader(`{"provider":"qq","email":"someone@qq.com","display_name":"Test",` +
		`"auth":{"type":"password","password":"an-app-specific-code"}}`)
	request, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/v1/accounts", body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", testAPIKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	created, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create account returned %d: %s", response.StatusCode, created)
	}
	cancel()
	if err := <-exited; err != nil {
		t.Fatalf("first shutdown: %v", err)
	}

	// Repoint it at a port nothing listens on and reset its status, so the only
	// thing that can move it is the supervisor starting on the next run. The preset
	// host is the real provider and must never be dialled from a test.
	database, err := sql.Open("sqlite3", "file:"+databasePath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := freePort(t)
	if _, err := database.Exec(
		`UPDATE accounts SET imap_host = ?, imap_port = ?, status = 'disconnected', last_error = NULL`,
		"127.0.0.1", deadPort); err != nil {
		t.Fatalf("repoint IMAP: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// Second run: Start must pick the stored account up and begin driving it.
	secondAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	t.Setenv("NEXUSMAIL_LISTEN_ADDR", secondAddr)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	exited2 := make(chan error, 1)
	go func() { exited2 <- run(ctx2) }()
	waitForServer(t, secondAddr, exited2)

	status := func() string {
		db, err := sql.Open("sqlite3", "file:"+databasePath+"?_busy_timeout=5000&mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var value string
		if err := db.QueryRow(`SELECT status FROM accounts LIMIT 1`).Scan(&value); err != nil {
			t.Fatalf("read status: %v", err)
		}
		return value
	}

	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = status()
		if last != "disconnected" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Any status other than the initial one proves the loop ran; which one it reached
	// depends on how far the refused connection got, and that belongs to the
	// supervisor's own tests rather than to the assembly.
	if last == "disconnected" {
		t.Error("the supervisor never touched the stored account, so it was not started")
	}

	cancel2()
	select {
	case err := <-exited2:
		if err != nil {
			t.Errorf("second shutdown returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not return with an account being driven")
	}
}

type countingMaint struct {
	sessions atomic.Int64
	evicted  atomic.Int64
	stamp    atomic.Int64
}

func (c *countingMaint) DeleteExpiredSessions(_ context.Context, before int64) error {
	c.stamp.Store(before)
	c.sessions.Add(1)
	return nil
}

func (c *countingMaint) Evict(context.Context) error {
	c.evicted.Add(1)
	return nil
}

func TestMaintenanceSweepsOnEveryTick(t *testing.T) {
	counter := &countingMaint{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); maintenance(ctx, counter, counter, 10*time.Millisecond) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if counter.sessions.Load() >= 2 && counter.evicted.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sessions, evicted := counter.sessions.Load(), counter.evicted.Load(); sessions < 2 || evicted < 2 {
		t.Fatalf("after repeated ticks: %d session sweeps and %d evictions, want at least 2 of each", sessions, evicted)
	}
	// The cutoff has to be the current time, or expired sessions are never collected.
	if stamp := counter.stamp.Load(); stamp <= 0 || time.Since(time.UnixMilli(stamp)) > time.Minute {
		t.Errorf("the session cutoff was %d, want roughly now", stamp)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("maintenance ignored the cancelled context")
	}
}

// A maintenance loop that is cancelled before its first tick must return without
// doing any work: it runs on the root context, and a restart must not be delayed
// by a sweep that started as the process was going down.
func TestMaintenanceStopsWithoutSweepingWhenCancelledFirst(t *testing.T) {
	counter := &countingMaint{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { defer close(done); maintenance(ctx, counter, counter, time.Hour) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("maintenance did not return on an already-cancelled context")
	}
	if counter.sessions.Load() != 0 || counter.evicted.Load() != 0 {
		t.Errorf("a cancelled maintenance loop did %d sweeps and %d evictions, want none",
			counter.sessions.Load(), counter.evicted.Load())
	}
}
