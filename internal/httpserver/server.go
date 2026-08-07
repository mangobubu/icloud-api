package httpserver

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/config"
	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

//go:embed templates/*.html static/*
var webAssets embed.FS

type Server struct {
	store                *store.Store
	cipher               *secure.Cipher
	cfg                  config.Config
	logger               *slog.Logger
	now                  func() time.Time
	sync                 func(int64) error
	hmeSync              HMESyncService
	lockAccount          func(context.Context, int64, func() error) error
	adminSPA             *adminSPA
	oauthTokenHash       []byte
	oauthTokenConfigured bool

	loginLimiter        *windowLimiter
	loginRequestLimiter *windowLimiter
	apiLimiter          *windowLimiter
	apiIPLimiter        *windowLimiter
	externalAPILimiter  *windowLimiter
}

// SetHMESyncService configures Apple Hide My Email authentication and
// directory synchronization. It must be called before Router starts serving.
func (s *Server) SetHMESyncService(service HMESyncService) {
	s.hmeSync = service
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

func (s *Server) withAccountLock(ctx context.Context, accountID int64, operation func() error) error {
	if s.lockAccount == nil {
		return operation()
	}
	return s.lockAccount(ctx, accountID, operation)
}

func (s *Server) recovery() gin.HandlerFunc {
	// Gin's default recovery dumps the request line, which would expose a
	// query-string API key. Keep diagnostics at path/request-ID granularity.
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, _ any) {
		s.logger.Error("HTTP 请求异常", "path", c.Request.URL.Path, "request_id", requestID(c))
		c.AbortWithStatus(http.StatusInternalServerError)
	})
}

type PageData struct {
	Title         string
	Subtitle      string
	Active        string
	AdminUsername string
	CSRF          string
	Flash         string
	FlashKind     string
	RequestID     string
	LoginUsername string
	Accounts      []domain.Account
	Account       domain.Account
	Aliases       []domain.Alias
	AuditLogs     []domain.AuditLog
	Secret        string
	FormAction    string
	IsEdit        bool
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
	oauthTokenConfigured := cfg.OAuthToken != ""
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
		oauthTokenHash:       oauthTokenHash,
		oauthTokenConfigured: oauthTokenConfigured,
		loginLimiter:         newWindowLimiter(8, 10*time.Minute),
		loginRequestLimiter:  newWindowLimiter(60, time.Minute),
		apiLimiter:           newWindowLimiter(120, time.Minute),
		apiIPLimiter:         newWindowLimiter(300, time.Minute),
		externalAPILimiter:   newWindowLimiter(300, time.Minute),
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
	rootRedirect := func(c *gin.Context) {
		target := "/admin"
		if s.adminSPA != nil {
			target = "/admin/"
		}
		c.Redirect(http.StatusFound, target)
	}
	router.GET("/", rootRedirect)
	router.HEAD("/", rootRedirect)

	api := router.Group("/api/v1")
	api.GET("/mail/latest", s.apiKeyAuth(), s.latestMail)
	recentAuth := s.apiKeyQueryAuth()
	api.GET("/mail/recent", recentAuth, s.recentMail)
	api.GET("/mail/recent/", recentAuth, s.recentMail)
	externalAuth := s.oauthTokenAuth()
	api.POST("/aliases", externalAuth, s.createExternalAlias)
	api.POST("/aliases/", externalAuth, s.createExternalAlias)

	adminAPI := router.Group("/admin/api/v1")
	s.registerAdminAPIRoutes(adminAPI)

	if s.adminSPA == nil {
		if err := s.registerLegacyAdmin(router); err != nil {
			return nil, err
		}
	} else {
		s.registerAdminSPA(router)
	}

	router.NoRoute(s.notFound)
	return router, nil
}

func (s *Server) registerLegacyAdmin(router *gin.Engine) error {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"timefmt":          formatOptionalTime,
		"timevalue":        func(value time.Time) string { return value.Local().Format("2006-01-02 15:04:05") },
		"compactSyncError": compactSyncError,
	}).ParseFS(webAssets, "templates/*.html")
	if err != nil {
		return fmt.Errorf("解析后台模板: %w", err)
	}
	router.SetHTMLTemplate(tmpl)
	staticRoot, err := fs.Sub(webAssets, "static")
	if err != nil {
		return fmt.Errorf("读取静态资源: %w", err)
	}
	router.GET("/assets/*filepath", func(c *gin.Context) {
		c.Header("Cache-Control", "public, max-age=3600")
		http.StripPrefix("/assets", http.FileServer(http.FS(staticRoot))).ServeHTTP(c.Writer, c.Request)
	})

	router.GET("/admin/login", s.loginPage)
	router.POST("/admin/login", s.loginRequestGate(), s.parseAdminForm(), s.login)

	admin := router.Group("/admin")
	admin.Use(s.adminAuth(), s.parseAdminForm(), s.csrfGuard())
	admin.POST("/logout", s.logout)
	admin.GET("", s.accountsPage)
	admin.GET("/accounts/new", s.newAccountPage)
	admin.POST("/accounts", s.createAccount)
	admin.GET("/accounts/:id", s.accountPage)
	admin.GET("/accounts/:id/edit", s.editAccountPage)
	admin.POST("/accounts/:id", s.updateAccount)
	admin.POST("/accounts/:id/sync", s.syncAccount)
	admin.POST("/accounts/:id/delete", s.deleteAccount)
	admin.POST("/accounts/:id/aliases", s.createAlias)
	admin.GET("/aliases", s.aliasesPage)
	admin.POST("/aliases/:id/rotate", s.rotateAliasKey)
	admin.POST("/aliases/:id/toggle", s.toggleAlias)
	admin.POST("/aliases/:id/delete", s.deleteAlias)
	admin.GET("/audit", s.auditPage)
	admin.GET("/security", s.securityPage)
	admin.POST("/security/password", s.changePassword)
	return nil
}

func (s *Server) registerAdminSPA(router *gin.Engine) {
	redirect := func(c *gin.Context) {
		c.Redirect(http.StatusPermanentRedirect, "/admin/")
	}
	router.GET("/admin", redirect)
	router.HEAD("/admin", redirect)
	router.GET("/admin/", s.serveAdminIndex)
	router.HEAD("/admin/", s.serveAdminIndex)
	router.GET("/admin/assets", s.pageNotFound)
	router.HEAD("/admin/assets", s.pageNotFound)
	router.GET("/admin/assets/*filepath", s.serveAdminAsset)
	router.HEAD("/admin/assets/*filepath", s.serveAdminAsset)
}

func (s *Server) serveAdminIndex(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store, private")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", s.adminSPA.index)
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
	http.StripPrefix("/admin/assets", s.adminSPA.assetHandler).ServeHTTP(c.Writer, c.Request)
}

func (s *Server) notFound(c *gin.Context) {
	requestPath := c.Request.URL.Path
	if pathWithin(requestPath, "/api") || pathWithin(requestPath, "/admin/api") {
		c.Header("Cache-Control", "no-store")
		s.writeAPIError(c, http.StatusNotFound, "NOT_FOUND", "接口不存在")
		return
	}
	if s.adminSPA != nil &&
		(c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) &&
		strings.HasPrefix(requestPath, "/admin/") &&
		!pathWithin(requestPath, "/admin/assets") {
		s.serveAdminIndex(c)
		return
	}
	s.pageNotFound(c)
}

func pathWithin(requestPath, prefix string) bool {
	return requestPath == prefix || strings.HasPrefix(requestPath, prefix+"/")
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
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func formatOptionalTime(value any) string {
	var t *time.Time
	switch typed := value.(type) {
	case *time.Time:
		t = typed
	case time.Time:
		t = &typed
	}
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func compactSyncError(value string) string {
	const maxRunes = 80

	normalized := strings.Join(strings.Fields(value), " ")
	runes := []rune(normalized)
	if len(runes) <= maxRunes {
		return normalized
	}
	return string(runes[:maxRunes-1]) + "…"
}
