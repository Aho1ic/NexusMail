package smtp

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	gosmtp "github.com/emersion/go-smtp"
)

// TestProgressReaderRefreshesDeadline covers the fix for the worst failure mode in
// delivery. One deadline for the whole session meant the clock kept running through
// the body transfer, so a large attachment on a slow uplink timed out mid-DATA and
// became a permanent "unknown" that is never retried. The deadline now moves with
// the transfer, so only a genuine stall trips it.
func TestProgressReaderRefreshesDeadline(t *testing.T) {
	conn := &recordingConn{}
	// Six chunks of 8 bytes each through a 4-byte buffer.
	reader := &progressReader{source: strings.NewReader("abcdefghijklmnopqrstuvwx"), conn: conn, window: 30 * time.Second}

	total := 0
	buffer := make([]byte, 4)
	for {
		read, err := reader.Read(buffer)
		total += read
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if total != 24 {
		t.Fatalf("read %d bytes, want 24", total)
	}
	if len(conn.deadlines) < 6 {
		t.Fatalf("deadline was refreshed %d times over 6 reads", len(conn.deadlines))
	}
	// Each refresh must be later than the last, otherwise the transfer is still
	// racing a fixed end time.
	for index := 1; index < len(conn.deadlines); index++ {
		if conn.deadlines[index].Before(conn.deadlines[index-1]) {
			t.Fatalf("deadline moved backwards at refresh %d", index)
		}
	}
	// And each must be a full window out, not a remainder of one budget.
	if remaining := time.Until(conn.deadlines[len(conn.deadlines)-1]); remaining < 25*time.Second {
		t.Fatalf("last deadline is only %v out, want about 30s", remaining)
	}
}

// TestProgressReaderSetsDeadlineBeforeRead pins the ordering. The refresh has to
// happen before the bytes are handed on, because the write that blocks is the one
// the new deadline must already cover.
func TestProgressReaderSetsDeadlineBeforeRead(t *testing.T) {
	conn := &recordingConn{}
	source := &observingReader{conn: conn}
	reader := &progressReader{source: source, conn: conn, window: time.Second}
	if _, err := reader.Read(make([]byte, 4)); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if source.deadlinesAtRead != 1 {
		t.Fatalf("deadline count at read time = %d, want 1", source.deadlinesAtRead)
	}
}

// TestProgressReaderPropagatesSourceError checks the wrapper stays transparent: a
// composition failure must still surface as a data-write error rather than being
// swallowed by the deadline bookkeeping.
func TestProgressReaderPropagatesSourceError(t *testing.T) {
	sentinel := errors.New("source exploded")
	reader := &progressReader{source: &failingReader{err: sentinel}, conn: &recordingConn{}, window: time.Second}
	if _, err := reader.Read(make([]byte, 4)); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want %v", err, sentinel)
	}
}

// TestClassifyMarksAmbiguousResultUnknown documents the four-state contract at its
// source. A DATA-stage break is not a failure to retry: the message may already have
// been queued, so a retry can deliver it twice.
func TestClassifyMarksAmbiguousResultUnknown(t *testing.T) {
	err := classify("data-commit", errors.New("connection reset"), true)
	var delivery *DeliveryError
	if !errors.As(err, &delivery) {
		t.Fatalf("classify returned %T", err)
	}
	if !delivery.Unknown {
		t.Fatal("ambiguous network break was not marked unknown")
	}
	if delivery.Temporary {
		t.Fatal("unknown result was also marked temporary, which would let it retry")
	}
}

// TestClassifyResolvesAmbiguityFromStatusCode is the other half: once the server has
// spoken, the result is no longer unknown even if the failure happened at DATA.
func TestClassifyResolvesAmbiguityFromStatusCode(t *testing.T) {
	err := classify("data-commit", &gosmtp.SMTPError{Code: 452, Message: "insufficient storage"}, true)
	var delivery *DeliveryError
	if !errors.As(err, &delivery) {
		t.Fatalf("classify returned %T", err)
	}
	if delivery.Unknown {
		t.Fatal("a reply with a status code was still treated as unknown")
	}
	if !delivery.Temporary || delivery.Code != 452 {
		t.Fatalf("temporary=%v code=%d, want true/452", delivery.Temporary, delivery.Code)
	}

	permanent := classify("rcpt-to", &gosmtp.SMTPError{Code: 550, Message: "no such user"}, false)
	if !errors.As(permanent, &delivery) {
		t.Fatalf("classify returned %T", permanent)
	}
	if delivery.Temporary || delivery.Unknown {
		t.Fatalf("5xx classified temporary=%v unknown=%v", delivery.Temporary, delivery.Unknown)
	}
}

type recordingConn struct {
	net.Conn
	deadlines []time.Time
}

func (c *recordingConn) SetDeadline(when time.Time) error {
	c.deadlines = append(c.deadlines, when)
	return nil
}

type observingReader struct {
	conn            *recordingConn
	deadlinesAtRead int
}

func (r *observingReader) Read([]byte) (int, error) {
	r.deadlinesAtRead = len(r.conn.deadlines)
	return 0, io.EOF
}

type failingReader struct{ err error }

func (r *failingReader) Read([]byte) (int, error) { return 0, r.err }
