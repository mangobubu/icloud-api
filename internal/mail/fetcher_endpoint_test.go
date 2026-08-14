package mail

import (
	"errors"
	"testing"

	"icloud-api/internal/domain"
)

func TestAccountEndpointDefaultsToICloudTLSPort(t *testing.T) {
	host, address, username, err := accountEndpoint(domain.Account{Email: "owner@icloud.com"})
	if err != nil {
		t.Fatal(err)
	}
	if host != domain.DefaultIMAPHost || address != "imap.mail.me.com:993" || username != "owner@icloud.com" {
		t.Fatalf("endpoint = %q %q %q", host, address, username)
	}
}

func TestAccountEndpointUsesConfiguredTLSServer(t *testing.T) {
	host, address, username, err := accountEndpoint(domain.Account{
		Email:        "owner@icloud.com",
		IMAPHost:     "Mail.Example.Test.",
		IMAPPort:     1993,
		IMAPUsername: "login@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if host != "mail.example.test" || address != "mail.example.test:1993" || username != "login@example.test" {
		t.Fatalf("endpoint = %q %q %q", host, address, username)
	}
}

func TestAccountEndpointFormatsIPv6AndRejectsInvalidServer(t *testing.T) {
	host, address, _, err := accountEndpoint(domain.Account{
		Email: "owner@icloud.com", IMAPHost: "2001:db8::1", IMAPPort: 993,
	})
	if err != nil {
		t.Fatal(err)
	}
	if host != "2001:db8::1" || address != "[2001:db8::1]:993" {
		t.Fatalf("IPv6 endpoint = %q %q", host, address)
	}

	for _, account := range []domain.Account{
		{Email: "owner@icloud.com", IMAPHost: "https://mail.example.test", IMAPPort: 993},
		{Email: "owner@icloud.com", IMAPHost: "mail.example.test:993", IMAPPort: 993},
		{Email: "owner@icloud.com", IMAPHost: "mail.example.test", IMAPPort: 65536},
	} {
		if _, _, _, err := accountEndpoint(account); !errors.Is(err, ErrInvalidIMAPConfig) {
			t.Fatalf("accountEndpoint(%#v) error = %v, want ErrInvalidIMAPConfig", account, err)
		}
	}
}
