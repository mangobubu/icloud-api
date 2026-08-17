package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestMailGroupLifecycleMovesAndUngroupsAliases(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	account := createAccount(t, ctx, db, "group owner", "group-owner@icloud.com")
	first := createAlias(t, ctx, db, account.ID, "group-first@icloud.com", []byte("group-first-hash"))
	second := createAlias(t, ctx, db, account.ID, "group-second@icloud.com", []byte("group-second-hash"))

	group, err := db.CreateMailGroup(ctx, "工作")
	if err != nil {
		t.Fatalf("create mail group: %v", err)
	}
	if _, err := db.CreateMailGroup(ctx, " 工作 "); !errors.Is(err, store.ErrMailGroupNameExists) {
		t.Fatalf("duplicate mail group error = %v, want ErrMailGroupNameExists", err)
	}
	if _, err := db.CreateMailGroup(ctx, strings.Repeat("组", 101)); !errors.Is(err, store.ErrMailGroupNameTooLong) {
		t.Fatalf("long mail group error = %v, want ErrMailGroupNameTooLong", err)
	}
	groupID := group.ID
	if err := db.SetAliasesGroup(ctx, []int64{first.ID, second.ID, first.ID}, &groupID); err != nil {
		t.Fatalf("move aliases to group: %v", err)
	}
	listed, err := db.ListMailGroups(ctx)
	if err != nil {
		t.Fatalf("list mail groups: %v", err)
	}
	if len(listed) != 1 || listed[0].AliasCount != 2 {
		t.Fatalf("listed groups = %#v", listed)
	}
	got, err := db.GetAlias(ctx, first.ID)
	if err != nil {
		t.Fatalf("get grouped alias: %v", err)
	}
	if got.GroupID == nil || *got.GroupID != group.ID || got.GroupName != "工作" {
		t.Fatalf("grouped alias = %#v", got)
	}

	page, err := db.ListAliasesPage(ctx, store.AliasListFilter{GroupID: &groupID, Limit: 20})
	if err != nil {
		t.Fatalf("list aliases by group: %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("group alias page = %#v", page)
	}
	if err := db.DeleteMailGroup(ctx, group.ID); err != nil {
		t.Fatalf("delete mail group: %v", err)
	}
	if _, err := db.SetAliasGroup(ctx, first.ID, &groupID); !errors.Is(err, store.ErrMailGroupNotFound) {
		t.Fatalf("single move to deleted group error = %v, want ErrMailGroupNotFound", err)
	}
	if err := db.SetAliasesGroup(ctx, []int64{first.ID, second.ID}, &groupID); !errors.Is(err, store.ErrMailGroupNotFound) {
		t.Fatalf("batch move to deleted group error = %v, want ErrMailGroupNotFound", err)
	}
	ungrouped, err := db.ListAliasesPage(ctx, store.AliasListFilter{Ungrouped: true, Limit: 20})
	if err != nil {
		t.Fatalf("list ungrouped aliases: %v", err)
	}
	if ungrouped.Total != 2 {
		t.Fatalf("ungrouped aliases = %#v", ungrouped)
	}
}

func TestCreateAliasCanStartInMailGroup(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	account := createAccount(t, ctx, db, "group create owner", "group-create-owner@icloud.com")
	group, err := db.CreateMailGroup(ctx, "注册")
	if err != nil {
		t.Fatalf("create mail group: %v", err)
	}
	alias, err := db.CreateAlias(ctx, domain.Alias{
		AccountID:  account.ID,
		Address:    "group-create-alias@icloud.com",
		GroupID:    &group.ID,
		APIKeyHash: []byte("group-create-alias-hash"),
		Enabled:    true,
	})
	if err != nil {
		t.Fatalf("create alias in group: %v", err)
	}
	if alias.GroupID == nil || *alias.GroupID != group.ID || alias.GroupName != group.Name {
		t.Fatalf("created grouped alias = %#v", alias)
	}
}

func TestMailGroupNamesUseUnicodeNormalizedCaseKey(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	group, err := db.CreateMailGroup(ctx, "Équipe")
	if err != nil {
		t.Fatalf("create Unicode mail group: %v", err)
	}
	got, err := db.GetMailGroupByName(ctx, " E\u0301QUIPE ")
	if err != nil {
		t.Fatalf("get mail group by normalized name: %v", err)
	}
	if got.ID != group.ID {
		t.Fatalf("normalized lookup group ID = %d, want %d", got.ID, group.ID)
	}
	if _, err := db.CreateMailGroup(ctx, "e\u0301quipe"); !errors.Is(err, store.ErrMailGroupNameExists) {
		t.Fatalf("Unicode duplicate error = %v, want ErrMailGroupNameExists", err)
	}

	widthGroup, err := db.CreateMailGroup(ctx, "Ａｌｐｈａ")
	if err != nil {
		t.Fatalf("create compatibility-width mail group: %v", err)
	}
	if _, err := db.CreateMailGroup(ctx, "alpha"); !errors.Is(err, store.ErrMailGroupNameExists) {
		t.Fatalf("compatibility-width duplicate error = %v, want ErrMailGroupNameExists", err)
	}
	other, err := db.CreateMailGroup(ctx, "Other")
	if err != nil {
		t.Fatalf("create rename source group: %v", err)
	}
	if _, err := db.UpdateMailGroup(ctx, other.ID, "ÉQUIPE"); !errors.Is(err, store.ErrMailGroupNameExists) {
		t.Fatalf("Unicode rename duplicate error = %v, want ErrMailGroupNameExists", err)
	}

	var storedKey string
	if err := db.DB().QueryRowContext(ctx,
		`SELECT name_key FROM mail_groups WHERE id = ?`, widthGroup.ID).Scan(&storedKey); err != nil {
		t.Fatalf("read stored mail group name key: %v", err)
	}
	if storedKey != "alpha" {
		t.Fatalf("stored mail group name key = %q, want %q", storedKey, "alpha")
	}
}

func TestMailGroupNameKeyConvergenceBackfillsExistingV8Database(t *testing.T) {
	ctx := context.Background()
	databasePath := createLegacyMailGroupNameFixture(t, "Ｃａｆｅ\u0301")

	db, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("open and converge legacy mail groups: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close converged store: %v", err)
		}
	})

	var nameKey string
	if err := db.DB().QueryRowContext(ctx,
		`SELECT name_key FROM mail_groups WHERE name = ?`, "Ｃａｆｅ\u0301").Scan(&nameKey); err != nil {
		t.Fatalf("read backfilled name key: %v", err)
	}
	if nameKey != "café" {
		t.Fatalf("backfilled name key = %q, want %q", nameKey, "café")
	}
	if _, err := db.CreateMailGroup(ctx, "CAFÉ"); !errors.Is(err, store.ErrMailGroupNameExists) {
		t.Fatalf("backfilled duplicate error = %v, want ErrMailGroupNameExists", err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO mail_groups(name, name_key, created_at, updated_at)
		VALUES('missing key', NULL, ?, ?)`, time.Now().UTC().UnixNano(), time.Now().UTC().UnixNano()); err == nil {
		t.Fatal("converged mail_groups accepted a NULL name_key on insert")
	}
	if _, err := db.DB().ExecContext(ctx,
		`UPDATE mail_groups SET name_key = NULL WHERE name = ?`, "Ｃａｆｅ\u0301"); err == nil {
		t.Fatal("converged mail_groups accepted a NULL name_key on update")
	}
}

func TestMailGroupNameKeyConvergenceReportsUnicodeCollision(t *testing.T) {
	databasePath := createLegacyMailGroupNameFixture(t, "Équipe", "e\u0301quipe")

	_, err := store.Open(databasePath)
	if err == nil {
		t.Fatal("converge legacy mail groups unexpectedly succeeded")
	}
	for _, wanted := range []string{
		"mail group name normalization conflict",
		"Équipe",
		"e\u0301quipe",
		"rename one group before retrying migration",
	} {
		if !strings.Contains(err.Error(), wanted) {
			t.Errorf("migration error %q does not contain %q", err, wanted)
		}
	}
}

func createLegacyMailGroupNameFixture(t *testing.T, names ...string) string {
	t.Helper()
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-mail-groups.db")
	db, err := store.Open(databasePath)
	if err != nil {
		t.Fatalf("create current mail group fixture: %v", err)
	}
	closeStore := func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close mail group fixture: %v", err)
		}
	}
	for _, statement := range []string{
		`DROP TRIGGER mail_groups_name_key_not_null_insert`,
		`DROP TRIGGER mail_groups_name_key_not_null_update`,
		`DROP INDEX mail_groups_name_key_uidx`,
		`ALTER TABLE mail_groups DROP COLUMN name_key`,
		`CREATE UNIQUE INDEX mail_groups_legacy_name_uidx ON mail_groups(name COLLATE NOCASE)`,
	} {
		if _, err := db.DB().ExecContext(ctx, statement); err != nil {
			closeStore()
			t.Fatalf("prepare legacy mail group schema with %q: %v", statement, err)
		}
	}
	now := time.Now().UTC().UnixNano()
	for _, name := range names {
		if _, err := db.DB().ExecContext(ctx, `
			INSERT INTO mail_groups(name, created_at, updated_at) VALUES(?, ?, ?)`, name, now, now); err != nil {
			closeStore()
			t.Fatalf("insert legacy mail group %q: %v", name, err)
		}
	}
	closeStore()
	return databasePath
}
