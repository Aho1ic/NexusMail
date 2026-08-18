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
)

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
	writeHeader(&output, "Subject", mime.QEncoding.Encode("UTF-8", input.Subject))
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
		contentType := attachment.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		encodedName := mime.QEncoding.Encode("UTF-8", filepath.Base(attachment.Filename))
		header.Set("Content-Type", fmt.Sprintf(`%s; name="%s"`, contentType, encodedName))
		header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, encodedName))
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

func writeHeader(writer io.Writer, key, value string) {
	if value != "" {
		_, _ = fmt.Fprintf(writer, "%s: %s\r\n", key, strings.ReplaceAll(value, "\r", ""))
	}
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
