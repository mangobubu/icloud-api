package httpserver

import (
	"testing"

	"icloud-api/internal/domain"
)

func TestAdminAPIAccountInputUsesReceiveRoutePasswordContract(t *testing.T) {
	tests := []struct {
		name         string
		base         domain.Account
		username     string
		password     string
		wantPassword string
		wantMessage  string
	}{
		{
			name: "direct iCloud trims App-specific password",
			base: domain.Account{
				MailboxType: domain.MailboxTypeICloud,
				Email:       "owner@icloud.com",
				IMAPHost:    domain.DefaultIMAPHost,
				IMAPPort:    domain.DefaultIMAPPort,
			},
			username:     "owner@icloud.com",
			password:     "  app-password  ",
			wantPassword: "app-password",
		},
		{
			name: "forwarded iCloud preserves third-party IMAP password",
			base: domain.Account{
				MailboxType: domain.MailboxTypeICloud,
				Email:       "owner@icloud.com",
				IMAPHost:    "mgbubu.com",
				IMAPPort:    993,
			},
			username:     "mango@mgbubu.com",
			password:     "  imap-password  ",
			wantPassword: "  imap-password  ",
		},
		{
			name: "direct iCloud keeps App-specific missing diagnostic",
			base: domain.Account{
				MailboxType: domain.MailboxTypeICloud,
				Email:       "owner@icloud.com",
				IMAPHost:    domain.DefaultIMAPHost,
				IMAPPort:    domain.DefaultIMAPPort,
			},
			username:    "owner@icloud.com",
			password:    "   ",
			wantMessage: "请填写 App 专用密码",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, password, message := adminAPIAccountInputWithMailbox(
				"", test.base.Email, test.username, test.password, nil, nil, test.base,
			)
			if password != test.wantPassword || message != test.wantMessage {
				t.Fatalf("password contract = (%q, %q), want (%q, %q)",
					password, message, test.wantPassword, test.wantMessage)
			}
		})
	}
}
