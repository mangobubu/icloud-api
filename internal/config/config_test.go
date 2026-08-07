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
