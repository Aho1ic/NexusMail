package mail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
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

// A bare LF terminates a header line for MTAs and most parsers just as CRLF does, so
// a value carrying one splits the header and injects whatever follows. The values that
// reach here now include In-Reply-To and References derived from remote message
// headers, so the filter has to live in Compose rather than in whichever caller
// happens to encode first.
func TestComposeStripsLineBreaksFromHeaderValues(t *testing.T) {
	raw, err := Compose(Outgoing{
		MessageID: "<new@example.com>",
		From:      mail.Address{Address: "me@example.com"},
		To:        []mail.Address{{Address: "you@example.com"}},
		Subject:   "injected",
		BodyText:  "body",
		InReplyTo: "<parent@example.com>\nBcc: victim@example.com",
		References: []string{
			"<root@example.com>\r\nX-Injected: one",
			"<parent@example.com>\nX-Injected: two",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	message := parseComposed(t, raw)
	for _, key := range []string{"Bcc", "X-Injected"} {
		if _, present := message.Header[key]; present {
			t.Fatalf("%s was injected through a header value:\n%s", key, raw)
		}
	}
	if got := message.Header.Get("In-Reply-To"); got != "<parent@example.com>Bcc: victim@example.com" {
		t.Errorf("In-Reply-To = %q", got)
	}

	// The attachment Content-Type is interpolated into a MIME part header, so it
	// carries the same exposure.
	withAttachment, err := Compose(Outgoing{
		From: mail.Address{Address: "me@example.com"}, To: []mail.Address{{Address: "you@example.com"}}, BodyText: "body",
		Attachments: []OutgoingAttachment{{
			Filename:    "notes.txt",
			ContentType: "text/plain\r\nX-Part-Injected: yes",
			Data:        strings.NewReader("payload"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The sanitised value still contains the text; what must not exist is a new
	// header line carrying it.
	if strings.Contains(string(withAttachment), "\nX-Part-Injected") {
		t.Fatalf("a part header was injected through Content-Type:\n%s", withAttachment)
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
// FormatMediaType emits RFC 2231 (`filename*=utf-8”…`) rather than an encoded-word
// inside a quoted parameter, which is what some older clients expect but is not
// a legal parameter value. The subject still travels as encoded-words.
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
	header := attachmentPartHeader(t, raw)
	_, params, err := mime.ParseMediaType(header.Get("Content-Disposition"))
	if err != nil {
		t.Fatalf("Content-Disposition does not parse: %v", err)
	}
	if params["filename"] != "季度报表.pdf" {
		t.Errorf("filename = %q, want 季度报表.pdf", params["filename"])
	}
	if !bytes.Contains(raw, []byte("filename*=")) && !bytes.Contains(raw, []byte("name*=")) {
		t.Error("the filename was not RFC 2231 encoded")
	}
	// The subject travels as encoded-words, and has to survive the round trip.
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

// assertHeaderLinesFit is the RFC 5321 §4.5.3.1.6 check applied to every physical
// line: an MTA is entitled to refuse a line over 998 octets outright, and several
// silently rewrite it instead, which surfaces to the user as a send that failed for
// no visible reason.
func assertHeaderLinesFit(t *testing.T, composed []byte) {
	t.Helper()
	headers, _, found := strings.Cut(string(composed), "\r\n\r\n")
	if !found {
		t.Fatalf("the composed message has no header/body separator")
	}
	for _, line := range strings.Split(headers, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("a %d-octet header line exceeds the 998-octet SMTP limit: %.120q...", len(line), line)
		}
	}
}

// TestComposeFoldsOverlongHeaders covers the three shapes a normal reply reaches the
// limit with. Unfolded, a 2000-character subject was a 2009-octet line, 40 recipients
// 1842 and a 30-deep References 1201 — all past what RFC 5321 lets an MTA accept.
// Folding is only correct if it also round-trips, so each case is read back and
// compared against the input.
func TestComposeFoldsOverlongHeaders(t *testing.T) {
	t.Run("long ascii subject", func(t *testing.T) {
		// No spaces at all: there is no fold point in the value, so this only fits
		// once it is rewritten as adjacent encoded-words.
		subject := strings.Repeat("A", 2000)
		composed, err := Compose(Outgoing{
			From: mail.Address{Address: "me@example.com"}, To: []mail.Address{{Address: "you@example.com"}},
			Subject: subject, BodyText: "body",
		})
		if err != nil {
			t.Fatal(err)
		}
		assertHeaderLinesFit(t, composed)
		got, err := new(mime.WordDecoder).DecodeHeader(parseComposed(t, composed).Header.Get("Subject"))
		if err != nil {
			t.Fatalf("decoding the folded subject: %v", err)
		}
		if got != subject {
			t.Errorf("subject round-tripped to %d characters, want %d", len(got), len(subject))
		}
	})

	t.Run("long cjk subject", func(t *testing.T) {
		subject := strings.Repeat("报", 100)
		composed, err := Compose(Outgoing{
			From: mail.Address{Address: "me@example.com"}, To: []mail.Address{{Address: "you@example.com"}},
			Subject: subject, BodyText: "body",
		})
		if err != nil {
			t.Fatal(err)
		}
		assertHeaderLinesFit(t, composed)
		got, err := new(mime.WordDecoder).DecodeHeader(parseComposed(t, composed).Header.Get("Subject"))
		if err != nil {
			t.Fatalf("decoding the folded subject: %v", err)
		}
		if got != subject {
			t.Errorf("subject = %q, want the 100 CJK characters back", got)
		}
	})

	t.Run("forty recipients", func(t *testing.T) {
		recipients := make([]mail.Address, 0, 40)
		for index := range 40 {
			recipients = append(recipients, mail.Address{Address: fmt.Sprintf("recipient%02d@example.com", index)})
		}
		composed, err := Compose(Outgoing{
			From: mail.Address{Address: "me@example.com"}, To: recipients, BodyText: "body",
		})
		if err != nil {
			t.Fatal(err)
		}
		assertHeaderLinesFit(t, composed)
		// Reading the list back is what proves the folds landed between addresses
		// rather than inside one.
		parsed, err := parseComposed(t, composed).Header.AddressList("To")
		if err != nil {
			t.Fatalf("the folded To header does not parse: %v", err)
		}
		if len(parsed) != len(recipients) {
			t.Fatalf("read back %d recipients, want %d", len(parsed), len(recipients))
		}
		for index, address := range parsed {
			if address.Address != recipients[index].Address {
				t.Errorf("recipient %d = %q, want %q", index, address.Address, recipients[index].Address)
			}
		}
	})

	t.Run("thirty references", func(t *testing.T) {
		// Provider Message-IDs are long — a random local part plus the provider's
		// own domain. Thirty of the short `<ancestor-00@mail.example.com>` shape
		// only reach 941 octets, which is under the limit and would not test
		// anything; this is the shape a real thirty-deep thread actually carries.
		references := make([]string, 0, 30)
		for index := range 30 {
			references = append(references, fmt.Sprintf("<%02d.1757%09d.JavaMail.nexus%%mail.example.com@mx-outbound-07.mail.example.com>", index, index))
		}
		composed, err := Compose(Outgoing{
			From: mail.Address{Address: "me@example.com"}, To: []mail.Address{{Address: "you@example.com"}},
			BodyText: "body", InReplyTo: references[len(references)-1], References: references,
		})
		if err != nil {
			t.Fatal(err)
		}
		assertHeaderLinesFit(t, composed)
		// References is the thread's ancestry, so both the tokens and their order
		// have to survive folding: net/mail unfolds, and the value must be identical.
		if got := parseComposed(t, composed).Header.Get("References"); got != strings.Join(references, " ") {
			t.Errorf("References = %q, want it unchanged by folding", got)
		}
	})

	// A short header must not be folded at all: a needless continuation line is a
	// difference every downstream signature and comparison would see.
	composed, err := Compose(Outgoing{
		From: mail.Address{Address: "me@example.com"}, To: []mail.Address{{Address: "you@example.com"}},
		Subject: "short subject", BodyText: "body",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(composed, []byte("Subject: short subject\r\n")) {
		t.Errorf("a short subject was rewritten:\n%s", composed)
	}
}

// TestFoldHeaderIsReversible pins the property the fold relies on: unfolding
// (CRLF + WSP collapsed to one SP) has to give back the original octets. Runs of
// more than one space are therefore not fold points — Go's own unfolder would
// collapse them and change the value.
func TestFoldHeaderIsReversible(t *testing.T) {
	for _, value := range []string{
		"To: " + strings.Repeat("a@example.com, ", 40) + "z@example.com",
		"Subject: " + strings.Repeat("word ", 400) + "end",
		"Subject: two  spaces  " + strings.Repeat("x", 200) + " tail",
		"References: " + strings.Repeat("<x@example.com> ", 30) + "<y@example.com>",
		"Subject: short",
	} {
		folded := foldHeader(value)
		if unfolded := unfold(folded); unfolded != value {
			t.Errorf("folding %.40q... did not round-trip:\n got %.80q\nwant %.80q", value, unfolded, value)
		}
		for _, line := range strings.Split(folded, "\r\n") {
			if len(line) > 998 {
				t.Errorf("a folded line is %d octets: %.80q...", len(line), line)
			}
		}
	}

	// A token with no fold point inside it is never cut: a broken Message-ID or
	// References token silently breaks threading, which is worse than a long line.
	// The fold after the field name is legal and reversible, so it is allowed —
	// what must survive is the token itself.
	unbreakable := "<" + strings.Repeat("a", 1200) + "@example.com>"
	folded := foldHeader("Message-ID: " + unbreakable)
	if unfolded := unfold(folded); unfolded != "Message-ID: "+unbreakable {
		t.Errorf("an unfoldable token did not round-trip: %.80q...", unfolded)
	}
	if !strings.Contains(folded, unbreakable) {
		t.Errorf("an unfoldable token was cut: %.120q...", folded)
	}
}

// unfold applies the RFC 5322 §2.2.3 rule: CRLF followed by WSP is removed, leaving
// the WSP. This is what a receiving parser does, so it is what the fold has to be
// reversible under.
func unfold(value string) string {
	return strings.NewReplacer("\r\n ", " ", "\r\n\t", "\t").Replace(value)
}

// TestComposeQuotesInAFilenameProduceOneParameter is the parameter-breakout case. A
// filename carrying `"; filename="evil.exe` used to close the quoted parameter and
// add a second filename=, and a client that takes the last one saves the attacker's
// name instead of the user's.
func TestComposeQuotesInAFilenameProduceOneParameter(t *testing.T) {
	composed, err := Compose(Outgoing{
		From: mail.Address{Address: "me@example.com"}, To: []mail.Address{{Address: "you@example.com"}},
		BodyText: "body",
		Attachments: []OutgoingAttachment{{
			Filename:    `a"; filename="evil.exe`,
			ContentType: "application/pdf",
			Data:        strings.NewReader("payload"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	header := attachmentPartHeader(t, composed)
	disposition := header.Get("Content-Disposition")
	// The broken output was `attachment; filename="a"; filename="evil.exe"`, which is
	// two parameters of the same name: ParseMediaType refuses it outright, and the
	// clients that accept it take the last one. So both the parse succeeding and the
	// single parameter carrying the user's literal name are the contract.
	mediaType, params, err := mime.ParseMediaType(disposition)
	if err != nil {
		t.Fatalf("Content-Disposition does not parse: %v (%q)", err, disposition)
	}
	if mediaType != "attachment" {
		t.Errorf("disposition = %q, want attachment", mediaType)
	}
	if len(params) != 1 || params["filename"] != `a"; filename="evil.exe` {
		t.Errorf("Content-Disposition parameters = %v, want filename only, holding the uploaded name", params)
	}
	contentType := header.Get("Content-Type")
	_, typeParams, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("Content-Type does not parse: %v (%q)", err, contentType)
	}
	if len(typeParams) != 1 || typeParams["name"] != `a"; filename="evil.exe` {
		t.Errorf("Content-Type parameters = %v, want name only, holding the uploaded name", typeParams)
	}
}

// TestComposeKeepsContentTypeParameters covers what switching to FormatMediaType
// could have cost: an upload's Content-Type arrives from the browser and may already
// carry a charset, and formatting it as if it were a bare media type would drop it
// and leave the part labelled application/octet-stream.
func TestComposeKeepsContentTypeParameters(t *testing.T) {
	composed, err := Compose(Outgoing{
		From: mail.Address{Address: "me@example.com"}, To: []mail.Address{{Address: "you@example.com"}},
		BodyText: "body",
		Attachments: []OutgoingAttachment{{
			Filename: "notes.txt", ContentType: "text/plain; charset=utf-8", Data: strings.NewReader("payload"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mediaType, params, err := mime.ParseMediaType(attachmentPartHeader(t, composed).Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	if mediaType != "text/plain" {
		t.Errorf("media type = %q, want text/plain", mediaType)
	}
	if params["charset"] != "utf-8" {
		t.Errorf("charset = %q, want it preserved", params["charset"])
	}
	if params["name"] != "notes.txt" {
		t.Errorf("name = %q", params["name"])
	}
}

// attachmentPartHeader returns the header of the first part that is not the text
// body, which is the attachment part Compose appends.
func attachmentPartHeader(t *testing.T, composed []byte) textproto.MIMEHeader {
	t.Helper()
	message := parseComposed(t, composed)
	_, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("Content-Type: %v", err)
	}
	reader := multipart.NewReader(message.Body, params["boundary"])
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			t.Fatal("the composed message has no attachment part")
		}
		if err != nil {
			t.Fatal(err)
		}
		if part.Header.Get("Content-Transfer-Encoding") == "base64" {
			return part.Header
		}
	}
}
