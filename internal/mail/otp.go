package mail

import (
	"html"
	"strings"
	"unicode"
	"unicode/utf8"

	xhtml "golang.org/x/net/html"
)

var otpKeywords = []string{
	"verification code", "security code", "one-time", "one time", "otp", "verify", "code",
	"验证码", "校验码", "动态码", "安全码", "一次性密码",
}

type otpCandidate struct {
	value string
	score int
	order int
}

// ExtractOTP returns one bounded numeric code. A candidate must contain four
// through eight ASCII digits and must not touch another ASCII letter or digit.
func ExtractOTP(subject, textBody, htmlBody string) string {
	sources := []string{subject, textBody}
	if strings.TrimSpace(htmlBody) != "" {
		plain := readableHTMLText(htmlBody)
		sources = append(sources, plain)
	}
	best := otpCandidate{score: -1}
	order := 0
	for sourceIndex, source := range sources {
		lower := strings.ToLower(source)
		for index := 0; index < len(source); {
			if source[index] < '0' || source[index] > '9' {
				_, size := utf8.DecodeRuneInString(source[index:])
				if size < 1 {
					size = 1
				}
				index += size
				continue
			}
			start := index
			for index < len(source) && source[index] >= '0' && source[index] <= '9' {
				index++
			}
			length := index - start
			if length < 4 || length > 8 || adjacentLetterOrNumber(source, start, index) {
				continue
			}
			windowStart := start - 64
			if windowStart < 0 {
				windowStart = 0
			}
			windowEnd := index + 64
			if windowEnd > len(lower) {
				windowEnd = len(lower)
			}
			score := 100 - sourceIndex
			window := lower[windowStart:windowEnd]
			if distance, found := nearestOTPKeyword(window, start-windowStart, index-windowStart); found {
				score += 10_000 - min(distance, 9_000)
			}
			candidate := otpCandidate{value: source[start:index], score: score, order: order}
			order++
			if candidate.score > best.score || candidate.score == best.score && candidate.order < best.order {
				best = candidate
			}
		}
	}
	return best.value
}

func adjacentLetterOrNumber(source string, start, end int) bool {
	if start > 0 {
		previous, _ := utf8.DecodeLastRuneInString(source[:start])
		if unicode.IsLetter(previous) || unicode.IsNumber(previous) {
			return true
		}
	}
	if end < len(source) {
		next, _ := utf8.DecodeRuneInString(source[end:])
		if unicode.IsLetter(next) || unicode.IsNumber(next) {
			return true
		}
	}
	return false
}

func nearestOTPKeyword(window string, candidateStart, candidateEnd int) (int, bool) {
	best := 0
	found := false
	for _, keyword := range otpKeywords {
		for offset := 0; offset <= len(window); {
			index := strings.Index(window[offset:], keyword)
			if index < 0 {
				break
			}
			start := offset + index
			end := start + len(keyword)
			distance := 0
			switch {
			case end < candidateStart:
				distance = candidateStart - end
			case start > candidateEnd:
				distance = start - candidateEnd
			}
			if !found || distance < best {
				best, found = distance, true
			}
			offset = start + 1
		}
	}
	return best, found
}

func readableHTMLText(source string) string {
	tokenizer := xhtml.NewTokenizer(strings.NewReader(source))
	var result strings.Builder
	ignoredDepth := 0
	for {
		typeValue := tokenizer.Next()
		switch typeValue {
		case xhtml.ErrorToken:
			return strings.Join(strings.Fields(html.UnescapeString(result.String())), " ")
		case xhtml.StartTagToken:
			name, _ := tokenizer.TagName()
			switch strings.ToLower(string(name)) {
			case "script", "style", "head", "template", "svg", "canvas":
				ignoredDepth++
			}
		case xhtml.EndTagToken:
			name, _ := tokenizer.TagName()
			switch strings.ToLower(string(name)) {
			case "script", "style", "head", "template", "svg", "canvas":
				if ignoredDepth > 0 {
					ignoredDepth--
				}
			}
		case xhtml.TextToken:
			if ignoredDepth == 0 {
				result.WriteByte(' ')
				result.Write(tokenizer.Text())
			}
		}
	}
}
