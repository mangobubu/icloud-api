package mail

import (
	stdmail "net/mail"
	"reflect"
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
