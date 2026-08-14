package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"icloud-api/internal/config"
	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

const adminAPIApplicationLogsPath = "/admin/api/v1/logs"

type adminAPITestEnv struct {
	store  *store.Store
	cipher *secure.Cipher
	server *Server
	router http.Handler
}

func newAdminAPITestEnv(t *testing.T) *adminAPITestEnv {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "admin-api-test.db"))
	if err != nil {
		t.Fatalf("open admin API test store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close admin API test store: %v", err)
		}
	})
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x6b}, 32))
	if err != nil {
		t.Fatalf("create admin API test cipher: %v", err)
	}
	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		_, material, issueErr := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
		return material, issueErr
	})
	db.ConfigureAliasCredentialRevealFactory(func(aliasID int64, credentialCiphertext string) (string, error) {
		credentials, decryptErr := cipher.DecryptAliasCredentials(aliasID, credentialCiphertext)
		if decryptErr != nil {
			return "", decryptErr
		}
		return credentials.APIKey, nil
	})
	db.ConfigureAliasAPIKeyRotationFactory(func(aliasID, version int64, credentialCiphertext, apiKey string) (domain.AliasCredentialMaterial, error) {
		credentials, decryptErr := cipher.DecryptAliasCredentials(aliasID, credentialCiphertext)
		if decryptErr != nil {
			return domain.AliasCredentialMaterial{}, decryptErr
		}
		credentials.APIKey = apiKey
		rotatedCiphertext, encryptErr := cipher.EncryptAliasCredentials(aliasID, credentials)
		if encryptErr != nil {
			return domain.AliasCredentialMaterial{}, encryptErr
		}
		return domain.AliasCredentialMaterial{
			Ciphertext:       rotatedCiphertext,
			APIKeyHash:       secure.HashToken(credentials.APIKey),
			APIKeyPrefix:     credentials.APIKey[:12],
			IMAPPasswordHash: secure.HashToken(credentials.IMAPPassword),
			OAuthClientID:    credentials.ClientID,
			RefreshTokenHash: secure.HashToken(credentials.RefreshToken),
			Version:          version,
		}, nil
	})
	server, err := New(db, cipher, config.Config{
		CookieSecure: false,
		SessionTTL:   time.Hour,
		PollInterval: time.Minute,
		GinMode:      gin.TestMode,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), func(int64) error { return nil })
	if err != nil {
		t.Fatalf("create admin API test server: %v", err)
	}
	server.cfg.AdminPath = "/admin"
	router := gin.New()
	router.Use(server.requestContext(), server.securityHeaders(), gin.Recovery())
	server.registerAdminAPIRoutes(router.Group("/admin/api/v1"))
	return &adminAPITestEnv{store: db, cipher: cipher, server: server, router: router}
}

func (e *adminAPITestEnv) createAdmin(t *testing.T, username, password string) domain.Admin {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash admin API test password: %v", err)
	}
	admin, err := e.store.CreateAdmin(context.Background(), username, string(hash))
	if err != nil {
		t.Fatalf("create admin API test admin: %v", err)
	}
	return admin
}

func (e *adminAPITestEnv) createSession(t *testing.T, username, password string) (*http.Cookie, string, domain.Admin) {
	t.Helper()
	admin := e.createAdmin(t, username, password)
	rawToken := "admin-api-session-" + username
	csrf := "admin-api-csrf-" + username
	if err := e.store.CreateSession(context.Background(), secure.HashToken(rawToken), domain.Session{
		AdminID:         admin.ID,
		Username:        admin.Username,
		PasswordVersion: admin.PasswordVersion,
		CSRF:            csrf,
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create admin API test session: %v", err)
	}
	return &http.Cookie{Name: sessionCookie, Value: rawToken, Path: "/admin"}, csrf, admin
}

func (e *adminAPITestEnv) request(
	t *testing.T,
	method, target string,
	body []byte,
	contentType string,
	cookies []*http.Cookie,
	csrf string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://admin.example.test"+target, bytes.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if csrf != "" {
		request.Header.Set(adminAPICSRFHeader, csrf)
	}
	request.Header.Set("Origin", "http://admin.example.test")
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	e.router.ServeHTTP(response, request)
	return response
}

func adminAPITestJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal admin API test body: %v", err)
	}
	return encoded
}

func adminAPITestErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode admin API error: %v; body=%s", err, response.Body.String())
	}
	if payload.Error.RequestID == "" {
		t.Fatalf("admin API error omitted request_id: %s", response.Body.String())
	}
	return payload.Error.Code
}

func strconvFormatInt(value int64) string {
	return strconv.FormatInt(value, 10)
}
