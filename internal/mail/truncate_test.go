package mail

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// truncateUTF8 caps how much of a subject or body the code scan reads. The cap is a
// byte count, and the mail this project handles is largely Chinese: cutting at an
// arbitrary byte lands inside a 3-byte rune roughly two times in three. The walk-back
// is what keeps the scanned segment valid UTF-8 — a segment cut mid-rune goes on to
// regexp matching and keyword search, and the detected code is stored and pushed to
// the browser as a notification.
func TestTruncateUTF8NeverCutsInsideARune(t *testing.T) {
	// Every offset within one rune, so the walk-back is entered at each of its
	// possible depths as well as at a boundary it must leave alone.
	body := strings.Repeat("验证码", 20)
	for limit := 0; limit <= len(body); limit++ {
		got := truncateUTF8(body, limit)
		if !utf8.ValidString(got) {
			t.Fatalf("limit %d produced invalid UTF-8: %q", limit, got)
		}
		if len(got) > limit {
			t.Fatalf("limit %d returned %d bytes, which exceeds the cap", limit, len(got))
		}
		if !strings.HasPrefix(body, got) {
			t.Fatalf("limit %d returned %q, which is not a prefix of the input", limit, got)
		}
		// The result must be the longest valid prefix within the cap, or the cap
		// would silently discard up to a whole rune of scannable text per call.
		if next := len(got); next < len(body) && next+utf8.RuneLen([]rune(body[next:])[0]) <= limit {
			t.Fatalf("limit %d stopped at %d bytes with room for another rune", limit, next)
		}
	}
}

// Input at or under the cap is returned untouched, which is the ordinary case: almost
// every mail is far below 4096 bytes and must not be reshaped on the way in.
func TestTruncateUTF8LeavesShortInputAlone(t *testing.T) {
	for _, input := range []string{"", "1", "验证码 123456", strings.Repeat("a", maxOTPScanBytes)} {
		if got := truncateUTF8(input, maxOTPScanBytes); got != input {
			t.Errorf("input of %d bytes was rewritten to %d", len(input), len(got))
		}
	}
}

// A code sitting past the cap is not found, and that is the intended trade: the cap
// bounds the work an attacker-sized body can cause. What must not happen is the cap
// turning a long body into a detection failure for a code that is inside it, or into
// a panic — both are reachable only through the walk-back.
func TestOTPDetectionSurvivesABodyPastTheScanCap(t *testing.T) {
	// Chinese filler so the cap lands mid-rune, with the code early enough to be
	// inside the window.
	filler := strings.Repeat("这是一封很长的邮件。", 800)
	if len(filler) <= maxOTPScanBytes {
		t.Fatalf("filler is only %d bytes, which does not exceed the %d-byte cap", len(filler), maxOTPScanBytes)
	}
	code, ok := DetectOTP("", "您的验证码是 385271，请勿泄露。"+filler, "")
	if !ok {
		t.Fatal("a code before the scan cap was not detected in an over-long body")
	}
	if code != "385271" {
		t.Errorf("detected %q, want %q", code, "385271")
	}

	// The same body with the code beyond the cap: no detection, no panic, and in
	// particular no half-rune fed to the matcher.
	if _, ok := DetectOTP("", filler+"您的验证码是 385271", ""); ok {
		t.Error("a code past the scan cap was detected, so the cap does not bound the scan")
	}
}
