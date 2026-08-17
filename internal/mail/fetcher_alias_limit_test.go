package mail

import (
	"errors"
	"fmt"
	"testing"

	"icloud-api/internal/domain"
)

func TestPrepareAliasesAllowsCustomMailboxBeyondICloudCapacity(t *testing.T) {
	t.Parallel()
	account := domain.Account{ID: 42, MailboxType: domain.MailboxTypeCustom}
	aliases := make([]domain.Alias, 0, domain.MaxEnabledAliasesPerAccount+1)
	for index := 0; index <= domain.MaxEnabledAliasesPerAccount; index++ {
		aliases = append(aliases, domain.Alias{
			ID:        int64(index + 1),
			AccountID: account.ID,
			Address:   fmt.Sprintf("alias%04d@custom.test", index),
			Enabled:   true,
		})
	}

	byAddress, err := prepareAliases(account, aliases, domain.MaxEnabledAliasesPerAccount)
	if err != nil {
		t.Fatalf("prepare custom aliases beyond iCloud capacity: %v", err)
	}
	if len(byAddress) != len(aliases) {
		t.Fatalf("prepared custom alias addresses = %d, want %d", len(byAddress), len(aliases))
	}
}

func TestPrepareAliasesRetainsICloudCapacity(t *testing.T) {
	t.Parallel()
	account := domain.Account{ID: 43, MailboxType: domain.MailboxTypeICloud}
	aliases := make([]domain.Alias, 0, domain.MaxEnabledAliasesPerAccount+1)
	for index := 0; index <= domain.MaxEnabledAliasesPerAccount; index++ {
		aliases = append(aliases, domain.Alias{
			ID:        int64(index + 1),
			AccountID: account.ID,
			Address:   fmt.Sprintf("alias%04d@icloud.test", index),
			Enabled:   true,
		})
	}

	_, err := prepareAliases(account, aliases, domain.MaxEnabledAliasesPerAccount)
	if !errors.Is(err, ErrTooManyAliases) {
		t.Fatalf("prepare iCloud aliases beyond capacity error = %v, want too many aliases", err)
	}
}
