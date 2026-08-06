package mail

import (
	stdmail "net/mail"
	"net/textproto"
	"sort"
	"strings"

	"icloud-api/internal/domain"
)

var strongRecipientHeaderFields = []string{
	"X-Original-To",
	"Original-Recipient",
	"Envelope-To",
	"X-Envelope-To",
	"X-Forwarded-To",
	"Delivered-To",
}

var weakRecipientHeaderFields = []string{
	"To",
	"Cc",
	"Apparently-To",
}

// RecipientAddresses returns normalized, unique recipients from trusted
// delivery headers. Weak headers such as To and Cc are intentionally excluded.
func RecipientAddresses(header stdmail.Header) []string {
	addresses, _ := recipientAddresses(header, false)
	return addresses
}

func recipientAddresses(header stdmail.Header, allowWeak bool) ([]string, bool) {
	if hasAnyHeader(header, strongRecipientHeaderFields) {
		return addressesFromHeaders(header, strongRecipientHeaderFields)
	}
	if !allowWeak {
		return nil, true
	}
	return addressesFromHeaders(header, weakRecipientHeaderFields)
}

func addressesFromHeaders(header stdmail.Header, fields []string) ([]string, bool) {
	seen := make(map[string]struct{})
	values := textproto.MIMEHeader(header)

	for _, field := range fields {
		for _, value := range values.Values(field) {
			if strings.EqualFold(field, "Original-Recipient") {
				addressType, addressValue, found := strings.Cut(value, ";")
				if !found || !strings.EqualFold(strings.TrimSpace(addressType), "rfc822") {
					return nil, false
				}
				value = strings.TrimSpace(addressValue)
			}
			addresses, err := headerAddressParser.ParseList(value)
			if err != nil {
				return nil, false
			}
			for _, address := range addresses {
				normalized := domain.NormalizeEmail(address.Address)
				if normalized != "" {
					seen[normalized] = struct{}{}
				}
			}
		}
	}

	result := make([]string, 0, len(seen))
	for address := range seen {
		result = append(result, address)
	}
	sort.Strings(result)
	return result, true
}

func hasAnyHeader(header stdmail.Header, fields []string) bool {
	for key := range header {
		for _, field := range fields {
			if strings.EqualFold(key, field) {
				return true
			}
		}
	}
	return false
}

func recipientSearchHeaderFields(allowWeak bool) []string {
	fields := append([]string(nil), strongRecipientHeaderFields...)
	if allowWeak {
		fields = append(fields, weakRecipientHeaderFields...)
	}
	return fields
}

func normalizeAliasAddress(value string) (string, bool) {
	value = strings.TrimSpace(value)
	parsed, err := headerAddressParser.Parse(value)
	if err != nil {
		return "", false
	}
	normalized := domain.NormalizeEmail(parsed.Address)
	return normalized, normalized != ""
}

func matchingAliasIDs(header stdmail.Header, aliases map[string][]int64, allowWeak bool) []int64 {
	aliasID, determinate := classifyRecipientAlias(header, aliases, allowWeak)
	if !determinate || aliasID == 0 {
		return nil
	}
	return []int64{aliasID}
}

func classifyRecipientAlias(header stdmail.Header, aliases map[string][]int64, allowWeak bool) (int64, bool) {
	addresses, valid := recipientAddresses(header, allowWeak)
	if !valid || len(addresses) != 1 {
		return 0, false
	}
	aliasIDs := aliases[addresses[0]]
	if len(aliasIDs) == 0 {
		return 0, true
	}
	if len(aliasIDs) != 1 {
		return 0, false
	}
	return aliasIDs[0], true
}
