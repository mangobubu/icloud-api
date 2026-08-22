package mail

import (
	stdmail "net/mail"
	"reflect"
	"strings"
	"testing"

	"icloud-api/internal/domain"
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
		if got := matchingAliasIDsForAccount(header, aliases, domain.Account{Email: "primary@icloud.com"}, allowWeak); !reflect.DeepEqual(got, []int64{7}) {
			t.Fatalf("allowWeak=%v HME matching = %#v, want [7]", allowWeak, got)
		}
	}
}

func TestICloudReceiveModeSeparatesDirectAndForwardedIMAP(t *testing.T) {
	tests := []struct {
		name    string
		account domain.Account
		want    iCloudReceiveMode
	}{
		{
			name: "default iCloud endpoint and primary username",
			account: domain.Account{
				Email:        "primary@icloud.com",
				IMAPHost:     domain.DefaultIMAPHost,
				IMAPPort:     domain.DefaultIMAPPort,
				IMAPUsername: "primary@icloud.com",
			},
			want: iCloudReceiveDirect,
		},
		{
			name: "third-party endpoint",
			account: domain.Account{
				Email:        "primary@icloud.com",
				IMAPHost:     "imap.example.com",
				IMAPPort:     993,
				IMAPUsername: "mango@example.com",
			},
			want: iCloudReceiveForwarded,
		},
		{
			name: "legacy in-memory account with forwarding username",
			account: domain.Account{
				Email:        "primary@icloud.com",
				IMAPUsername: "mango@example.com",
			},
			want: iCloudReceiveForwarded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := iCloudReceiveModeForAccount(test.account); got != test.want {
				t.Fatalf("iCloud receive mode = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDirectICloudRouteDoesNotTrustUnconfiguredStrongForwardAddress(t *testing.T) {
	header := stdmail.Header{
		"X-Original-To":      {`other@icloud.com`},
		"Delivered-To":       {`primary@icloud.com`},
		"To":                 {`Hide My Email <hidden.alias@icloud.com>`},
		icloudHMEHeaderField: {`p=hidden.alias@icloud.com; f=other@icloud.com; r=to`},
	}
	aliases := map[string][]int64{"hidden.alias@icloud.com": {7}}
	account := domain.Account{
		MailboxType:  domain.MailboxTypeICloud,
		Email:        "primary@icloud.com",
		IMAPHost:     domain.DefaultIMAPHost,
		IMAPPort:     domain.DefaultIMAPPort,
		IMAPUsername: "primary@icloud.com",
	}
	if got := matchingAliasIDsForAccount(header, aliases, account, false); len(got) != 0 {
		t.Fatalf("direct iCloud route accepted unconfigured forwarding address: %#v", got)
	}
}

func TestForwardedICloudRouteRequiresConfiguredFinalTargetEvidence(t *testing.T) {
	aliases := map[string][]int64{"hidden.alias@icloud.com": {7}}
	account := domain.Account{
		MailboxType:  domain.MailboxTypeICloud,
		Email:        "primary@icloud.com",
		IMAPHost:     "imap.example.com",
		IMAPPort:     993,
		IMAPUsername: "mango@example.com",
	}
	withoutFinalTarget := stdmail.Header{
		"X-Original-To":      {`intermediate@example.com`},
		"To":                 {`Hide My Email <hidden.alias@icloud.com>`},
		icloudHMEHeaderField: {`p=hidden.alias@icloud.com; f=intermediate@example.com; r=to`},
	}
	if got := matchingAliasIDsForAccount(withoutFinalTarget, aliases, account, false); len(got) != 0 {
		t.Fatalf("forwarded route without final target matched: %#v", got)
	}
	withFinalTarget := stdmail.Header{
		"X-Original-To":      {`intermediate@example.com`},
		"Delivered-To":       {`mango@example.com`},
		"To":                 {`Hide My Email <hidden.alias@icloud.com>`},
		icloudHMEHeaderField: {`p=hidden.alias@icloud.com; f=intermediate@example.com; r=to`},
	}
	if got := matchingAliasIDsForAccount(withFinalTarget, aliases, account, false); !reflect.DeepEqual(got, []int64{7}) {
		t.Fatalf("forwarded route with final target = %#v, want [7]", got)
	}
}

func TestForwardedICloudHMERouteRejectsWeakOnlyDeliveryHeaders(t *testing.T) {
	aliases := map[string][]int64{"hidden.alias@icloud.com": {7}}
	account := domain.Account{
		MailboxType:  domain.MailboxTypeICloud,
		Email:        "primary@icloud.com",
		IMAPHost:     "imap.example.com",
		IMAPPort:     993,
		IMAPUsername: "mango@example.com",
	}
	for _, forwardAddress := range []string{"primary@icloud.com", "mango@example.com"} {
		header := stdmail.Header{
			"To":                 {`Hide My Email <hidden.alias@icloud.com>`},
			icloudHMEHeaderField: {`p=hidden.alias@icloud.com; f=` + forwardAddress + `; r=to`},
		}
		if got := matchingAliasIDsForAccount(header, aliases, account, false); len(got) != 0 {
			t.Fatalf("forwarded weak-only HME route %s matched: %#v", forwardAddress, got)
		}
	}
}

func TestForwardedICloudHMERouteRequiresThirdPartyFinalTarget(t *testing.T) {
	aliases := map[string][]int64{"hidden.alias@icloud.com": {7}}
	account := domain.Account{
		MailboxType:  domain.MailboxTypeICloud,
		Email:        "primary@icloud.com",
		IMAPHost:     "imap.example.com",
		IMAPPort:     993,
		IMAPUsername: "mango@example.com",
	}
	header := stdmail.Header{
		"X-Original-To":      {`primary@icloud.com`},
		"Delivered-To":       {`primary@icloud.com`},
		"To":                 {`Hide My Email <hidden.alias@icloud.com>`},
		icloudHMEHeaderField: {`p=hidden.alias@icloud.com; f=primary@icloud.com; r=to`},
	}
	if got := matchingAliasIDsForAccount(header, aliases, account, false); len(got) != 0 {
		t.Fatalf("forwarded route accepted iCloud primary as final target: %#v", got)
	}
}

func TestHMERouteAcceptsEquivalentOriginalRecipientHeaderVariants(t *testing.T) {
	header := stdmail.Header{
		"Original-Recipient":   {`rfc822; primary@icloud.com`},
		"X-Original-Recipient": {`rfc822; primary@icloud.com`},
		"To":                   {`Hide My Email <hidden.alias@icloud.com>`},
		icloudHMEHeaderField:   {`p=hidden.alias@icloud.com; f=primary@icloud.com; r=to`},
	}
	aliases := map[string][]int64{"hidden.alias@icloud.com": {7}}
	account := domain.Account{Email: "primary@icloud.com"}
	if got := matchingAliasIDsForAccount(header, aliases, account, false); !reflect.DeepEqual(got, []int64{7}) {
		t.Fatalf("equivalent original recipient headers = %#v, want [7]", got)
	}
}

func TestAppleRecipientVariantsAcceptBareAddressValues(t *testing.T) {
	for _, field := range []string{"X-Apple-Original-Recipient", "Final-Recipient"} {
		t.Run(field, func(t *testing.T) {
			header := stdmail.Header{field: {`primary@icloud.com`}}
			if got := RecipientAddresses(header); !reflect.DeepEqual(got, []string{"primary@icloud.com"}) {
				t.Fatalf("%s addresses = %#v, want [primary@icloud.com]", field, got)
			}
		})
	}
}

func TestMatchingAliasIDsAcceptsHMERouteWithRewrittenOriginalRecipient(t *testing.T) {
	header := stdmail.Header{
		"Original-Recipient": {`rfc822; mango@mgbubu.com`},
		"To":                 {`Hide My Email <hidden.alias@icloud.com>`},
		icloudHMEHeaderField: {`p=hidden.alias@icloud.com; d=; f=primary@icloud.com; r=to`},
	}
	aliases := map[string][]int64{"hidden.alias@icloud.com": {7}}
	account := domain.Account{
		MailboxType:  domain.MailboxTypeICloud,
		Email:        "primary@icloud.com",
		IMAPUsername: "mango@mgbubu.com",
	}
	if got := matchingAliasIDsForAccount(header, aliases, account, false); !reflect.DeepEqual(got, []int64{7}) {
		t.Fatalf("rewritten HME original recipient matching = %#v, want [7]", got)
	}
}

func TestMatchingAliasIDsUsesPastedICloudForwardingHeaders(t *testing.T) {
	// This is the routing subset from the reported message. Apple records the
	// forwarding address in X-ICLOUD-HME and X-Original-To, while the final
	// domain mailbox appears in Delivered-To.
	header := stdmail.Header{
		"Delivered-To":  {`mango@mgbubu.com`},
		"X-Original-To": {`ling@mgbubu.com`},
		"To":            {`Hide My Email <benefit.gimmes.2y@icloud.com>`},
		icloudHMEHeaderField: {
			`p=benefit.gimmes.2y@icloud.com; d=; f=ling@mgbubu.com; r=to; s=noreply@tm.openai.com`,
		},
	}
	aliases := map[string][]int64{"benefit.gimmes.2y@icloud.com": {7}}
	account := domain.Account{
		MailboxType:  domain.MailboxTypeICloud,
		Email:        "chenxiuzhan2026@icloud.com",
		IMAPHost:     "mgbubu.com",
		IMAPPort:     993,
		IMAPUsername: "mango@mgbubu.com",
	}
	if got := matchingAliasIDsForAccount(header, aliases, account, false); !reflect.DeepEqual(got, []int64{7}) {
		t.Fatalf("pasted iCloud forwarding headers matched = %#v, want [7]", got)
	}
	if got, determinate := classifyArchiveRecipientAliases(header, aliases, account, false); !determinate || !reflect.DeepEqual(got, []int64{7}) {
		t.Fatalf("pasted iCloud archive classification = %#v, %v, want [7], true", got, determinate)
	}
}

func TestICloudForwardedMailboxRecoversAliasFromVisibleRecipient(t *testing.T) {
	header := stdmail.Header{
		"To":           {`Hide My Email <hidden.alias@icloud.com>`},
		"Delivered-To": {`mango@mgbubu.com`},
	}
	aliases := map[string][]int64{"hidden.alias@icloud.com": {7}}
	account := domain.Account{
		MailboxType:  domain.MailboxTypeICloud,
		Email:        "primary@icloud.com",
		IMAPUsername: "mango@mgbubu.com",
	}

	if got := matchingAliasIDsForAccount(header, aliases, account, false); len(got) != 0 {
		t.Fatalf("default forwarding route trusted visible-only alias: %#v", got)
	}
	if got := matchingAliasIDsForAccount(header, aliases, account, true); !reflect.DeepEqual(got, []int64{7}) {
		t.Fatalf("opt-in forwarded iCloud matching = %#v, want [7]", got)
	}
	if got, determinate := classifyArchiveRecipientAliases(header, aliases, account, true); !determinate || !reflect.DeepEqual(got, []int64{7}) {
		t.Fatalf("opt-in forwarded iCloud archive matching = %#v, %v, want [7], true", got, determinate)
	}
}

func TestICloudForwardedMailboxRejectsWrongPhysicalTarget(t *testing.T) {
	header := stdmail.Header{
		"To":           {`hidden.alias@icloud.com`},
		"Delivered-To": {`different@example.com`},
	}
	aliases := map[string][]int64{"hidden.alias@icloud.com": {7}}
	account := domain.Account{
		MailboxType:  domain.MailboxTypeICloud,
		Email:        "primary@icloud.com",
		IMAPUsername: "mango@mgbubu.com",
	}
	if got := matchingAliasIDsForAccount(header, aliases, account, false); len(got) != 0 {
		t.Fatalf("wrong forwarded target matched alias: %#v", got)
	}
}

func TestICloudForwardedMailboxUsesAppleOriginalHeaders(t *testing.T) {
	header := stdmail.Header{
		"X-Apple-Original-To": {`hidden.alias@icloud.com`},
		"To":                  {`hidden.alias@icloud.com`},
		"Delivered-To":        {`mango@mgbubu.com`},
	}
	aliases := map[string][]int64{"hidden.alias@icloud.com": {7}}
	account := domain.Account{
		MailboxType:  domain.MailboxTypeICloud,
		Email:        "primary@icloud.com",
		IMAPUsername: "mango@mgbubu.com",
	}
	if got := matchingAliasIDsForAccount(header, aliases, account, false); !reflect.DeepEqual(got, []int64{7}) {
		t.Fatalf("Apple original recipient matching = %#v, want [7]", got)
	}
}

func TestICloudForwardedMailboxSupportsRFC822AppleOriginalRecipient(t *testing.T) {
	header := stdmail.Header{
		"X-Apple-Original-Recipient": {`rfc822; hidden.alias@icloud.com`},
		"Delivered-To":               {`mango@mgbubu.com`},
		"To":                         {`hidden.alias@icloud.com`},
	}
	aliases := map[string][]int64{"hidden.alias@icloud.com": {7}}
	account := domain.Account{
		MailboxType:  domain.MailboxTypeICloud,
		Email:        "primary@icloud.com",
		IMAPUsername: "mango@mgbubu.com",
	}
	if got := matchingAliasIDsForAccount(header, aliases, account, false); !reflect.DeepEqual(got, []int64{7}) {
		t.Fatalf("Apple RFC822 recipient matching = %#v, want [7]", got)
	}
}

func TestICloudForwardedMailboxDoesNotTreatVisibleRecipientAsStrongWithoutTarget(t *testing.T) {
	header := stdmail.Header{
		"To":           {`hidden.alias@icloud.com`},
		"Delivered-To": {`mango@mgbubu.com`},
	}
	aliases := map[string][]int64{"hidden.alias@icloud.com": {7}}
	account := domain.Account{MailboxType: domain.MailboxTypeICloud, Email: "primary@icloud.com"}
	if got := matchingAliasIDsForAccount(header, aliases, account, false); len(got) != 0 {
		t.Fatalf("unconfigured forwarding target matched alias: %#v", got)
	}
}

func TestICloudForwardedMailboxRejectsConflictingVisibleAlias(t *testing.T) {
	header := stdmail.Header{
		"To":           {`first.alias@icloud.com`},
		"Cc":           {`second.alias@icloud.com`},
		"Delivered-To": {`mango@mgbubu.com`},
	}
	aliases := map[string][]int64{
		"first.alias@icloud.com":  {1},
		"second.alias@icloud.com": {2},
	}
	account := domain.Account{
		MailboxType:  domain.MailboxTypeICloud,
		Email:        "primary@icloud.com",
		IMAPUsername: "mango@mgbubu.com",
	}
	if got := matchingAliasIDsForAccount(header, aliases, account, false); len(got) != 0 {
		t.Fatalf("conflicting forwarded aliases matched: %#v", got)
	}
}

func TestMatchingAliasIDsHandlesRawAppleHeaderCasing(t *testing.T) {
	header := stdmail.Header{
		"Original-recipient": {`rfc822;primary@icloud.com`},
		"To":                 {`Hide My Email <private.alias@icloud.com>`},
		"X-Icloud-Hme":       {`p=private.alias@icloud.com; d=; f=primary@icloud.com; r=to; s=sender@example.com`},
	}
	aliases := map[string][]int64{"private.alias@icloud.com": {7}}
	if got, determinate := classifyRecipientAlias(header, aliases, domain.Account{Email: "PRIMARY@iCloud.com"}, false); !determinate || got != 7 {
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
			}, domain.Account{Email: "primary@icloud.com"}, false); determinate || got != 0 {
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
	if got, determinate := classifyRecipientAlias(header, aliases, domain.Account{Email: "primary@icloud.com"}, false); determinate || got != 0 {
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
	}, domain.Account{Email: "primary@icloud.com"}, false); !determinate || got != 0 {
		t.Fatalf("unregistered HME classification = (%d, %v), want (0, true)", got, determinate)
	}
}

func TestICloudArchiveRecipientClassificationKeepsLegacyContract(t *testing.T) {
	header := stdmail.Header{
		"Original-Recipient": {`rfc822; primary@icloud.com`},
		"To":                 {`Hide My Email <hidden.alias@icloud.com>`},
		icloudHMEHeaderField: {`p=hidden.alias@icloud.com; f=primary@icloud.com; r=to`},
	}
	aliases := map[string][]int64{"hidden.alias@icloud.com": {7}}

	for _, mailboxType := range []string{"", domain.MailboxTypeICloud} {
		account := domain.Account{MailboxType: mailboxType, Email: "primary@icloud.com"}
		got, determinate := classifyArchiveRecipientAliases(header, aliases, account, false)
		if !determinate || !reflect.DeepEqual(got, []int64{7}) {
			t.Fatalf("mailbox type %q archive classification = (%#v, %v), want ([7], true)", mailboxType, got, determinate)
		}
	}
}

func TestCustomArchiveRecipientMatchesProvidedRawHeaders(t *testing.T) {
	raw := strings.Join([]string{
		"Return-Path: <bounces+20216706-0e27-ybl=mgbubu.com@em7877.tm.openai.com>",
		"Delivered-To: mango@mgbubu.com",
		"X-Original-To: ybl@mgbubu.com",
		"From: ChatGPT <noreply@tm.openai.com>",
		"To: ybl@mgbubu.com",
		"Subject: Your temporary ChatGPT verification code",
		"Content-Type: text/html; charset=utf-8",
		"Content-Transfer-Encoding: quoted-printable",
		"",
		"<p>191302</p>",
	}, "\r\n")
	message, err := stdmail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parse provided raw message headers: %v", err)
	}
	aliases := map[string][]int64{
		"ybl@mgbubu.com":   {1},
		"mango@mgbubu.com": {2},
	}
	account := domain.Account{
		MailboxType:  domain.MailboxTypeCustom,
		Email:        "custom@mgbubu.com",
		IMAPUsername: "mango@mgbubu.com",
	}

	got, determinate := classifyArchiveRecipientAliases(message.Header, aliases, account, false)
	if !determinate || !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("provided raw header classification = (%#v, %v), want ([1], true)", got, determinate)
	}
}

func TestCustomArchiveRecipientHeaderPriority(t *testing.T) {
	account := domain.Account{
		MailboxType:  domain.MailboxTypeCustom,
		Email:        "mango@example.com",
		IMAPUsername: "imap-login-is-not-a-route",
	}
	aliases := map[string][]int64{
		"first@example.com":  {1},
		"second@example.com": {2},
		"third@example.com":  {3},
	}
	tests := []struct {
		name   string
		header stdmail.Header
		want   []int64
	}{
		{
			name: "X-Original-To wins over physical delivery and visible recipient",
			header: stdmail.Header{
				"X-Original-To": {`first@example.com`},
				"Delivered-To":  {`second@example.com`},
				"To":            {`first@example.com`},
			},
			want: []int64{1},
		},
		{
			name: "Original-Recipient wins over envelope tier",
			header: stdmail.Header{
				"Original-Recipient": {`rfc822; first@example.com`},
				"Envelope-To":        {`second@example.com`},
				"Delivered-To":       {`third@example.com`},
			},
			want: []int64{1},
		},
		{
			name: "envelope tier wins over Delivered-To",
			header: stdmail.Header{
				"X-Envelope-To": {`second@example.com`},
				"Delivered-To":  {`third@example.com`},
			},
			want: []int64{2},
		},
		{
			name:   "Delivered-To is the last strong tier",
			header: stdmail.Header{"Delivered-To": {`third@example.com`}},
			want:   []int64{3},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, determinate := classifyArchiveRecipientAliases(test.header, aliases, account, false)
			if !determinate || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("custom archive classification = (%#v, %v), want (%#v, true)", got, determinate, test.want)
			}
		})
	}
}

func TestCustomRecipientClassificationIgnoresAppleHMERoute(t *testing.T) {
	aliases := map[string][]int64{
		"custom.alias@example.com": {1},
		"hidden.alias@icloud.com":  {7},
	}
	account := domain.Account{
		MailboxType: domain.MailboxTypeCustom,
		Email:       "primary@icloud.com",
	}

	for _, test := range []struct {
		name string
		hme  string
	}{
		{name: "valid Apple route", hme: `p=hidden.alias@icloud.com; f=primary@icloud.com; r=to`},
		{name: "malformed Apple route", hme: `not-an-apple-route`},
	} {
		t.Run(test.name, func(t *testing.T) {
			header := stdmail.Header{
				"X-Original-To":      {`custom.alias@example.com`},
				"To":                 {`Hide My Email <hidden.alias@icloud.com>`},
				icloudHMEHeaderField: {test.hme},
			}
			if got, determinate := classifyRecipientAlias(header, aliases, account, false); !determinate || got != 1 {
				t.Fatalf("custom classification = (%d, %v), want (1, true)", got, determinate)
			}
			if got, determinate := classifyArchiveRecipientAliases(header, aliases, account, false); !determinate || !reflect.DeepEqual(got, []int64{1}) {
				t.Fatalf("custom archive classification = (%#v, %v), want ([1], true)", got, determinate)
			}
		})
	}
}

func TestCustomRecipientClassificationDoesNotFallThroughAuthoritativeTier(t *testing.T) {
	account := domain.Account{MailboxType: domain.MailboxTypeCustom}
	tests := []struct {
		name    string
		header  stdmail.Header
		aliases map[string][]int64
	}{
		{
			name: "malformed higher tier",
			header: stdmail.Header{
				"X-Original-To": {`<broken`},
				"Delivered-To":  {`fallback@example.com`},
				"To":            {`fallback@example.com`},
			},
			aliases: map[string][]int64{"fallback@example.com": {1}},
		},
		{
			name: "unregistered higher tier",
			header: stdmail.Header{
				"X-Original-To": {`unregistered@example.com`},
				"Delivered-To":  {`fallback@example.com`},
			},
			aliases: map[string][]int64{"fallback@example.com": {1}},
		},
		{
			name: "address maps to multiple aliases",
			header: stdmail.Header{
				"X-Original-To": {`ambiguous@example.com`},
				"Delivered-To":  {`fallback@example.com`},
			},
			aliases: map[string][]int64{
				"ambiguous@example.com": {1, 2},
				"fallback@example.com":  {3},
			},
		},
		{
			name: "mixed registered and unregistered addresses",
			header: stdmail.Header{
				"X-Original-To": {`registered@example.com, unregistered@example.com`},
				"Delivered-To":  {`registered@example.com`},
			},
			aliases: map[string][]int64{"registered@example.com": {1}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, determinate := classifyArchiveRecipientAliases(test.header, test.aliases, account, true)
			if determinate || len(got) != 0 {
				t.Fatalf("unsafe higher tier classification = (%#v, %v), want (nil, false)", got, determinate)
			}
		})
	}
}

func TestCustomArchiveRecipientSupportsMultipleUniquelyMappedAliases(t *testing.T) {
	header := stdmail.Header{
		"X-Original-To": {`first@example.com, second@example.com`},
		"Delivered-To":  {`physical@example.com`},
	}
	aliases := map[string][]int64{
		"first@example.com":    {2},
		"second@example.com":   {1},
		"physical@example.com": {3},
	}
	account := domain.Account{MailboxType: domain.MailboxTypeCustom}

	got, determinate := classifyArchiveRecipientAliases(header, aliases, account, false)
	if !determinate || !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Fatalf("multi-alias classification = (%#v, %v), want ([1 2], true)", got, determinate)
	}
	if got, determinate := classifyRecipientAlias(header, aliases, account, false); determinate || got != 0 {
		t.Fatalf("single-alias classification accepted multiple routes: (%d, %v)", got, determinate)
	}
}

func TestCustomRecipientWeakHeadersRemainPolicyControlled(t *testing.T) {
	header := stdmail.Header{"To": {`alias@example.com`}}
	aliases := map[string][]int64{"alias@example.com": {1}}
	account := domain.Account{MailboxType: domain.MailboxTypeCustom}

	if got, determinate := classifyArchiveRecipientAliases(header, aliases, account, false); !determinate || len(got) != 0 {
		t.Fatalf("disabled weak headers = (%#v, %v), want (nil, true)", got, determinate)
	}
	if got, determinate := classifyArchiveRecipientAliases(header, aliases, account, true); !determinate || !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("enabled weak headers = (%#v, %v), want ([1], true)", got, determinate)
	}
}

func TestCustomRecipientDoesNotRouteReturnPathOrReceived(t *testing.T) {
	header := stdmail.Header{
		"Return-Path": {`<alias@example.com>`},
		"Received":    {`from sender.example by mx.example for <alias@example.com>`},
	}
	aliases := map[string][]int64{"alias@example.com": {1}}
	account := domain.Account{MailboxType: domain.MailboxTypeCustom}

	if got, determinate := classifyArchiveRecipientAliases(header, aliases, account, true); !determinate || len(got) != 0 {
		t.Fatalf("non-routing headers = (%#v, %v), want (nil, true)", got, determinate)
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
