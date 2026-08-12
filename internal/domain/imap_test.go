package domain

import "testing"

func TestNormalizeIMAPEndpoint(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		port      int
		wantHost  string
		wantPort  int
		wantError bool
	}{
		{name: "default DNS", host: " imap.mail.me.com ", port: 993, wantHost: "imap.mail.me.com", wantPort: 993},
		{name: "DNS lowercase and trailing dot", host: "IMAP.Mail.Me.Com.", port: 143, wantHost: "imap.mail.me.com", wantPort: 143},
		{name: "IPv4", host: "192.0.2.10", port: 1, wantHost: "192.0.2.10", wantPort: 1},
		{name: "IPv6", host: "2001:DB8::1", port: 65535, wantHost: "2001:db8::1", wantPort: 65535},
		{name: "empty", host: "", port: 993, wantError: true},
		{name: "whitespace", host: "imap mail.me.com", port: 993, wantError: true},
		{name: "scheme", host: "imaps://imap.mail.me.com", port: 993, wantError: true},
		{name: "path", host: "imap.mail.me.com/path", port: 993, wantError: true},
		{name: "query", host: "imap.mail.me.com?x=1", port: 993, wantError: true},
		{name: "fragment", host: "imap.mail.me.com#x", port: 993, wantError: true},
		{name: "userinfo", host: "user@imap.mail.me.com", port: 993, wantError: true},
		{name: "mixed port", host: "imap.mail.me.com:993", port: 993, wantError: true},
		{name: "bracketed IPv6", host: "[2001:db8::1]", port: 993, wantError: true},
		{name: "leading hyphen", host: "-imap.mail.me.com", port: 993, wantError: true},
		{name: "trailing hyphen", host: "imap-.mail.me.com", port: 993, wantError: true},
		{name: "invalid character", host: "imap_mail.me.com", port: 993, wantError: true},
		{name: "empty label", host: "imap..me.com", port: 993, wantError: true},
		{name: "port zero", host: "imap.mail.me.com", port: 0, wantError: true},
		{name: "port too large", host: "imap.mail.me.com", port: 65536, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotHost, gotPort, err := NormalizeIMAPEndpoint(tt.host, tt.port)
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, wantError %v", err, tt.wantError)
			}
			if tt.wantError {
				return
			}
			if gotHost != tt.wantHost || gotPort != tt.wantPort {
				t.Fatalf("got (%q, %d), want (%q, %d)", gotHost, gotPort, tt.wantHost, tt.wantPort)
			}
		})
	}
}

func TestNormalizeIMAPEndpointRejectsLongHost(t *testing.T) {
	longLabel := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, _, err := NormalizeIMAPEndpoint(longLabel+".com", 993); err == nil {
		t.Fatal("expected overlong DNS label to be rejected")
	}
}
