package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr                      string
	DatabasePath              string
	MasterKeyFile             string
	AdminUsername             string
	AdminPassword             string
	CookieSecure              bool
	SessionTTL                time.Duration
	PollInterval              time.Duration
	IMAPTimeout               time.Duration
	SyncTimeout               time.Duration
	SyncConcurrency           int
	ShutdownTimeout           time.Duration
	MaxMessageBytes           int64
	MaxBodyBytes              int64
	AllowWeakRecipientHeaders bool
	TrustedProxies            []string
	GinMode                   string
}

func Load() (Config, error) {
	cfg := Config{
		Addr:          env("ICLOUD_API_ADDR", "127.0.0.1:8080"),
		DatabasePath:  env("ICLOUD_API_DB", "data/icloud-api.db"),
		AdminUsername: env("ICLOUD_API_ADMIN_USER", "admin"),
		AdminPassword: os.Getenv("ICLOUD_API_ADMIN_PASSWORD"),
		GinMode:       env("GIN_MODE", "release"),
	}
	cfg.MasterKeyFile = env("ICLOUD_API_MASTER_KEY_FILE", cfg.DatabasePath+".key")
	if value := strings.TrimSpace(os.Getenv("ICLOUD_API_TRUSTED_PROXIES")); value != "" {
		for _, proxy := range strings.Split(value, ",") {
			if proxy = strings.TrimSpace(proxy); proxy != "" {
				cfg.TrustedProxies = append(cfg.TrustedProxies, proxy)
			}
		}
	}

	var err error
	if cfg.CookieSecure, err = envBool("ICLOUD_API_COOKIE_SECURE", false); err != nil {
		return Config{}, err
	}
	if cfg.AllowWeakRecipientHeaders, err = envBool("ICLOUD_API_ALLOW_WEAK_RECIPIENT_HEADERS", false); err != nil {
		return Config{}, err
	}
	if cfg.SessionTTL, err = envDuration("ICLOUD_API_SESSION_TTL", 8*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.PollInterval, err = envDuration("ICLOUD_API_POLL_INTERVAL", time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.IMAPTimeout, err = envDuration("ICLOUD_API_IMAP_TIMEOUT", 25*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.SyncTimeout, err = envDuration("ICLOUD_API_SYNC_TIMEOUT", 2*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = envDuration("ICLOUD_API_SHUTDOWN_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.SyncConcurrency, err = envInt("ICLOUD_API_SYNC_CONCURRENCY", 3); err != nil {
		return Config{}, err
	}
	if cfg.MaxMessageBytes, err = envInt64("ICLOUD_API_MAX_MESSAGE_BYTES", 10<<20); err != nil {
		return Config{}, err
	}
	if cfg.MaxBodyBytes, err = envInt64("ICLOUD_API_MAX_BODY_BYTES", 1<<20); err != nil {
		return Config{}, err
	}

	if strings.TrimSpace(cfg.AdminUsername) == "" {
		return Config{}, fmt.Errorf("ICLOUD_API_ADMIN_USER 不能为空")
	}
	if cfg.SyncConcurrency < 1 || cfg.SyncConcurrency > 16 {
		return Config{}, fmt.Errorf("ICLOUD_API_SYNC_CONCURRENCY 必须在 1 到 16 之间")
	}
	if cfg.PollInterval < 10*time.Second {
		return Config{}, fmt.Errorf("ICLOUD_API_POLL_INTERVAL 不能短于 10s")
	}
	if cfg.SessionTTL < 5*time.Minute {
		return Config{}, fmt.Errorf("ICLOUD_API_SESSION_TTL 不能短于 5m")
	}
	if cfg.IMAPTimeout < time.Second || cfg.IMAPTimeout > 5*time.Minute {
		return Config{}, fmt.Errorf("ICLOUD_API_IMAP_TIMEOUT 必须在 1s 到 5m 之间")
	}
	if cfg.SyncTimeout < 10*time.Second || cfg.SyncTimeout > 30*time.Minute {
		return Config{}, fmt.Errorf("ICLOUD_API_SYNC_TIMEOUT 必须在 10s 到 30m 之间")
	}
	if cfg.ShutdownTimeout < time.Second {
		return Config{}, fmt.Errorf("ICLOUD_API_SHUTDOWN_TIMEOUT 不能短于 1s")
	}
	if cfg.MaxMessageBytes < 64<<10 || cfg.MaxMessageBytes > 100<<20 {
		return Config{}, fmt.Errorf("ICLOUD_API_MAX_MESSAGE_BYTES 必须在 64 KiB 到 100 MiB 之间")
	}
	if cfg.MaxBodyBytes < 1<<10 || cfg.MaxBodyBytes > cfg.MaxMessageBytes {
		return Config{}, fmt.Errorf("ICLOUD_API_MAX_BODY_BYTES 必须在 1 KiB 到邮件上限之间")
	}
	return cfg, nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s 不是有效布尔值: %w", name, err)
	}
	return parsed, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s 不是有效时长: %w", name, err)
	}
	return parsed, nil
}

func envInt(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s 不是有效整数: %w", name, err)
	}
	return parsed, nil
}

func envInt64(name string, fallback int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s 不是有效整数: %w", name, err)
	}
	return parsed, nil
}
