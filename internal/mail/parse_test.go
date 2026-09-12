package mail

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The branches Parse takes on hostile or merely awkward mail. Every case here is a
// real message shape: a Chinese-encoded body, a part too large to hold in memory, a
// truncated MIME structure, a header a provider encoded wrongly. What they have in
// common is that the parser must degrade rather than fail, because the alternative is
// a message that never appears in the mailbox at all.

// raw assembles a message from CRLF-joined lines. Mail is CRLF-delimited and the
// parser is entitled to rely on that, so tests must not hand it bare newlines.
func raw(lines ...string) string { return strings.Join(lines, "\r\n") }

func TestParseDecodesAGBKBody(t *testing.T) {
	// GBK is what QQ and 163 send for Chinese mail, so this is the common path for
	// this application rather than an edge case. The bytes are real GBK for 你好世界.
	message := raw(
		"From: sender@qq.com",
		"To: me@example.com",
		"Subject: =?gb2312?B?vLzI69fUvLo=?=",
		`Content-Type: text/plain; charset="gb2312"`,
		"",
		"\xc4\xe3\xba\xc3\xca\xc0\xbd\xe7",
	)
	parsed, err := Parse(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parsed.Text, "你好世界") {
		t.Errorf("a GBK body decoded to %q, want 你好世界", parsed.Text)
	}
}

func TestParseKeepsTheBodyWhenTheCharsetIsUnknown(t *testing.T) {
	// An invented charset must not lose the body: the bytes are still mostly
	// readable, and an empty message is worse than an imperfectly decoded one.
	message := raw(
		"From: sender@example.com",
		`Content-Type: text/plain; charset="x-not-a-charset"`,
		"",
		"plain ascii survives",
	)
	parsed, err := Parse(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parsed.Text, "plain ascii survives") {
		t.Errorf("an unknown charset lost the body: %q", parsed.Text)
	}
}

func TestParseReportsAnUnreadableMessage(t *testing.T) {
	// Not a MIME entity at all. This is the one input Parse is allowed to refuse,
	// and it has to refuse it rather than return an empty Parsed that would be
	// stored as a blank message.
	if _, err := Parse(strings.NewReader("\x00\x00 not a message")); err == nil {
		t.Error("Parse accepted input that is not a MIME entity")
	}
}

func TestParseSurvivesATruncatedMultipart(t *testing.T) {
	// A body that ends mid-part, as happens when a fetch is cut short. Whatever was
	// already parsed has to be kept: the headers are the useful part.
	message := raw(
		"From: sender@example.com",
		"Subject: truncated",
		`Content-Type: multipart/mixed; boundary="b"`,
		"",
		"--b",
		"Content-Type: text/plain",
		"",
		"first part text",
		"--b",
		"Content-Type: text/plain",
		"",
		"second part, no closing boundary",
	)
	parsed, _ := Parse(strings.NewReader(message))
	// Parse may or may not report an error here depending on how the underlying
	// reader treats the missing terminator; what must hold either way is that the
	// headers and the first part survived.
	if parsed.Subject != "truncated" {
		t.Errorf("subject = %q, want it preserved through the truncation", parsed.Subject)
	}
	if !strings.Contains(parsed.Text, "first part text") {
		t.Errorf("text = %q, want the part that did arrive", parsed.Text)
	}
}

func TestParseSkipsAnOversizedPart(t *testing.T) {
	// A part past the in-memory ceiling is skipped, not truncated: half a message
	// body reads as a complete one to the user, which is worse than an empty text
	// with the body still fetchable as an attachment.
	big := strings.Repeat("A", maxParsedPartBytes+1024)
	message := raw(
		"From: sender@example.com",
		`Content-Type: multipart/alternative; boundary="b"`,
		"",
		"--b",
		"Content-Type: text/plain",
		"",
		big,
		"--b",
		"Content-Type: text/html",
		"",
		"<p>small enough</p>",
		"--b--",
		"",
	)
	parsed, err := Parse(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(parsed.Text, big[:2048]) {
		t.Error("an oversized part was kept")
	}
	// The small HTML part still has to come through, and with no text part the
	// snippet falls back to the stripped HTML.
	if !strings.Contains(parsed.HTML, "small enough") {
		t.Errorf("the small part was lost: HTML = %q", parsed.HTML)
	}
	if !strings.Contains(parsed.Text, "small enough") {
		t.Errorf("text did not fall back to the stripped HTML: %q", parsed.Text)
	}
}

func TestParseFallsBackToStrippedHTMLForTheSnippet(t *testing.T) {
	message := raw(
		"From: sender@example.com",
		"Content-Type: text/html",
		"",
		`<div><b>Hello</b> <a href="https://example.com">link</a><script>alert(1)</script></div>`,
	)
	parsed, err := Parse(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parsed.Text, "Hello") {
		t.Errorf("text = %q, want the HTML stripped into it", parsed.Text)
	}
	// The stripped text is what the list view and notifications show, so script
	// content must not reach it.
	if strings.Contains(parsed.Text, "alert") {
		t.Errorf("script content leaked into the plain text: %q", parsed.Text)
	}
	if parsed.Snippet == "" {
		t.Error("no snippet was produced for an HTML-only message")
	}
}

func TestParseDoesNotTreatNamedPartsAsTheBody(t *testing.T) {
	// A part with a name but no disposition is still an attachment, which is how
	// several providers send them. Its payload must not stand in for the message
	// body, and an inline image is the same: both are fetched later by part path.
	message := raw(
		"From: sender@example.com",
		`Content-Type: multipart/mixed; boundary="b"`,
		"",
		"--b",
		"Content-Type: text/plain",
		"",
		"body",
		"--b",
		`Content-Type: application/pdf; name="=?utf-8?B?5oql6KGoLnBkZg==?="`,
		"",
		"pdf-payload-must-not-become-text",
		"--b",
		`Content-Type: image/png`,
		`Content-Disposition: inline; filename="logo.png"`,
		"Content-Id: <logo@cid>",
		"",
		"<p>image-payload-must-not-become-html</p>",
		"--b--",
		"",
	)
	parsed, err := Parse(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Text != "body" {
		t.Errorf("text = %q, want the unnamed text part only", parsed.Text)
	}
	if strings.Contains(parsed.Text, "pdf-payload") || strings.Contains(parsed.HTML, "image-payload") {
		t.Errorf("an attachment leaked into the body: text=%q html=%q", parsed.Text, parsed.HTML)
	}
}

func TestParseKeepsAMalformedHeaderVerbatim(t *testing.T) {
	// A broken encoded-word must not be dropped. Showing the raw header is ugly;
	// showing nothing loses the only subject the user has.
	message := raw(
		"From: sender@example.com",
		"Subject: =?utf-8?B?!!!not-base64!!!?=",
		"Content-Type: text/plain",
		"",
		"body",
	)
	parsed, err := Parse(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Subject == "" {
		t.Error("a malformed subject header was dropped entirely")
	}
}

func TestParseReadsThreadingAndAddressHeaders(t *testing.T) {
	message := raw(
		"From: 张三 <zhang@example.com>",
		"To: a@example.com, b@example.com",
		"Cc: c@example.com",
		"Message-Id: <child@example.com>",
		"In-Reply-To: <parent@example.com>",
		"References: <root@example.com> <parent@example.com>",
		"Content-Type: text/plain",
		"",
		"body",
	)
	parsed, err := Parse(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.MessageID != "<child@example.com>" || parsed.InReplyTo != "<parent@example.com>" {
		t.Errorf("threading headers = %q / %q", parsed.MessageID, parsed.InReplyTo)
	}
	if len(parsed.References) != 2 {
		t.Errorf("references = %v, want two entries", parsed.References)
	}
	if len(parsed.To) != 2 || len(parsed.CC) != 1 || len(parsed.From) != 1 {
		t.Fatalf("addresses: from %d, to %d, cc %d", len(parsed.From), len(parsed.To), len(parsed.CC))
	}
	if parsed.From[0].Name != "张三" {
		t.Errorf("from name = %q, want 张三", parsed.From[0].Name)
	}
}

func TestSnippetTruncatesOnRuneBoundaries(t *testing.T) {
	// Truncating a multi-byte rune mid-sequence produces a replacement character in
	// the list view, so the cut is by rune. 240 is what Parse asks for.
	message := raw(
		"From: sender@example.com",
		"Content-Type: text/plain",
		"",
		strings.Repeat("验证码通知", 200),
	)
	parsed, err := Parse(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(parsed.Snippet, "…") {
		t.Errorf("a long snippet was not marked as truncated: %q", parsed.Snippet)
	}
	if strings.Contains(parsed.Snippet, "�") {
		t.Error("the snippet was cut mid-rune")
	}
	if runes := len([]rune(parsed.Snippet)); runes != 241 {
		t.Errorf("snippet is %d runes, want 240 plus the ellipsis", runes)
	}

	// Whitespace is collapsed, so a body of newlines does not produce a snippet of
	// blank space in the list.
	spaced := raw("From: s@example.com", "Content-Type: text/plain", "", "one\r\n\r\n\ttwo   three")
	short, err := Parse(strings.NewReader(spaced))
	if err != nil {
		t.Fatal(err)
	}
	if short.Snippet != "one two three" {
		t.Errorf("snippet = %q, want collapsed whitespace", short.Snippet)
	}
}

// TestBlockRemoteImagesStripsSrcset is the privacy branch. srcset is a second way to
// name a remote image, and a client that honours it fetches the tracker even though
// src was rewritten — so it is removed rather than rewritten. Nothing exercised this
// before, which meant the leak would not have been noticed.
func TestBlockRemoteImagesStripsSrcset(t *testing.T) {
	message := raw(
		"From: sender@example.com",
		"Content-Type: text/html",
		"",
		`<p><img src="https://tracker.example/pixel.gif" srcset="https://tracker.example/2x.gif 2x" alt="x"></p>`,
	)
	parsed, err := Parse(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(parsed.HTML, "srcset") {
		t.Errorf("srcset survived sanitising: %q", parsed.HTML)
	}
	if strings.Contains(parsed.HTML, "2x.gif") {
		t.Errorf("a srcset URL survived: %q", parsed.HTML)
	}
	// And the ordinary src is deferred rather than deleted, so the UI can still
	// offer to load it.
	if !strings.Contains(parsed.HTML, "data-nexusmail-remote-src") {
		t.Errorf("the remote src was not deferred: %q", parsed.HTML)
	}
	// The leading space matters: the deferred attribute is data-nexusmail-remote-src,
	// which ends with the bare src="…" spelling. Without a boundary this assertion
	// matches the rewritten attribute and fails on correct output.
	if strings.Contains(parsed.HTML, ` src="https://tracker.example/pixel.gif"`) {
		t.Errorf("the remote src was left live: %q", parsed.HTML)
	}
}

// TestParseKeepsLaterPartsAfterAnUndecodableOne puts the bad part first, which is what
// separates this from the unknown-charset test above: there the whole message was one
// part, so tolerating the error only recovered that part. Here NextPart reports the same
// advisory error partway through a multipart, and treating it as fatal cost the reader
// everything after it — the legible HTML alternative and the attachment list — because
// one part carried a charset label x/text does not have.
func TestParseKeepsLaterPartsAfterAnUndecodableOne(t *testing.T) {
	parsed, err := Parse(strings.NewReader(raw(
		"From: sender@qq.com",
		"Subject: mixed",
		`Content-Type: multipart/mixed; boundary="b"`,
		"",
		"--b",
		`Content-Type: text/plain; charset="x-not-a-charset"`,
		"",
		"\xc4\xe3\xba\xc3",
		"--b",
		`Content-Type: text/html; charset="utf-8"`,
		"",
		"<p>readable</p>",
		"--b",
		`Content-Type: application/pdf; name="report.pdf"`,
		"Content-Disposition: attachment",
		"",
		"%PDF-1.4",
		"--b--",
	)))
	if err != nil {
		t.Fatalf("an undecodable part made the parse fail: %v", err)
	}
	if !strings.Contains(parsed.HTML, "readable") {
		t.Errorf("the part after the undecodable one was lost: HTML=%q", parsed.HTML)
	}
	if strings.Contains(parsed.Text, "%PDF") {
		t.Errorf("the attachment past the undecodable part leaked into text: %q", parsed.Text)
	}
	// The undecodable part is still offered as text rather than dropped: raw bytes beat
	// nothing, and the alternative is a message that looks empty.
	if parsed.Text == "" {
		t.Error("the undecodable part contributed no text at all")
	}
}

// nestedMultipart wraps a leaf in `depth` layers of multipart/mixed. Depth 0 is
// a single-part message. The leaf sits at the innermost part, so a ceiling of N
// layers keeps it at depth N and drops it at depth N+1.
func nestedMultipart(depth int, leaf string) string {
	var open, close strings.Builder
	open.WriteString("From: a@b.c\r\nSubject: nested\r\n")
	for i := range depth {
		fmt.Fprintf(&open, "Content-Type: multipart/mixed; boundary=\"b%d\"\r\n\r\n--b%d\r\n", i, i)
	}
	open.WriteString("Content-Type: text/plain\r\n\r\n")
	open.WriteString(leaf)
	open.WriteString("\r\n")
	for i := depth - 1; i >= 0; i-- {
		fmt.Fprintf(&close, "--b%d--\r\n", i)
	}
	return open.String() + close.String()
}

func TestParseKeepsAShallowNestedMessage(t *testing.T) {
	// mixed > related > alternative > leaf is the everyday shape (a message with
	// an HTML alternative and an inline image). Four layers is also the signed
	// case. Both have to parse identically to before the depth ceiling existed.
	message := raw(
		"From: sender@example.com",
		"Subject: everyday",
		`Content-Type: multipart/mixed; boundary="mixed"`,
		"",
		"--mixed",
		`Content-Type: multipart/related; boundary="related"`,
		"",
		"--related",
		`Content-Type: multipart/alternative; boundary="alt"`,
		"",
		"--alt",
		"Content-Type: text/plain",
		"",
		"plain body",
		"--alt",
		"Content-Type: text/html",
		"",
		"<p>html body</p>",
		"--alt--",
		"--related",
		`Content-Type: image/png`,
		`Content-Disposition: inline; filename="logo.png"`,
		"",
		"png-bytes",
		"--related--",
		"--mixed",
		`Content-Type: application/pdf; name="file.pdf"`,
		"Content-Disposition: attachment",
		"",
		"%PDF",
		"--mixed--",
		"",
	)
	parsed, err := Parse(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Subject != "everyday" {
		t.Errorf("subject = %q", parsed.Subject)
	}
	if parsed.Text != "plain body" {
		t.Errorf("text = %q, want the innermost plain part", parsed.Text)
	}
	if !strings.Contains(parsed.HTML, "html body") {
		t.Errorf("html = %q, want the innermost html part", parsed.HTML)
	}
	if strings.Contains(parsed.Text, "%PDF") || strings.Contains(parsed.HTML, "png-bytes") {
		t.Errorf("a sibling attachment leaked into the body: text=%q html=%q", parsed.Text, parsed.HTML)
	}
}

func TestParseAdoptsALeafAtTheDepthCeiling(t *testing.T) {
	parsed, err := Parse(strings.NewReader(nestedMultipart(maxParsedMIMEDepth, "at the ceiling")))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Text != "at the ceiling" {
		t.Errorf("text = %q, want the leaf that sits on the last allowed layer", parsed.Text)
	}
}

func TestParseDropsALeafPastTheDepthCeiling(t *testing.T) {
	// A part just below the ceiling is dropped, but everything already parsed —
	// here the headers — is kept, and the call is not an error. A malformed
	// message must not vanish from the mailbox.
	parsed, err := Parse(strings.NewReader(nestedMultipart(maxParsedMIMEDepth+1, "too deep")))
	if err != nil {
		t.Fatalf("a part past the depth ceiling failed the parse: %v", err)
	}
	if parsed.Subject != "nested" {
		t.Errorf("subject = %q, want the headers kept", parsed.Subject)
	}
	if strings.Contains(parsed.Text, "too deep") {
		t.Errorf("a part below the depth ceiling was adopted: %q", parsed.Text)
	}
}

func TestParseBoundsADeeplyNestedBomb(t *testing.T) {
	// Unbounded, this input is the measured DoS: depth 8000 used to take ~7 s and
	// held the IMAP command lock the whole time. With the ceiling it has to return
	// well under 2 s, and the shallow sibling has to survive.
	var open, close strings.Builder
	open.WriteString("From: a@b.c\r\nSubject: bomb\r\n")
	open.WriteString("Content-Type: multipart/mixed; boundary=\"outer\"\r\n\r\n")
	open.WriteString("--outer\r\nContent-Type: text/plain\r\n\r\nvisible\r\n")
	open.WriteString("--outer\r\n")
	const depth = 8000
	for i := range depth {
		fmt.Fprintf(&open, "Content-Type: multipart/mixed; boundary=\"b%d\"\r\n\r\n--b%d\r\n", i, i)
	}
	open.WriteString("Content-Type: text/plain\r\n\r\nburied\r\n")
	for i := depth - 1; i >= 0; i-- {
		fmt.Fprintf(&close, "--b%d--\r\n", i)
	}
	close.WriteString("--outer--\r\n")
	start := time.Now()
	parsed, err := Parse(strings.NewReader(open.String() + close.String()))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the nested bomb failed the parse: %v", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("Parse took %v on an %d-deep nest, want < 2s", elapsed, depth)
	}
	if parsed.Text != "visible" {
		t.Errorf("text = %q, want the sibling that sat above the ceiling", parsed.Text)
	}
	if strings.Contains(parsed.Text, "buried") {
		t.Error("the buried leaf leaked into the body")
	}
}

func TestParseStopsAtTheMessageByteCeiling(t *testing.T) {
	// The first part is small and complete; the second is large. The ceiling sits
	// just after the first, so the first is kept and the rest is dropped — not an
	// error, because a message past the ceiling is truncated input, not malformed
	// input. parse is called with a small ceiling so the fixture stays tiny; the
	// contract under test is the same as the production 48 MiB cap.
	first := raw(
		"From: a@b.c",
		"Subject: cut",
		`Content-Type: multipart/mixed; boundary="b"`,
		"",
		"--b",
		"Content-Type: text/plain",
		"",
		"keep me",
		"--b",
		"Content-Type: text/html",
		"",
	)
	msg := first + strings.Repeat("x", 4096) + "\r\n--b--\r\n"
	parsed, err := parse(strings.NewReader(msg), int64(len(first)+10))
	if err != nil {
		t.Fatalf("a message past the ceiling failed the parse: %v", err)
	}
	if parsed.Subject != "cut" {
		t.Errorf("subject = %q", parsed.Subject)
	}
	if parsed.Text != "keep me" {
		t.Errorf("text = %q, want the part that fit under the ceiling", parsed.Text)
	}
	if parsed.HTML != "" {
		t.Errorf("the part past the ceiling leaked into HTML: %q", parsed.HTML)
	}
}

func TestParseKeepsAMessageAtTheByteCeiling(t *testing.T) {
	msg := raw(
		"From: a@b.c",
		"Subject: exact",
		"Content-Type: text/plain",
		"",
		"fits",
	)
	parsed, err := parse(strings.NewReader(msg), int64(len(msg)))
	if err != nil {
		t.Fatalf("a message of exactly the ceiling failed: %v", err)
	}
	if parsed.Text != "fits" {
		t.Errorf("text = %q, want the whole body of a message that fits exactly", parsed.Text)
	}
}

func TestParseDropsATruncatedSinglePartBody(t *testing.T) {
	// The multipart ceiling is reported as a structural error and swallowed; a
	// single-part body is different. Cutting the source mid-body lets ReadAll
	// succeed with a prefix and a nil error, which would look like a complete
	// message if the cap flag were not checked. Half a body is worse than none.
	header := raw("From: a@b.c", "Subject: cut", "Content-Type: text/plain", "")
	body := strings.Repeat("X", 80)
	msg := header + "\r\n" + body
	parsed, err := parse(strings.NewReader(msg), int64(len(header)+40))
	if err != nil {
		t.Fatalf("a truncated single-part body failed the parse: %v", err)
	}
	if parsed.Subject != "cut" {
		t.Errorf("subject = %q, want the headers kept", parsed.Subject)
	}
	if parsed.Text != "" {
		t.Errorf("a truncated body was stored as complete text: %q", parsed.Text)
	}
}

func TestBlockRemoteImagesHandlesSourceElements(t *testing.T) {
	// <source> inside <picture> is the same leak by another element name.
	parsed, err := Parse(strings.NewReader(raw(
		"From: s@example.com", "Content-Type: text/html", "",
		`<picture><source src="//tracker.example/a.png"><img src="cid:local"></picture>`,
	)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(parsed.HTML, ` src="//tracker.example/a.png"`) {
		t.Errorf("a protocol-relative source stayed live: %q", parsed.HTML)
	}
	// A cid: reference is local to the message and must not be deferred, or inline
	// images never render.
	if !strings.Contains(parsed.HTML, "cid:local") {
		t.Errorf("a cid reference was altered: %q", parsed.HTML)
	}
}

// TestAllowInlineRasterImageRejectsDecoratedDataURIs covers the two guards that the
// data-URI policy applies before the media type is even looked at. A query or a
// fragment on a data URI is not something a real inline image carries; it is a way to
// smuggle content past a prefix match.
func TestAllowInlineRasterImageRejectsDecoratedDataURIs(t *testing.T) {
	// A single-pixel GIF, which is the smallest thing that must be accepted.
	const pixel = "R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"

	for _, testCase := range []struct {
		name     string
		uri      string
		accepted bool
	}{
		{"a plain raster image", "data:image/gif;base64," + pixel, true},
		{"a query string", "data:image/gif;base64," + pixel + "?a=1", false},
		{"a fragment", "data:image/gif;base64," + pixel + "#frag", false},
		{"payload that is not base64", "data:image/gif;base64,!!!!", false},
		{"svg, which can carry script", "data:image/svg+xml;base64," + pixel, false},
		{"html wearing an image label", "data:text/html;base64," + pixel, false},
		{"no base64 marker", "data:image/gif,raw", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := fmt.Sprintf(`<img src=%q alt="x">`, testCase.uri)
			parsed, err := Parse(strings.NewReader(raw(
				"From: s@example.com", "Content-Type: text/html", "", body,
			)))
			if err != nil {
				t.Fatal(err)
			}
			kept := strings.Contains(parsed.HTML, "data:")
			if kept != testCase.accepted {
				t.Errorf("kept = %v, want %v; HTML = %q", kept, testCase.accepted, parsed.HTML)
			}
		})
	}
}
