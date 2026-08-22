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
	"X-Apple-Original-To",
	"Original-Recipient",
	"X-Original-Recipient",
	"X-Apple-Original-Recipient",
	"Final-Recipient",
	"X-Original-Rcpt-To",
	"Envelope-To",
	"X-Envelope-To",
	"X-Forwarded-To",
	"Delivered-To",
	"X-Delivered-To",
	"X-Rcpt-To",
}

var weakRecipientHeaderFields = []string{
	"To",
	"Cc",
	"Apparently-To",
	"Resent-To",
	"Resent-Cc",
}

// Custom mailbox delivery agents do not share iCloud's routing contract.
// Select the first header tier that is present and never fall through when
// that tier is malformed or cannot be mapped to enabled aliases.
var customRecipientHeaderTiers = [][]string{
	{"X-Original-To", "X-Apple-Original-To", "X-Original-Rcpt-To"},
	{"Original-Recipient", "X-Original-Recipient", "X-Apple-Original-Recipient", "Final-Recipient"},
	{"Envelope-To", "X-Envelope-To", "X-Forwarded-To"},
	{"Delivered-To", "X-Delivered-To", "X-Rcpt-To"},
}

type icloudHMERoute struct {
	privateAddress string
	forwardAddress string
	recipientRole  string
}

// iCloudReceiveMode describes the two delivery routes that can exist while an
// account still uses the iCloud Hide My Email contract. The persisted mailbox
// type intentionally remains "icloud" for both routes: Apple synchronization
// and alias management must continue to use the iCloud integration even when
// the IMAP source is a third-party forwarding mailbox.
type iCloudReceiveMode uint8

const (
	iCloudReceiveDirect iCloudReceiveMode = iota
	iCloudReceiveForwarded
)

// iCloudReceiveModeForAccount infers the route from the configured IMAP
// source. A default iCloud endpoint with the primary address as its username
// is direct delivery. A non-iCloud endpoint or a different email username is
// the forwarding route. The username check also keeps the classifier useful
// for legacy/in-memory accounts that do not have an endpoint populated.
func iCloudReceiveModeForAccount(account domain.Account) iCloudReceiveMode {
	if domain.UsesForwardedICloudIMAP(account) {
		return iCloudReceiveForwarded
	}
	return iCloudReceiveDirect
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
			if isRFC822RecipientHeader(field) {
				var ok bool
				value, ok = normalizeRFC822RecipientValue(field, value)
				if !ok {
					return nil, false
				}
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

func normalizeRFC822RecipientValue(field, value string) (string, bool) {
	addressType, addressValue, found := strings.Cut(value, ";")
	if found {
		if !strings.EqualFold(strings.TrimSpace(addressType), "rfc822") {
			return "", false
		}
		return strings.TrimSpace(addressValue), true
	}
	// A few Apple/MTA variants omit the RFC 3461 type prefix even though the
	// field name is retained. Keep the stricter behavior for the standardized
	// Original-Recipient fields while accepting these two known variants.
	if strings.EqualFold(field, "X-Apple-Original-Recipient") ||
		strings.EqualFold(field, "Final-Recipient") {
		return strings.TrimSpace(value), strings.TrimSpace(value) != ""
	}
	return "", false
}

func isRFC822RecipientHeader(field string) bool {
	switch {
	case strings.EqualFold(field, "Original-Recipient"),
		strings.EqualFold(field, "X-Original-Recipient"),
		strings.EqualFold(field, "X-Apple-Original-Recipient"),
		strings.EqualFold(field, "Final-Recipient"):
		return true
	default:
		return false
	}
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

	// Preserve the legacy weak-header contract for the single-alias projection:
	// a visible To/Cc list is only decisive when it contains exactly one
	// mailbox. The archive projection can still retain multiple uniquely mapped
	// aliases when the explicit weak-header opt-in is enabled.
	if allowWeak && !hasAnyHeader(header, strongRecipientHeaderFields) {
		if _, present, _ := parseICloudHMERoute(header); !present {
			weakAddresses, valid := addressesFromHeaders(header, weakRecipientHeaderFields)
			if !valid || len(weakAddresses) != 1 {
				return 0, false
			}
		}
	}

	aliasIDs, determinate := classifyICloudRecipientAliases(header, aliases, account, allowWeak)
	if !determinate {
		return 0, false
	}
	if len(aliasIDs) == 0 {
		return 0, true
	}
	if len(aliasIDs) != 1 {
		return 0, false
	}
	return aliasIDs[0], true
}

// classifyICloudRecipientAliases classifies an iCloud message for both the
// legacy single-alias projection and the v2 archive. Apple normally preserves
// an explicit HME routing header. Forwarding providers may instead retain the
// HME address only in To/Cc while rewriting Delivered-To (or another strong
// envelope header) to the configured IMAP mailbox. In that case the visible
// address is accepted only when the strong delivery metadata also names the
// configured mailbox identity.
func classifyICloudRecipientAliases(
	header stdmail.Header,
	aliases map[string][]int64,
	account domain.Account,
	allowWeak bool,
) ([]int64, bool) {
	route, present, valid := parseICloudHMERoute(header)
	if present {
		accountEmail := domain.NormalizeEmail(account.Email)
		if !valid || accountEmail == "" ||
			!hmeRouteMatchesForwardTarget(header, route, account) ||
			!hmeRouteMatchesVisibleRecipient(header, route) ||
			!hmeRouteMatchesOriginalRecipient(header, route, account) ||
			hmeRouteHasUnexpectedStrongRecipient(header, route, account) ||
			hmeRouteHasConflictingAlias(header, route, aliases) {
			return nil, false
		}
		aliasID, determinate := aliasIDForAddress(route.privateAddress, aliases)
		if !determinate {
			return nil, false
		}
		if aliasID == 0 {
			return nil, true
		}
		return []int64{aliasID}, true
	}

	strongPresent := hasAnyHeader(header, strongRecipientHeaderFields)
	if strongPresent {
		strongAddresses, valid := addressesFromHeaders(header, strongRecipientHeaderFields)
		if !valid {
			return nil, false
		}
		strongIDs, determinate := iCloudStrongAliasIDs(strongAddresses, aliases, account)
		if !determinate {
			return nil, false
		}

		// If a strong header already names one or more registered HME aliases,
		// it is authoritative. A visible header that names a different local
		// alias is a conflict, while the forwarding mailbox itself is ignored.
		visibleIDs, visibleValid := visibleAliasIDs(header, aliases)
		if !visibleValid {
			return nil, false
		}
		if len(strongIDs) > 0 {
			if len(visibleIDs) > 0 && !sameAliasIDSet(strongIDs, visibleIDs) {
				return nil, false
			}
			return strongIDs, true
		}

		// A third-party forwarding mailbox commonly leaves only the physical
		// delivery address in strong headers. Recovering the HME address from
		// To/Cc remains an explicit weak-header opt-in even when that physical
		// address matches the configured mailbox.
		if !allowWeak {
			return nil, true
		}
		if !iCloudDeliveryTargetsOnly(strongAddresses, account) {
			return nil, true
		}
		return visibleIDs, true
	}

	if !allowWeak {
		return nil, true
	}
	weakAddresses, valid := addressesFromHeaders(header, weakRecipientHeaderFields)
	if !valid {
		return nil, false
	}
	return aliasIDsForAddresses(weakAddresses, aliases)
}

// hmeRouteMatchesForwardTarget accepts Apple's account-level forwarding
// address. In a direct iCloud mailbox it is normally Account.Email; after an
// iCloud forwarding chain it can instead be the forwarding address recorded in
// X-Original-To/Original-Recipient, while Delivered-To is the final local
// mailbox. The HME header is Apple-signed, and the additional header match
// prevents an unrelated account's route from being accepted accidentally.
func hmeRouteMatchesForwardTarget(
	header stdmail.Header,
	route icloudHMERoute,
	account domain.Account,
) bool {
	accountEmail := domain.NormalizeEmail(account.Email)
	mode := iCloudReceiveModeForAccount(account)
	if route.forwardAddress == accountEmail {
		// Direct iCloud delivery has historically allowed the signed HME route
		// with only the visible To/Cc field. A forwarded source must still show
		// a configured physical target in a strong delivery header.
		return mode == iCloudReceiveDirect || hasConfiguredICloudTargetHeader(header, account)
	}
	// A direct iCloud mailbox has no intermediate forwarding address. Do not
	// let an arbitrary strong header turn a mismatching `f=` value into a
	// valid route. A different configured email username is the explicit signal
	// that this iCloud account is reading a forwarded third-party mailbox.
	if mode != iCloudReceiveForwarded {
		return false
	}
	targets := iCloudFinalDeliveryTargets(account)
	if route.forwardAddress == domain.NormalizeEmail(account.IMAPUsername) {
		return hasConfiguredICloudTargetHeader(header, account)
	}

	// In a forwarding chain `f=` can name an intermediate address (the
	// reported sample has `f=ling@...` and `Delivered-To: mango@...`). Accept
	// that address only when the same message also carries a configured final
	// delivery target in a strong envelope header. This preserves the chain
	// evidence without treating a bare, forged X-Original-To as sufficient.
	foundRouteAddress := false
	foundConfiguredTarget := false
	for _, field := range strongRecipientHeaderFields {
		if !hasAnyHeader(header, []string{field}) {
			continue
		}
		addresses, valid := addressesFromHeaders(header, []string{field})
		if !valid {
			return false
		}
		for _, address := range addresses {
			if address == route.forwardAddress {
				foundRouteAddress = true
			}
			if _, isTarget := targets[address]; isTarget {
				foundConfiguredTarget = true
			}
		}
	}
	return foundRouteAddress && foundConfiguredTarget
}

func hasConfiguredICloudTargetHeader(header stdmail.Header, account domain.Account) bool {
	targets := iCloudFinalDeliveryTargets(account)
	if len(targets) == 0 {
		return false
	}
	found := false
	for _, field := range strongRecipientHeaderFields {
		if !hasAnyHeader(header, []string{field}) {
			continue
		}
		addresses, valid := addressesFromHeaders(header, []string{field})
		if !valid {
			return false
		}
		for _, address := range addresses {
			if _, ok := targets[address]; ok {
				found = true
			}
		}
	}
	return found
}

// visibleAliasIDs extracts aliases from the user-visible recipient fields. It
// intentionally does not include strong envelope fields; those are evaluated
// separately so a forwarding mailbox cannot be mistaken for an HME alias.
func visibleAliasIDs(header stdmail.Header, aliases map[string][]int64) ([]int64, bool) {
	if !hasAnyHeader(header, weakRecipientHeaderFields) {
		return nil, true
	}
	addresses, valid := addressesFromHeaders(header, weakRecipientHeaderFields)
	if !valid {
		return nil, false
	}
	return aliasIDsForAddresses(addresses, aliases)
}

func aliasIDsForAddresses(addresses []string, aliases map[string][]int64) ([]int64, bool) {
	seen := make(map[int64]struct{})
	for _, address := range addresses {
		ids, exists := aliases[address]
		if !exists {
			continue
		}
		if len(ids) != 1 || ids[0] <= 0 {
			return nil, false
		}
		seen[ids[0]] = struct{}{}
	}
	result := make([]int64, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, true
}

func sameAliasIDSet(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func iCloudStrongAliasIDs(
	addresses []string,
	aliases map[string][]int64,
	account domain.Account,
) ([]int64, bool) {
	ids, valid := aliasIDsForAddresses(addresses, aliases)
	if !valid {
		return nil, false
	}
	routeTargets := iCloudRouteAddresses(account)
	finalTargets := iCloudFinalDeliveryTargets(account)
	foundFinalTarget := false
	for _, address := range addresses {
		if _, registered := aliases[address]; registered {
			continue
		}
		if _, isTarget := routeTargets[address]; !isTarget {
			return nil, false
		}
		if _, isFinalTarget := finalTargets[address]; isFinalTarget {
			foundFinalTarget = true
		}
	}
	if iCloudReceiveModeForAccount(account) == iCloudReceiveForwarded && !foundFinalTarget {
		return nil, false
	}
	return ids, true
}

func iCloudDeliveryTargetsOnly(addresses []string, account domain.Account) bool {
	if len(addresses) == 0 {
		return false
	}
	targets := iCloudRouteAddresses(account)
	if len(targets) == 0 {
		return false
	}
	finalTargets := iCloudFinalDeliveryTargets(account)
	foundFinalTarget := false
	for _, address := range addresses {
		if _, ok := targets[address]; !ok {
			return false
		}
		if _, ok := finalTargets[address]; ok {
			foundFinalTarget = true
		}
	}
	return iCloudReceiveModeForAccount(account) == iCloudReceiveDirect || foundFinalTarget
}

// iCloudRouteAddresses contains every address that may legitimately appear in
// the delivery chain: the Apple account address and, for a forwarded source,
// the configured physical IMAP username.
func iCloudRouteAddresses(account domain.Account) map[string]struct{} {
	values := []string{account.Email}
	if iCloudReceiveModeForAccount(account) == iCloudReceiveForwarded {
		values = append(values, account.IMAPUsername)
	}
	targets := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		parsed, err := headerAddressParser.Parse(value)
		if err != nil || parsed.Name != "" {
			continue
		}
		if normalized := domain.NormalizeEmail(parsed.Address); normalized != "" {
			targets[normalized] = struct{}{}
		}
	}
	return targets
}

// iCloudFinalDeliveryTargets identifies the address that proves this message
// reached the mailbox being read. In forwarded mode the iCloud primary is an
// allowed chain address, but it is not sufficient as final delivery evidence.
func iCloudFinalDeliveryTargets(account domain.Account) map[string]struct{} {
	if iCloudReceiveModeForAccount(account) == iCloudReceiveForwarded {
		return normalizedICloudAddresses([]string{account.IMAPUsername})
	}
	return normalizedICloudAddresses([]string{account.Email})
}

func normalizedICloudAddresses(values []string) map[string]struct{} {
	targets := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		parsed, err := headerAddressParser.Parse(value)
		if err != nil || parsed.Name != "" {
			continue
		}
		if normalized := domain.NormalizeEmail(parsed.Address); normalized != "" {
			targets[normalized] = struct{}{}
		}
	}
	return targets
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

func hmeRouteMatchesOriginalRecipient(header stdmail.Header, route icloudHMERoute, account domain.Account) bool {
	var fields = []string{
		"Original-Recipient",
		"X-Original-Recipient",
		"X-Apple-Original-Recipient",
		"Final-Recipient",
	}
	present := false
	seen := make(map[string]struct{})
	for _, field := range fields {
		if !hasAnyHeader(header, []string{field}) {
			continue
		}
		present = true
		addresses, valid := addressesFromHeaders(header, []string{field})
		if !valid || len(addresses) == 0 {
			return false
		}
		for _, address := range addresses {
			seen[address] = struct{}{}
		}
	}
	if !present {
		return true
	}
	targets := iCloudRouteAddresses(account)
	for address := range seen {
		if address == route.forwardAddress {
			continue
		}
		if _, isConfiguredTarget := targets[address]; !isConfiguredTarget {
			return false
		}
	}
	// A forwarding MTA may preserve the original forwarding address, rewrite it
	// to the physical target, or retain both values in equivalent RFC822
	// headers. All observed values must belong to that same route.
	return len(seen) > 0
}

// hmeRouteHasUnexpectedStrongRecipient keeps an HME route bound to the same
// physical delivery chain. A forwarding mailbox may expose the Apple route,
// its intermediate address, and the configured final target; an unrelated
// strong recipient is evidence that the message belongs to another mailbox.
func hmeRouteHasUnexpectedStrongRecipient(
	header stdmail.Header,
	route icloudHMERoute,
	account domain.Account,
) bool {
	targets := iCloudRouteAddresses(account)
	for _, field := range strongRecipientHeaderFields {
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
			if _, isTarget := targets[address]; isTarget {
				continue
			}
			return true
		}
	}
	return false
}

func hmeRouteHasConflictingAlias(
	header stdmail.Header,
	route icloudHMERoute,
	aliases map[string][]int64,
) bool {
	fields := append([]string(nil), strongRecipientHeaderFields...)
	fields = append(fields, weakRecipientHeaderFields...)
	for _, field := range fields {
		if isRFC822RecipientHeader(field) {
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
