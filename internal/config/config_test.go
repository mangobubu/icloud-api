package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var configEnvironment = []string{
	"ICLOUD_API_ADDR",
	"ICLOUD_API_DATABASE_URL",
	"ICLOUD_API_LEGACY_SQLITE",
	"ICLOUD_API_WEB_ROOT",
	"ICLOUD_API_MASTER_KEY_FILE",
	"ICLOUD_API_OAUTH_TOKEN",
	"ICLOUD_API_ADMIN_USER",
	"ICLOUD_API_ADMIN_PASSWORD",
	"ICLOUD_API_COOKIE_SECURE",
	"ICLOUD_API_SESSION_TTL",
	"ICLOUD_API_POLL_INTERVAL",
	"ICLOUD_API_IMAP_TIMEOUT",
	"ICLOUD_API_SYNC_TIMEOUT",
	"ICLOUD_API_SYNC_CONCURRENCY",
	"ICLOUD_API_SHUTDOWN_TIMEOUT",
	"ICLOUD_API_MAX_MESSAGE_BYTES",
	"ICLOUD_API_MAX_BODY_BYTES",
	"ICLOUD_API_ALLOW_WEAK_RECIPIENT_HEADERS",
	"ICLOUD_API_TRUSTED_PROXIES",
	"GIN_MODE",
	"TZ",
}

func TestDatabaseURLDefault(t *testing.T) {
	clearConfigEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	const want = "postgres://icloud_api@/icloud_api?host=/var/run/postgresql&sslmode=disable"
	if cfg.DatabaseURL != want {
		t.Fatalf("默认数据库 URL = %q, want %q", cfg.DatabaseURL, want)
	}
}

func TestLegacySQLitePathConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LegacySQLitePath != "" {
		t.Fatalf("默认旧 SQLite 路径 = %q, want empty", cfg.LegacySQLitePath)
	}

	t.Setenv("ICLOUD_API_LEGACY_SQLITE", "  /app/legacy/icloud-api.db  ")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LegacySQLitePath != "/app/legacy/icloud-api.db" {
		t.Fatalf("旧 SQLite 路径 = %q, want 已去除首尾空白的配置值", cfg.LegacySQLitePath)
	}
}

func TestDatabaseURLOverride(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("ICLOUD_API_DATABASE_URL", "  postgres://app@db/app?sslmode=disable  ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgres://app@db/app?sslmode=disable" {
		t.Fatalf("数据库 URL = %q, want 已去除首尾空白的配置值", cfg.DatabaseURL)
	}
}

func TestDatabaseURLValidation(t *testing.T) {
	for _, value := range []string{
		"data/icloud-api.db",
		"file:data/icloud-api.db",
		"sqlite://data/icloud-api.db",
		"postgres:data/icloud-api.db",
		"postgresql:data/icloud-api.db",
		"postgres:/data/icloud-api.db",
		"postgresql:/data/icloud-api.db",
		"POSTGRES:data/icloud-api.db",
		"postgres://%zz",
		"://invalid",
	} {
		t.Run(value, func(t *testing.T) {
			clearConfigEnvironment(t)
			t.Setenv("ICLOUD_API_DATABASE_URL", value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "ICLOUD_API_DATABASE_URL") {
				t.Fatalf("ICLOUD_API_DATABASE_URL=%q 错误 = %v", value, err)
			}
		})
	}
}

func TestPostgreSQLSchemeIsAccepted(t *testing.T) {
	for _, value := range []string{
		"postgresql://app@db/app?sslmode=disable",
		"POSTGRES://app@db/app?sslmode=disable",
		"postgres://app@/app?host=/var/run/postgresql&sslmode=disable",
	} {
		t.Run(value, func(t *testing.T) {
			clearConfigEnvironment(t)
			t.Setenv("ICLOUD_API_DATABASE_URL", value)
			if _, err := Load(); err != nil {
				t.Fatalf("合法 PostgreSQL 数据库 URL %q 不应被拒绝: %v", value, err)
			}
		})
	}
}

func TestMasterKeyFileDefaultDoesNotDependOnDatabaseURL(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("ICLOUD_API_DATABASE_URL", "postgres://app@db/app?sslmode=disable")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MasterKeyFile != "data/master.key" {
		t.Fatalf("默认主密钥文件 = %q, want %q", cfg.MasterKeyFile, "data/master.key")
	}
}

func TestOAuthTokenConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("ICLOUD_API_OAUTH_TOKEN", "  0123456789abcdef0123456789abcdef  ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OAuthToken != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("OAuth 令牌 = %q, want 已去除首尾空白的配置值", cfg.OAuthToken)
	}
}

func TestOAuthTokenLengthValidation(t *testing.T) {
	for _, value := range []string{"too-short", strings.Repeat("x", 4097), strings.Repeat("x", 31) + " " + strings.Repeat("y", 31)} {
		t.Run(fmt.Sprintf("length_%d", len(value)), func(t *testing.T) {
			clearConfigEnvironment(t)
			t.Setenv("ICLOUD_API_OAUTH_TOKEN", value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "ICLOUD_API_OAUTH_TOKEN") {
				t.Fatalf("ICLOUD_API_OAUTH_TOKEN 长度错误 = %v", err)
			}
		})
	}
}

func TestWebRootOverride(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("ICLOUD_API_WEB_ROOT", "  /app/web  ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebRoot != "/app/web" {
		t.Fatalf("前端目录 = %q, want %q", cfg.WebRoot, "/app/web")
	}
}

func TestPollIntervalBoundaries(t *testing.T) {
	for _, value := range []string{"10s", "24h"} {
		t.Run(value, func(t *testing.T) {
			clearConfigEnvironment(t)
			t.Setenv("ICLOUD_API_POLL_INTERVAL", value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("ICLOUD_API_POLL_INTERVAL=%q 不应被拒绝: %v", value, err)
			}
			if 3*cfg.PollInterval > 72*time.Hour {
				t.Fatalf("三倍轮询周期 = %v, want 不超过 72h", 3*cfg.PollInterval)
			}
		})
	}
}

func TestPollIntervalValidation(t *testing.T) {
	for _, value := range []string{
		"9.999999999s",
		"24h0.000000001s",
		"2562047h47m16.854775807s",
		"not-a-duration",
	} {
		t.Run(value, func(t *testing.T) {
			clearConfigEnvironment(t)
			t.Setenv("ICLOUD_API_POLL_INTERVAL", value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "ICLOUD_API_POLL_INTERVAL") {
				t.Fatalf("ICLOUD_API_POLL_INTERVAL=%q 错误 = %v", value, err)
			}
		})
	}
}

func clearConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range configEnvironment {
		t.Setenv(name, "")
	}
}

func TestSyncTimeoutDefault(t *testing.T) {
	clearConfigEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SyncTimeout != 10*time.Minute {
		t.Fatalf("默认同步总时限 = %v, want %v", cfg.SyncTimeout, 10*time.Minute)
	}
}

func TestSyncTimeoutOverride(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("ICLOUD_API_IMAP_TIMEOUT", "5s")
	t.Setenv("ICLOUD_API_SYNC_TIMEOUT", "17s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SyncTimeout != 17*time.Second {
		t.Fatalf("同步总时限 = %v, want %v", cfg.SyncTimeout, 17*time.Second)
	}
}

func TestSyncTimeoutValidation(t *testing.T) {
	for _, value := range []string{"9s", "30m1s", "not-a-duration"} {
		t.Run(value, func(t *testing.T) {
			clearConfigEnvironment(t)
			t.Setenv("ICLOUD_API_SYNC_TIMEOUT", value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "ICLOUD_API_SYNC_TIMEOUT") {
				t.Fatalf("ICLOUD_API_SYNC_TIMEOUT=%q 错误 = %v", value, err)
			}
		})
	}
}

func TestSyncTimeoutMustCoverTwoIMAPTimeouts(t *testing.T) {
	for _, syncTimeout := range []string{"24s", "25s", "49s"} {
		t.Run(syncTimeout, func(t *testing.T) {
			clearConfigEnvironment(t)
			t.Setenv("ICLOUD_API_IMAP_TIMEOUT", "25s")
			t.Setenv("ICLOUD_API_SYNC_TIMEOUT", syncTimeout)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "ICLOUD_API_SYNC_TIMEOUT 必须至少为 ICLOUD_API_IMAP_TIMEOUT 的两倍") {
				t.Fatalf("不一致的同步/IMAP 超时错误 = %v", err)
			}
		})
	}
}

func TestSyncTimeoutAllowsTwiceIMAPTimeout(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("ICLOUD_API_IMAP_TIMEOUT", "25s")
	t.Setenv("ICLOUD_API_SYNC_TIMEOUT", "50s")
	if _, err := Load(); err != nil {
		t.Fatalf("两倍 IMAP 超时的同步总时限不应被拒绝: %v", err)
	}
}

func TestTimezoneDefault(t *testing.T) {
	clearConfigEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timezone != time.Local {
		t.Fatalf("默认时区 = %v, want time.Local", cfg.Timezone)
	}
}

func TestTimezoneOverride(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("TZ", "  Asia/Shanghai  ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timezone == nil || cfg.Timezone.String() != "Asia/Shanghai" {
		t.Fatalf("时区 = %v, want Asia/Shanghai", cfg.Timezone)
	}
	_, offset := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC).In(cfg.Timezone).Zone()
	if offset != 8*60*60 {
		t.Fatalf("Asia/Shanghai UTC 偏移 = %d, want %d", offset, 8*60*60)
	}
}

func TestTimezoneValidation(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("TZ", "not/a-real-timezone")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "TZ") {
		t.Fatalf("无效 TZ 错误 = %v, want 包含 TZ", err)
	}
}
