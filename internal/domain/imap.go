package domain

import (
	"fmt"
	"net"
	"strings"
	"unicode"
)

const (
	DefaultIMAPHost = "imap.mail.me.com"
	DefaultIMAPPort = 993
)

// NormalizeIMAPEndpoint validates and normalizes an IMAP host and port.
func NormalizeIMAPEndpoint(host string, port int) (string, int, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", 0, fmt.Errorf("imap host is empty")
	}
	if len(host) > 253 {
		return "", 0, fmt.Errorf("imap host exceeds 253 bytes")
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("imap port must be between 1 and 65535")
	}
	for _, r := range host {
		if unicode.IsSpace(r) {
			return "", 0, fmt.Errorf("imap host contains whitespace")
		}
	}

	// Brackets are URI notation and are not part of the accepted host form.
	if strings.ContainsAny(host, "[]") {
		return "", 0, fmt.Errorf("imap host must not be bracketed")
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), port, nil
	}

	if strings.HasSuffix(host, ".") {
		host = strings.TrimSuffix(host, ".")
		if host == "" {
			return "", 0, fmt.Errorf("imap host is empty")
		}
	}
	if len(host) > 253 {
		return "", 0, fmt.Errorf("imap host exceeds 253 bytes")
	}

	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) < 1 || len(label) > 63 {
			return "", 0, fmt.Errorf("invalid imap DNS label")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", 0, fmt.Errorf("invalid imap DNS label")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') || c == '-') {
				return "", 0, fmt.Errorf("invalid imap DNS label")
			}
		}
	}

	return strings.ToLower(host), port, nil
}

// UsesForwardedICloudIMAP reports whether an iCloud Hide My Email account is
// reading from a non-default physical mailbox. The mailbox type remains
// iCloud so Apple alias synchronization continues to work; this only selects
// the receive-routing and upstream-password contract.
func UsesForwardedICloudIMAP(account Account) bool {
	if NormalizeMailboxType(account.MailboxType) != MailboxTypeICloud {
		return false
	}
	accountEmail := NormalizeEmail(account.Email)
	imapUsername := NormalizeEmail(account.IMAPUsername)
	if accountEmail != "" && imapUsername != "" && accountEmail != imapUsername {
		return true
	}
	host := strings.TrimSpace(account.IMAPHost)
	if host == "" {
		host = DefaultIMAPHost
	}
	port := account.IMAPPort
	if port == 0 {
		port = DefaultIMAPPort
	}
	host, port, err := NormalizeIMAPEndpoint(host, port)
	return err == nil && (host != DefaultIMAPHost || port != DefaultIMAPPort)
}
