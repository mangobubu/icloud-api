package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
)

// The external registration endpoint must return the credential bundle that
// was actually committed. A provisional API key generated at the HTTP layer
// would authenticate neither the legacy routes nor the returned direct link.
func TestExternalAliasResponseCredentialsMatchCommittedV2Bundle(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.oauthTokenConfigured = true
	env.server.oauthTokenHash = secure.HashToken("external-compat-oauth-token")
	env.server.sync = nil
	now := time.Date(2026, 8, 12, 4, 0, 0, 0, time.UTC)
	env.server.now = func() time.Time { return now }
	account := adminAPITestCreateAccount(t, env, "external-response@icloud.com")
	form := url.Values{
		externalAliasAddressField: {"external-response-alias@icloud.com"},
		externalAliasAccountField: {account.Email},
	}.Encode()

	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/aliases", strings.NewReader(form))
	request.Header.Set("Authorization", "Bearer external-compat-oauth-token")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("external alias status = %d; body=%s", response.Code, response.Body.String())
	}

	var payload struct {
		Data externalAliasResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode external alias response: %v", err)
	}
	alias, err := env.store.GetAliasByAddress(context.Background(), payload.Data.Alias)
	if err != nil {
		t.Fatalf("load created alias: %v", err)
	}
	credentials, err := env.cipher.DecryptAliasCredentials(alias.ID, alias.CredentialCiphertext)
	if err != nil {
		t.Fatalf("decrypt committed credentials: %v", err)
	}
	if payload.Data.APIKey != credentials.APIKey ||
		!secure.HashEqual(alias.APIKeyHash, secure.HashToken(payload.Data.APIKey)) {
		t.Fatalf("returned API key does not match committed bundle: response=%q committed=%q", payload.Data.APIKey, credentials.APIKey)
	}
	directURL, err := url.Parse(payload.Data.MailAPIDirectLink)
	if err != nil {
		t.Fatalf("parse returned recent-mail link: %v", err)
	}
	directToken := directURL.Query().Get("api_key")
	if !env.cipher.VerifyRecentMailToken(directToken, alias.ID, alias.APIKeyHash) ||
		env.cipher.VerifyOTPToken(directToken, alias.ID, alias.APIKeyHash) ||
		env.cipher.VerifyDirectLinkToken(directToken, alias.ID, alias.APIKeyHash) {
		t.Fatal("external alias endpoint did not return a recent-mail-purpose token")
	}

	// Publish a legacy snapshot so the returned direct link exercises the
	// original one-shot endpoint, not merely token parsing.
	messageTime := now.Add(-time.Minute)
	if _, err := env.store.UpsertLatestMessage(context.Background(), domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 91, UID: 3, Subject: "external compatibility",
		TextBody: "legacy-compatible body", InternalDate: messageTime, SyncedAt: now,
	}); err != nil {
		t.Fatalf("publish latest snapshot: %v", err)
	}
	if err := env.store.UpdateAliasSyncStatus(context.Background(), alias.ID, domain.SyncStatusOK, "", &now); err != nil {
		t.Fatalf("mark alias synced: %v", err)
	}
	legacy := serveV2Request(router, http.MethodGet,
		payload.Data.MailAPIDirectLink, "", nil)
	if legacy.Code != http.StatusOK || !strings.Contains(legacy.Body.String(), "legacy-compatible body") {
		t.Fatalf("returned direct link status = %d; body=%s", legacy.Code, legacy.Body.String())
	}
	latest := serveV2Request(router, http.MethodGet, "/api/v1/mail/latest", "", map[string]string{
		"Authorization": "Bearer " + payload.Data.APIKey,
	})
	if latest.Code != http.StatusOK || !strings.Contains(latest.Body.String(), "external compatibility") {
		t.Fatalf("returned API key status = %d; body=%s", latest.Code, latest.Body.String())
	}
}

func TestExternalAliasAcceptsLegacyQueryParametersWithoutBody(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.oauthTokenConfigured = true
	env.server.oauthTokenHash = secure.HashToken("query-compat-oauth-token")
	account := adminAPITestCreateAccount(t, env, "external-query@icloud.com")
	query := url.Values{
		externalAliasAddressField: {"external-query-alias@icloud.com"},
		externalAliasAccountField: {account.Email},
	}.Encode()

	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/aliases?"+query, nil)
	request.Header.Set("Authorization", "Bearer query-compat-oauth-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("query-only external alias status = %d; body=%s", response.Code, response.Body.String())
	}
	if _, err := env.store.GetAliasByAddress(context.Background(), "external-query-alias@icloud.com"); err != nil {
		t.Fatalf("load query-only external alias: %v", err)
	}
}

func TestStrictBearerTokenCompatibilityBoundary(t *testing.T) {
	tests := []struct {
		name      string
		values    []string
		wantToken string
		wantValid bool
	}{
		{name: "canonical", values: []string{"Bearer token-value"}, wantToken: "token-value", wantValid: true},
		{name: "case insensitive scheme", values: []string{"bEaReR token-value"}, wantToken: "token-value", wantValid: true},
		{name: "missing", values: nil},
		{name: "wrong scheme", values: []string{"Basic token-value"}},
		{name: "missing separator", values: []string{"Bearertoken-value"}},
		{name: "empty token", values: []string{"Bearer "}},
		{name: "extra separator", values: []string{"Bearer  token-value"}},
		{name: "token contains whitespace", values: []string{"Bearer token value"}},
		{name: "duplicate headers", values: []string{"Bearer token-value", "Bearer token-value"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/aliases", nil)
			for _, value := range test.values {
				request.Header.Add("Authorization", value)
			}
			token, valid := strictBearerToken(request)
			if token != test.wantToken || valid != test.wantValid {
				t.Fatalf("strictBearerToken() = (%q, %t), want (%q, %t)", token, valid, test.wantToken, test.wantValid)
			}
		})
	}
}

func TestExternalAliasRejectsInvalidOAuthBeforeParsingBody(t *testing.T) {
	tests := []struct {
		name                string
		authorizationValues []string
		configured          bool
	}{
		{name: "missing", configured: true},
		{name: "wrong token", authorizationValues: []string{"Bearer wrong-token"}, configured: true},
		{name: "wrong scheme", authorizationValues: []string{"Basic external-router-oauth-token"}, configured: true},
		{name: "extra separator", authorizationValues: []string{"Bearer  external-router-oauth-token"}, configured: true},
		{name: "trailing whitespace", authorizationValues: []string{"Bearer external-router-oauth-token "}, configured: true},
		{name: "duplicate headers", authorizationValues: []string{
			"Bearer external-router-oauth-token", "Bearer external-router-oauth-token",
		}, configured: true},
		{name: "token not configured", authorizationValues: []string{"Bearer external-router-oauth-token"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newAdminAPITestEnv(t)
			env.server.oauthTokenConfigured = test.configured
			if test.configured {
				env.server.oauthTokenHash = secure.HashToken("external-router-oauth-token")
			}
			adminAPITestCreateAccount(t, env, "external-auth-order@icloud.com")
			router, err := env.server.Router()
			if err != nil {
				t.Fatalf("build router: %v", err)
			}

			// The body is both oversized and the wrong media type. Authentication
			// must reject it before either parsing error becomes observable.
			request := httptest.NewRequest(http.MethodPost, "/api/v1/aliases",
				strings.NewReader(strings.Repeat("x", externalAliasMaxFormBytes+1)))
			request.Header.Set("Content-Type", "application/json")
			for _, value := range test.authorizationValues {
				request.Header.Add("Authorization", value)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			assertExternalAliasError(t, response, http.StatusUnauthorized, "INVALID_OAUTH_TOKEN")
			if response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q, want Bearer", response.Header().Get("WWW-Authenticate"))
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
			}
			assertExternalAliasCount(t, env, 0)
		})
	}
}

func TestExternalAliasOAuthAndAPIKeyCredentialsArePurposeBound(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.oauthTokenConfigured = true
	env.server.oauthTokenHash = secure.HashToken("purpose-bound-oauth-token")
	account := adminAPITestCreateAccount(t, env, "external-purpose-bound@icloud.com")
	alias, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID: account.ID,
		Address:   "existing-purpose-bound@icloud.com",
		Enabled:   true,
	})
	if err != nil {
		t.Fatalf("create purpose-bound alias: %v", err)
	}
	credentials, err := env.cipher.DecryptAliasCredentials(alias.ID, alias.CredentialCiphertext)
	if err != nil {
		t.Fatalf("decrypt purpose-bound alias credentials: %v", err)
	}
	form := url.Values{
		externalAliasAddressField: {"rejected-purpose-bound@icloud.com"},
		externalAliasAccountField: {account.Email},
	}.Encode()
	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/aliases", strings.NewReader(form))
	request.Header.Set("Authorization", "Bearer "+credentials.APIKey)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assertExternalAliasError(t, response, http.StatusUnauthorized, "INVALID_OAUTH_TOKEN")
	assertExternalAliasCount(t, env, 1)
}

func TestExternalAliasRejectsInvalidParametersWithoutWriting(t *testing.T) {
	tests := []struct {
		name     string
		form     url.Values
		wantCode string
	}{
		{name: "missing alias", form: url.Values{externalAliasAccountField: {"external-validation@icloud.com"}}, wantCode: "INVALID_REQUEST"},
		{name: "missing account", form: url.Values{externalAliasAddressField: {"external-validation-alias@icloud.com"}}, wantCode: "INVALID_REQUEST"},
		{name: "duplicate alias", form: url.Values{
			externalAliasAddressField: {"external-validation-alias@icloud.com", "other-validation-alias@icloud.com"},
			externalAliasAccountField: {"external-validation@icloud.com"},
		}, wantCode: "INVALID_REQUEST"},
		{name: "unknown parameter", form: url.Values{
			externalAliasAddressField: {"external-validation-alias@icloud.com"},
			externalAliasAccountField: {"external-validation@icloud.com"},
			"unexpected":              {"value"},
		}, wantCode: "INVALID_REQUEST"},
		{name: "invalid alias email", form: url.Values{
			externalAliasAddressField: {"not-an-email"},
			externalAliasAccountField: {"external-validation@icloud.com"},
		}, wantCode: "INVALID_REQUEST"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newAdminAPITestEnv(t)
			env.server.oauthTokenConfigured = true
			env.server.oauthTokenHash = secure.HashToken("validation-oauth-token")
			adminAPITestCreateAccount(t, env, "external-validation@icloud.com")
			router, err := env.server.Router()
			if err != nil {
				t.Fatalf("build router: %v", err)
			}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/aliases", strings.NewReader(test.form.Encode()))
			request.Header.Set("Authorization", "Bearer validation-oauth-token")
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			assertExternalAliasError(t, response, http.StatusBadRequest, test.wantCode)
			assertExternalAliasCount(t, env, 0)
		})
	}
}

func TestExternalAliasRejectsDuplicateQueryAndBodyFields(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.oauthTokenConfigured = true
	env.server.oauthTokenHash = secure.HashToken("duplicate-compat-oauth-token")
	account := adminAPITestCreateAccount(t, env, "external-duplicate@icloud.com")
	body := url.Values{
		externalAliasAddressField: {"external-duplicate-alias@icloud.com"},
		externalAliasAccountField: {account.Email},
	}.Encode()
	query := url.Values{
		externalAliasAddressField: {"external-duplicate-alias@icloud.com"},
	}.Encode()

	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/aliases?"+query, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer duplicate-compat-oauth-token")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assertExternalAliasError(t, response, http.StatusBadRequest, "INVALID_REQUEST")
	if _, err := env.store.GetAliasByAddress(context.Background(), "external-duplicate-alias@icloud.com"); err == nil {
		t.Fatal("duplicate query/body request unexpectedly created an alias")
	}
}

func TestExternalAliasRejectsBodyWithUnsupportedMediaType(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.oauthTokenConfigured = true
	env.server.oauthTokenHash = secure.HashToken("content-type-compat-oauth-token")
	body := `{"add_hide_my_eamil":"alias@icloud.com","icloud":"primary@icloud.com"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/aliases", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer content-type-compat-oauth-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	router.ServeHTTP(response, request)
	assertExternalAliasError(t, response, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")
}

func TestExternalAliasRejectsKnownLengthOversizedForm(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.oauthTokenConfigured = true
	env.server.oauthTokenHash = secure.HashToken("oversized-compat-oauth-token")
	body := strings.NewReader(strings.Repeat("x", externalAliasMaxFormBytes+1))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/aliases", body)
	request.Header.Set("Authorization", "Bearer oversized-compat-oauth-token")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	router.ServeHTTP(response, request)
	assertExternalAliasError(t, response, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE")
}

func TestExternalAliasRejectsChunkedOversizedForm(t *testing.T) {
	env := newAdminAPITestEnv(t)
	env.server.oauthTokenConfigured = true
	env.server.oauthTokenHash = secure.HashToken("chunked-compat-oauth-token")
	body := strings.NewReader(strings.Repeat("x", externalAliasMaxFormBytes+1))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/aliases", body)
	request.ContentLength = -1
	request.TransferEncoding = []string{"chunked"}
	request.Header.Set("Authorization", "Bearer chunked-compat-oauth-token")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	router.ServeHTTP(response, request)
	assertExternalAliasError(t, response, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE")
}

func assertExternalAliasError(t *testing.T, response *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("external alias error status = %d, want %d; body=%s", response.Code, wantStatus, response.Body.String())
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode external alias error: %v", err)
	}
	if payload.Error.Code != wantCode {
		t.Fatalf("external alias error code = %q, want %q", payload.Error.Code, wantCode)
	}
}

func assertExternalAliasCount(t *testing.T, env *adminAPITestEnv, want int) {
	t.Helper()
	aliases, err := env.store.ListAliases(context.Background())
	if err != nil {
		t.Fatalf("list external aliases: %v", err)
	}
	if len(aliases) != want {
		t.Fatalf("external alias count = %d, want %d", len(aliases), want)
	}
}
