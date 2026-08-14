package publicimap

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapserver"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
)

type credentialModeBoundaryRepo struct {
	binding domain.MailboxBinding
}

func (r credentialModeBoundaryRepo) GetMailboxBindingByAddress(context.Context, string) (domain.MailboxBinding, error) {
	return r.binding, nil
}

func (r credentialModeBoundaryRepo) GetMailboxBindingByIMAPPasswordHash(context.Context, []byte) (domain.MailboxBinding, error) {
	return r.binding, nil
}

func (credentialModeBoundaryRepo) ListArchivedMailboxMessages(context.Context, int64) ([]domain.ArchivedMailboxMessage, error) {
	return nil, nil
}

func (credentialModeBoundaryRepo) OpenArchivedContent(domain.ArchivedMailboxMessage) (*os.File, error) {
	return nil, errors.New("content not needed in credential boundary test")
}

func TestLegacyCredentialModeRejectedByPublicIMAPAuth(t *testing.T) {
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x44}, 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 3, 0, 0, 0, time.UTC)
	refreshHash := secure.HashToken("icr_boundary_refresh")
	binding := domain.MailboxBinding{
		Alias: domain.Alias{
			ID:                41,
			Address:           "legacy-imap-boundary@icloud.com",
			CredentialMode:    domain.AliasCredentialModeLegacy,
			CredentialVersion: 1,
			IMAPPasswordHash:  secure.HashToken("imp_boundary_password"),
			RefreshTokenHash:  refreshHash,
			Enabled:           true,
		},
		Account: domain.Account{ID: 9, Enabled: true},
	}
	repo := credentialModeBoundaryRepo{binding: binding}
	session := NewSession(repo, cipher)
	session.now = func() time.Time { return now }
	if err := session.Login(binding.Alias.Address, "imp_boundary_password"); err == nil {
		t.Fatal("legacy IMAP password unexpectedly authenticated")
	}
	token, err := cipher.IssueAliasAccessToken(binding.Alias.ID, binding.Alias.CredentialVersion, refreshHash, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.loginAccessToken(binding.Alias.Address, token); err == nil {
		t.Fatal("legacy XOAUTH2 token unexpectedly authenticated")
	}
}

func TestV2CredentialModeStillAuthenticatesPublicIMAP(t *testing.T) {
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x45}, 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 3, 0, 0, 0, time.UTC)
	refreshHash := secure.HashToken("icr_v2_boundary_refresh")
	binding := domain.MailboxBinding{
		Alias: domain.Alias{
			ID:                42,
			Address:           "v2-imap-boundary@icloud.com",
			CredentialMode:    domain.AliasCredentialModeV2,
			CredentialVersion: 1,
			IMAPPasswordHash:  secure.HashToken("imp_v2_boundary_password"),
			RefreshTokenHash:  refreshHash,
			Enabled:           true,
		},
		Account: domain.Account{ID: 10, Enabled: true},
	}
	repo := credentialModeBoundaryRepo{binding: binding}
	session := NewSession(repo, cipher)
	session.now = func() time.Time { return now }
	if err := session.Login(binding.Alias.Address, "imp_v2_boundary_password"); err != nil {
		t.Fatalf("v2 IMAP password rejected: %v", err)
	}
	token, err := cipher.IssueAliasAccessToken(binding.Alias.ID, binding.Alias.CredentialVersion, refreshHash, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.loginAccessToken(binding.Alias.Address, token); err != nil {
		t.Fatalf("v2 XOAUTH2 token rejected: %v", err)
	}
}

type mutableCredentialModeBoundaryRepo struct {
	binding domain.MailboxBinding
}

func (r *mutableCredentialModeBoundaryRepo) GetMailboxBindingByAddress(context.Context, string) (domain.MailboxBinding, error) {
	return r.binding, nil
}

func (r *mutableCredentialModeBoundaryRepo) GetMailboxBindingByIMAPPasswordHash(context.Context, []byte) (domain.MailboxBinding, error) {
	return r.binding, nil
}

func (r *mutableCredentialModeBoundaryRepo) ListArchivedMailboxMessages(context.Context, int64) ([]domain.ArchivedMailboxMessage, error) {
	return nil, nil
}

func (r *mutableCredentialModeBoundaryRepo) OpenArchivedContent(domain.ArchivedMailboxMessage) (*os.File, error) {
	return nil, errors.New("content not needed in credential mode refresh test")
}

func TestAuthenticatedPublicIMAPSessionRejectsCredentialModeDowngrade(t *testing.T) {
	binding := domain.MailboxBinding{
		Alias: domain.Alias{
			ID:                43,
			AccountID:         11,
			Address:           "refresh-mode-boundary@icloud.com",
			CredentialMode:    domain.AliasCredentialModeV2,
			CredentialVersion: 1,
			IMAPPasswordHash:  secure.HashToken("refresh_mode_password"),
			Enabled:           true,
		},
		Account: domain.Account{ID: 11, Enabled: true},
	}
	repo := &mutableCredentialModeBoundaryRepo{binding: binding}
	session := NewSession(repo, nil)
	if err := session.Login(binding.Alias.Address, "refresh_mode_password"); err != nil {
		t.Fatalf("initial v2 login failed: %v", err)
	}

	repo.binding.Alias.CredentialMode = domain.AliasCredentialModeLegacy
	if _, err := session.Select("INBOX", nil); !errors.Is(err, imapserver.ErrAuthFailed) {
		t.Fatalf("session continued after credential mode downgrade: %v", err)
	}
}

func TestAuthenticatedPublicIMAPSessionRejectsReplacementAlias(t *testing.T) {
	binding := domain.MailboxBinding{
		Alias: domain.Alias{
			ID:                44,
			AccountID:         12,
			Address:           "replacement-boundary@icloud.com",
			CredentialMode:    domain.AliasCredentialModeV2,
			CredentialVersion: 1,
			IMAPPasswordHash:  secure.HashToken("replacement_password"),
			Enabled:           true,
		},
		Account: domain.Account{ID: 12, Enabled: true},
	}
	repo := &mutableCredentialModeBoundaryRepo{binding: binding}
	session := NewSession(repo, nil)
	if err := session.Login(binding.Alias.Address, "replacement_password"); err != nil {
		t.Fatalf("initial v2 login failed: %v", err)
	}

	repo.binding.Alias.ID++
	if _, err := session.Select("INBOX", nil); !errors.Is(err, imapserver.ErrAuthFailed) {
		t.Fatalf("session continued after alias replacement: %v", err)
	}
}
