package mail

import (
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const (
	maxOTPScanBytes = 16 * 1024
	// A code normally follows its keyword ("验证码：123456"), but a few templates
	// lead with it ("123456 is your verification code"), so both sides are scanned
	// with the trailing side given a much larger window. Provider HTML often
	// inserts a table of spacers between the phrase and the digits, so the
	// trailing window has to outrun a few dozen block-tag newlines.
	otpWindowAfter   = 200
	otpWindowBefore  = 64
	otpBeforePenalty = 100
)

// otpKeywords anchor the search. ASCII entries are matched on word boundaries so
// "pin code" cannot fire on "shipping" and "otp" cannot fire mid-word. Multi-word
// phrases allow any whitespace — including the newlines htmlToText inserts at
// block tags — between their words, so "Here is your<br>code" still matches.
//
// Bare "code" is deliberately absent: promo, coupon, discount and QR codes are
// far more common in mail than one-time passwords. Provider templates that only
// say "code" almost always qualify it ("your code", "reset code", "enter this
// code"); those phrases are listed instead.
var otpKeywords = []string{
	"验证码", "校验码", "验证代码", "动态密码", "动态口令", "短信码", "验证碼", "驗證碼",
	"登录码", "登陆码", "确认码", "安全码", "校验代码", "你的代码", "此代码", "该代码",
	"verification code", "verify code", "confirmation code", "security code",
	"login code", "access code", "auth code", "authentication code",
	"one-time password", "one time password", "one-time code", "one time code",
	"passcode", "pin code", "otp",
	// Microsoft, Adobe and similar identity mails lead with "Here is your code:"
	// and never say "verification code". "your code" is the smallest phrase that
	// catches those without also matching "discount code" / "promo code".
	"your code", "reset code", "recovery code",
	"sign-in code", "signin code", "sign in code",
	"enter this code", "enter the code", "enter code",
	"secret code", "temporary code", "backup code",
	"sms code", "email code", "2fa code", "totp",
}

// DetectOTP reports the one-time code a message carries, if any.
//
// Detection is keyword-anchored and never guesses from a bare digit run: order
// numbers, tracking numbers, dates and amounts all look like codes, and putting
// the wrong number in a notification the user copies blindly is worse than
// showing no notification at all.
func DetectOTP(subject, text, htmlBody string) (string, bool) {
	for _, segment := range otpSegments(subject, text, htmlBody) {
		if code, ok := detectOTPInSegment(segment); ok {
			return code, true
		}
	}
	return "", false
}

// otpSegments orders the corpus by trustworthiness. The HTML rendition is
// scanned before Parsed.Text because when a message has no text/plain part the
// parser fills Text with a tag-stripped copy (parser.go) that glues adjacent
// cells together, so "<td>123456</td><td>10 分钟</td>" arrives as "12345610" —
// an 8-digit run that would pass for a code.
func otpSegments(subject, text, htmlBody string) []string {
	segments := make([]string, 0, 3)
	for _, candidate := range []string{subject, htmlToText(htmlBody), text} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			segments = append(segments, truncateUTF8(trimmed, maxOTPScanBytes))
		}
	}
	return segments
}

func detectOTPInSegment(segment string) (string, bool) {
	runs := alnumRuns(segment)
	if len(runs) == 0 {
		return "", false
	}
	best, bestRank := "", 0
	for _, keyword := range otpKeywords {
		for _, match := range indexAllKeyword(segment, keyword) {
			for _, run := range runs {
				distance, near := keywordDistance(run, match.start, match.end)
				if !near {
					continue
				}
				score := otpScore(run, segment)
				if score == 0 {
					continue
				}
				// Shape dominates proximity: a 6-digit run 40 bytes away still beats
				// a 4-digit run sitting right next to the keyword.
				if rank := score*1000 - distance; rank > bestRank {
					best, bestRank = run.value, rank
				}
			}
		}
	}
	return best, bestRank > 0
}

func keywordDistance(run alnumRun, keywordStart, keywordEnd int) (int, bool) {
	if run.start >= keywordEnd {
		distance := run.start - keywordEnd
		return distance, distance <= otpWindowAfter
	}
	if run.end <= keywordStart {
		distance := keywordStart - run.end
		return distance + otpBeforePenalty, distance <= otpWindowBefore
	}
	// The run overlaps the keyword, so it is the keyword itself ("OTP") rather
	// than a code next to it.
	return 0, false
}

// otpScore ranks a candidate by shape, returning 0 for anything that cannot be a
// code. Six digits is by far the most common length, so it outranks every other
// shape regardless of how close a rival sits to the keyword.
func otpScore(run alnumRun, segment string) int {
	value := run.value
	if len(value) < 4 || len(value) > 8 {
		return 0
	}
	digits := 0
	for index := 0; index < len(value); index++ {
		if value[index] >= '0' && value[index] <= '9' {
			digits++
		}
	}
	if digits == 0 {
		return 0
	}
	if digits < len(value) {
		// Mixed alphanumeric codes are always printed uppercase; a lower-case mix
		// is a word with a digit stuck to it ("step2", "utf8", "h5page").
		if value != strings.ToUpper(value) || digits < 2 {
			return 0
		}
		return 50
	}
	if looksLikeAmountOrDate(run, segment) {
		return 0
	}
	switch len(value) {
	case 6:
		return 100
	case 4:
		return 85
	case 5:
		return 80
	case 8:
		return 70
	default:
		return 65
	}
}

// looksLikeAmountOrDate rejects digit runs whose neighbours mark them as prices,
// percentages, dates, times or dotted version numbers.
func looksLikeAmountOrDate(run alnumRun, segment string) bool {
	before := segment[max(0, run.start-6):run.start]
	after := segment[run.end:min(len(segment), run.end+6)]
	// Units and currency marks are commonly spaced away from the number, while a
	// date separator never is, so only the former tolerate whitespace.
	spacedBefore := strings.TrimRight(before, " \t ")
	spacedAfter := strings.TrimLeft(after, " \t ")
	for _, symbol := range []string{"¥", "￥", "$", "€", "£"} {
		if strings.HasSuffix(spacedBefore, symbol) {
			return true
		}
	}
	for _, unit := range []string{"年", "%", "％", "元"} {
		if strings.HasPrefix(spacedAfter, unit) {
			return true
		}
	}
	// A digit on the far side of a separator means this run is one field of a
	// larger number: 2024-08-20, 12:30:00, 1.234.567, 3/4.
	if len(after) >= 2 && strings.ContainsRune(".-/:", rune(after[0])) && after[1] >= '0' && after[1] <= '9' {
		return true
	}
	if length := len(before); length >= 2 && strings.ContainsRune(".-/:", rune(before[length-1])) && before[length-2] >= '0' && before[length-2] <= '9' {
		return true
	}
	return false
}

type alnumRun struct {
	value string
	start int
	end   int
}

func alnumRuns(input string) []alnumRun {
	var runs []alnumRun
	for offset := 0; offset < len(input); {
		if !isASCIIAlnum(input[offset]) {
			offset++
			continue
		}
		start := offset
		for offset < len(input) && isASCIIAlnum(input[offset]) {
			offset++
		}
		runs = append(runs, alnumRun{value: input[start:offset], start: start, end: offset})
	}
	return runs
}

type keywordSpan struct {
	start int
	end   int
}

// indexAllKeyword finds every case-insensitive occurrence of keyword while
// keeping offsets in the original string, and requires a non-alphanumeric
// neighbour on any ASCII edge so keywords cannot match inside a longer word.
// Multi-word keywords accept any Unicode whitespace between their words so a
// phrase split across HTML blocks still counts as one hit.
func indexAllKeyword(haystack, keyword string) []keywordSpan {
	parts := strings.Fields(keyword)
	if len(parts) == 0 {
		return nil
	}
	if len(parts) == 1 {
		return indexAllKeywordExact(haystack, parts[0])
	}
	return indexAllKeywordFlexible(haystack, parts)
}

func indexAllKeywordExact(haystack, keyword string) []keywordSpan {
	var positions []keywordSpan
	for offset := 0; offset+len(keyword) <= len(haystack); offset++ {
		end := offset + len(keyword)
		if !strings.EqualFold(haystack[offset:end], keyword) {
			continue
		}
		if !asciiKeywordBoundary(haystack, offset, end, keyword[0], keyword[len(keyword)-1]) {
			continue
		}
		positions = append(positions, keywordSpan{start: offset, end: end})
		offset = end - 1
	}
	return positions
}

func indexAllKeywordFlexible(haystack string, parts []string) []keywordSpan {
	var positions []keywordSpan
	first := parts[0]
	for offset := 0; offset+len(first) <= len(haystack); offset++ {
		if !strings.EqualFold(haystack[offset:offset+len(first)], first) {
			continue
		}
		if !asciiKeywordBoundaryLeft(haystack, offset, first[0]) {
			continue
		}
		end, ok := matchKeywordParts(haystack, offset, parts)
		if !ok {
			continue
		}
		positions = append(positions, keywordSpan{start: offset, end: end})
		offset = end - 1
	}
	return positions
}

func matchKeywordParts(haystack string, start int, parts []string) (int, bool) {
	cursor := start + len(parts[0])
	for _, part := range parts[1:] {
		gap := skipKeywordGap(haystack, cursor)
		if gap == cursor {
			return 0, false
		}
		if gap+len(part) > len(haystack) || !strings.EqualFold(haystack[gap:gap+len(part)], part) {
			return 0, false
		}
		cursor = gap + len(part)
	}
	if !asciiKeywordBoundaryRight(haystack, cursor, parts[len(parts)-1][len(parts[len(parts)-1])-1]) {
		return 0, false
	}
	return cursor, true
}

func skipKeywordGap(input string, offset int) int {
	cursor := offset
	for cursor < len(input) {
		r, size := utf8.DecodeRuneInString(input[cursor:])
		if !unicode.IsSpace(r) && r != '-' {
			break
		}
		cursor += size
	}
	return cursor
}

func asciiKeywordBoundary(haystack string, start, end int, first, last byte) bool {
	return asciiKeywordBoundaryLeft(haystack, start, first) && asciiKeywordBoundaryRight(haystack, end, last)
}

func asciiKeywordBoundaryLeft(haystack string, start int, first byte) bool {
	return start == 0 || !isASCIIAlnum(first) || !isASCIIAlnum(haystack[start-1])
}

func asciiKeywordBoundaryRight(haystack string, end int, last byte) bool {
	return end == len(haystack) || !isASCIIAlnum(last) || !isASCIIAlnum(haystack[end])
}

func isASCIIAlnum(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}

func truncateUTF8(input string, limit int) string {
	if len(input) <= limit {
		return input
	}
	cut := limit
	for cut > 0 && input[cut]&0xC0 == 0x80 {
		cut--
	}
	return input[:cut]
}

// htmlBlockTags are the elements whose boundaries become a newline, so digits
// from neighbouring cells or paragraphs never merge into one run.
var htmlBlockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true, "br": true,
	"caption": true, "dd": true, "div": true, "dl": true, "dt": true, "fieldset": true,
	"figure": true, "footer": true, "form": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "header": true, "hr": true, "li": true,
	"main": true, "nav": true, "ol": true, "p": true, "pre": true, "section": true,
	"span": true, "table": true, "tbody": true, "td": true, "tfoot": true, "th": true,
	"thead": true, "tr": true, "ul": true,
}

// htmlToText flattens HTML for code detection. bluemonday's StrictPolicy — used
// as the parser's text fallback — drops tags without inserting whitespace, so
// "<td>123456</td><td>10 分钟</td>" collapses into "12345610" and the real code
// is unrecoverable. Emitting a newline at every block boundary keeps the runs
// apart.
func htmlToText(input string) string {
	if input == "" {
		return ""
	}
	tokenizer := html.NewTokenizer(strings.NewReader(input))
	var output strings.Builder
	skipDepth := 0
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if !errors.Is(tokenizer.Err(), io.EOF) {
				// Malformed markup: whatever was flattened so far still beats nothing.
				return output.String()
			}
			return output.String()
		case html.TextToken:
			if skipDepth == 0 {
				output.Write(tokenizer.Text())
			}
		case html.StartTagToken:
			name, _ := tokenizer.TagName()
			tag := string(name)
			if tag == "script" || tag == "style" {
				skipDepth++
				continue
			}
			if htmlBlockTags[tag] {
				output.WriteByte('\n')
			}
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			tag := string(name)
			if tag == "script" || tag == "style" {
				if skipDepth > 0 {
					skipDepth--
				}
				continue
			}
			if htmlBlockTags[tag] {
				output.WriteByte('\n')
			}
		case html.SelfClosingTagToken:
			name, _ := tokenizer.TagName()
			if htmlBlockTags[string(name)] {
				output.WriteByte('\n')
			}
		}
	}
}
