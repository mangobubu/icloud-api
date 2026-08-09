package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"icloud-api/internal/apple"
	"icloud-api/internal/autocreate"
	"icloud-api/internal/config"
	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
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
	case len(os.Args) == 2 && os.Args[1] == "verify-startup":
		err = runStartupVerification()
	case len(os.Args) == 3 && os.Args[1] == "admin" && os.Args[2] == "reset":
		err = runAdminReset()
	default:
		err = fmt.Errorf("未知命令；可用命令：keygen、verify-startup、admin reset")
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

	db, cipher, keyCreated, err := openInitializedStore(context.Background(), cfg)
	if err != nil {
		return err
	}
	closeDBOnReturn := true
	defer func() {
		if closeDBOnReturn {
			_ = db.Close()
		}
	}()
	if keyCreated {
		logger.Warn("已生成本机主密钥文件，请与数据库一起安全备份", "path", cfg.MasterKeyFile)
	}
	if cfg.LegacySQLitePath != "" {
		logger.Info("旧 SQLite 数据导入检查完成", "path", cfg.LegacySQLitePath)
	}
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

	signalContext, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	requestContext, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	workerContext, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	manager := syncer.New(db, cipher, fetcher, logger, cfg.PollInterval, cfg.SyncConcurrency)
	manager.SetSyncTimeout(cfg.SyncTimeout)
	appleClient, err := apple.NewClient(apple.Config{})
	if err != nil {
		return fmt.Errorf("初始化 Apple 客户端: %w", err)
	}
	hmeService, err := hmesync.New(db, cipher, appleClient, manager)
	if err != nil {
		return fmt.Errorf("初始化隐私邮箱同步服务: %w", err)
	}
	autoManager, err := autocreate.New(
		db,
		func(ctx context.Context, accountID int64) (domain.Alias, error) {
			alias, createErr := hmeService.CreateAutoAlias(ctx, accountID)
			if errors.Is(createErr, store.ErrAliasLimit) {
				return domain.Alias{}, fmt.Errorf("%w: %v", autocreate.ErrCapacityReached, createErr)
			}
			return alias, createErr
		},
		logger,
	)
	if err != nil {
		return fmt.Errorf("初始化隐私邮箱自动创建服务: %w", err)
	}
	seenWorker := syncer.NewSeenWorker(db, cipher, fetcher, manager, logger, cfg.PollInterval)
	seenWorker.SetOperationTimeout(seenOperationTimeout(cfg.IMAPTimeout))

	web, err := httpserver.New(db, cipher, cfg, logger, func(accountID int64) error {
		return manager.QueueAccountSync(workerContext, accountID)
	})
	cfg.OAuthToken = ""
	if err != nil {
		return fmt.Errorf("初始化 HTTP 服务: %w", err)
	}
	web.SetAccountLocker(manager.WithAccountLock)
	web.SetHMESyncService(hmeService)
	web.SetAliasAutoCreationService(autoManager)
	web.SetSeenNotifier(seenWorker.Notify)
	router, err := web.Router()
	if err != nil {
		return err
	}
	drainingRouter := newDrainingHandler(router)
	httpService := &http.Server{
		Addr:              cfg.Addr,
		Handler:           drainingRouter,
		BaseContext:       func(net.Listener) context.Context { return requestContext },
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      httpWriteTimeout(cfg.SyncTimeout),
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	var background sync.WaitGroup
	background.Add(3)
	go func() {
		defer background.Done()
		manager.Run(workerContext)
	}()
	go func() {
		defer background.Done()
		seenWorker.Run(workerContext)
	}()
	go func() {
		defer background.Done()
		autoManager.Run(workerContext)
	}()
	backgroundDone := make(chan struct{})
	go func() {
		background.Wait()
		close(backgroundDone)
	}()

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("服务已启动", "address", cfg.Addr, "admin", "http://"+cfg.Addr+"/admin")
		serverErrors <- httpService.ListenAndServe()
	}()

	var serveErr error
	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr = fmt.Errorf("HTTP 服务异常退出: %w", err)
		}
	case <-signalContext.Done():
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	requestDone := drainingRouter.Stop()
	// Stop background loops only after new HTTP work is rejected. Existing
	// requests retain requestContext until HTTP draining finishes or times out.
	manager.BeginShutdown()
	cancelWorkers()
	stopSignal()
	shutdownErr := shutdownHTTPAndWaitForBackground(
		shutdownContext,
		httpService,
		requestDone,
		backgroundDone,
		cancelRequests,
	)
	if !channelsClosed(requestDone, backgroundDone) {
		// Do not close the shared database while an owner is still running. The
		// process will exit on the returned error; if it remains alive, close the
		// handle after both owners release it.
		closeDBOnReturn = false
		go func() {
			<-requestDone
			<-backgroundDone
			_ = db.Close()
		}()
	}
	if err := errors.Join(serveErr, shutdownErr); err != nil {
		return err
	}
	logger.Info("服务已关闭")
	return nil
}

func httpWriteTimeout(syncTimeout time.Duration) time.Duration {
	if syncTimeout <= 0 {
		return 10 * time.Second
	}
	return 2*syncTimeout + 10*time.Second
}

func seenOperationTimeout(imapTimeout time.Duration) time.Duration {
	const (
		minimum = 2 * time.Minute
		maximum = 5 * time.Minute
		stages  = 6
		grace   = 10 * time.Second
	)
	if imapTimeout <= (minimum-grace)/stages {
		return minimum
	}
	if imapTimeout >= (maximum-grace)/stages {
		return maximum
	}
	return stages*imapTimeout + grace
}

type httpShutdowner interface {
	Shutdown(context.Context) error
	Close() error
}

func shutdownHTTPAndWaitForBackground(
	ctx context.Context,
	service httpShutdowner,
	requestDone <-chan struct{},
	backgroundDone <-chan struct{},
	cancelRequests context.CancelFunc,
) error {
	var shutdownErr error
	forcedClose := false
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- service.Shutdown(ctx)
	}()
	select {
	case err := <-shutdownDone:
		if err != nil {
			shutdownErr = fmt.Errorf("关闭 HTTP 服务: %w", err)
		}
	case <-ctx.Done():
		shutdownErr = fmt.Errorf("关闭 HTTP 服务: %w", ctx.Err())
		forcedClose = true
		if cancelRequests != nil {
			cancelRequests()
		}
		if closeErr := service.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("强制关闭 HTTP 服务: %w", closeErr))
		}
	}
	if shutdownErr != nil && !forcedClose {
		// Shutdown may have returned a non-context error before the deadline;
		// close active connections before waiting for the ownership markers.
		if cancelRequests != nil {
			cancelRequests()
		}
		if closeErr := service.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("强制关闭 HTTP 服务: %w", closeErr))
		}
	}
	if cancelRequests != nil {
		cancelRequests()
	}
	return errors.Join(
		shutdownErr,
		waitForOwnedTasks(ctx, "HTTP 请求", requestDone),
		waitForOwnedTasks(ctx, "后台任务", backgroundDone),
	)
}

func waitForOwnedTasks(ctx context.Context, name string, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}
	return fmt.Errorf("等待%s结束: %w", name, ctx.Err())
}

func channelsClosed(channels ...<-chan struct{}) bool {
	for _, channel := range channels {
		select {
		case <-channel:
		default:
			return false
		}
	}
	return true
}

type drainingHandler struct {
	handler http.Handler

	mu        sync.Mutex
	accepting bool
	active    int
	done      chan struct{}
}

func newDrainingHandler(handler http.Handler) *drainingHandler {
	return &drainingHandler{
		handler:   handler,
		accepting: true,
		done:      make(chan struct{}),
	}
}

func (h *drainingHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	h.mu.Lock()
	if !h.accepting {
		h.mu.Unlock()
		response.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	h.active++
	h.mu.Unlock()

	defer h.finishRequest()
	h.handler.ServeHTTP(response, request)
}

func (h *drainingHandler) Stop() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.accepting {
		h.accepting = false
		if h.active == 0 {
			close(h.done)
		}
	}
	return h.done
}

func (h *drainingHandler) finishRequest() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active--
	if !h.accepting && h.active == 0 {
		close(h.done)
	}
}

type startupStore interface {
	InitializeMasterKeyWithLegacySQLite(
		context.Context,
		[]byte,
		string,
		store.MasterKeyCipherValidator,
	) error
}

func initializeCipherWithStore(
	ctx context.Context,
	database startupStore,
	masterKey []byte,
	legacySQLitePath string,
) (*secure.Cipher, error) {
	defer zeroBytes(masterKey)
	cipher, err := secure.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("初始化凭据加密: %w", err)
	}
	if err := database.InitializeMasterKeyWithLegacySQLite(
		ctx, masterKey, legacySQLitePath, cipher,
	); err != nil {
		return nil, masterKeyVerificationError(err, legacySQLitePath)
	}
	return cipher, nil
}

func masterKeyVerificationError(err error, legacySQLitePath string) error {
	if errors.Is(err, store.ErrMasterKeyMismatch) {
		return fmt.Errorf("主密钥与 PostgreSQL 数据库不匹配；请恢复与该数据库配套的 keys 卷或原 ICLOUD_API_MASTER_KEY: %w", err)
	}
	if errors.Is(err, store.ErrLegacySQLiteImport) {
		if legacySQLitePath = strings.TrimSpace(legacySQLitePath); legacySQLitePath != "" {
			return fmt.Errorf("迁移旧 SQLite 数据并校验 PostgreSQL 主密钥（路径 %q）: %w", legacySQLitePath, err)
		}
		return fmt.Errorf("迁移旧 SQLite 数据并校验 PostgreSQL 主密钥: %w", err)
	}
	return fmt.Errorf("校验 PostgreSQL 主密钥指纹: %w", err)
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func openInitializedStore(
	ctx context.Context,
	cfg config.Config,
) (*store.Store, *secure.Cipher, bool, error) {
	masterKey, keyCreated, err := secure.LoadOrCreateMasterKey(cfg.MasterKeyFile)
	if err != nil {
		return nil, nil, false, fmt.Errorf("加载主密钥: %w", err)
	}
	defer zeroBytes(masterKey)

	db, err := store.OpenContext(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, false, fmt.Errorf("打开数据库: %w", err)
	}
	cipher, err := initializeCipherWithStore(ctx, db, masterKey, cfg.LegacySQLitePath)
	if err != nil {
		_ = db.Close()
		return nil, nil, false, err
	}
	return db, cipher, keyCreated, nil
}

type adminBootstrapStatus string

const (
	adminBootstrapCreated  adminBootstrapStatus = "admin-created"
	adminBootstrapExisting adminBootstrapStatus = "admin-existing"
	adminBootstrapRequired adminBootstrapStatus = "admin-required"
)

func runStartupVerification() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("读取配置: %w", err)
	}
	db, _, _, err := openInitializedStore(context.Background(), cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	status, err := checkAdminBootstrap(
		context.Background(), db, cfg.AdminUsername, cfg.AdminPassword,
	)
	cfg.AdminPassword = ""
	if err != nil {
		return err
	}
	fmt.Println(status)
	return nil
}

func runAdminReset() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("读取配置: %w", err)
	}
	masterKey, _, err := secure.LoadOrCreateMasterKey(cfg.MasterKeyFile)
	if err != nil {
		return fmt.Errorf("加载主密钥: %w", err)
	}
	defer zeroBytes(masterKey)

	ctx := context.Background()
	db, err := store.OpenContext(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("打开数据库: %w", err)
	}
	defer db.Close()

	username, err := initializeAndResetAdmin(
		ctx,
		db,
		masterKey,
		cfg.LegacySQLitePath,
		func() (string, error) {
			return resetAdminCredentials(ctx, db, cfg.AdminUsername, cfg.AdminPassword)
		},
	)
	cfg.AdminPassword = ""
	if err != nil {
		return err
	}
	fmt.Printf("管理员凭据已重置，当前用户名：%s；所有旧登录会话已注销。\n", username)
	return nil
}

func initializeAndResetAdmin(
	ctx context.Context,
	database startupStore,
	masterKey []byte,
	legacySQLitePath string,
	reset func() (string, error),
) (string, error) {
	if _, err := initializeCipherWithStore(ctx, database, masterKey, legacySQLitePath); err != nil {
		return "", err
	}
	return reset()
}

func bootstrapAdmin(ctx context.Context, db *store.Store, username, configuredPassword string) error {
	_, err := bootstrapAdminWithStatus(ctx, db, username, configuredPassword)
	return err
}

func bootstrapAdminWithStatus(
	ctx context.Context,
	db *store.Store,
	username, configuredPassword string,
) (adminBootstrapStatus, error) {
	status, err := checkAdminBootstrap(ctx, db, username, configuredPassword)
	if err != nil || status == adminBootstrapExisting {
		return status, err
	}
	username = strings.TrimSpace(username)
	hash, err := bcrypt.GenerateFromPassword([]byte(configuredPassword), 12)
	if err != nil {
		return "", fmt.Errorf("哈希管理员密码: %w", err)
	}
	if _, err := db.CreateAdmin(ctx, username, string(hash)); err != nil {
		return "", fmt.Errorf("创建初始管理员: %w", err)
	}
	return adminBootstrapCreated, nil
}

func checkAdminBootstrap(
	ctx context.Context,
	db *store.Store,
	username, configuredPassword string,
) (adminBootstrapStatus, error) {
	count, err := db.CountAdmins(ctx)
	if err != nil {
		return "", fmt.Errorf("检查管理员: %w", err)
	}
	if count > 0 {
		return adminBootstrapExisting, nil
	}
	username = strings.TrimSpace(username)
	if err := validateAdminCredentials(username, configuredPassword); err != nil {
		return "", fmt.Errorf("首次启动管理员配置无效: %w", err)
	}
	return adminBootstrapRequired, nil
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
