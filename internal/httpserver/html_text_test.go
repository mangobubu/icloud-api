package httpserver

import (
	"strings"
	"testing"
)

func TestPlainTextFromHTML(t *testing.T) {
	tests := []struct {
		name   string
		html   string
		wanted string
	}{
		{
			name: "preserves readable structure",
			html: `<center>Hello <strong>world</strong></center>
				<p>Code<br>123456</p>
				<table><tr><td>Column A</td><td>Column B</td></tr></table>`,
			wanted: "Hello world Code 123456 Column A Column B",
		},
		{
			name:   "keeps adjacent Chinese text together",
			html:   `<span>验</span><strong>证码</strong>：<code>739638</code>`,
			wanted: "验证码：739638",
		},
		{
			name:   "decodes character references",
			html:   `<p>A &amp; B &lt; C&nbsp;&#x4E2D;&#25991;</p>`,
			wanted: "A & B < C 中文",
		},
		{
			name: "omits non-content nodes",
			html: `<!doctype html><html><head><title>Ignored title</title>
				<style>.secret { display: none }</style></head><body>
				<!-- ignored comment --><p>Visible text</p>
				<script>window.secret = "ignored"</script><template>Ignored template</template>
				<noscript><p>Email fallback text</p></noscript>
				<iframe><b>Ignored frame text</b></iframe><xmp><b>Ignored xmp text</b></xmp>
				<textarea><b>Ignored form text</b></textarea>
				</body></html>`,
			wanted: "Visible text Email fallback text",
		},
		{
			name: "omits common hidden email content",
			html: `<p>Visible first</p>
					<div hidden>Hidden attribute</div>
					<div hidden><div/>Nested hidden</div>Still hidden</div>
					<div aria-hidden="true">Aria hidden</div>
				<div style="display: none !important">Hidden preview</div>
				<div style="visibility: hidden">Invisible</div>
				<div style="mso-hide: all">Outlook hidden</div>
				<p>Visible last</p>`,
			wanted: "Visible first Visible last",
		},
		{
			name:   "omits style text in malformed element context",
			html:   `<select><style>.secret{color:red}</style><option>Shown</option></select>`,
			wanted: "Shown",
		},
		{
			name:   "separates implicitly closed table cells",
			html:   `<table><tr><td>First<td>Second</tr></table>`,
			wanted: "First Second",
		},
		{
			name:   "treats self-closing raw elements with HTML semantics",
			html:   `<p>Before</p><script/>alert("ignored")</script><p>After</p>`,
			wanted: "Before After",
		},
		{
			name:   "extracts text from truncated markup",
			html:   `<div>您的登录代码是 <strong>123456`,
			wanted: "您的登录代码是 123456",
		},
		{
			name:   "returns empty text for markup without content",
			html:   `<html><head><style>body { color: red }</style></head><body><!-- empty --></body></html>`,
			wanted: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := plainTextFromHTML(test.html)
			if got != test.wanted {
				t.Fatalf("plainTextFromHTML() = %q, want %q", got, test.wanted)
			}
			if strings.ContainsAny(got, "\r\n") {
				t.Fatalf("plainTextFromHTML() returned line breaks: %q", got)
			}
		})
	}
}

func TestSingleLinePlainTextCollapsesWhitespace(t *testing.T) {
	source := "  第一行\r\n第二行\t\u00a0第三行  "
	got := singleLinePlainText(source)
	if got != "第一行 第二行 第三行" {
		t.Fatalf("singleLinePlainText() = %q, want single-line text", got)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("singleLinePlainText() returned line breaks: %q", got)
	}
}

func TestPlainTextFromHTMLHandlesDeepNesting(t *testing.T) {
	const depth = 20_000
	source := strings.Repeat("<div>", depth) + "deep text" + strings.Repeat("</div>", depth)
	if got := plainTextFromHTML(source); got != "deep text" {
		t.Fatalf("plainTextFromHTML() = %q, want deep text", got)
	}
}
