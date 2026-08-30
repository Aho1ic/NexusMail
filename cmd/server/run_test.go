//go:build sqlite_fts5

package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// A listen failure is the most common real startup failure there is: the port is
// already taken, by an older copy of this process or by anything else on the host.
// It is the one path out of run that nothing else here reaches, and it is worth
// reaching because what run does with it decides whether the process dies loudly or
// sits there having bound nothing — healthy to a supervisor, and serving no one.
//
// The rest of run's surface is covered in main_test.go, and main's own job of turning
// this return into a non-zero exit status is covered over a real child process in
// process_test.go. This adds only the branch none of those take.
func TestRunReportsAListenFailure(t *testing.T) {
	_, addr := configure(t)

	// configure picked a free port and pointed run at it; taking it now is what makes
	// the bind fail. Held for the whole test, so run cannot acquire it on a retry.
	blocker, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("could not occupy %s: %v", addr, err)
	}
	defer blocker.Close()

	// Cancelled at the end so the goroutines run starts before listening — the send
	// worker and the maintenance ticker — do not outlive the test.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	failed := make(chan error, 1)
	go func() { failed <- run(ctx) }()

	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("run returned nil with its port already taken")
		}
		// The address is what makes the operator's log line actionable, so it has to
		// survive out of run rather than being flattened into a generic failure.
		if !strings.Contains(err.Error(), addr) {
			t.Errorf("error %q does not name the address it failed to bind", err)
		}
	case <-time.After(60 * time.Second):
		// A run that neither binds nor returns is the outcome this test exists to
		// rule out: it would pass a liveness check while serving nothing.
		t.Fatal("run neither bound the port nor reported the failure")
	}
}
