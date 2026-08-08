package store

import (
	"testing"
	"unicode/utf8"
)

func TestTruncatePreservesUTF8Boundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		limit int
		want  string
	}{
		{name: "multibyte", value: "同步邮件正常", limit: 4, want: "同步邮件"},
		{name: "within limit", value: "隐私邮箱", limit: 4, want: "隐私邮箱"},
		{name: "invalid input", value: "a" + string([]byte{0xff}) + "b", limit: 3, want: "a\uFFFDb"},
		{name: "postgres NUL", value: "a\x00b", limit: 3, want: "a\uFFFDb"},
		{name: "zero limit", value: "mail", limit: 0, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.value, tt.limit)
			if got != tt.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", tt.value, tt.limit, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncate(%q, %d) returned invalid UTF-8", tt.value, tt.limit)
			}
		})
	}
}
