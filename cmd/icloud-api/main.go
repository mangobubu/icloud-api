package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"icloud-api/internal/config"
	"icloud-api/internal/httpserver"
	mailfetch "icloud-api/internal/mail"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
	"icloud-api/internal/syncer"
)

func main() {
	var err error
	switch {
	case len(os.Args) == 1:
		err = run()
	case len(os.Args) == 2 && os.Args[1] == "keygen":
		err = printKey()
	case len(os.Args) == 3 && os.Args[1] == "admin" && os.Args[2] == "reset":
		err = runAdminReset()
	default:
		err = fmt.Errorf("未知命令；可用命令：keygen、admin reset")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("读取配置: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o700); err != nil {
		return fmt.Errorf("创建数据目录: %w", err)
	}
	masterKey, keyCreated, err := secure.LoadOrCreateMasterKey(cfg.MasterKeyFile)
	if err != nil {
		return fmt.Errorf("加载主密钥: %w", err)
	}
	if keyCreated {
		logger.Warn("已生成本机主密钥文件，请与数据库一起安全备份", "path", cfg.MasterKeyFile)
	}
	cipher, err := secure.NewCipher(masterKey)
	if err != nil {
		return fmt.Errorf("初始化凭据加密: %w", err)
	}
	for i := range masterKey {
		masterKey[i] = 0
	}

	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("打开数据库: %w", err)
	}
	defer db.Close()
	if err := bootstrapAdmin(context.Background(), db, cfg.AdminUsername, cfg.AdminPassword); err != nil {
		return err
	}
	if cfg.AdminPassword != "" {
		matches, err := configuredAdminMatches(context.Background(), db, cfg.AdminUsername, cfg.AdminPassword)
		if err != nil {
			logger.Warn("检查管理员环境变量失败，将继续启动", "error", err)
		} else if !matches {
			logger.Warn(
				"环境变量中的管理员凭据与持久化数据库不一致；修改 .env 不会覆盖已有管理员",
				"configured_username", cfg.AdminUsername,
				"recovery_command", "docker compose run --rm --no-deps icloud-api admin reset",
			)
		}
	}
	cfg.AdminPassword = ""

	fetcher := mailfetch.NewFetcher()
	fetcher.IMAPTimeout = cfg.IMAPTimeout
	fetcher.MaxMessageBytes = int(cfg.MaxMessageBytes)
	fetcher.MaxBodyBytes = int(cfg.MaxBodyBytes)
	fetcher.AllowWeakRecipientHeaders = cfg.AllowWeakRecipientHeaders

	appContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	manager := syncer.New(db, cipher, fetcher, logger, cfg.PollInterval, cfg.SyncConcurrency)
	manager.SetSyncTimeout(cfg.SyncTimeout)
	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		manager.Run(appContext)
	}()

	web, err := httpserver.New(db, cipher, cfg, logger, func(accountID int64) error {
		return manager.SyncAccountWithTimeout(appContext, accountID)
	})
	if err != nil {
		return fmt.Errorf("初始化 HTTP 服务: %w", err)
	}
	web.SetAccountLocker(manager.WithAccountLock)
	router, err := web.Router()
	if err != nil {
		return err
	}
	httpService := &http.Server{
		Addr:              cfg.Addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      httpWriteTimeout(cfg.SyncTimeout),
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("服务已启动", "address", cfg.Addr, "admin", "http://"+cfg.Addr+"/admin")
		serverErrors <- httpService.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP 服务异常退出: %w", err)
		}
	case <-appContext.Done():
	}
	stop()

	shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpService.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("关闭 HTTP 服务: %w", err)
	}
	select {
	case <-syncDone:
	case <-shutdownContext.Done():
		return fmt.Errorf("等待同步任务结束: %w", shutdownContext.Err())
	}
	logger.Info("服务已关闭")
	return nil
}

func httpWriteTimeout(syncTimeout time.Duration) time.Duration {
	return syncTimeout + 10*time.Second
}

func runAdminReset() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("读取配置: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o700); err != nil {
		return fmt.Errorf("创建数据目录: %w", err)
	}
	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("打开数据库: %w", err)
	}
	defer db.Close()

	username, err := resetAdminCredentials(context.Background(), db, cfg.AdminUsername, cfg.AdminPassword)
	if err != nil {
		return err
	}
	fmt.Printf("管理员凭据已重置，当前用户名：%s；所有旧登录会话已注销。\n", username)
	return nil
}

func bootstrapAdmin(ctx context.Context, db *store.Store, username, configuredPassword string) error {
	count, err := db.CountAdmins(ctx)
	if err != nil {
		return fmt.Errorf("检查管理员: %w", err)
	}
	if count > 0 {
		return nil
	}
	username = strings.TrimSpace(username)
	if err := validateAdminCredentials(username, configuredPassword); err != nil {
		return fmt.Errorf("首次启动管理员配置无效: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(configuredPassword), 12)
	if err != nil {
		return fmt.Errorf("哈希管理员密码: %w", err)
	}
	if _, err := db.CreateAdmin(ctx, username, string(hash)); err != nil {
		return fmt.Errorf("创建初始管理员: %w", err)
	}
	return nil
}

func resetAdminCredentials(ctx context.Context, db *store.Store, username, configuredPassword string) (string, error) {
	username = strings.TrimSpace(username)
	if err := validateAdminCredentials(username, configuredPassword); err != nil {
		return "", fmt.Errorf("管理员重置配置无效: %w", err)
	}

	admin, lookupErr := db.GetAdminByUsername(ctx, username)
	if lookupErr != nil {
		if !errors.Is(lookupErr, store.ErrNotFound) {
			return "", fmt.Errorf("读取管理员: %w", lookupErr)
		}
		admins, err := db.ListAdmins(ctx)
		if err != nil {
			return "", fmt.Errorf("读取管理员: %w", err)
		}
		if len(admins) == 0 {
			if err := bootstrapAdmin(ctx, db, username, configuredPassword); err != nil {
				return "", err
			}
			return username, nil
		}
		if len(admins) != 1 {
			return "", fmt.Errorf("数据库中有 %d 个管理员且没有用户名 %q；无法安全确定要重置的账号", len(admins), username)
		}
		admin = admins[0]
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(configuredPassword), 12)
	if err != nil {
		return "", fmt.Errorf("哈希管理员密码: %w", err)
	}
	if err := db.ResetAdminCredentialsAndRevokeSessions(ctx, admin.ID, admin.PasswordVersion, username, string(hash)); err != nil {
		return "", fmt.Errorf("重置管理员凭据: %w", err)
	}
	return username, nil
}

func configuredAdminMatches(ctx context.Context, db *store.Store, username, configuredPassword string) (bool, error) {
	admin, err := db.GetAdminByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(configuredPassword)) == nil, nil
}

func validateAdminCredentials(username, password string) error {
	if username == "" {
		return fmt.Errorf("ICLOUD_API_ADMIN_USER 不能为空")
	}
	if len(username) > 128 {
		return fmt.Errorf("ICLOUD_API_ADMIN_USER 不能超过 128 字节")
	}
	if password == "" {
		return fmt.Errorf("必须设置 ICLOUD_API_ADMIN_PASSWORD")
	}
	if len(password) < 12 {
		return fmt.Errorf("ICLOUD_API_ADMIN_PASSWORD 至少需要 12 字节")
	}
	if len(password) > 72 {
		return fmt.Errorf("ICLOUD_API_ADMIN_PASSWORD 不能超过 72 字节")
	}
	return nil
}

func printKey() error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("生成主密钥: %w", err)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(key))
	return nil
}
