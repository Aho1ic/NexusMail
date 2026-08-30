package mail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

// Compose builds the bytes that go on the wire, so what is asserted here is the wire
// format rather than the function's return: a header this gets wrong is a message that
// threads incorrectly, is rejected by the server, or discloses a recipient.

func parseComposed(t *testing.T, raw []byte) *mail.Message {
	t.Helper()
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the composed message does not parse: %v", err)
	}
	return message
}

func TestComposeRequiresASenderAndARecipient(t *testing.T) {
	sender := mail.Address{Name: "Me", Address: "me@example.com"}
	recipient := mail.Address{Address: "you@example.com"}

	for _, testCase := range []struct {
		name  string
		input Outgoing
	}{
		{"no sender", Outgoing{To: []mail.Address{recipient}}},
		{"no recipient at all", Outgoing{From: sender}},
		{"empty recipient slices", Outgoing{From: sender, To: []mail.Address{}, CC: []mail.Address{}, BCC: []mail.Address{}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := Compose(testCase.input); err == nil {
				t.Error("Compose accepted a message that cannot be delivered")
			}
		})
	}

	// A BCC-only message is deliverable, so it must not be refused: the recipient
	// count is what matters, not which field carries it.
	if _, err := Compose(Outgoing{From: sender, BCC: []mail.Address{recipient}, BodyText: "hi"}); err != nil {
		t.Errorf("Compose refused a BCC-only message: %v", err)
	}
}

// TestComposeCarriesReplyThreadingHeaders covers the two headers that make a reply a
// reply. Without In-Reply-To and References the message arrives as a new thread, which
// is the visible symptom of dropping them.
func TestComposeCarriesReplyThreadingHeaders(t *testing.T) {
	raw, err := Compose(Outgoing{
		MessageID:  "<new@example.com>",
		From:       mail.Address{Address: "me@example.com"},
		To:         []mail.Address{{Address: "you@example.com"}},
		Subject:    "Re: hello",
		BodyText:   "replying",
		InReplyTo:  "<parent@example.com>",
		References: []string{"<root@example.com>", "<parent@example.com>"},
	})
	if err != nil {
		t.Fatal(err)
	}
	message := parseComposed(t, raw)

	if got := message.Header.Get("In-Reply-To"); got != "<parent@example.com>" {
		t.Errorf("In-Reply-To = %q", got)
	}
	// References is a space-separated list in the order given: it is the thread's
	// ancestry, so the order is part of the meaning.
	if got := message.Header.Get("References"); got != "<root@example.com> <parent@example.com>" {
		t.Errorf("References = %q", got)
	}

	// And a message that is not a reply must not carry either header, rather than
	// carrying them empty.
	plain, err := Compose(Outgoing{
		From: mail.Address{Address: "me@example.com"}, To: []mail.Address{{Address: "you@example.com"}}, BodyText: "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	header := parseComposed(t, plain).Header
	if _, present := header["In-Reply-To"]; present {
		t.Error("a non-reply carries an In-Reply-To header")
	}
	if _, present := header["References"]; present {
		t.Error("a non-reply carries a References header")
	}
}

// TestComposeWrapsBase64AtSeventySixColumns is the one that matters most here. RFC 2045
// caps an encoded line at 76 characters and RFC 5321 refuses a line over 998 octets
// outright, so an unwrapped attachment is a message the server rejects or silently
// mangles. No test used an attachment larger than 57 bytes before this one — 57 bytes
// is exactly 76 base64 characters — so the wrapping branch had never run.
func TestComposeWrapsBase64AtSeventySixColumns(t *testing.T) {
	// Well past a single line, and not a multiple of 57, so the final short line is
	// exercised too.
	payload := bytes.Repeat([]byte{0xAB, 0x1F, 0x00, 0x7E}, 500)

	raw, err := Compose(Outgoing{
		From:        mail.Address{Address: "me@example.com"},
		To:          []mail.Address{{Address: "you@example.com"}},
		BodyText:    "see attached",
		Attachments: []OutgoingAttachment{{Filename: "blob.bin", ContentType: "application/octet-stream", Data: bytes.NewReader(payload)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The 76-column cap is RFC 2045's rule for encoded body lines; a header carrying
	// a multipart boundary is legitimately longer and is folded by different rules.
	// So the cap is checked against the encoded part, and the 998-octet limit that
	// RFC 5321 applies to every line is checked against the whole message.
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("a %d-octet line exceeds the 998-octet SMTP limit", len(line))
		}
	}
	for _, line := range strings.Split(encodedAttachmentBody(t, raw), "\r\n") {
		if len(line) > 76 {
			t.Fatalf("a %d-character encoded line exceeds the 76-column limit: %q", len(line), line)
		}
	}

	// Wrapping must not corrupt the payload: it comes back byte for byte.
	decoded := extractAttachment(t, raw)
	if !bytes.Equal(decoded, payload) {
		t.Errorf("the attachment decoded to %d bytes, want the original %d", len(decoded), len(payload))
	}
}

// encodedAttachmentBody returns the attachment part's body exactly as it sits on the
// wire, still base64. The multipart reader cannot be used for this: it decodes the
// transfer encoding, and the line breaks are the thing under test.
func encodedAttachmentBody(t *testing.T, raw []byte) string {
	t.Helper()
	message := parseComposed(t, raw)
	_, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("Content-Type: %v", err)
	}
	body, err := io.ReadAll(message.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range strings.Split(string(body), "--"+params["boundary"]) {
		headers, content, split := strings.Cut(section, "\r\n\r\n")
		if !split || !strings.Contains(headers, "base64") {
			continue
		}
		return strings.TrimSuffix(strings.TrimSpace(content), "--")
	}
	t.Fatal("no base64 part found in the composed message")
	return ""
}

// extractAttachment walks the composed multipart body and returns the decoded
// attachment, which is what proves the encoding round-trips rather than merely being
// short enough.
func extractAttachment(t *testing.T, raw []byte) []byte {
	t.Helper()
	message := parseComposed(t, raw)
	mediaType, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("Content-Type: %v", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		t.Fatalf("media type is %q, want multipart", mediaType)
	}
	reader := multipart.NewReader(message.Body, params["boundary"])
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			t.Fatal("no attachment part in the composed message")
		}
		if err != nil {
			t.Fatal(err)
		}
		if part.Header.Get("Content-Disposition") == "" {
			continue
		}
		if encoding := part.Header.Get("Content-Transfer-Encoding"); encoding != "base64" {
			t.Errorf("attachment encoding is %q, want base64", encoding)
		}
		decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
		if err != nil {
			t.Fatalf("decode attachment: %v", err)
		}
		return decoded
	}
}

func TestComposeDefaultsAMissingContentType(t *testing.T) {
	raw, err := Compose(Outgoing{
		From:        mail.Address{Address: "me@example.com"},
		To:          []mail.Address{{Address: "you@example.com"}},
		Attachments: []OutgoingAttachment{{Filename: "notes", Data: strings.NewReader("x")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A part with no type at all is treated as text by some clients, which turns a
	// binary attachment into mojibake rather than a download.
	if !bytes.Contains(raw, []byte("application/octet-stream")) {
		t.Error("an attachment with no content type did not fall back to application/octet-stream")
	}
}

// TestComposeStripsAPathFromTheFilename pins that only the base name travels. A
// filename carrying a path is what a receiving client would have to defend against
// when it saves the attachment.
func TestComposeStripsAPathFromTheFilename(t *testing.T) {
	raw, err := Compose(Outgoing{
		From:        mail.Address{Address: "me@example.com"},
		To:          []mail.Address{{Address: "you@example.com"}},
		Attachments: []OutgoingAttachment{{Filename: "../../../etc/passwd", ContentType: "text/plain", Data: strings.NewReader("x")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("../")) {
		t.Error("the composed message carries a relative path in the filename")
	}
	if !bytes.Contains(raw, []byte("passwd")) {
		t.Error("the base name was lost along with the path")
	}
}

// TestComposeEncodesANonASCIIFilename covers the header encoding. A raw UTF-8 filename
// in a header is a protocol violation, and the practical result is a garbled name.
func TestComposeEncodesANonASCIIFilename(t *testing.T) {
	raw, err := Compose(Outgoing{
		From:        mail.Address{Address: "me@example.com"},
		To:          []mail.Address{{Address: "you@example.com"}},
		Subject:     "报表",
		Attachments: []OutgoingAttachment{{Filename: "季度报表.pdf", ContentType: "application/pdf", Data: strings.NewReader("x")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("季度报表")) {
		t.Error("a non-ASCII filename was written to the header unencoded")
	}
	if !bytes.Contains(raw, []byte("=?utf-8?")) && !bytes.Contains(raw, []byte("=?UTF-8?")) {
		t.Error("the filename was not encoded-word encoded")
	}
	// The subject travels the same way, and has to survive the round trip.
	if got := parseComposed(t, raw).Header.Get("Subject"); got == "报表" {
		t.Error("the subject was written unencoded")
	} else if decoded, err := new(mime.WordDecoder).DecodeHeader(got); err != nil || decoded != "报表" {
		t.Errorf("the subject decoded to %q (%v), want 报表", decoded, err)
	}
}

// failingReader fails after handing over a prefix, which is the shape of a blob that
// disappears from storage midway through being read.
type failingReader struct {
	remaining int
	err       error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, r.err
	}
	n := min(len(p), r.remaining)
	for i := range n {
		p[i] = 'A'
	}
	r.remaining -= n
	return n, nil
}

// TestComposeReportsAnUnreadableAttachment pins that the read failure propagates. The
// alternative is a message sent with a truncated attachment, which the sender has no
// way to detect: it looks delivered.
func TestComposeReportsAnUnreadableAttachment(t *testing.T) {
	wanted := errors.New("blob evicted mid-read")
	_, err := Compose(Outgoing{
		From:        mail.Address{Address: "me@example.com"},
		To:          []mail.Address{{Address: "you@example.com"}},
		Attachments: []OutgoingAttachment{{Filename: "big.bin", Data: &failingReader{remaining: 200, err: wanted}}},
	})
	if err == nil {
		t.Fatal("Compose returned a message built from an attachment it could not read")
	}
	if !errors.Is(err, wanted) {
		t.Errorf("error is %v, want it to wrap %v", err, wanted)
	}
}

// The lineWriter is unit-tested directly for the cases Compose cannot reach: a
// destination that fails mid-write, and the exact column boundaries.
func TestLineWriterBoundaries(t *testing.T) {
	for _, size := range []int{0, 1, 75, 76, 77, 152, 153, 1000} {
		var out bytes.Buffer
		writer := &lineWriter{writer: &out}
		n, err := writer.Write(bytes.Repeat([]byte("x"), size))
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		// The count must be the input length, not the output length: the caller is
		// io.Copy, and a short count without an error makes it report ErrShortWrite.
		if n != size {
			t.Errorf("size %d: reported %d bytes written", size, n)
		}
		for _, line := range strings.Split(out.String(), "\r\n") {
			if len(line) > 76 {
				t.Errorf("size %d produced a %d-character line", size, len(line))
			}
		}
		// The payload itself is unchanged once the breaks are removed.
		if got := strings.ReplaceAll(out.String(), "\r\n", ""); len(got) != size {
			t.Errorf("size %d: %d payload characters survived", size, len(got))
		}
	}
}

// shortWriter accepts a fixed number of bytes and then fails, so both the mid-payload
// failure and the failure on the line break itself are reachable.
type shortWriter struct {
	allow int
	err   error
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if w.allow <= 0 {
		return 0, w.err
	}
	if len(p) > w.allow {
		n := w.allow
		w.allow = 0
		return n, w.err
	}
	w.allow -= len(p)
	return len(p), nil
}

func TestLineWriterPropagatesWriteFailures(t *testing.T) {
	wanted := errors.New("socket closed")

	// Failing partway through the first line: the error surfaces and the count is
	// what was actually accepted, which is what io.Copy needs to stop.
	writer := &lineWriter{writer: &shortWriter{allow: 10, err: wanted}}
	n, err := writer.Write(bytes.Repeat([]byte("x"), 40))
	if !errors.Is(err, wanted) {
		t.Errorf("error is %v, want %v", err, wanted)
	}
	if n != 10 {
		t.Errorf("reported %d bytes written, want the 10 that were accepted", n)
	}

	// Failing on the line break, once exactly one full line has been accepted. This
	// is the branch that writes "\r\n" rather than payload.
	breaking := &lineWriter{writer: &shortWriter{allow: 76, err: wanted}}
	if _, err := breaking.Write(bytes.Repeat([]byte("x"), 120)); !errors.Is(err, wanted) {
		t.Errorf("a failure on the line break returned %v, want %v", err, wanted)
	}
}
