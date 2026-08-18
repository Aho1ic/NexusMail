package mail

import (
	"net/mail"
	"strings"
	"testing"
)

func TestParseCharsetMultipartAndSanitizeHTML(t *testing.T) {
	raw := strings.Join([]string{
		"From: =?UTF-8?B?5rWL6K+V?= <sender@example.com>",
		"To: receiver@example.com",
		"Subject: =?GB2312?B?1eLKx9bQ?=",
		"Message-ID: <fixture@example.com>",
		"MIME-Version: 1.0",
		`Content-Type: multipart/alternative; boundary="nexus"`,
		"",
		"--nexus",
		`Content-Type: text/plain; charset="utf-8"`,
		"",
		"你好，NexusMail",
		"--nexus",
		`Content-Type: text/html; charset="utf-8"`,
		"",
		`<p onclick="alert(1)">正文<script>alert(1)</script><img src="https://tracker.example/pixel"><img src="cid:logo"></p>`,
		"--nexus--",
		"",
	}, "\r\n")
	parsed, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Subject != "这是中" {
		t.Fatalf("subject = %q", parsed.Subject)
	}
	if parsed.Text != "你好，NexusMail" || parsed.Snippet != parsed.Text {
		t.Fatalf("unexpected text/snippet: %#v", parsed)
	}
	if strings.Contains(parsed.HTML, "script") || strings.Contains(parsed.HTML, "onclick") {
		t.Fatalf("unsafe HTML survived: %s", parsed.HTML)
	}
	if strings.Contains(parsed.HTML, `<img src="https://tracker.example`) || !strings.Contains(parsed.HTML, "data-nexusmail-remote-src") {
		t.Fatalf("remote image was not blocked: %s", parsed.HTML)
	}
	if !strings.Contains(parsed.HTML, "cid:logo") {
		t.Fatalf("CID image was removed: %s", parsed.HTML)
	}
}

func TestComposeDoesNotExposeBCCHeader(t *testing.T) {
	message, err := Compose(Outgoing{
		MessageID: "<out@example.com>",
		From:      mail.Address{Address: "sender@example.com"},
		To:        []mail.Address{{Address: "to@example.com"}},
		BCC:       []mail.Address{{Address: "hidden@example.com"}},
		Subject:   "主题",
		BodyText:  "正文",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(message)), "bcc:") || strings.Contains(string(message), "hidden@example.com") {
		t.Fatal("BCC leaked into MIME headers")
	}
}
