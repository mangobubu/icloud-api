package mail

import "testing"

func TestExtractOTPBoundariesKeywordsAndReadableHTML(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name                string
		subject, text, html string
		want                string
	}{
		{name: "four digits", subject: "Code: 1234", want: "1234"},
		{name: "eight digits", text: "验证码 12345678", want: "12345678"},
		{name: "three digits rejected", subject: "Code 123"},
		{name: "nine digits rejected", subject: "Code 123456789"},
		{name: "ASCII letter adjacency rejected", subject: "code A123456Z"},
		{name: "ASCII digit adjacency rejected", subject: "code 912345678"},
		{name: "Unicode letter adjacency rejected", subject: "code 验123456证"},
		{name: "Unicode number adjacency rejected", subject: "code Ⅷ123456"},
		{
			name:    "keyword candidate wins",
			subject: "Order 20260811; 验证码 654321",
			want:    "654321",
		},
		{
			name: "nearest keyword wins",
			text: "reference 111111 then a long description; verification code 222222",
			want: "222222",
		},
		{
			name:    "subject wins equal score",
			subject: "1234",
			text:    "5678",
			want:    "1234",
		},
		{
			name: "HTML readable text and ignored script",
			html: `<html><head><title>code 111111</title></head><body>` +
				`<script>verification code 999999</script><p>Verification code <b>246810</b></p></body></html>`,
			want: "246810",
		},
		{name: "full-width digits are not ASCII OTP", text: "验证码 １２３４５６"},
		{name: "no code", subject: "Welcome", text: "No numeric token here"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractOTP(test.subject, test.text, test.html); got != test.want {
				t.Fatalf("ExtractOTP() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestArchiveMessageHardLimitIncludesExactlyOneHundredMiB(t *testing.T) {
	t.Parallel()

	settings := NewFetcher().settings()
	if settings.maxMessageBytes != 100<<20 {
		t.Fatalf("default archive message limit = %d, want %d", settings.maxMessageBytes, 100<<20)
	}
	if messageExceedsArchiveLimit(100<<20, settings.maxMessageBytes) {
		t.Fatal("exactly 100 MiB was classified as oversized")
	}
	if !messageExceedsArchiveLimit(100<<20+1, settings.maxMessageBytes) {
		t.Fatal("100 MiB + 1 byte was not classified as oversized")
	}
}
