package mail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	messagecharset "github.com/emersion/go-message/charset"
	message "github.com/emersion/go-message/mail"
	"github.com/microcosm-cc/bluemonday"
	"github.com/microcosm-cc/bluemonday/css"
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
	policy.AllowURLSchemes("cid")
	allowPresentationalLayout(policy)
	// Inline attachments arrive as data URIs, so the scheme cannot simply be dropped,
	// but allowing it outright also allows data:text/html on a link — script execution
	// the rest of the policy does not cover. bluemonday's own AllowDataURIImages is
	// not used because its allowlist includes svg+xml, and SVG carries script.
	policy.RequireParseableURLs(true)
	policy.AllowURLSchemeWithCustomPolicy("data", allowInlineRasterImage)
	policy.RequireNoFollowOnLinks(true)
	policy.RequireNoReferrerOnLinks(true)
	return blockRemoteImages(policy.Sanitize(input))
}

// layoutStyleProperties is the CSS allowlist for the style attribute. Mail is
// laid out almost entirely with nested tables plus inline style, so dropping the
// attribute outright collapsed every such message into unstyled rows with no
// padding, spacing or colour. Only typography, colour and box metrics are listed:
// position/z-index/transform would let a message escape its own flow, and every
// property whose value can hold url() is excluded so a style declaration cannot
// re-open the remote fetch blockRemoteImages exists to prevent.
var layoutStyleProperties = []string{
	"color", "background", "background-color", "font", "font-family", "font-size", "font-style",
	"font-weight", "font-variant", "letter-spacing", "line-height", "text-align",
	"text-decoration", "text-indent", "text-transform", "vertical-align",
	"white-space", "word-break", "word-spacing", "overflow-wrap", "word-wrap",
	"direction", "unicode-bidi",
	"margin", "margin-top", "margin-right", "margin-bottom", "margin-left",
	"padding", "padding-top", "padding-right", "padding-bottom", "padding-left",
	"width", "min-width", "max-width", "height", "min-height", "max-height",
	"border", "border-top", "border-right", "border-bottom", "border-left",
	"border-color", "border-style", "border-width", "border-radius",
	"border-top-color", "border-right-color", "border-bottom-color", "border-left-color",
	"border-top-style", "border-right-style", "border-bottom-style", "border-left-style",
	"border-top-width", "border-right-width", "border-bottom-width", "border-left-width",
	"border-collapse", "border-spacing", "caption-side", "empty-cells", "table-layout",
	"display", "float", "clear", "box-sizing", "opacity", "list-style-type",
	"list-style-position", "text-overflow",
}

// presentationalTableAttrs restores the pre-CSS table attributes. Mail written for
// broad client support carries the layout twice — once as style, once as these
// attributes — so keeping only one half still renders wrong in the other clients'
// dialect. Values stay bound to bluemonday's own numeric/enum matchers.
func allowPresentationalLayout(policy *bluemonday.Policy) {
	policy.AllowAttrs("cellpadding", "cellspacing", "border").Matching(bluemonday.Integer).OnElements("table")
	policy.AllowAttrs("align").Matching(bluemonday.CellAlign).OnElements("table")
	policy.AllowAttrs("bgcolor").Matching(cssColorValue).OnElements("table", "thead", "tbody", "tfoot", "tr", "td", "th")
	policy.AllowAttrs("width", "height").Matching(bluemonday.NumberOrPercent).OnElements("img", "table", "td", "th")
	for _, property := range layoutStyleProperties {
		handler := css.GetDefaultHandler(property)
		policy.AllowStyles(property).MatchingHandler(func(value string) bool {
			// bluemonday lowercases the value and expands CSS unicode escapes before
			// the handler runs, so a literal check catches url\28 as well as url(.
			// The shorthand handlers for border and background accept url() by way of
			// their image sub-handler; rejecting it here keeps the image blocker whole.
			if strings.Contains(value, "url(") {
				return false
			}
			return handler(value)
		}).Globally()
	}
}

// cssColorValue bounds bgcolor to a colour. bluemonday has no exported matcher for
// it, and Paragraph — the closest one — would also admit arbitrary prose.
var cssColorValue = regexp.MustCompile(`(?i)^(#[0-9a-f]{3,8}|[a-z]+|rgba?\([0-9%,.\s]+\)|hsla?\([0-9%,.\s]+\))$`)

// inlineRasterImage matches the data URI payloads an embedded image legitimately
// uses. Raster only and base64 only: SVG is a document format that can carry script,
// and a non-base64 payload is a text body wearing an image label. The match is
// case-insensitive because RFC 2397 and RFC 2045 both define these halves that way,
// and a sender that capitalises either is still sending a valid inline image. It
// widens nothing: no capitalisation of svg+xml or text/html is in the alternation.
var inlineRasterImage = regexp.MustCompile(`(?i)^image/(gif|jpeg|png|webp);base64,`)

func allowInlineRasterImage(target *url.URL) bool {
	if target.RawQuery != "" || target.Fragment != "" {
		return false
	}
	prefix := inlineRasterImage.FindString(target.Opaque)
	if prefix == "" {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(target.Opaque[len(prefix):])
	return err == nil
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
