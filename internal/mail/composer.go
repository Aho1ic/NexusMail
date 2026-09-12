package mail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// headerFoldOctets is the RFC 5322 §2.1.1 recommended line length, CRLF excluded.
// Folding here rather than at the 998-octet SMTP hard limit leaves room for an
// MTA to rewrite or re-encode a header without pushing it over the line that
// RFC 5321 §4.5.3.1.6 refuses.
const headerFoldOctets = 78

// headerLineHardOctets is the RFC 5321 §4.5.3.1.6 ceiling. A line that cannot
// be folded below it is still emitted, because dropping a Message-ID or a
// References token would silently break threading; that case is the caller's
// to avoid, not this writer's to invent a fold point for.
const headerLineHardOctets = 998

type OutgoingAttachment struct {
	Filename    string
	ContentType string
	Data        io.Reader
}

type Outgoing struct {
	MessageID   string
	From        mail.Address
	To          []mail.Address
	CC          []mail.Address
	BCC         []mail.Address
	Subject     string
	BodyText    string
	InReplyTo   string
	References  []string
	Attachments []OutgoingAttachment
}

func Compose(input Outgoing) ([]byte, error) {
	if input.From.Address == "" || len(input.To)+len(input.CC)+len(input.BCC) == 0 {
		return nil, errors.New("sender and at least one recipient are required")
	}
	var output bytes.Buffer
	writeHeader(&output, "Date", time.Now().Format(time.RFC1123Z))
	writeHeader(&output, "Message-ID", input.MessageID)
	writeHeader(&output, "From", input.From.String())
	writeHeader(&output, "To", joinAddresses(input.To))
	if len(input.CC) > 0 {
		writeHeader(&output, "Cc", joinAddresses(input.CC))
	}
	writeHeader(&output, "Subject", encodeUnstructured(input.Subject))
	writeHeader(&output, "MIME-Version", "1.0")
	if input.InReplyTo != "" {
		writeHeader(&output, "In-Reply-To", input.InReplyTo)
	}
	if len(input.References) > 0 {
		writeHeader(&output, "References", strings.Join(input.References, " "))
	}
	if len(input.Attachments) == 0 {
		writeHeader(&output, "Content-Type", `text/plain; charset="UTF-8"`)
		writeHeader(&output, "Content-Transfer-Encoding", "quoted-printable")
		output.WriteString("\r\n")
		writeQuotedPrintable(&output, input.BodyText)
		return output.Bytes(), nil
	}

	multipartWriter := multipart.NewWriter(&output)
	writeHeader(&output, "Content-Type", fmt.Sprintf(`multipart/mixed; boundary="%s"`, multipartWriter.Boundary()))
	output.WriteString("\r\n")
	textHeader := textproto.MIMEHeader{}
	textHeader.Set("Content-Type", `text/plain; charset="UTF-8"`)
	textHeader.Set("Content-Transfer-Encoding", "quoted-printable")
	part, err := multipartWriter.CreatePart(textHeader)
	if err != nil {
		return nil, err
	}
	writeQuotedPrintable(part, input.BodyText)
	for _, attachment := range input.Attachments {
		header := textproto.MIMEHeader{}
		filename := filepath.Base(attachment.Filename)
		header.Set("Content-Type", formatPartType(attachment.ContentType, filename))
		header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
		header.Set("Content-Transfer-Encoding", "base64")
		part, err := multipartWriter.CreatePart(header)
		if err != nil {
			return nil, err
		}
		encoder := base64.NewEncoder(base64.StdEncoding, &lineWriter{writer: part})
		if _, err := io.Copy(encoder, attachment.Data); err != nil {
			_ = encoder.Close()
			return nil, err
		}
		if err := encoder.Close(); err != nil {
			return nil, err
		}
	}
	if err := multipartWriter.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// headerSanitizer removes both CR and LF from a header value. Stripping CR alone
// was not enough: MTAs and most parsers accept a bare LF as a line terminator, so
// an embedded "\n" splits the header exactly as CRLF would and injects whatever
// follows it. The guarantee has to be made here rather than inherited from the
// callers that happen to Q-encode or otherwise neutralise their values — reply
// threading feeds In-Reply-To and References straight from remote message
// headers, which is precisely the untrusted case.
var headerSanitizer = strings.NewReplacer("\r", "", "\n", "")

func writeHeader(writer io.Writer, key, value string) {
	if value == "" {
		return
	}
	_, _ = io.WriteString(writer, foldHeader(key+": "+headerSanitizer.Replace(value)))
	_, _ = io.WriteString(writer, "\r\n")
}

// foldHeader inserts CRLF + SP at RFC 5322 FWS so no physical line exceeds
// headerFoldOctets when a legal break exists. A break is a single SP with a
// non-space on both sides: that is the only transformation that unfolding
// (CRLF + WSP → SP) reconstitutes byte-for-byte. Encoded-words are never cut
// internally because they contain no such space; they fold at the SP that
// already separates adjacent words. Address lists fold at the ", " that
// joinAddresses emits.
func foldHeader(line string) string {
	if len(line) <= headerFoldOctets {
		return line
	}
	var output strings.Builder
	output.Grow(len(line) + 16)
	lineStart := 0
	lastBreak := -1
	for index := range line {
		if index-lineStart >= headerFoldOctets && lastBreak > lineStart {
			output.WriteString(line[lineStart:lastBreak])
			output.WriteString("\r\n")
			lineStart = lastBreak
			lastBreak = -1
		}
		if isFoldPoint(line, index) {
			lastBreak = index
		}
	}
	output.WriteString(line[lineStart:])
	return output.String()
}

func isFoldPoint(line string, index int) bool {
	return index > 0 && index+1 < len(line) && line[index] == ' ' && line[index-1] != ' ' && line[index+1] != ' '
}

// encodeUnstructured Q-encodes a Subject (or any unstructured field). Go's
// WordEncoder leaves a long ASCII run alone, so a 2000-character subject with
// no spaces is one unfoldable token past headerLineHardOctets. Those are
// rewritten as adjacent encoded-words, which foldHeader can then break between.
func encodeUnstructured(value string) string {
	encoded := mime.QEncoding.Encode("UTF-8", value)
	if longestUnfoldableRun(encoded) <= headerLineHardOctets-len("Subject: ") {
		return encoded
	}
	return encodedWords(value)
}

func longestUnfoldableRun(value string) int {
	longest, last := 0, 0
	for index := range value {
		if isFoldPoint(value, index) {
			if run := index - last; run > longest {
				longest = run
			}
			last = index + 1
		}
	}
	if run := len(value) - last; run > longest {
		return run
	}
	return longest
}

// encodedWords emits RFC 2047 B-encoded words even for ASCII. Each word is
// capped at 75 octets (RFC 2047 §2) by packing at most 45 raw bytes, which
// base64-encode to 60 and leave room for the `=?UTF-8?B?...?=` wrapper. Cuts
// fall on rune boundaries so a decoder never sees a torn character.
func encodedWords(value string) string {
	const maxRaw = 45
	var words []string
	for len(value) > 0 {
		cut := maxRaw
		if cut >= len(value) {
			words = append(words, encodedWord(value))
			break
		}
		// Back up to the start of the rune that straddles the cut. A rune is at
		// most 4 octets, so this cannot walk the cut to zero on valid UTF-8; on
		// invalid input it can, and the whole remainder goes in one word rather
		// than looping forever.
		for cut > 0 && !utf8.RuneStart(value[cut]) {
			cut--
		}
		if cut == 0 {
			words = append(words, encodedWord(value))
			break
		}
		words = append(words, encodedWord(value[:cut]))
		value = value[cut:]
	}
	return strings.Join(words, " ")
}

func encodedWord(raw string) string {
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(raw)) + "?="
}

// formatPartType builds a Content-Type with a name parameter via
// mime.FormatMediaType so a filename carrying a quote cannot break out of the
// parameter and inject a second filename= that some clients honour over the
// first. Incoming types may already carry parameters (a browser's
// "text/plain; charset=utf-8"), so they are parsed rather than treated as a
// bare type; FormatMediaType rejects a type with a semicolon in it. A type
// that cannot be represented falls back to application/octet-stream so a
// binary attachment is never labelled as text.
func formatPartType(contentType, filename string) string {
	contentType = headerSanitizer.Replace(contentType)
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType == "" {
		mediaType = "application/octet-stream"
		params = map[string]string{}
	}
	if params == nil {
		params = map[string]string{}
	}
	params["name"] = filename
	formatted := mime.FormatMediaType(mediaType, params)
	if formatted == "" {
		return mime.FormatMediaType("application/octet-stream", map[string]string{"name": filename})
	}
	return formatted
}

func writeQuotedPrintable(writer io.Writer, value string) {
	encoder := quotedprintable.NewWriter(writer)
	_, _ = io.WriteString(encoder, strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n"))
	_ = encoder.Close()
}

func joinAddresses(addresses []mail.Address) string {
	values := make([]string, 0, len(addresses))
	for _, address := range addresses {
		values = append(values, address.String())
	}
	return strings.Join(values, ", ")
}

type lineWriter struct {
	writer io.Writer
	column int
}

func (w *lineWriter) Write(input []byte) (int, error) {
	written := 0
	for len(input) > 0 {
		remaining := 76 - w.column
		if remaining == 0 {
			if _, err := io.WriteString(w.writer, "\r\n"); err != nil {
				return written, err
			}
			w.column = 0
			remaining = 76
		}
		chunk := len(input)
		if chunk > remaining {
			chunk = remaining
		}
		n, err := w.writer.Write(input[:chunk])
		written += n
		w.column += n
		input = input[n:]
		if err != nil {
			return written, err
		}
	}
	return written, nil
}
