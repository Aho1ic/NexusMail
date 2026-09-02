//go:build sqlite_fts5

package imap

import (
	"context"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/ports"

	goimap "github.com/emersion/go-imap/v2"
)

// A verification code is worth a notification only while it is still usable, so
// the supervisor carries the code on the event that announces the mail instead of
// making the browser fetch the body to find out there was one. Three places
// publish it — arrival (subject only), the prefetch worker, and a foreground
// FetchBody — and each was uncovered: the frontend tests consume otp_code from a
// mocked socket, and the HTTP test covers the read path, which is a different
// function. Nothing asserted the server ever puts the field on an event.
//
// The failure these guard is silent and user-visible: the code still reaches the
// reading pane, so a review sees a working feature. Only the notification, which
// is the entire point of detecting the code early, goes missing.

// otpCodeFor returns the code and subject an event carries, or empty strings when
// it carries neither. Events reach the recorder as map[string]any, and a missing
// key and a wrong-typed key fail the same way.
func otpCodeFor(event ports.Event, messageID int64) (code string, subject string, matched bool) {
	data, ok := event.Data.(map[string]any)
	if !ok {
		return "", "", false
	}
	if id, ok := data["message_id"].(int64); !ok || id != messageID {
		return "", "", false
	}
	code, _ = data["otp_code"].(string)
	subject, _ = data["otp_subject"].(string)
	return code, subject, true
}

// otpMessage builds a mail whose body carries a code the detector will accept.
// The keyword anchors it; a bare digit run is deliberately ignored by DetectOTP.
//
// pad appends filler to push the message past a size threshold. The code stays at
// the top because the detector only scans the first maxOTPScanBytes of a segment.
func otpMessage(subject, code string, pad int) string {
	body := "Your verification code is " + code + ", valid for 10 minutes.\r\n"
	if pad > 0 {
		body += strings.Repeat("filler line to grow the body past the prefetch cap.\r\n", pad)
	}
	return "MIME-Version: 1.0\r\nMessage-Id: <" + code + "@example.com>\r\n" +
		"From: Service <no-reply@example.com>\r\nTo: mail@example.com\r\n" +
		"Subject: " + subject + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body
}

// TestFetchBodyCarriesTheVerificationCode covers the foreground path. The handler
// answers 202 after 3s, so for a slow body this event is the only thing that can
// raise the notification at all.
func TestFetchBodyCarriesTheVerificationCode(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.supervisor.Stop)

	const code = "482913"
	const subject = "Sign-in verification"
	// Over the prefetch size cap, so the worker never makes this row a candidate
	// and FetchBody is the only path that can announce it. That is precisely the
	// case FetchBody's comment names as permanently invisible before it published
	// its own event — and it also removes the race that resetting body_state has,
	// where a probe re-enqueues the row and the worker reaches 'ready' first.
	padded := otpMessage(subject, code, 24_000)
	if len(padded) <= maxInlineDraftImportBytes {
		t.Fatalf("message is %d bytes, want more than the %d prefetch cap", len(padded), maxInlineDraftImportBytes)
	}
	if _, err := h.user.Append("INBOX", literal{strings.NewReader(padded)}, &goimap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	messageID := waitForMessage(t, h)

	message, _, err := h.repo.GetMessage(ctx, messageID)
	if err != nil {
		t.Fatal(err)
	}
	if message.BodyState == "ready" {
		t.Fatalf("body is already ready: the prefetch took a message over its size cap, so this test no longer covers the foreground path")
	}
	before := len(h.events.snapshot())

	if err := h.supervisor.FetchBody(ctx, messageID); err != nil {
		t.Fatalf("FetchBody: %v", err)
	}

	found := false
	for _, event := range h.events.snapshot()[before:] {
		if event.Type != "MESSAGE_UPDATED" {
			continue
		}
		got, gotSubject, matched := otpCodeFor(event, messageID)
		if !matched {
			continue
		}
		found = true
		if got != code {
			t.Errorf("otp_code = %q, want %q: the browser cannot raise a copyable notification without it", got, code)
		}
		// The subject travels with the code so the notification can say which
		// service the code is for without another round-trip.
		if gotSubject != subject {
			t.Errorf("otp_subject = %q, want %q", gotSubject, subject)
		}
	}
	if !found {
		t.Fatal("FetchBody published no MESSAGE_UPDATED for this message")
	}
}

// TestPrefetchCarriesTheVerificationCode covers the worker path, which is the one
// that normally wins: the prefetch usually has the body before the user opens the
// mail, so this is the event that raises the notification in practice.
func TestPrefetchCarriesTheVerificationCode(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.supervisor.Stop)

	const code = "735204"
	const subject = "Password reset"
	if _, err := h.user.Append("INBOX", literal{strings.NewReader(otpMessage(subject, code, 0))}, &goimap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	messageID := waitForMessage(t, h)

	// The prefetch runs on its own schedule, so wait for the event rather than
	// driving it: the body reaching 'ready' is what says the worker ran.
	var got, gotSubject string
	waitFor(t, 30*time.Second, func() bool {
		for _, event := range h.events.snapshot() {
			if event.Type != "MESSAGE_UPDATED" {
				continue
			}
			if code, subject, matched := otpCodeFor(event, messageID); matched && code != "" {
				got, gotSubject = code, subject
				return true
			}
		}
		return false
	})
	if got != code {
		t.Errorf("otp_code = %q, want %q", got, code)
	}
	if gotSubject != subject {
		t.Errorf("otp_subject = %q, want %q", gotSubject, subject)
	}
}

// TestArrivalCarriesACodeFoundInTheSubject covers the ingest path. The body is
// still empty when the mail lands, so only the subject can be scanned — that is
// what makes the notification arrive without waiting for a body fetch.
func TestArrivalCarriesACodeFoundInTheSubject(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.supervisor.Stop)

	// The Chinese shape the ingest comment names, since that is the one this
	// subject-only scan exists for.
	const subject = "【示例服务】验证码 385271"
	const code = "385271"
	if _, err := h.user.Append("INBOX", literal{strings.NewReader(otpMessage(subject, code, 0))}, &goimap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	messageID := waitForMessage(t, h)

	var got, gotSubject string
	waitFor(t, 30*time.Second, func() bool {
		for _, event := range h.events.snapshot() {
			if event.Type != "NEW_EMAIL" {
				continue
			}
			if code, subject, matched := otpCodeFor(event, messageID); matched && code != "" {
				got, gotSubject = code, subject
				return true
			}
		}
		return false
	})
	if got != code {
		t.Errorf("NEW_EMAIL otp_code = %q, want %q: the notification would wait for the prefetch instead of arriving with the mail", got, code)
	}
	if gotSubject != subject {
		t.Errorf("NEW_EMAIL otp_subject = %q, want %q", gotSubject, subject)
	}
}

// TestAMailWithNoCodeCarriesNoOTPFields is the negative half. An unconditional
// field would put an empty code on every event, and the browser reads presence,
// not emptiness — every arriving mail would raise a blank notification.
func TestAMailWithNoCodeCarriesNoOTPFields(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.supervisor.Stop)

	h.deliver(t, "an ordinary mail with 385271 in it")
	messageID := waitForMessage(t, h)
	waitFor(t, 30*time.Second, func() bool {
		message, _, err := h.repo.GetMessage(ctx, messageID)
		return err == nil && message.BodyState == "ready"
	})

	for _, event := range h.events.snapshot() {
		data, ok := event.Data.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := data["message_id"].(int64); !ok || id != messageID {
			continue
		}
		if _, present := data["otp_code"]; present {
			t.Errorf("%s carries otp_code for a mail with no code: a bare digit run is not a verification code", event.Type)
		}
		if _, present := data["otp_subject"]; present {
			t.Errorf("%s carries otp_subject for a mail with no code", event.Type)
		}
	}
}
