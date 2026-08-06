package httpserver

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
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
	store       *store.Store
	cipher      *secure.Cipher
	cfg         config.Config
	logger      *slog.Logger
	sync        func(int64) error
	lockAccount func(context.Context, int64, func() error) error

	loginLimiter        *windowLimiter
	loginRequestLimiter *windowLimiter
	apiLimiter          *windowLimiter
	apiIPLimiter        *windowLimiter
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
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		store:               st,
		cipher:              cipher,
		cfg:                 cfg,
		logger:              logger,
		sync:                syncFn,
		loginLimiter:        newWindowLimiter(8, 10*time.Minute),
		loginRequestLimiter: newWindowLimiter(60, time.Minute),
		apiLimiter:          newWindowLimiter(120, time.Minute),
		apiIPLimiter:        newWindowLimiter(300, time.Minute),
	}, nil
}

func (s *Server) Router() (*gin.Engine, error) {
	gin.SetMode(s.cfg.GinMode)
	router := gin.New()
	if err := router.SetTrustedProxies(s.cfg.TrustedProxies); err != nil {
		return nil, fmt.Errorf("配置受信代理: %w", err)
	}
	router.Use(s.requestContext(), s.securityHeaders(), gin.Recovery())

	tmpl, err := template.New("").Funcs(template.FuncMap{
		"timefmt":          formatOptionalTime,
		"timevalue":        func(value time.Time) string { return value.Local().Format("2006-01-02 15:04:05") },
		"compactSyncError": compactSyncError,
	}).ParseFS(webAssets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("解析后台模板: %w", err)
	}
	router.SetHTMLTemplate(tmpl)
	staticRoot, err := fs.Sub(webAssets, "static")
	if err != nil {
		return nil, fmt.Errorf("读取静态资源: %w", err)
	}
	router.GET("/assets/*filepath", func(c *gin.Context) {
		c.Header("Cache-Control", "public, max-age=3600")
		http.StripPrefix("/assets", http.FileServer(http.FS(staticRoot))).ServeHTTP(c.Writer, c.Request)
	})

	router.GET("/healthz", s.health)
	router.GET("/", func(c *gin.Context) { c.Redirect(http.StatusFound, "/admin") })

	api := router.Group("/api/v1")
	api.Use(s.apiKeyAuth())
	api.GET("/mail/latest", s.latestMail)

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

	router.NoRoute(func(c *gin.Context) {
		if len(c.Request.URL.Path) >= 5 && c.Request.URL.Path[:5] == "/api/" {
			s.writeAPIError(c, http.StatusNotFound, "NOT_FOUND", "接口不存在")
			return
		}
		c.String(http.StatusNotFound, "页面不存在")
	})
	return router, nil
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
