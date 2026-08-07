package httpserver

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

func plainTextFromHTML(source string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(source))
	// The MIME parser already bounds source. Keep the tokenizer and output from
	// amplifying that configured limit when processing untrusted email HTML.
	tokenizer.SetMaxBuf(len(source) + 1)

	text := htmlTextWriter{maxBytes: len(source)}
	text.output.Grow(min(len(source), 64<<10))
	var skippedTag string
	skippedTagDepth := 0

	for {
		tokenType := tokenizer.Next()
		switch tokenType {
		case html.ErrorToken:
			return text.String()
		case html.TextToken:
			if skippedTagDepth == 0 && !text.write(tokenizer.Text()) {
				return text.String()
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttributes := tokenizer.TagName()
			tag := string(name)

			// Email is rendered without scripting. Parse noscript children as HTML
			// so their tags cannot leak into the plain-text result.
			if tag == "noscript" {
				tokenizer.NextIsNotRawText()
			}

			if skippedTagDepth > 0 {
				if skippedTag == "head" && tag == "body" {
					skippedTag = ""
					skippedTagDepth = 0
				} else {
					if tag == skippedTag && !isVoidHTMLElement(tag) {
						skippedTagDepth++
					}
					continue
				}
			}

			hidden := htmlElementIsHidden(tokenizer, hasAttributes)
			if isDiscardedHTMLElement(tag) || hidden {
				// In HTML, a self-closing flag is ignored on non-void elements.
				if !isVoidHTMLElement(tag) {
					skippedTag = tag
					skippedTagDepth = 1
				}
				continue
			}
			if tag == "br" {
				text.lineBreak()
				continue
			}
			if tag == "td" || tag == "th" {
				text.space()
			}
			if isBlockHTMLElement(tag) {
				text.lineBreak()
			}
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			tag := string(name)
			if skippedTagDepth > 0 {
				if tag == skippedTag {
					skippedTagDepth--
					if skippedTagDepth == 0 {
						skippedTag = ""
					}
				}
				continue
			}
			if tag == "td" || tag == "th" {
				text.space()
			}
			if isBlockHTMLElement(tag) {
				text.lineBreak()
			}
		}
	}
}

func htmlElementIsHidden(tokenizer *html.Tokenizer, hasAttributes bool) bool {
	hidden := false
	for hasAttributes {
		key, value, moreAttributes := tokenizer.TagAttr()
		switch string(key) {
		case "hidden":
			hidden = true
		case "aria-hidden":
			hidden = hidden || strings.EqualFold(strings.TrimSpace(string(value)), "true")
		case "style":
			hidden = hidden || inlineStyleHidesContent(string(value))
		}
		hasAttributes = moreAttributes
	}
	return hidden
}

func inlineStyleHidesContent(style string) bool {
	for declaration := range strings.SplitSeq(style, ";") {
		property, value, found := strings.Cut(declaration, ":")
		if !found {
			continue
		}
		property = strings.ToLower(strings.TrimSpace(property))
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.TrimSpace(strings.TrimSuffix(value, "!important"))
		switch property {
		case "display":
			if value == "none" {
				return true
			}
		case "visibility":
			if value == "hidden" || value == "collapse" {
				return true
			}
		case "content-visibility":
			if value == "hidden" {
				return true
			}
		case "mso-hide":
			if value == "all" {
				return true
			}
		case "opacity":
			if value == "0" {
				return true
			}
		}
	}
	return false
}

func isDiscardedHTMLElement(name string) bool {
	switch name {
	case "head", "iframe", "noembed", "noframes", "plaintext", "script", "style", "template",
		"textarea", "title", "xmp":
		return true
	default:
		return false
	}
}

func isVoidHTMLElement(name string) bool {
	switch name {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta",
		"param", "source", "track", "wbr":
		return true
	default:
		return false
	}
}

func isBlockHTMLElement(name string) bool {
	switch name {
	case "address", "article", "aside", "blockquote", "body", "caption", "center", "dd",
		"details", "dialog", "dir", "div", "dl", "dt", "fieldset", "figcaption", "figure",
		"footer", "form", "h1", "h2", "h3", "h4", "h5", "h6", "header", "hgroup", "hr",
		"html", "legend", "li", "main", "menu", "nav", "ol", "optgroup", "option", "p",
		"pre", "section", "summary", "table", "tbody", "tfoot", "thead", "tr", "ul":
		return true
	default:
		return false
	}
}

type htmlTextWriter struct {
	output           strings.Builder
	maxBytes         int
	pendingSpace     bool
	pendingLineBreak bool
}

func (w *htmlTextWriter) write(value []byte) bool {
	for len(value) > 0 {
		character, size := utf8.DecodeRune(value)
		value = value[size:]
		if unicode.IsSpace(character) {
			w.pendingSpace = true
			continue
		}

		separatorBytes := 0
		if w.pendingLineBreak || (w.pendingSpace && w.output.Len() > 0) {
			separatorBytes = 1
		}
		characterBytes := utf8.RuneLen(character)
		if characterBytes < 0 || w.output.Len()+separatorBytes+characterBytes > w.maxBytes {
			return false
		}
		w.flushSeparator()
		w.output.WriteRune(character)
	}
	return true
}

func (w *htmlTextWriter) space() {
	if w.output.Len() > 0 && !w.pendingLineBreak {
		w.pendingSpace = true
	}
}

func (w *htmlTextWriter) lineBreak() {
	if w.output.Len() > 0 {
		w.pendingLineBreak = true
		w.pendingSpace = false
	}
}

func (w *htmlTextWriter) flushSeparator() {
	if w.pendingLineBreak {
		w.output.WriteByte('\n')
	} else if w.pendingSpace && w.output.Len() > 0 {
		w.output.WriteByte(' ')
	}
	w.pendingLineBreak = false
	w.pendingSpace = false
}

func (w *htmlTextWriter) String() string {
	return w.output.String()
}
