package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"icloud-api/internal/store"
)

const (
	minPollInterval = 10 * time.Second
	maxPollInterval = 24 * time.Hour
)

type Config struct {
	Addr             string
	DatabaseURL      string
	LegacySQLitePath string
	WebRoot          string
	MasterKeyFile    string
	// OAuthToken authenticates the legacy external alias-registration API.
	// It is hashed into the HTTP server at startup and never retained in cfg.
	OAuthToken                string
	AdminPath                 string
	AdminPathFile             string
	MailContentDir            string
	MailContentLimitBytes     int64
	PublicIMAPAddr            string
	PublicIMAPServerName      string
	PublicIMAPTLSCertFile     string
	PublicIMAPTLSKeyFile      string
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
	Timezone                  *time.Location
}

func Load() (Config, error) {
	cfg := Config{
		Addr:                  env("ICLOUD_API_ADDR", "127.0.0.1:8080"),
		DatabaseURL:           env("ICLOUD_API_DATABASE_URL", "postgres://icloud_api@/icloud_api?host=/var/run/postgresql&sslmode=disable"),
		LegacySQLitePath:      strings.TrimSpace(os.Getenv("ICLOUD_API_LEGACY_SQLITE")),
		WebRoot:               strings.TrimSpace(os.Getenv("ICLOUD_API_WEB_ROOT")),
		OAuthToken:            strings.TrimSpace(os.Getenv("ICLOUD_API_OAUTH_TOKEN")),
		AdminPath:             strings.TrimSpace(os.Getenv("ICLOUD_API_ADMIN_PATH")),
		AdminPathFile:         env("ICLOUD_API_ADMIN_PATH_FILE", "/app/keys/admin-path"),
		MailContentDir:        env("ICLOUD_API_MAIL_CONTENT_DIR", "/app/mail-archive"),
		PublicIMAPAddr:        env("ICLOUD_API_PUBLIC_IMAP_ADDR", "127.0.0.1:1993"),
		PublicIMAPServerName:  env("ICLOUD_API_PUBLIC_IMAP_SERVER_NAME", "localhost"),
		PublicIMAPTLSCertFile: strings.TrimSpace(os.Getenv("ICLOUD_API_PUBLIC_IMAP_TLS_CERT_FILE")),
		PublicIMAPTLSKeyFile:  strings.TrimSpace(os.Getenv("ICLOUD_API_PUBLIC_IMAP_TLS_KEY_FILE")),
		AdminUsername:         env("ICLOUD_API_ADMIN_USER", "admin"),
		AdminPassword:         os.Getenv("ICLOUD_API_ADMIN_PASSWORD"),
		GinMode:               env("GIN_MODE", "release"),
		Timezone:              time.Local,
	}
	if value := strings.TrimSpace(os.Getenv("TZ")); value != "" {
		location, err := time.LoadLocation(value)
		if err != nil {
			return Config{}, fmt.Errorf("TZ 不是有效时区: %w", err)
		}
		cfg.Timezone = location
	}
	cfg.MasterKeyFile = env("ICLOUD_API_MASTER_KEY_FILE", "data/master.key")
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
	if cfg.PollInterval, err = envDuration("ICLOUD_API_POLL_INTERVAL", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.IMAPTimeout, err = envDuration("ICLOUD_API_IMAP_TIMEOUT", 8*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.SyncTimeout, err = envDuration("ICLOUD_API_SYNC_TIMEOUT", 70*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = envDuration("ICLOUD_API_SHUTDOWN_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.SyncConcurrency, err = envInt("ICLOUD_API_SYNC_CONCURRENCY", 3); err != nil {
		return Config{}, err
	}
	if cfg.MaxMessageBytes, err = envInt64("ICLOUD_API_MAX_MESSAGE_BYTES", 100<<20); err != nil {
		return Config{}, err
	}
	if cfg.MaxBodyBytes, err = envInt64("ICLOUD_API_MAX_BODY_BYTES", 512<<10); err != nil {
		return Config{}, err
	}
	if cfg.MailContentLimitBytes, err = envInt64("ICLOUD_API_MAIL_CONTENT_LIMIT_BYTES", 10<<30); err != nil {
		return Config{}, err
	}

	if strings.TrimSpace(cfg.AdminUsername) == "" {
		return Config{}, fmt.Errorf("ICLOUD_API_ADMIN_USER 不能为空")
	}
	if err := store.ValidatePostgresURL(cfg.DatabaseURL); err != nil {
		return Config{}, fmt.Errorf("ICLOUD_API_DATABASE_URL 必须使用合法的 postgres:// 或 postgresql:// URL")
	}
	if cfg.OAuthToken != "" && (len(cfg.OAuthToken) < 32 || len(cfg.OAuthToken) > 4096 || strings.ContainsAny(cfg.OAuthToken, " \t\r\n")) {
		return Config{}, fmt.Errorf("ICLOUD_API_OAUTH_TOKEN 必须为 32 到 4096 个不含空白的字符")
	}
	if cfg.SyncConcurrency < 1 || cfg.SyncConcurrency > 16 {
		return Config{}, fmt.Errorf("ICLOUD_API_SYNC_CONCURRENCY 必须在 1 到 16 之间")
	}
	if cfg.PollInterval < minPollInterval || cfg.PollInterval > maxPollInterval {
		return Config{}, fmt.Errorf("ICLOUD_API_POLL_INTERVAL 必须在 10s 到 24h 之间")
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
	if cfg.SyncTimeout < 2*cfg.IMAPTimeout {
		return Config{}, fmt.Errorf(
			"ICLOUD_API_SYNC_TIMEOUT 必须至少为 ICLOUD_API_IMAP_TIMEOUT 的两倍（当前分别为 %s 和 %s）",
			cfg.SyncTimeout,
			cfg.IMAPTimeout,
		)
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
	if cfg.MailContentLimitBytes < 1<<20 || cfg.MailContentLimitBytes > 1<<50 {
		return Config{}, fmt.Errorf("ICLOUD_API_MAIL_CONTENT_LIMIT_BYTES 必须在 1 MiB 到 1 PiB 之间")
	}
	if strings.TrimSpace(cfg.MailContentDir) == "" {
		return Config{}, fmt.Errorf("ICLOUD_API_MAIL_CONTENT_DIR 不能为空")
	}
	if strings.TrimSpace(cfg.AdminPathFile) == "" {
		return Config{}, fmt.Errorf("ICLOUD_API_ADMIN_PATH_FILE 不能为空")
	}
	if _, _, err := net.SplitHostPort(cfg.PublicIMAPAddr); err != nil {
		return Config{}, fmt.Errorf("ICLOUD_API_PUBLIC_IMAP_ADDR 必须是 host:port")
	}
	if cfg.PublicIMAPServerName == "" || strings.ContainsAny(cfg.PublicIMAPServerName, " \t\r\n") {
		return Config{}, fmt.Errorf("ICLOUD_API_PUBLIC_IMAP_SERVER_NAME 必须是非空且不含空白的名称")
	}
	if (cfg.PublicIMAPTLSCertFile == "") != (cfg.PublicIMAPTLSKeyFile == "") {
		return Config{}, fmt.Errorf("公开 IMAPS 证书与私钥必须同时配置")
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
