package mail

import (
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/testimap"
)

func TestTestEndpointDoesNotRelaxStoredAppleHostValidation(t *testing.T) {
	fetcher := NewFetcher()
	_, caPEM, err := testimap.GenerateTLSConfig("localhost", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := fetcher.ConfigureTestIMAPEndpoint("127.0.0.1:1993", "localhost", caPEM); err != nil {
		t.Fatal(err)
	}
	settings := fetcher.settings()
	account := domain.Account{
		Email:        "owner@icloud.com",
		IMAPHost:     "other.example.test",
		IMAPPort:     993,
		IMAPUsername: "owner@icloud.com",
		Enabled:      true,
	}
	if _, _, _, err := accountEndpointForSettings(account, settings.testEndpoint); err == nil {
		t.Fatal("test endpoint accepted a non-Apple stored IMAP host")
	}
}

func TestConfigureTestIMAPEndpointRejectsInvalidCA(t *testing.T) {
	for _, caPEM := range [][]byte{nil, []byte("not a certificate")} {
		fetcher := NewFetcher()
		if err := fetcher.ConfigureTestIMAPEndpoint("127.0.0.1:1993", "localhost", caPEM); err == nil {
			t.Fatalf("ConfigureTestIMAPEndpoint(%q) unexpectedly succeeded", caPEM)
		}
	}
}
