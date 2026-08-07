package mail

import (
	stdmail "net/mail"
	"reflect"
	"strings"
	"testing"
)

func TestRecipientAddresses(t *testing.T) {
	header := stdmail.Header{
		"To":            {`Alias Owner <Alias@Example.COM>, other@example.com`},
		"Cc":            {`cc@example.com`},
		"Delivered-To":  {`alias@example.com`},
		"X-Original-To": {`Alias@Example.com`},
		"Resent-To":     {`ignored@example.com`},
	}

	want := []string{"alias@example.com"}
	if got := RecipientAddresses(header); !reflect.DeepEqual(got, want) {
		t.Fatalf("RecipientAddresses() = %#v, want %#v", got, want)
	}
}

func TestMatchingAliasIDsUsesExactMailbox(t *testing.T) {
	header := stdmail.Header{"To": {`Not Alias <notalias@example.com>`}}
	aliases := map[string][]int64{
		"alias@example.com":    {1},
		"notalias@example.com": {2},
	}

	if got := matchingAliasIDs(header, aliases, true); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("matchingAliasIDs() = %#v, want [2]", got)
	}
}

func TestMatchingAliasIDsRejectsForgedToByDefault(t *testing.T) {
	header := stdmail.Header{"To": {`alias@example.com`}}
	aliases := map[string][]int64{"alias@example.com": {1}}

	if got := matchingAliasIDs(header, aliases, false); len(got) != 0 {
		t.Fatalf("default matching accepted forged To header: %#v", got)
	}
	if got := matchingAliasIDs(header, aliases, true); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("weak-header matching = %#v, want [1]", got)
	}
}

func TestStrongRecipientHeaderPreventsWeakFallback(t *testing.T) {
	header := stdmail.Header{
		"Delivered-To": {`other@example.com`},
		"To":           {`alias@example.com`},
	}
	aliases := map[string][]int64{"alias@example.com": {1}}

	if got := matchingAliasIDs(header, aliases, true); len(got) != 0 {
		t.Fatalf("weak fallback overrode strong delivery header: %#v", got)
	}
}

func TestConflictingAliasDeliveryHeadersFailClosed(t *testing.T) {
	header := stdmail.Header{
		"X-Original-To": {`first@example.com`, `second@example.com`},
		"Delivered-To":  {`second@example.com`},
	}
	aliases := map[string][]int64{
		"first@example.com":  {1},
		"second@example.com": {2},
	}

	if got := matchingAliasIDs(header, aliases, false); len(got) != 0 {
		t.Fatalf("conflicting delivery headers matched aliases: %#v", got)
	}
}

func TestAllStrongHeadersMustAgree(t *testing.T) {
	header := stdmail.Header{
		"X-Original-To": {`first@example.com`},
		"Delivered-To":  {`second@example.com`},
	}
	aliases := map[string][]int64{
		"first@example.com":  {1},
		"second@example.com": {2},
	}

	if got := matchingAliasIDs(header, aliases, false); len(got) != 0 {
		t.Fatalf("conflicting strong headers matched aliases: %#v", got)
	}
}

func TestMalformedStrongHeaderFailsClosed(t *testing.T) {
	header := stdmail.Header{
		"Envelope-To": {`malformed value containing alias@example.com`},
		"To":          {`alias@example.com`},
	}
	aliases := map[string][]int64{"alias@example.com": {1}}

	if got := matchingAliasIDs(header, aliases, true); len(got) != 0 {
		t.Fatalf("malformed strong header fell back to weak recipient: %#v", got)
	}
}

func TestWeakHeadersMustHaveOneAddress(t *testing.T) {
	header := stdmail.Header{
		"To": {`alias@example.com`},
		"Cc": {`other@example.com`},
	}
	aliases := map[string][]int64{"alias@example.com": {1}}

	if got := matchingAliasIDs(header, aliases, true); len(got) != 0 {
		t.Fatalf("ambiguous weak recipients matched alias: %#v", got)
	}
}

func TestOriginalRecipientRFC822Syntax(t *testing.T) {
	header := stdmail.Header{"Original-Recipient": {`rfc822; Alias@Example.com`}}
	if got := RecipientAddresses(header); !reflect.DeepEqual(got, []string{"alias@example.com"}) {
		t.Fatalf("RecipientAddresses() = %#v", got)
	}
}

func TestMatchingAliasIDsUsesAppleHMERouteBeforePhysicalRecipient(t *testing.T) {
	header := stdmail.Header{
		"Original-Recipient": {`rfc822; primary@icloud.com`},
		"To":                 {`Hide My Email <hidden.alias@icloud.com>`},
		icloudHMEHeaderField: {`p=hidden.alias@icloud.com; d=; f=primary@icloud.com; r=to; s=sender@example.com`},
	}
	aliases := map[string][]int64{"hidden.alias@icloud.com": {7}}
	for _, allowWeak := range []bool{false, true} {
		if got := matchingAliasIDsForAccount(header, aliases, "primary@icloud.com", allowWeak); !reflect.DeepEqual(got, []int64{7}) {
			t.Fatalf("allowWeak=%v HME matching = %#v, want [7]", allowWeak, got)
		}
	}
}

func TestMatchingAliasIDsHandlesRawAppleHeaderCasing(t *testing.T) {
	header := stdmail.Header{
		"Original-recipient": {`rfc822;primary@icloud.com`},
		"To":                 {`Hide My Email <private.alias@icloud.com>`},
		"X-Icloud-Hme":       {`p=private.alias@icloud.com; d=; f=primary@icloud.com; r=to; s=sender@example.com`},
	}
	aliases := map[string][]int64{"private.alias@icloud.com": {7}}
	if got, determinate := classifyRecipientAlias(header, aliases, "PRIMARY@iCloud.com", false); !determinate || got != 7 {
		t.Fatalf("raw Apple header classification = (%d, %v), want (7, true)", got, determinate)
	}
}

func TestMatchingAliasIDsRejectsInvalidAppleHMERoutes(t *testing.T) {
	tests := []struct {
		name   string
		hme    string
		extra  string
		values []string
	}{
		{name: "missing private address", hme: `d=; f=primary@icloud.com; r=to`},
		{name: "missing forward address", hme: `p=hidden.alias@icloud.com; d=; r=to`},
		{name: "missing recipient role", hme: `p=hidden.alias@icloud.com; d=; f=primary@icloud.com`},
		{name: "duplicate private address", hme: `p=hidden.alias@icloud.com; p=other.alias@icloud.com; f=primary@icloud.com; r=to`},
		{name: "display name is not an address parameter", hme: `p=Hidden Alias <hidden.alias@icloud.com>; f=primary@icloud.com; r=to`},
		{name: "forward address mismatch", hme: `p=hidden.alias@icloud.com; f=other@icloud.com; r=to`},
		{name: "recipient role mismatch", hme: `p=hidden.alias@icloud.com; f=primary@icloud.com; r=cc`, extra: "Cc: other@example.com\r\n"},
		{name: "original recipient mismatch", hme: `p=hidden.alias@icloud.com; f=primary@icloud.com; r=to`, extra: "Original-Recipient: rfc822; other@icloud.com\r\n"},
		{name: "malformed sender", hme: `p=hidden.alias@icloud.com; f=primary@icloud.com; r=to; s=not an address`},
		{name: "trailing empty parameter", hme: `p=hidden.alias@icloud.com; f=primary@icloud.com; r=to;`},
		{name: "duplicate HME fields", values: []string{
			`p=hidden.alias@icloud.com; f=primary@icloud.com; r=to`,
			`p=hidden.alias@icloud.com; f=primary@icloud.com; r=to`,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hmeValues := test.values
			if hmeValues == nil {
				hmeValues = []string{test.hme}
			}
			header := stdmail.Header{
				"Original-Recipient": {`rfc822; primary@icloud.com`},
				"To":                 {`Hide My Email <hidden.alias@icloud.com>`},
				icloudHMEHeaderField: hmeValues,
			}
			if test.extra != "" {
				parsed, err := stdmail.ReadMessage(strings.NewReader(test.extra + "\r\n"))
				if err != nil {
					t.Fatalf("parse extra headers: %v", err)
				}
				for key, values := range parsed.Header {
					header[key] = values
				}
			}
			if got, determinate := classifyRecipientAlias(header, map[string][]int64{
				"hidden.alias@icloud.com": {7},
			}, "primary@icloud.com", false); determinate || got != 0 {
				t.Fatalf("classification = (%d, %v), want (0, false)", got, determinate)
			}
		})
	}
}

func TestMatchingAliasIDsRejectsConflictingRegisteredAliasInAppleHMEHeaders(t *testing.T) {
	header := stdmail.Header{
		"To":                 {`Hide My Email <hidden.alias@icloud.com>`},
		"Cc":                 {`other.alias@icloud.com`},
		icloudHMEHeaderField: {`p=hidden.alias@icloud.com; d=; f=primary@icloud.com; r=to`},
	}
	aliases := map[string][]int64{
		"hidden.alias@icloud.com": {7},
		"other.alias@icloud.com":  {8},
	}
	if got, determinate := classifyRecipientAlias(header, aliases, "primary@icloud.com", false); determinate || got != 0 {
		t.Fatalf("conflicting HME classification = (%d, %v), want (0, false)", got, determinate)
	}
}

func TestMatchingAliasIDsAcceptsUnregisteredAppleHMEAddressAsDeterminateOther(t *testing.T) {
	header := stdmail.Header{
		"To":                 {`Hide My Email <unregistered.alias@icloud.com>`},
		icloudHMEHeaderField: {`p=unregistered.alias@icloud.com; d=; f=primary@icloud.com; r=to`},
	}
	if got, determinate := classifyRecipientAlias(header, map[string][]int64{
		"hidden.alias@icloud.com": {7},
	}, "primary@icloud.com", false); !determinate || got != 0 {
		t.Fatalf("unregistered HME classification = (%d, %v), want (0, true)", got, determinate)
	}
}

func FuzzRecipientAddresses(f *testing.F) {
	f.Add(`Alias <alias@example.com>`)
	f.Add(`notalias@example.com, alias@example.com`)
	f.Add(`=?UTF-8?Q?Alias?= <ALIAS@example.com>`)
	f.Fuzz(func(t *testing.T, value string) {
		addresses, _ := recipientAddresses(stdmail.Header{"To": {value}}, true)
		seen := make(map[string]struct{}, len(addresses))
		for _, address := range addresses {
			if address == "" {
				t.Fatal("empty normalized address")
			}
			if _, exists := seen[address]; exists {
				t.Fatalf("duplicate address %q", address)
			}
			seen[address] = struct{}{}
		}
	})
}

func FuzzICloudHMEValue(f *testing.F) {
	f.Add(`p=hidden.alias@icloud.com; d=; f=primary@icloud.com; r=to; s=sender@example.com`)
	f.Add(`p=;f=;r=`)
	f.Add(`p=hidden.alias@icloud.com; p=other.alias@icloud.com`)
	f.Fuzz(func(t *testing.T, value string) {
		_, _, _ = parseICloudHMERoute(stdmail.Header{icloudHMEHeaderField: {value}})
	})
}
