package mail

import (
	stdmail "net/mail"
	"sort"
	"strings"

	"icloud-api/internal/domain"
)

const (
	icloudHMEHeaderField = "X-ICLOUD-HME"
	maxICloudHMEBytes    = 8 << 10
	maxICloudHMEParams   = 32
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

// Custom mailbox delivery agents do not share iCloud's routing contract.
// Select the first header tier that is present and never fall through when
// that tier is malformed or cannot be mapped to enabled aliases.
var customRecipientHeaderTiers = [][]string{
	{"X-Original-To"},
	{"Original-Recipient"},
	{"Envelope-To", "X-Envelope-To", "X-Forwarded-To"},
	{"Delivered-To"},
}

type icloudHMERoute struct {
	privateAddress string
	forwardAddress string
	recipientRole  string
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

// recipientHeaderFieldsForFetch includes weak fields even when weak matching
// is disabled. They are needed to validate the recipient role recorded by the
// Apple HME routing header; they are never trusted on their own unless the
// existing allowWeak policy permits it.
func recipientHeaderFieldsForFetch() []string {
	fields := []string{icloudHMEHeaderField}
	fields = append(fields, strongRecipientHeaderFields...)
	fields = append(fields, weakRecipientHeaderFields...)
	return fields
}

func addressesFromHeaders(header stdmail.Header, fields []string) ([]string, bool) {
	seen := make(map[string]struct{})

	for _, field := range fields {
		for _, value := range headerValues(header, field) {
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

func headerValues(header stdmail.Header, field string) []string {
	var result []string
	for key, values := range header {
		if strings.EqualFold(key, field) {
			result = append(result, values...)
		}
	}
	return result
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
	return matchingAliasIDsForAccount(header, aliases, domain.Account{}, allowWeak)
}

func matchingAliasIDsForAccount(
	header stdmail.Header,
	aliases map[string][]int64,
	account domain.Account,
	allowWeak bool,
) []int64 {
	aliasID, determinate := classifyRecipientAlias(header, aliases, account, allowWeak)
	if !determinate || aliasID == 0 {
		return nil
	}
	return []int64{aliasID}
}

func classifyRecipientAlias(
	header stdmail.Header,
	aliases map[string][]int64,
	account domain.Account,
	allowWeak bool,
) (int64, bool) {
	switch domain.NormalizeMailboxType(account.MailboxType) {
	case domain.MailboxTypeCustom:
		aliasIDs, determinate := classifyCustomRecipientAliases(header, aliases, allowWeak)
		if !determinate || len(aliasIDs) != 1 {
			return 0, false
		}
		return aliasIDs[0], true
	case domain.MailboxTypeICloud:
		// Continue with the historical iCloud routing contract below. Empty
		// mailbox types normalize to iCloud for legacy in-memory accounts.
	default:
		return 0, false
	}

	route, present, valid := parseICloudHMERoute(header)
	if present {
		accountEmail := domain.NormalizeEmail(account.Email)
		if !valid || accountEmail == "" ||
			route.forwardAddress != accountEmail ||
			!hmeRouteMatchesVisibleRecipient(header, route) ||
			!hmeRouteMatchesOriginalRecipient(header, route) ||
			hmeRouteHasConflictingAlias(header, route, aliases) {
			return 0, false
		}
		return aliasIDForAddress(route.privateAddress, aliases)
	}

	addresses, valid := recipientAddresses(header, allowWeak)
	if !valid || len(addresses) != 1 {
		return 0, false
	}
	return aliasIDForAddress(addresses[0], aliases)
}

func classifyCustomRecipientAliases(
	header stdmail.Header,
	aliases map[string][]int64,
	allowWeak bool,
) ([]int64, bool) {
	addresses, valid := customRecipientAddresses(header, allowWeak)
	if !valid {
		return nil, false
	}
	if len(addresses) == 0 {
		return nil, true
	}

	seenIDs := make(map[int64]string, len(addresses))
	for _, address := range addresses {
		aliasIDs := aliases[address]
		if len(aliasIDs) != 1 || aliasIDs[0] <= 0 {
			return nil, false
		}
		aliasID := aliasIDs[0]
		if previousAddress, duplicate := seenIDs[aliasID]; duplicate && previousAddress != address {
			return nil, false
		}
		seenIDs[aliasID] = address
	}

	result := make([]int64, 0, len(seenIDs))
	for aliasID := range seenIDs {
		result = append(result, aliasID)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, true
}

func customRecipientAddresses(header stdmail.Header, allowWeak bool) ([]string, bool) {
	for _, fields := range customRecipientHeaderTiers {
		if hasAnyHeader(header, fields) {
			addresses, valid := addressesFromHeaders(header, fields)
			return addresses, valid && len(addresses) > 0
		}
	}
	if !allowWeak {
		return nil, true
	}
	if !hasAnyHeader(header, weakRecipientHeaderFields) {
		return nil, true
	}
	addresses, valid := addressesFromHeaders(header, weakRecipientHeaderFields)
	return addresses, valid && len(addresses) > 0
}

func aliasIDForAddress(address string, aliases map[string][]int64) (int64, bool) {
	aliasIDs := aliases[address]
	if len(aliasIDs) == 0 {
		return 0, true
	}
	if len(aliasIDs) != 1 {
		return 0, false
	}
	return aliasIDs[0], true
}

// Apple encodes HME routing as semicolon-separated metadata, not an address
// list, so validate it independently before ordinary recipient classification.
func parseICloudHMERoute(header stdmail.Header) (icloudHMERoute, bool, bool) {
	values := headerValues(header, icloudHMEHeaderField)
	if len(values) == 0 {
		return icloudHMERoute{}, false, true
	}
	if len(values) != 1 {
		return icloudHMERoute{}, true, false
	}
	route, valid := parseICloudHMEValue(values[0])
	return route, true, valid
}

func parseICloudHMEValue(value string) (icloudHMERoute, bool) {
	if len(value) == 0 || len(value) > maxICloudHMEBytes || strings.ContainsAny(value, "\r\n") {
		return icloudHMERoute{}, false
	}
	parts := strings.Split(value, ";")
	if len(parts) == 0 || len(parts) > maxICloudHMEParams {
		return icloudHMERoute{}, false
	}
	params := make(map[string]string, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return icloudHMERoute{}, false
		}
		key, parameterValue, found := strings.Cut(part, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		parameterValue = strings.TrimSpace(parameterValue)
		if !found || !validICloudHMEParameterKey(key) ||
			strings.ContainsAny(parameterValue, "\r\n") {
			return icloudHMERoute{}, false
		}
		if _, duplicate := params[key]; duplicate {
			return icloudHMERoute{}, false
		}
		params[key] = parameterValue
	}

	privateAddress, ok := parseICloudHMEAddress(params, "p")
	if !ok {
		return icloudHMERoute{}, false
	}
	forwardAddress, ok := parseICloudHMEAddress(params, "f")
	if !ok || privateAddress == forwardAddress {
		return icloudHMERoute{}, false
	}
	recipientRole, ok := params["r"]
	if !ok {
		return icloudHMERoute{}, false
	}
	recipientRole = strings.ToLower(strings.TrimSpace(recipientRole))
	if recipientRole != "to" && recipientRole != "cc" {
		return icloudHMERoute{}, false
	}
	if sender, exists := params["s"]; exists {
		if _, ok := parseICloudHMEAddressValue(sender); !ok {
			return icloudHMERoute{}, false
		}
	}
	return icloudHMERoute{
		privateAddress: privateAddress,
		forwardAddress: forwardAddress,
		recipientRole:  recipientRole,
	}, true
}

func parseICloudHMEAddress(params map[string]string, key string) (string, bool) {
	value, exists := params[key]
	if !exists {
		return "", false
	}
	return parseICloudHMEAddressValue(value)
}

func parseICloudHMEAddressValue(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " \t;,<>\"") {
		return "", false
	}
	parsed, err := headerAddressParser.Parse(value)
	if err != nil || parsed.Name != "" || !strings.EqualFold(parsed.Address, value) {
		return "", false
	}
	normalized := domain.NormalizeEmail(parsed.Address)
	return normalized, normalized != ""
}

func validICloudHMEParameterKey(key string) bool {
	if key == "" {
		return false
	}
	for _, char := range key {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func hmeRouteMatchesVisibleRecipient(header stdmail.Header, route icloudHMERoute) bool {
	field := "To"
	if route.recipientRole == "cc" {
		field = "Cc"
	}
	addresses, valid := addressesFromHeaders(header, []string{field})
	if !valid {
		return false
	}
	for _, address := range addresses {
		if address == route.privateAddress {
			return true
		}
	}
	return false
}

func hmeRouteMatchesOriginalRecipient(header stdmail.Header, route icloudHMERoute) bool {
	values := headerValues(header, "Original-Recipient")
	if len(values) == 0 {
		return true
	}
	if len(values) != 1 {
		return false
	}
	addresses, valid := addressesFromHeaders(header, []string{"Original-Recipient"})
	return valid && len(addresses) == 1 && addresses[0] == route.forwardAddress
}

func hmeRouteHasConflictingAlias(
	header stdmail.Header,
	route icloudHMERoute,
	aliases map[string][]int64,
) bool {
	fields := append([]string(nil), strongRecipientHeaderFields...)
	fields = append(fields, weakRecipientHeaderFields...)
	for _, field := range fields {
		if strings.EqualFold(field, "Original-Recipient") {
			continue
		}
		if !hasAnyHeader(header, []string{field}) {
			continue
		}
		addresses, valid := addressesFromHeaders(header, []string{field})
		if !valid {
			return true
		}
		for _, address := range addresses {
			if address == route.privateAddress || address == route.forwardAddress {
				continue
			}
			if _, registered := aliases[address]; registered {
				return true
			}
		}
	}
	return false
}
