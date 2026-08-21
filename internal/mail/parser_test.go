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

// TestSanitizeHTMLNarrowsDataURIs covers the scheme allowlist. Inline images arrive
// as data URIs so the scheme cannot simply be dropped, but allowing it outright also
// allowed data:text/html on a link — script execution the rest of the policy does
// not cover.
func TestSanitizeHTMLNarrowsDataURIs(t *testing.T) {
	const pngPixel = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAAAAAA6fptVAAAACklEQVQI12NgAAAAAgAB4iG8MwAAAABJRU5ErkJggg=="

	inline := sanitizeHTML(`<p><img src="` + pngPixel + `"></p>`)
	if !strings.Contains(inline, "data:image/png;base64,") {
		t.Fatalf("inline image was stripped: %s", inline)
	}

	for _, payload := range []string{
		`<a href="data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==">click</a>`,
		`<a href="data:text/html,<script>alert(1)</script>">click</a>`,
		`<img src="data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==">`,
		`<img src="data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciPjxzY3JpcHQ+YWxlcnQoMSk8L3NjcmlwdD48L3N2Zz4=">`,
		`<a href="javascript:alert(1)">click</a>`,
	} {
		got := sanitizeHTML(payload)
		if strings.Contains(got, "data:text/html") || strings.Contains(got, "svg+xml") || strings.Contains(got, "javascript:") {
			t.Errorf("sanitizeHTML(%q) kept an unsafe URL: %s", payload, got)
		}
	}
}

// TestSanitizeHTMLKeepsCIDReferences protects the other half: a stripped cid: URL
// turns every embedded image in normal mail into a broken one.
func TestSanitizeHTMLKeepsCIDReferences(t *testing.T) {
	got := sanitizeHTML(`<p><img src="cid:logo@example.com"></p>`)
	if !strings.Contains(got, "cid:logo@example.com") {
		t.Fatalf("cid reference was stripped: %s", got)
	}
}

// TestSanitizeHTMLDefersRemoteImages records the privacy behaviour: the URL is kept
// for an explicit opt-in, but the initial render must not fetch it, because that
// fetch reports the reader's address and open time to the sender.
func TestSanitizeHTMLDefersRemoteImages(t *testing.T) {
	got := sanitizeHTML(`<p><img src="https://tracker.example/open.gif?id=42"></p>`)
	// The check is on the src attribute specifically: the URL stays in the output under
	// the deferred attribute name, so searching for the host alone proves nothing.
	if strings.Contains(got, `<img src=`) {
		t.Fatalf("remote image is still loaded on render: %s", got)
	}
	if !strings.Contains(got, "data-nexusmail-remote-src") {
		t.Fatalf("remote URL was discarded instead of deferred: %s", got)
	}
}

// TestSanitizeHTMLKeepsTableLayout covers the presentation half of the policy. Mail
// is laid out with nested tables plus inline style, and stripping both left every
// such message as unstyled rows with no padding, spacing or colour — which read on
// screen as a garbled table rather than as a plain one.
func TestSanitizeHTMLKeepsTableLayout(t *testing.T) {
	got := sanitizeHTML(`<table width="600" border="1" cellpadding="8" cellspacing="0" bgcolor="#f5f5f5" style="border-collapse:collapse;width:600px">` +
		`<tr><th style="padding:8px;text-align:left;background-color:#eee">项目</th></tr>` +
		`<tr><td align="center" bgcolor="#ffffff" style="padding:20px;font-size:14px;color:#333">阿里云服务器续费</td></tr></table>`)
	for _, want := range []string{
		`width="600"`, `border="1"`, `cellpadding="8"`, `cellspacing="0"`, `bgcolor="#f5f5f5"`,
		"border-collapse: collapse", "width: 600px", "padding: 8px", "text-align: left",
		"background-color: #eee", `align="center"`, `bgcolor="#ffffff"`, "font-size: 14px", "color: #333",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("table layout lost %q: %s", want, got)
		}
	}
}

// TestSanitizeHTMLStyleCannotLeakOrEscape is the other side of allowing style. The
// attribute must not become a second route to a remote fetch — that would defeat
// blockRemoteImages — nor a way for a message to leave its own flow.
func TestSanitizeHTMLStyleCannotLeakOrEscape(t *testing.T) {
	for _, payload := range []string{
		`<div style="background-image:url(https://tracker.example/x.gif)">a</div>`,
		`<div style="background:#fff url(https://tracker.example/x.gif) no-repeat">a</div>`,
		// bluemonday expands CSS unicode escapes before the handler runs, so the
		// escaped spelling of url( has to be rejected by the same check.
		`<div style="background:url\28 https://tracker.example/x.gif\29">a</div>`,
		`<div style="list-style-image:url(https://tracker.example/x.gif)">a</div>`,
		`<div style="border-image-source:url(https://tracker.example/x.gif)">a</div>`,
		`<div style="behavior:url(#default#time2)">a</div>`,
		`<div style="position:fixed;top:0;left:0;z-index:99999">overlay</div>`,
		`<div style="transform:scale(40)">huge</div>`,
		`<div style="width:expression(alert(1))">a</div>`,
		`<td bgcolor="javascript:alert(1)">a</td>`,
		`<td bgcolor="url(https://tracker.example/x.gif)">a</td>`,
	} {
		got := sanitizeHTML(payload)
		for _, forbidden := range []string{"url(", "tracker.example", "position", "z-index", "transform", "expression", "javascript:", "behavior"} {
			if strings.Contains(got, forbidden) {
				t.Errorf("sanitizeHTML(%q) kept %q: %s", payload, forbidden, got)
			}
		}
	}
}
