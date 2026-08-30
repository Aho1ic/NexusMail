package mail

import (
	"fmt"
	"strings"
	"testing"
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

func TestParseCollectsAttachmentMetadata(t *testing.T) {
	message := raw(
		"From: sender@example.com",
		`Content-Type: multipart/mixed; boundary="b"`,
		"",
		"--b",
		"Content-Type: text/plain",
		"",
		"body",
		"--b",
		// A part with a name but no disposition still has to be treated as an
		// attachment: this is how several providers send them.
		`Content-Type: application/pdf; name="=?utf-8?B?5oql6KGoLnBkZg==?="`,
		"",
		"payload",
		"--b",
		`Content-Type: image/png`,
		`Content-Disposition: inline; filename="logo.png"`,
		"Content-Id: <logo@cid>",
		"",
		"payload",
		"--b--",
		"",
	)
	parsed, err := Parse(strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Attachments) != 2 {
		t.Fatalf("found %d attachments, want 2: %+v", len(parsed.Attachments), parsed.Attachments)
	}

	// The encoded-word filename has to be decoded, or the user sees the raw header.
	if parsed.Attachments[0].Filename != "报表.pdf" {
		t.Errorf("filename = %q, want 报表.pdf", parsed.Attachments[0].Filename)
	}
	// No disposition means it is stored as an attachment anyway.
	if parsed.Attachments[0].Disposition != "attachment" {
		t.Errorf("disposition = %q, want attachment", parsed.Attachments[0].Disposition)
	}
	// The CID is what an inline image's src resolves against, so the angle brackets
	// must be stripped for the lookup to match.
	if parsed.Attachments[1].ContentID != "logo@cid" {
		t.Errorf("content id = %q, want logo@cid without brackets", parsed.Attachments[1].ContentID)
	}
	if parsed.Attachments[1].Disposition != "inline" {
		t.Errorf("disposition = %q, want inline", parsed.Attachments[1].Disposition)
	}
	// Part ids address the part on a later fetch, so they must be distinct.
	if parsed.Attachments[0].PartID == parsed.Attachments[1].PartID {
		t.Error("two attachments share one part id")
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
	if len(parsed.Attachments) != 1 {
		t.Fatalf("want the attachment past the undecodable part, got %d", len(parsed.Attachments))
	}
	if parsed.Attachments[0].Filename != "report.pdf" {
		t.Errorf("attachment filename = %q", parsed.Attachments[0].Filename)
	}
	// The undecodable part is still offered as text rather than dropped: raw bytes beat
	// nothing, and the alternative is a message that looks empty.
	if parsed.Text == "" {
		t.Error("the undecodable part contributed no text at all")
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
