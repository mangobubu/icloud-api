package httpserver

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/config"
	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
	"icloud-api/internal/syncer"
)

//go:embed docs.html
var publicDocsAssets embed.FS

type Server struct {
	store           *store.Store
	cipher          *secure.Cipher
	cfg             config.Config
	logger          *slog.Logger
	applicationLogs ApplicationLogSource
	now             func() time.Time
	sync            func(int64) error
	syncProgress    func(int64) (domain.MailboxSyncProgress, bool)
	hmeSync         HMESyncService
	autoCreate      AliasAutoCreationService
	lockAccount     func(context.Context, int64, func() error) error
	seenNotify      func()
	ready           func() bool
	adminSPA        *adminSPA

	mailSyncWakeMu sync.Mutex
	mailSyncWake   map[int64]time.Time

	loginLimiter         *windowLimiter
	loginRequestLimiter  *windowLimiter
	apiLimiter           *windowLimiter
	apiIPLimiter         *windowLimiter
	externalAPILimiter   *windowLimiter
	oauthTokenHash       []byte
	oauthTokenConfigured bool
}

// SetHMESyncService configures Apple Hide My Email authentication and
// directory synchronization. It must be called before Router starts serving.
func (s *Server) SetHMESyncService(service HMESyncService) {
	s.hmeSync = service
}

// AliasAutoCreationService supplies the durable per-account scheduler to the
// administrator API. It is optional so embedders that do not run the worker
// can still use the rest of the server.
type AliasAutoCreationService interface {
	GetSchedule(context.Context, int64) (domain.AliasCreationSchedule, error)
	SetEnabled(context.Context, int64, bool) (domain.AliasCreationSchedule, error)
}

func (s *Server) SetAliasAutoCreationService(service AliasAutoCreationService) {
	s.autoCreate = service
}

// SetAutoCreationService is an alias retained for embedders using the shorter
// name introduced during the worker integration.
func (s *Server) SetAutoCreationService(service AliasAutoCreationService) {
	s.SetAliasAutoCreationService(service)
}

type adminSPA struct {
	index        []byte
	assets       fs.FS
	assetHandler http.Handler
}

// SetAccountLocker makes account mutations share the sync manager's keyed
// lock. It must be configured before Router starts serving requests.
func (s *Server) SetAccountLocker(locker func(context.Context, int64, func() error) error) {
	s.lockAccount = locker
}

// SetSeenNotifier wakes the durable IMAP flag worker after a legacy direct
// link consumes a message. It is optional for embedders without that worker.
func (s *Server) SetSeenNotifier(notify func()) {
	s.seenNotify = notify
}

// SetSyncProgressProvider exposes the sync manager's in-memory activity to
// administrator API reads. Progress is intentionally runtime state; durable
// success and failure remain on the account record.
func (s *Server) SetSyncProgressProvider(provider func(int64) (domain.MailboxSyncProgress, bool)) {
	s.syncProgress = provider
}

// SetReadinessChecker adds process-level dependencies, such as the public
// IMAPS listener, to /healthz. It must be configured before Router is served.
func (s *Server) SetReadinessChecker(checker func() bool) {
	s.ready = checker
}

func (s *Server) withAccountLock(ctx context.Context, accountID int64, operation func() error) error {
	if s.lockAccount == nil {
		return operation()
	}
	return s.lockAccount(ctx, accountID, operation)
}

// requestMailboxSync coalesces external polling into a bounded background
// sync. API handlers never wait for IMAP work; the cooldown prevents a client
// polling every few seconds from creating a new login for every request.
func (s *Server) requestMailboxSync(accountID int64, now time.Time) {
	if s == nil || s.sync == nil || accountID < 1 {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	const cooldown = 10 * time.Second
	s.mailSyncWakeMu.Lock()
	if s.mailSyncWake == nil {
		s.mailSyncWake = make(map[int64]time.Time)
	}
	if last, ok := s.mailSyncWake[accountID]; ok && !now.Before(last) && now.Sub(last) < cooldown {
		s.mailSyncWakeMu.Unlock()
		return
	}
	s.mailSyncWake[accountID] = now
	s.mailSyncWakeMu.Unlock()
	syncFn := s.sync
	logger := s.logger
	go func() {
		err := syncFn(accountID)
		if err == nil || errors.Is(err, syncer.ErrSyncQueued) || errors.Is(err, syncer.ErrSyncPending) {
			return
		}
		if logger != nil {
			logger.Warn("外部邮件查询触发同步未入队", "account_id", accountID, "error", err)
		}
	}()
}

func (s *Server) recovery() gin.HandlerFunc {
	// Gin's default recovery dumps the request line, which would expose a
	// query-string API key. Keep diagnostics at path/request-ID granularity.
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, _ any) {
		s.logger.Error("HTTP 请求异常", "path", s.redactedRequestPath(c.Request.URL.Path), "request_id", requestID(c))
		c.AbortWithStatus(http.StatusInternalServerError)
	})
}

func New(st *store.Store, cipher *secure.Cipher, cfg config.Config, logger *slog.Logger, syncFn func(int64) error) (*Server, error) {
	if st == nil {
		return nil, fmt.Errorf("数据存储未配置")
	}
	if cipher == nil {
		return nil, fmt.Errorf("凭据加密器未配置")
	}
	if logger == nil {
		logger = slog.Default()
	}
	spa, err := loadAdminSPA(cfg.WebRoot)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.AdminPath) == "" {
		// Direct unit embedders do not run the process bootstrap. Production
		// startup always replaces this fixture prefix from the keys volume.
		cfg.AdminPath = "/00000000000000000000000000000000/admin"
	}
	cfg.AdminPath, err = secure.NormalizeAdminPath(cfg.AdminPath)
	if err != nil {
		return nil, fmt.Errorf("配置管理路径: %w", err)
	}
	oauthTokenConfigured := strings.TrimSpace(cfg.OAuthToken) != ""
	oauthTokenHash := secure.HashToken(cfg.OAuthToken)
	cfg.OAuthToken = ""
	return &Server{
		store:                st,
		cipher:               cipher,
		cfg:                  cfg,
		logger:               logger,
		now:                  time.Now,
		sync:                 syncFn,
		adminSPA:             spa,
		loginLimiter:         newWindowLimiter(8, 10*time.Minute),
		loginRequestLimiter:  newWindowLimiter(60, time.Minute),
		apiLimiter:           newWindowLimiter(120, time.Minute),
		apiIPLimiter:         newWindowLimiter(300, time.Minute),
		externalAPILimiter:   newWindowLimiter(300, time.Minute),
		oauthTokenHash:       oauthTokenHash,
		oauthTokenConfigured: oauthTokenConfigured,
	}, nil
}

func loadAdminSPA(webRoot string) (*adminSPA, error) {
	webRoot = strings.TrimSpace(webRoot)
	if webRoot == "" {
		return nil, nil
	}

	root := os.DirFS(webRoot)
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		return nil, fmt.Errorf("读取 Vue 管理端首页 %q: %w", webRoot, err)
	}
	if len(index) == 0 {
		return nil, fmt.Errorf("Vue 管理端首页 %q 为空", webRoot)
	}
	assetInfo, err := fs.Stat(root, "assets")
	if err != nil {
		return nil, fmt.Errorf("读取 Vue 管理端资源目录 %q: %w", webRoot, err)
	}
	if !assetInfo.IsDir() {
		return nil, fmt.Errorf("Vue 管理端资源路径 %q 不是目录", webRoot)
	}
	assets, err := fs.Sub(root, "assets")
	if err != nil {
		return nil, fmt.Errorf("打开 Vue 管理端资源目录 %q: %w", webRoot, err)
	}
	return &adminSPA{
		index:        index,
		assets:       assets,
		assetHandler: http.FileServer(http.FS(assets)),
	}, nil
}

func (s *Server) Router() (*gin.Engine, error) {
	gin.SetMode(s.cfg.GinMode)
	router := gin.New()
	if err := router.SetTrustedProxies(s.cfg.TrustedProxies); err != nil {
		return nil, fmt.Errorf("配置受信代理: %w", err)
	}
	router.Use(s.requestContext(), s.securityHeaders(), s.recovery())

	router.GET("/healthz", s.health)
	rootRedirect := func(c *gin.Context) { c.Redirect(http.StatusFound, "/docs/") }
	router.GET("/", rootRedirect)
	router.HEAD("/", rootRedirect)
	router.GET("/docs", func(c *gin.Context) { c.Redirect(http.StatusPermanentRedirect, "/docs/") })
	router.HEAD("/docs", func(c *gin.Context) { c.Redirect(http.StatusPermanentRedirect, "/docs/") })
	router.GET("/docs/", s.publicDocs)
	router.HEAD("/docs/", s.publicDocs)

	router.GET("/api/v1/otp", s.otpHistory)
	legacyAPI := router.Group("/api/v1")
	legacyAPI.GET("/mail/latest", s.apiKeyAuth(), s.latestMail)
	recentAuth := s.apiKeyQueryAuth()
	legacyAPI.GET("/mail/recent", recentAuth, s.recentMail)
	legacyAPI.GET("/mail/recent/", recentAuth, s.recentMail)
	externalAuth := s.oauthTokenAuth()
	legacyAPI.POST("/aliases", externalAuth, s.createExternalAlias)
	legacyAPI.POST("/aliases/", externalAuth, s.createExternalAlias)
	router.POST("/oauth2/v2.0/token", s.issueIMAPAccessToken)

	adminAPI := router.Group(s.cfg.AdminPath + "/api/v1")
	s.registerAdminAPIRoutes(adminAPI)
	if adminAPI.BasePath() != legacyAdminAPIBasePath {
		s.registerAdminAPIRoutes(router.Group(legacyAdminAPIBasePath))
	}

	if s.adminSPA != nil {
		s.registerAdminSPA(router)
	}

	router.NoRoute(s.notFound)
	return router, nil
}

func (s *Server) registerAdminSPA(router *gin.Engine) {
	redirect := func(c *gin.Context) {
		c.Redirect(http.StatusPermanentRedirect, s.cfg.AdminPath+"/")
	}
	router.GET(s.cfg.AdminPath, redirect)
	router.HEAD(s.cfg.AdminPath, redirect)
	router.GET(s.cfg.AdminPath+"/", s.serveAdminIndex)
	router.HEAD(s.cfg.AdminPath+"/", s.serveAdminIndex)
	router.GET(s.cfg.AdminPath+"/assets", s.pageNotFound)
	router.HEAD(s.cfg.AdminPath+"/assets", s.pageNotFound)
	router.GET(s.cfg.AdminPath+"/assets/*filepath", s.serveAdminAsset)
	router.HEAD(s.cfg.AdminPath+"/assets/*filepath", s.serveAdminAsset)
}

func (s *Server) serveAdminIndex(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store, private")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	baseElement := `<base href="` + s.cfg.AdminPath + `/">`
	index := strings.Replace(string(s.adminSPA.index), `<base href="./">`, baseElement, 1)
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(index))
}

func (s *Server) serveAdminAsset(c *gin.Context) {
	name := strings.TrimPrefix(c.Param("filepath"), "/")
	if name == "" || !fs.ValidPath(name) {
		s.pageNotFound(c)
		return
	}
	info, err := fs.Stat(s.adminSPA.assets, name)
	if err != nil || !info.Mode().IsRegular() {
		s.pageNotFound(c)
		return
	}
	c.Writer.Header().Del("Pragma")
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	http.StripPrefix(s.cfg.AdminPath+"/assets", s.adminSPA.assetHandler).ServeHTTP(c.Writer, c.Request)
}

func (s *Server) notFound(c *gin.Context) {
	requestPath := c.Request.URL.Path
	if pathWithin(requestPath, "/api") || pathWithin(requestPath, "/oauth2") ||
		pathWithin(requestPath, s.cfg.AdminPath+"/api") ||
		pathWithin(requestPath, legacyAdminAPIBasePath) {
		c.Header("Cache-Control", "no-store")
		s.writeAPIError(c, http.StatusNotFound, "NOT_FOUND", "接口不存在")
		return
	}
	if s.adminSPA != nil &&
		(c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) &&
		strings.HasPrefix(requestPath, s.cfg.AdminPath+"/") &&
		!pathWithin(requestPath, s.cfg.AdminPath+"/assets") {
		s.serveAdminIndex(c)
		return
	}
	s.pageNotFound(c)
}

func (s *Server) publicDocs(c *gin.Context) {
	content, err := publicDocsAssets.ReadFile("docs.html")
	if err != nil {
		s.writeAPIError(c, http.StatusInternalServerError, "DOCS_UNAVAILABLE", "文档暂不可用")
		return
	}
	c.Header("Cache-Control", "public, max-age=300")
	c.Header("Content-Type", "text/html; charset=utf-8")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", content)
}

func pathWithin(requestPath, prefix string) bool {
	return requestPath == prefix || strings.HasPrefix(requestPath, prefix+"/")
}

func (s *Server) redactedRequestPath(requestPath string) string {
	if s != nil && s.cfg.AdminPath != "" && pathWithin(requestPath, s.cfg.AdminPath) {
		return "/<admin-prefix>/admin" + strings.TrimPrefix(requestPath, s.cfg.AdminPath)
	}
	if pathWithin(requestPath, legacyAdminAPIBasePath) {
		return "/<legacy-admin-api>" + strings.TrimPrefix(requestPath, legacyAdminAPIBasePath)
	}
	return requestPath
}

func (s *Server) pageNotFound(c *gin.Context) {
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusNotFound)
		return
	}
	c.String(http.StatusNotFound, "页面不存在")
}

func (s *Server) health(c *gin.Context) {
	ctx := c.Request.Context()
	if err := s.store.DB().PingContext(ctx); err != nil {
		s.writeAPIError(c, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "数据库不可用")
		return
	}
	if s.ready != nil && !s.ready() {
		s.writeAPIError(c, http.StatusServiceUnavailable, "SERVICE_STARTING", "服务尚未就绪")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
