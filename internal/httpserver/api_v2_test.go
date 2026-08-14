package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
)

func TestOTPV2ReturnsBareRepeatableHistoryForBearerAndDerivedURL(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.sync = nil
	env.server.cfg.Timezone = time.FixedZone("UTC+8", 8*60*60)
	if err := env.store.ConfigureMailArchive(t.TempDir(), 1<<20); err != nil {
		t.Fatalf("configure mail archive: %v", err)
	}
	account := adminAPITestCreateAccount(t, env, "otp-v2@icloud.com")
	alias, credentials := createV2AliasFixture(t, env, account.ID, "otp-alias@icloud.com")

	older := time.Date(2026, 8, 11, 3, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	account, err := env.store.GetAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.ApplyMailboxSync(
		context.Background(),
		account.ID,
		account.UpdatedAt,
		[]domain.Alias{alias},
		domain.MailboxSyncResult{
			ArchivedMessages: []domain.ArchivedMessage{
				{
					AccountID: account.ID, UIDValidity: 77, UID: 1,
					InternalDate: older, Subject: "older", RawMIME: []byte("Subject: older\r\n\r\n123456"),
					OTP: "123456", AliasIDs: []int64{alias.ID},
				},
				{
					AccountID: account.ID, UIDValidity: 77, UID: 2,
					InternalDate: newer, Subject: "newer", RawMIME: []byte("Subject: newer\r\n\r\n876543"),
					OTP: "876543", AliasIDs: []int64{alias.ID},
				},
			},
			State: domain.IMAPSyncState{
				AccountID: account.ID, UIDValidity: 77, LastUID: 2, UpdatedAt: newer,
			},
			Reset: true,
		},
		newer,
	); err != nil {
		t.Fatalf("archive OTP fixtures: %v", err)
	}

	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build public router: %v", err)
	}
	bearer := serveV2Request(router, http.MethodGet, "/api/v1/otp", "", map[string]string{
		"Authorization": "Bearer " + credentials.APIKey,
	})
	if bearer.Code != http.StatusOK {
		t.Fatalf("Bearer OTP status = %d; body=%s", bearer.Code, bearer.Body.String())
	}
	if bearer.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Bearer OTP Cache-Control = %q", bearer.Header().Get("Cache-Control"))
	}
	var got []otpResponse
	if err := json.Unmarshal(bearer.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode bare OTP response: %v", err)
	}
	if len(got) != 2 || got[0].OTP != "876543" || got[1].OTP != "123456" ||
		got[0].Time != "2026-08-11T12:00:00+08:00" || got[1].Time != "2026-08-11T11:00:00+08:00" {
		t.Fatalf("OTP history = %#v", got)
	}
	if strings.Contains(bearer.Body.String(), `"data"`) || !strings.HasPrefix(bearer.Body.String(), "[") {
		t.Fatalf("OTP response is not a bare array: %s", bearer.Body.String())
	}

	derived, err := env.cipher.OTPToken(alias.ID, alias.APIKeyHash)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		response := serveV2Request(
			router,
			http.MethodGet,
			"/api/v1/otp?token="+url.QueryEscape(derived),
			"",
			nil,
		)
		if response.Code != http.StatusOK || response.Body.String() != bearer.Body.String() {
			t.Fatalf("repeatable derived OTP response %d = status %d body %s", attempt, response.Code, response.Body.String())
		}
	}

	_, emptyCredentials := createV2AliasFixture(t, env, account.ID, "empty-otp@icloud.com")
	empty := serveV2Request(router, http.MethodGet, "/api/v1/otp", "", map[string]string{
		"Authorization": "Bearer " + emptyCredentials.APIKey,
	})
	if empty.Code != http.StatusOK || strings.TrimSpace(empty.Body.String()) != "[]" {
		t.Fatalf("empty OTP response = status %d body %q", empty.Code, empty.Body.String())
	}
}

func TestOAuthV2IssuesOneHourBearerAndBundleRotationRevokesEverything(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.sync = nil
	now := time.Date(2026, 8, 11, 4, 5, 6, 0, time.UTC)
	env.server.now = func() time.Time { return now }
	account := adminAPITestCreateAccount(t, env, "oauth-v2@icloud.com")
	alias, credentials := createV2AliasFixture(t, env, account.ID, "oauth-alias@icloud.com")
	oldOTPToken, err := env.cipher.OTPToken(alias.ID, alias.APIKeyHash)
	if err != nil {
		t.Fatal(err)
	}
	router, err := env.server.Router()
	if err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {credentials.ClientID},
		"refresh_token": {credentials.RefreshToken},
	}.Encode()
	issued := serveV2Request(router, http.MethodPost, "/oauth2/v2.0/token", form, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	})
	if issued.Code != http.StatusOK {
		t.Fatalf("OAuth token status = %d; body=%s", issued.Code, issued.Body.String())
	}
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	if token.TokenType != "Bearer" || token.ExpiresIn != 3600 || token.AccessToken == "" {
		t.Fatalf("OAuth token response = %#v", token)
	}
	aliasID, version, expiresAt, ok := secure.AliasAccessTokenIdentity(token.AccessToken)
	if !ok || aliasID != alias.ID || version != alias.CredentialVersion || !expiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("access token identity = alias:%d version:%d expiry:%s valid:%t", aliasID, version, expiresAt, ok)
	}

	rotated, err := env.store.RotateAliasCredentials(context.Background(), alias.ID)
	if err != nil {
		t.Fatalf("rotate credential bundle: %v", err)
	}
	newCredentials, err := env.cipher.DecryptAliasCredentials(rotated.ID, rotated.CredentialCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	if newCredentials == credentials || rotated.CredentialVersion != alias.CredentialVersion+1 {
		t.Fatalf("rotation did not replace complete bundle: old=%#v new=%#v alias=%#v", credentials, newCredentials, rotated)
	}

	oldOAuth := serveV2Request(router, http.MethodPost, "/oauth2/v2.0/token", form, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	})
	if oldOAuth.Code != http.StatusUnauthorized {
		t.Fatalf("old refresh token status = %d; body=%s", oldOAuth.Code, oldOAuth.Body.String())
	}
	for name, fixture := range map[string][2]string{
		"API Key":     {"/api/v1/otp", "Bearer " + credentials.APIKey},
		"derived URL": {"/api/v1/otp?token=" + url.QueryEscape(oldOTPToken), ""},
	} {
		target, authorization := fixture[0], fixture[1]
		response := serveV2Request(router, http.MethodGet, target, "", map[string]string{"Authorization": authorization})
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("old %s status = %d; body=%s", name, response.Code, response.Body.String())
		}
	}
	if env.cipher.VerifyAliasAccessToken(
		token.AccessToken,
		rotated.ID,
		rotated.CredentialVersion,
		rotated.RefreshTokenHash,
		now,
	) {
		t.Fatal("old access token survived credential-bundle rotation")
	}
	newAPI := serveV2Request(router, http.MethodGet, "/api/v1/otp", "", map[string]string{
		"Authorization": "Bearer " + newCredentials.APIKey,
	})
	if newAPI.Code != http.StatusOK || strings.TrimSpace(newAPI.Body.String()) != "[]" {
		t.Fatalf("new API Key response = status %d body %s", newAPI.Code, newAPI.Body.String())
	}
}

func TestV2PublicRoutesHideManagementAndProtectLegacyMailAPIs(t *testing.T) {
	env := newAdminAPITestEnv(t)
	const adminPath = "/0123456789abcdef0123456789abcdef/admin"
	env.server.cfg.AdminPath = adminPath
	router, err := env.server.Router()
	if err != nil {
		t.Fatal(err)
	}

	root := serveV2Request(router, http.MethodGet, "/", "", nil)
	if root.Code != http.StatusFound || root.Header().Get("Location") != "/docs/" {
		t.Fatalf("root response = status %d location %q", root.Code, root.Header().Get("Location"))
	}
	docs := serveV2Request(router, http.MethodGet, "/docs/", "", nil)
	if docs.Code != http.StatusOK || !strings.Contains(docs.Body.String(), "/api/v1/otp") ||
		!strings.Contains(docs.Body.String(), "/oauth2/v2.0/token") || !strings.Contains(docs.Body.String(), "IMAPS") ||
		strings.Contains(docs.Body.String(), adminPath) || strings.Contains(docs.Body.String(), "管理 API") {
		t.Fatalf("public docs contract failed: status=%d body=%s", docs.Code, docs.Body.String())
	}
	if response := serveV2Request(router, http.MethodGet, "/admin", "", nil); response.Code != http.StatusNotFound {
		t.Fatalf("fixed /admin status = %d", response.Code)
	}
	if response := serveV2Request(router, http.MethodGet, "/api/v1/aliases", "", nil); response.Code != http.StatusNotFound {
		t.Errorf("unsupported GET /api/v1/aliases status = %d", response.Code)
	}
	for _, legacyPath := range []string{
		"/api/v1/mail/latest",
		"/api/v1/mail/recent",
	} {
		response := serveV2Request(router, http.MethodGet, legacyPath, "", nil)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("legacy route %s status = %d", legacyPath, response.Code)
		}
	}
}

func TestAdminV2DeepLinksUseInstallationBasePathAllowedByCSP(t *testing.T) {
	env := newAdminAPITestEnv(t)
	const adminPath = "/0123456789abcdef0123456789abcdef/admin"
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "index.html"),
		[]byte(`<!doctype html><base href="./"><script src="./assets/app.js"></script>`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app.js"), []byte("export {};"), 0o600); err != nil {
		t.Fatal(err)
	}
	spa, err := loadAdminSPA(root)
	if err != nil {
		t.Fatal(err)
	}
	env.server.adminSPA = spa
	env.server.cfg.AdminPath = adminPath
	router, err := env.server.Router()
	if err != nil {
		t.Fatal(err)
	}

	deepLink := serveV2Request(router, http.MethodGet, adminPath+"/accounts/42", "", nil)
	if deepLink.Code != http.StatusOK ||
		!strings.Contains(deepLink.Body.String(), `<base href="`+adminPath+`/">`) {
		t.Fatalf("admin deep link = status %d body %q", deepLink.Code, deepLink.Body.String())
	}
	csp := deepLink.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "base-uri 'self'") || strings.Contains(csp, "base-uri 'none'") {
		t.Fatalf("admin deep-link CSP = %q", csp)
	}
	asset := serveV2Request(router, http.MethodGet, adminPath+"/assets/app.js", "", nil)
	if asset.Code != http.StatusOK || strings.TrimSpace(asset.Body.String()) != "export {};" {
		t.Fatalf("admin asset = status %d body %q", asset.Code, asset.Body.String())
	}
}

func TestAdminV2AlwaysReturnsCompleteCredentialBundleWithoutCaching(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "credential-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "credential-admin@icloud.com")
	alias, credentials := createV2AliasFixture(t, env, account.ID, "credential-view@icloud.com")
	target := "/admin/api/v1/aliases/" + strconvFormatInt(alias.ID)

	response := env.request(t, http.MethodGet, target, nil, "", []*http.Cookie{sessionCookie}, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("credential detail status/cache = %d %q; body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	var payload struct {
		Data adminAPIAliasDTO `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.APIKey != credentials.APIKey || payload.Data.IMAPPassword != credentials.IMAPPassword ||
		payload.Data.ClientID != credentials.ClientID || payload.Data.RefreshToken != credentials.RefreshToken ||
		payload.Data.CredentialVersion != 1 || !strings.HasPrefix(payload.Data.OTPURLPath, "/api/v1/otp?token=") {
		t.Fatalf("credential detail = %#v", payload.Data)
	}

	rotatedResponse := env.request(t, http.MethodPost, target+"/rotate-credentials", nil, "", []*http.Cookie{sessionCookie}, csrf)
	if rotatedResponse.Code != http.StatusOK || !strings.Contains(rotatedResponse.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("credential rotation status/cache = %d %q; body=%s", rotatedResponse.Code, rotatedResponse.Header().Get("Cache-Control"), rotatedResponse.Body.String())
	}
	var rotatedPayload struct {
		Data adminAPIAliasDTO `json:"data"`
	}
	if err := json.Unmarshal(rotatedResponse.Body.Bytes(), &rotatedPayload); err != nil {
		t.Fatal(err)
	}
	rotated := rotatedPayload.Data
	if rotated.CredentialVersion != 2 || rotated.APIKey == credentials.APIKey ||
		rotated.IMAPPassword == credentials.IMAPPassword || rotated.ClientID == credentials.ClientID ||
		rotated.RefreshToken == credentials.RefreshToken || rotated.OTPURLPath == payload.Data.OTPURLPath {
		t.Fatalf("rotated credential detail = %#v", rotated)
	}
}

func createV2AliasFixture(
	t *testing.T,
	env *adminAPITestEnv,
	accountID int64,
	address string,
) (domain.Alias, domain.AliasCredentials) {
	t.Helper()
	alias, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID: accountID,
		Address:   address,
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("create v2 alias fixture: %v", err)
	}
	credentials, err := env.cipher.DecryptAliasCredentials(alias.ID, alias.CredentialCiphertext)
	if err != nil {
		t.Fatalf("decrypt v2 alias credentials: %v", err)
	}
	return alias, credentials
}

func serveV2Request(
	router http.Handler,
	method, target, body string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://mail.example.test"+target, strings.NewReader(body))
	for name, value := range headers {
		if value != "" {
			request.Header.Set(name, value)
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
