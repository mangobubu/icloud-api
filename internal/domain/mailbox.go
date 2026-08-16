package domain

import (
	"errors"
	"strings"
	"unicode"
)

// NormalizeMailboxType returns the persisted mailbox mode. Empty values are
// deliberately mapped to iCloud for source compatibility with pre-custom
// account records and in-memory adapters.
func NormalizeMailboxType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case MailboxTypeCustom:
		return MailboxTypeCustom
	case MailboxTypeICloud, "":
		return MailboxTypeICloud
	default:
		return ""
	}
}

// NormalizeEmailSuffix canonicalizes a custom mailbox domain. The generator
// only emits ASCII addresses, so accepting ASCII DNS-style labels keeps the
// resulting addresses valid for both net/mail and the database's case
// insensitive address keys. A leading '@' is accepted as a convenience for
// administrator input.
func NormalizeEmailSuffix(value string) (string, error) {
	suffix := strings.ToLower(strings.TrimSpace(value))
	suffix = strings.TrimPrefix(suffix, "@")
	// The longest generated local part is 12 bytes. Limiting the suffix to
	// 241 bytes keeps every generated address within the common 254-byte SMTP
	// mailbox limit: 12 + one '@' + 241.
	if suffix == "" || len([]byte(suffix)) > 241 || strings.ContainsAny(suffix, " \t\r\n@") {
		return "", errors.New("invalid email suffix")
	}
	if strings.HasPrefix(suffix, ".") || strings.HasSuffix(suffix, ".") || strings.Contains(suffix, "..") {
		return "", errors.New("invalid email suffix")
	}
	for _, label := range strings.Split(suffix, ".") {
		if label == "" || len([]byte(label)) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", errors.New("invalid email suffix")
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			// Keep this branch explicit so non-ASCII letters are rejected rather
			// than accidentally accepted by unicode's broad letter category.
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return "", errors.New("invalid email suffix")
			}
			return "", errors.New("invalid email suffix")
		}
	}
	return suffix, nil
}

// IsValidEmailSuffix is the predicate form used by validation boundaries.
func IsValidEmailSuffix(value string) bool {
	_, err := NormalizeEmailSuffix(value)
	return err == nil
}
