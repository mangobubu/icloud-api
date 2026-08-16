package domain

import (
	"strings"
	"testing"
)

func TestNormalizeEmailSuffix(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		valid bool
	}{
		{name: "normalizes leading at and case", input: " @Example.COM ", want: "example.com", valid: true},
		{name: "single label is allowed", input: "localhost", want: "localhost", valid: true},
		{name: "empty", input: "", valid: false},
		{name: "full address", input: "user@example.com", valid: false},
		{name: "double at", input: "@@example.com", valid: false},
		{name: "invalid underscore", input: "example_test.com", valid: false},
		{name: "empty label", input: "example..com", valid: false},
		{name: "leading hyphen", input: "-example.com", valid: false},
		{name: "suffix too long for generated address", input: strings.Repeat("a", 242), valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeEmailSuffix(test.input)
			if test.valid {
				if err != nil || got != test.want {
					t.Fatalf("NormalizeEmailSuffix(%q) = %q, %v; want %q", test.input, got, err, test.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("NormalizeEmailSuffix(%q) unexpectedly accepted as %q", test.input, got)
			}
		})
	}
}

func TestNormalizeMailboxTypeDefaultsLegacyRowsToICloud(t *testing.T) {
	if got := NormalizeMailboxType(""); got != MailboxTypeICloud {
		t.Fatalf("empty mailbox type = %q, want %q", got, MailboxTypeICloud)
	}
	if got := NormalizeMailboxType(" CUSTOM "); got != MailboxTypeCustom {
		t.Fatalf("custom mailbox type = %q, want %q", got, MailboxTypeCustom)
	}
	if got := NormalizeMailboxType("unknown"); got != "" {
		t.Fatalf("unknown mailbox type = %q, want empty", got)
	}
}
