package config

import (
	"strings"
	"testing"
	"time"
)

var configEnvironment = []string{
	"ICLOUD_API_ADDR",
	"ICLOUD_API_DB",
	"ICLOUD_API_WEB_ROOT",
	"ICLOUD_API_MASTER_KEY_FILE",
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
	if cfg.SyncTimeout != 2*time.Minute {
		t.Fatalf("默认同步总时限 = %v, want %v", cfg.SyncTimeout, 2*time.Minute)
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
