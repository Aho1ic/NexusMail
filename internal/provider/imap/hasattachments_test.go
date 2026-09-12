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

// has_attachments is what the message list draws its paperclip from. The column
// and the domain field existed from the first migration and nothing ever assigned
// them, so every row read 0 and the indicator was dead: a user could not tell a
// mail carrying an invoice from a one-line reply without opening it.
//
// The three shapes are asserted together because the interesting part is the
// boundary between them, not any one value: a fix that simply wrote true whenever
// the MIME tree had more than one part would satisfy the attachment case and fail
// the other two.
func TestIngestFlagsOnlyDownloadableAttachments(t *testing.T) {
	const boundary = "BOUND"
	cases := []struct {
		subject string
		raw     string
		want    bool
	}{
		{
			subject: "plain-reply",
			raw:     rawMessage("plain-reply"),
			want:    false,
		},
		{
			subject: "with-invoice",
			raw: "MIME-Version: 1.0\r\nMessage-Id: <with-invoice@example.com>\r\n" +
				"From: Sender <sender@example.com>\r\nTo: mail@example.com\r\nSubject: with-invoice\r\n" +
				"Content-Type: multipart/mixed; boundary=" + boundary + "\r\n\r\n" +
				"--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n请见附件\r\n" +
				"--" + boundary + "\r\nContent-Type: application/pdf\r\n" +
				"Content-Disposition: attachment; filename=\"invoice.pdf\"\r\n\r\n%PDF-1.4 payload\r\n" +
				"--" + boundary + "--\r\n",
			want: true,
		},
		{
			// A CID image the HTML references: a signature logo or a tracking pixel.
			// It is stored as an attachment row so the reading pane can resolve the
			// cid: URL, but there is nothing here for the user to save, and marking
			// this mail as having attachments would light the paperclip on most
			// marketing mail in the account.
			subject: "inline-logo-only",
			raw: "MIME-Version: 1.0\r\nMessage-Id: <inline-logo-only@example.com>\r\n" +
				"From: Sender <sender@example.com>\r\nTo: mail@example.com\r\nSubject: inline-logo-only\r\n" +
				"Content-Type: multipart/related; boundary=" + boundary + "\r\n\r\n" +
				"--" + boundary + "\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
				"<p>hi<img src=\"cid:logo@example.com\"></p>\r\n" +
				"--" + boundary + "\r\nContent-Type: image/png\r\n" +
				"Content-Id: <logo@example.com>\r\n" +
				"Content-Disposition: inline; filename=\"logo.png\"\r\n\r\nPNGDATA\r\n" +
				"--" + boundary + "--\r\n",
			want: false,
		},
	}

	h := newHarness(t)
	for _, testCase := range cases {
		if _, err := h.user.Append("INBOX", literal{strings.NewReader(testCase.raw)}, &goimap.AppendOptions{Time: time.Now()}); err != nil {
			t.Fatalf("append %s: %v", testCase.subject, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.supervisor.Stop()
	waitConnected(t, h)

	stored := make(map[string]bool)
	waitFor(t, 60*time.Second, func() bool {
		page, err := h.repo.ListMessages(ctx, ports.MessageFilter{AccountID: &h.account.ID, Limit: 50})
		if err != nil || len(page.Items) < len(cases) {
			return false
		}
		for _, item := range page.Items {
			stored[item.Subject] = item.HasAttachments
		}
		return true
	})

	for _, testCase := range cases {
		got, ok := stored[testCase.subject]
		if !ok {
			t.Fatalf("%s was never ingested", testCase.subject)
		}
		if got != testCase.want {
			t.Errorf("%s: has_attachments = %v, want %v", testCase.subject, got, testCase.want)
		}
	}

	// The inline part must still be stored, otherwise the paperclip fix would have
	// been bought by breaking inline image rendering.
	page, err := h.repo.ListMessages(ctx, ports.MessageFilter{AccountID: &h.account.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.Subject != "inline-logo-only" {
			continue
		}
		_, attachments, err := h.repo.GetMessage(ctx, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(attachments) != 1 || attachments[0].Disposition != "inline" {
			t.Fatalf("inline part rows are %+v, want one inline attachment", attachments)
		}
	}
}
