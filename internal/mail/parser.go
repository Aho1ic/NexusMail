package mail

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"strings"
	"unicode/utf8"

	messagecharset "github.com/emersion/go-message/charset"
	message "github.com/emersion/go-message/mail"
	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
)

const maxParsedPartBytes = 4 << 20

type AttachmentMeta struct {
	PartID      string
	Filename    string
	ContentType string
	Disposition string
	ContentID   string
	SizeBytes   int64
}

type Parsed struct {
	Subject     string
	From        []*mail.Address
	To          []*mail.Address
	CC          []*mail.Address
	BCC         []*mail.Address
	MessageID   string
	InReplyTo   string
	References  []string
	Text        string
	HTML        string
	Snippet     string
	Attachments []AttachmentMeta
}

func Parse(reader io.Reader) (Parsed, error) {
	entity, err := message.CreateReader(reader)
	if err != nil {
		return Parsed{}, fmt.Errorf("create MIME reader: %w", err)
	}
	result := Parsed{
		Subject:    decodeHeader(entity.Header.Get("Subject")),
		MessageID:  strings.TrimSpace(entity.Header.Get("Message-Id")),
		InReplyTo:  strings.TrimSpace(entity.Header.Get("In-Reply-To")),
		References: strings.Fields(entity.Header.Get("References")),
	}
	result.From, _ = parseAddressList(entity.Header.Get("From"))
	result.To, _ = parseAddressList(entity.Header.Get("To"))
	result.CC, _ = parseAddressList(entity.Header.Get("Cc"))
	result.BCC, _ = parseAddressList(entity.Header.Get("Bcc"))

	partIndex := 0
	for {
		part, err := entity.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, fmt.Errorf("read MIME part: %w", err)
		}
		partIndex++
		contentType, params, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		disposition, dispositionParams, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		filename := decodeHeader(dispositionParams["filename"])
		if filename == "" {
			filename = decodeHeader(params["name"])
		}
		if disposition == "attachment" || filename != "" {
			result.Attachments = append(result.Attachments, AttachmentMeta{
				PartID: fmt.Sprintf("%d", partIndex), Filename: filename, ContentType: contentType,
				Disposition: choose(disposition, "attachment"), ContentID: strings.Trim(part.Header.Get("Content-Id"), "<>"),
			})
			continue
		}
		limited := io.LimitReader(part.Body, maxParsedPartBytes+1)
		body, readErr := io.ReadAll(limited)
		if readErr != nil || len(body) > maxParsedPartBytes {
			continue
		}
		if charsetName := params["charset"]; charsetName != "" && !strings.EqualFold(charsetName, "utf-8") && !strings.EqualFold(charsetName, "us-ascii") {
			converted, convertErr := io.ReadAll(mustCharsetReader(charsetName, bytes.NewReader(body)))
			if convertErr == nil {
				body = converted
			}
		}
		switch strings.ToLower(contentType) {
		case "text/plain", "":
			if result.Text == "" {
				result.Text = normalizeText(string(body))
			}
		case "text/html":
			if result.HTML == "" {
				result.HTML = sanitizeHTML(string(body))
			}
		}
	}
	if result.Text == "" && result.HTML != "" {
		result.Text = strings.TrimSpace(bluemonday.StrictPolicy().Sanitize(result.HTML))
	}
	result.Snippet = snippet(result.Text, 240)
	return result, nil
}

func sanitizeHTML(input string) string {
	policy := bluemonday.UGCPolicy()
	policy.AllowAttrs("class").OnElements("pre", "code")
	policy.AllowURLSchemes("cid", "data")
	policy.RequireNoFollowOnLinks(true)
	policy.RequireNoReferrerOnLinks(true)
	return blockRemoteImages(policy.Sanitize(input))
}

// blockRemoteImages keeps the sanitized URL available for an explicit UI opt-in
// without allowing the initial render to leak the user's IP or tracking token.
func blockRemoteImages(input string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(input))
	var output strings.Builder
	for {
		tokenType := tokenizer.Next()
		if tokenType == html.ErrorToken {
			if errors.Is(tokenizer.Err(), io.EOF) {
				return output.String()
			}
			return input
		}
		token := tokenizer.Token()
		if (tokenType == html.StartTagToken || tokenType == html.SelfClosingTagToken) && (token.Data == "img" || token.Data == "source") {
			attrs := token.Attr[:0]
			for _, attr := range token.Attr {
				if attr.Key == "srcset" {
					continue
				}
				if attr.Key == "src" && isRemoteURL(attr.Val) {
					attr.Key = "data-nexusmail-remote-src"
				}
				attrs = append(attrs, attr)
			}
			token.Attr = attrs
		}
		output.WriteString(token.String())
	}
}

func isRemoteURL(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "//")
}

func decodeHeader(input string) string {
	decoder := &mime.WordDecoder{CharsetReader: messagecharset.Reader}
	decoded, err := decoder.DecodeHeader(input)
	if err != nil {
		return input
	}
	return decoded
}

func mustCharsetReader(charsetName string, input io.Reader) io.Reader {
	reader, err := messagecharset.Reader(charsetName, input)
	if err != nil {
		return input
	}
	return reader
}

func parseAddressList(input string) ([]*mail.Address, error) {
	if strings.TrimSpace(input) == "" {
		return nil, nil
	}
	parser := &mail.AddressParser{WordDecoder: &mime.WordDecoder{CharsetReader: messagecharset.Reader}}
	return parser.ParseList(input)
}

func normalizeText(input string) string {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	input = strings.ReplaceAll(input, "\r", "\n")
	return strings.TrimSpace(input)
}

func snippet(input string, maxRunes int) string {
	input = strings.Join(strings.Fields(input), " ")
	if utf8.RuneCountInString(input) <= maxRunes {
		return input
	}
	runes := []rune(input)
	return string(runes[:maxRunes]) + "…"
}

func choose(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
