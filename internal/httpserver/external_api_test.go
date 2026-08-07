package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
)

const testExternalOAuthToken = "external-oauth-test-token-0123456789abcdef"

func TestExternalAddAliasPostBodyCreatesOwnedAliasAndDirectLink(t *testing.T) {
	env := newHTTPTestEnv(t)
	account := env.createAccount(t, "External API Primary", "primary-external@icloud.com", "encrypted")
	form := url.Values{
		externalAliasAddressField: {"  Hidden.External@iCloud.COM  "},
		externalAliasAccountField: {"  PRIMARY-EXTERNAL@ICLOUD.COM  "},
	}

	response := externalAddAliasRequest(
		t, env, "/api/v1/aliases", strings.NewReader(form.Encode()),
		"application/x-www-form-urlencoded", testExternalOAuthToken,
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("POST body status = %d, want %d; body=%s", response.Code, http.StatusCreated, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("POST body Cache-Control = %q, want no-store", got)
	}

	payload := decodeExternalAliasResponse(t, response)
	if payload.Alias != "hidden.external@icloud.com" {
		t.Fatalf("data.alias = %q", payload.Alias)
	}
	if payload.ICloud != account.Email {
		t.Fatalf("data.icloud = %q, want %q", payload.ICloud, account.Email)
	}
	if !strings.HasPrefix(payload.APIKey, "icm_") || len(payload.APIKey) != 47 {
		t.Fatalf("data.api_key has invalid format: %q", payload.APIKey)
	}

	alias, err := env.store.GetAliasByAddress(context.Background(), payload.Alias)
	if err != nil {
		t.Fatalf("load created alias: %v", err)
	}
	if alias.AccountID != account.ID || alias.AccountEmail != account.Email {
		t.Fatalf("created alias ownership = account %d/%q, want %d/%q", alias.AccountID, alias.AccountEmail, account.ID, account.Email)
	}
	if !alias.Enabled || alias.Address != payload.Alias {
		t.Fatalf("created alias state = %#v", alias)
	}
	if alias.LastSyncStatus != domain.SyncStatusPending {
		t.Fatalf("created alias sync status = %q, want %q", alias.LastSyncStatus, domain.SyncStatusPending)
	}
	if !secure.HashEqual(alias.APIKeyHash, secure.HashToken(payload.APIKey)) {
		t.Fatal("stored API key hash does not match returned one-time key")
	}
	if alias.APIKeyPrefix != payload.APIKey[:12] {
		t.Fatalf("stored API key prefix = %q, want %q", alias.APIKeyPrefix, payload.APIKey[:12])
	}
	for _, forbidden := range []string{testExternalOAuthToken, "api_key_hash", "APIKeyHash"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("external alias response exposed %q: %s", forbidden, response.Body.String())
		}
	}
	assertExternalDirectLink(t, env, alias, payload.MailAPIDirectLink)

	now := time.Now().UTC().Truncate(time.Second)
	env.server.now = func() time.Time { return now }
	setAliasSyncedAt(t, env, alias, now)
	env.upsertMessage(t, domain.LatestMessage{
		AliasID: alias.ID, UIDValidity: 901, UID: 7,
		InternalDate: now.Add(-time.Minute), TextBody: "external direct-link body", SyncedAt: now,
	})
	directResponse := env.request(t, http.MethodGet, payload.MailAPIDirectLink, nil, nil)
	if directResponse.Code != http.StatusOK || !strings.Contains(directResponse.Body.String(), "external direct-link body") {
		t.Fatalf("returned direct link response = %d; body=%s", directResponse.Code, directResponse.Body.String())
	}
}

func TestExternalAddAliasAcceptsPostQueryParameters(t *testing.T) {
	env := newHTTPTestEnv(t)
	account := env.createAccount(t, "Query Primary", "query-primary@icloud.com", "encrypted")
	query := url.Values{
		externalAliasAddressField: {"query-hidden@icloud.com"},
		externalAliasAccountField: {account.Email},
	}

	response := externalAddAliasRequest(
		t, env, "/api/v1/aliases?"+query.Encode(), nil, "", testExternalOAuthToken,
	)
	if response.Code != http.StatusCreated || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("POST query status/cache = %d/%q; body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	payload := decodeExternalAliasResponse(t, response)
	if payload.Alias != "query-hidden@icloud.com" || payload.ICloud != account.Email || payload.APIKey == "" || payload.MailAPIDirectLink == "" {
		t.Fatalf("POST query data = %#v", payload)
	}
	alias, err := env.store.GetAliasByAddress(context.Background(), payload.Alias)
	if err != nil {
		t.Fatalf("load query-created alias: %v", err)
	}
	if alias.AccountID != account.ID || !secure.HashEqual(alias.APIKeyHash, secure.HashToken(payload.APIKey)) {
		t.Fatalf("query-created alias persistence = %#v", alias)
	}
	assertExternalDirectLink(t, env, alias, payload.MailAPIDirectLink)
}

func TestExternalAddAliasRequiresStrictOAuthBearerToken(t *testing.T) {
	env := newHTTPTestEnv(t)
	account := env.createAccount(t, "Auth Primary", "auth-primary@icloud.com", "encrypted")
	form := url.Values{
		externalAliasAddressField: {"auth-hidden@icloud.com"},
		externalAliasAccountField: {account.Email},
	}.Encode()
	tests := []struct {
		name    string
		headers []string
	}{
		{name: "missing"},
		{name: "wrong", headers: []string{"Bearer wrong-oauth-token"}},
		{name: "basic", headers: []string{"Basic " + testExternalOAuthToken}},
		{name: "extra leading space", headers: []string{"Bearer  " + testExternalOAuthToken}},
		{name: "extra trailing space", headers: []string{"Bearer " + testExternalOAuthToken + " "}},
		{name: "multiple", headers: []string{"Bearer " + testExternalOAuthToken, "Bearer " + testExternalOAuthToken}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := externalAddAliasRequestWithAuthorizationValues(
				t, env, "/api/v1/aliases", strings.NewReader(form),
				"application/x-www-form-urlencoded", tt.headers,
			)
			assertAPIError(t, response, http.StatusUnauthorized, "INVALID_OAUTH_TOKEN")
			if got := response.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
			}
			if got := response.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
			assertExternalAliasCount(t, env, 0)
		})
	}
}

func TestExternalAddAliasRejectsWhenOAuthTokenIsNotConfigured(t *testing.T) {
	env := newHTTPTestEnv(t)
	account := env.createAccount(t, "Unconfigured OAuth Primary", "unconfigured-oauth@icloud.com", "encrypted")
	env.server.oauthTokenConfigured = false
	env.server.oauthTokenHash = nil
	form := url.Values{
		externalAliasAddressField: {"unconfigured-hidden@icloud.com"},
		externalAliasAccountField: {account.Email},
	}.Encode()

	response := externalAddAliasRequest(
		t, env, "/api/v1/aliases", strings.NewReader(form),
		"application/x-www-form-urlencoded", testExternalOAuthToken,
	)
	assertAPIError(t, response, http.StatusUnauthorized, "INVALID_OAUTH_TOKEN")
	assertExternalAliasCount(t, env, 0)
}

func TestExternalAddAliasAuthenticatesBeforeParsingRequest(t *testing.T) {
	env := newHTTPTestEnv(t)
	account := env.createAccount(t, "Auth Order Primary", "auth-order@icloud.com", "encrypted")
	validForm := url.Values{
		externalAliasAddressField: {"auth-order-hidden@icloud.com"},
		externalAliasAccountField: {account.Email},
	}.Encode()
	tests := []struct {
		name        string
		body        string
		contentType string
	}{
		{name: "unsupported content type", body: validForm, contentType: "application/json"},
		{name: "oversized body", body: strings.Repeat("x", externalAliasMaxFormBytes+1), contentType: "application/x-www-form-urlencoded"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := externalAddAliasRequest(
				t, env, "/api/v1/aliases", strings.NewReader(tt.body), tt.contentType, "wrong-token",
			)
			assertAPIError(t, response, http.StatusUnauthorized, "INVALID_OAUTH_TOKEN")
			assertExternalAliasCount(t, env, 0)
		})
	}
}

func TestExternalAddAliasRejectsInvalidParametersWithoutWriting(t *testing.T) {
	env := newHTTPTestEnv(t)
	account := env.createAccount(t, "Validation Primary", "validation-primary@icloud.com", "encrypted")
	validAlias := "validation-hidden@icloud.com"
	tests := []struct {
		name string
		form url.Values
	}{
		{
			name: "duplicate alias",
			form: url.Values{
				externalAliasAddressField: {validAlias, "other-hidden@icloud.com"},
				externalAliasAccountField: {account.Email},
			},
		},
		{
			name: "duplicate icloud",
			form: url.Values{
				externalAliasAddressField: {validAlias},
				externalAliasAccountField: {account.Email, "other-primary@icloud.com"},
			},
		},
		{
			name: "missing alias",
			form: url.Values{externalAliasAccountField: {account.Email}},
		},
		{
			name: "missing icloud",
			form: url.Values{externalAliasAddressField: {validAlias}},
		},
		{
			name: "unknown parameter",
			form: url.Values{
				externalAliasAddressField: {validAlias},
				externalAliasAccountField: {account.Email},
				"unexpected":              {"value"},
			},
		},
		{
			name: "invalid alias email",
			form: url.Values{
				externalAliasAddressField: {"not-an-email"},
				externalAliasAccountField: {account.Email},
			},
		},
		{
			name: "invalid icloud email",
			form: url.Values{
				externalAliasAddressField: {validAlias},
				externalAliasAccountField: {"not-an-email"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := externalAddAliasRequest(
				t, env, "/api/v1/aliases", strings.NewReader(tt.form.Encode()),
				"application/x-www-form-urlencoded", testExternalOAuthToken,
			)
			assertAPIError(t, response, http.StatusBadRequest, "INVALID_REQUEST")
			assertExternalAliasCount(t, env, 0)
		})
	}
}

func TestExternalAddAliasRejectsFieldRepeatedAcrossQueryAndBody(t *testing.T) {
	env := newHTTPTestEnv(t)
	account := env.createAccount(t, "Repeated Field Primary", "repeated-field@icloud.com", "encrypted")
	query := url.Values{externalAliasAddressField: {"repeated-hidden@icloud.com"}}
	body := url.Values{
		externalAliasAddressField: {"repeated-hidden@icloud.com"},
		externalAliasAccountField: {account.Email},
	}.Encode()

	response := externalAddAliasRequest(
		t, env, "/api/v1/aliases?"+query.Encode(), strings.NewReader(body),
		"application/x-www-form-urlencoded", testExternalOAuthToken,
	)
	assertAPIError(t, response, http.StatusBadRequest, "INVALID_REQUEST")
	assertExternalAliasCount(t, env, 0)
}

func TestExternalAddAliasCredentialsArePurposeBound(t *testing.T) {
	env := newHTTPTestEnv(t)
	mailboxes := env.createMailboxFixture(t)

	mailResponse := env.apiRequest(t, "/api/v1/mail/latest", testExternalOAuthToken)
	assertAPIError(t, mailResponse, http.StatusUnauthorized, "INVALID_API_KEY")

	form := url.Values{
		externalAliasAddressField: {"purpose-bound-hidden@icloud.com"},
		externalAliasAccountField: {mailboxes.accountA.Email},
	}.Encode()
	aliasResponse := externalAddAliasRequest(
		t, env, "/api/v1/aliases", strings.NewReader(form),
		"application/x-www-form-urlencoded", mailboxes.keyA,
	)
	assertAPIError(t, aliasResponse, http.StatusUnauthorized, "INVALID_OAUTH_TOKEN")
	assertExternalAliasCount(t, env, 2)
}

func TestExternalAddAliasRejectsUnsupportedContentType(t *testing.T) {
	env := newHTTPTestEnv(t)
	account := env.createAccount(t, "Media Primary", "media-primary@icloud.com", "encrypted")
	form := url.Values{
		externalAliasAddressField: {"media-hidden@icloud.com"},
		externalAliasAccountField: {account.Email},
	}.Encode()

	response := externalAddAliasRequest(
		t, env, "/api/v1/aliases", strings.NewReader(form), "application/json", testExternalOAuthToken,
	)
	assertAPIError(t, response, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")
	assertExternalAliasCount(t, env, 0)
}

func TestExternalAddAliasRejectsOversizedBody(t *testing.T) {
	env := newHTTPTestEnv(t)
	env.createAccount(t, "Large Body Primary", "large-primary@icloud.com", "encrypted")
	body := strings.Repeat("x", externalAliasMaxFormBytes+1)

	response := externalAddAliasRequest(
		t, env, "/api/v1/aliases", strings.NewReader(body),
		"application/x-www-form-urlencoded", testExternalOAuthToken,
	)
	assertAPIError(t, response, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE")
	assertExternalAliasCount(t, env, 0)
}

func TestExternalAddAliasReturnsNotFoundForUnknownAccount(t *testing.T) {
	env := newHTTPTestEnv(t)
	form := url.Values{
		externalAliasAddressField: {"orphan-hidden@icloud.com"},
		externalAliasAccountField: {"missing-primary@icloud.com"},
	}.Encode()

	response := externalAddAliasRequest(
		t, env, "/api/v1/aliases", strings.NewReader(form),
		"application/x-www-form-urlencoded", testExternalOAuthToken,
	)
	assertAPIError(t, response, http.StatusNotFound, "ACCOUNT_NOT_FOUND")
	assertExternalAliasCount(t, env, 0)
}

func TestExternalAddAliasRejectsRegisteredAccountIdentities(t *testing.T) {
	env := newHTTPTestEnv(t)
	account, err := env.store.CreateAccount(context.Background(), domain.Account{
		Name: "Identity Primary", Email: "identity-primary@icloud.com",
		IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPUsername: "identity-login@icloud.com",
		PasswordCiphertext: "encrypted", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create identity account: %v", err)
	}
	other := env.createAccount(t, "Other Primary", "other-primary@icloud.com", "encrypted")

	for _, address := range []string{account.Email, account.IMAPUsername, other.Email} {
		t.Run(address, func(t *testing.T) {
			form := url.Values{
				externalAliasAddressField: {address},
				externalAliasAccountField: {account.Email},
			}.Encode()
			response := externalAddAliasRequest(
				t, env, "/api/v1/aliases", strings.NewReader(form),
				"application/x-www-form-urlencoded", testExternalOAuthToken,
			)
			assertAPIError(t, response, http.StatusBadRequest, "INVALID_REQUEST")
			assertExternalAliasCount(t, env, 0)
		})
	}
}

func TestExternalAddAliasReturnsConflictForDuplicateAddress(t *testing.T) {
	env := newHTTPTestEnv(t)
	account := env.createAccount(t, "Duplicate Primary", "duplicate-primary@icloud.com", "encrypted")
	const address = "duplicate-hidden@icloud.com"
	if _, err := env.store.CreateAlias(context.Background(), domain.Alias{
		AccountID:    account.ID,
		Address:      address,
		APIKeyHash:   secure.HashToken("existing-duplicate-api-key"),
		APIKeyPrefix: "existing-key",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("create duplicate fixture: %v", err)
	}
	form := url.Values{
		externalAliasAddressField: {strings.ToUpper(address)},
		externalAliasAccountField: {account.Email},
	}.Encode()

	response := externalAddAliasRequest(
		t, env, "/api/v1/aliases", strings.NewReader(form),
		"application/x-www-form-urlencoded", testExternalOAuthToken,
	)
	assertAPIError(t, response, http.StatusConflict, "ALIAS_EXISTS")
	assertExternalAliasCount(t, env, 1)
}

func TestExternalAddAliasReturnsConflictAtEnabledAliasLimit(t *testing.T) {
	env := newHTTPTestEnv(t)
	account := env.createAccount(t, "Limit Primary", "limit-primary@icloud.com", "encrypted")
	for index := 0; index < domain.MaxEnabledAliasesPerAccount; index++ {
		key := fmt.Sprintf("limit-api-key-%03d", index)
		if _, err := env.store.CreateAlias(context.Background(), domain.Alias{
			AccountID:    account.ID,
			Address:      fmt.Sprintf("limit-hidden-%03d@icloud.com", index),
			APIKeyHash:   secure.HashToken(key),
			APIKeyPrefix: key,
			Enabled:      true,
		}); err != nil {
			t.Fatalf("create limit fixture %d: %v", index, err)
		}
	}
	duplicateForm := url.Values{
		externalAliasAddressField: {"limit-hidden-000@icloud.com"},
		externalAliasAccountField: {account.Email},
	}.Encode()
	duplicateResponse := externalAddAliasRequest(
		t, env, "/api/v1/aliases", strings.NewReader(duplicateForm),
		"application/x-www-form-urlencoded", testExternalOAuthToken,
	)
	assertAPIError(t, duplicateResponse, http.StatusConflict, "ALIAS_EXISTS")

	form := url.Values{
		externalAliasAddressField: {"over-limit-hidden@icloud.com"},
		externalAliasAccountField: {account.Email},
	}.Encode()

	response := externalAddAliasRequest(
		t, env, "/api/v1/aliases", strings.NewReader(form),
		"application/x-www-form-urlencoded", testExternalOAuthToken,
	)
	assertAPIError(t, response, http.StatusConflict, "ALIAS_LIMIT_REACHED")
	assertExternalAliasCount(t, env, domain.MaxEnabledAliasesPerAccount)
}

func TestExternalAddAliasReportsDatabaseFailure(t *testing.T) {
	env := newHTTPTestEnv(t)
	account := env.createAccount(t, "Closed DB Primary", "closed-db-primary@icloud.com", "encrypted")
	if err := env.store.Close(); err != nil {
		t.Fatalf("close test database: %v", err)
	}
	form := url.Values{
		externalAliasAddressField: {"closed-db-hidden@icloud.com"},
		externalAliasAccountField: {account.Email},
	}.Encode()

	response := externalAddAliasRequest(
		t, env, "/api/v1/aliases", strings.NewReader(form),
		"application/x-www-form-urlencoded", testExternalOAuthToken,
	)
	assertAPIError(t, response, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE")
}

func externalAddAliasRequest(
	t *testing.T,
	env *httpTestEnv,
	target string,
	body io.Reader,
	contentType string,
	oauthToken string,
) *httptest.ResponseRecorder {
	t.Helper()
	var headers []string
	if oauthToken != "" {
		headers = []string{"Bearer " + oauthToken}
	}
	return externalAddAliasRequestWithAuthorizationValues(t, env, target, body, contentType, headers)
}

func externalAddAliasRequestWithAuthorizationValues(
	t *testing.T,
	env *httpTestEnv,
	target string,
	body io.Reader,
	contentType string,
	authorizationValues []string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, target, body)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for _, value := range authorizationValues {
		request.Header.Add("Authorization", value)
	}
	response := httptest.NewRecorder()
	env.router.ServeHTTP(response, request)
	return response
}

func decodeExternalAliasResponse(t *testing.T, response *httptest.ResponseRecorder) externalAliasResponse {
	t.Helper()
	var payload struct {
		Data externalAliasResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode external alias response: %v; body=%s", err, response.Body.String())
	}
	return payload.Data
}

func assertExternalDirectLink(t *testing.T, env *httpTestEnv, alias domain.Alias, directLink string) {
	t.Helper()
	parsed, err := url.Parse(directLink)
	if err != nil {
		t.Fatalf("parse mail_api_direct_link: %v", err)
	}
	if parsed.IsAbs() || parsed.Path != "/api/v1/mail/recent" {
		t.Fatalf("mail_api_direct_link = %q, want same-origin recent API path", directLink)
	}
	token := parsed.Query().Get("api_key")
	if token == "" || parsed.Query().Encode() != (url.Values{"api_key": {token}}).Encode() {
		t.Fatalf("mail_api_direct_link query = %q", parsed.RawQuery)
	}
	expected, err := env.cipher.DirectLinkToken(alias.ID, alias.APIKeyHash)
	if err != nil {
		t.Fatalf("derive expected direct-link token: %v", err)
	}
	if token != expected || !env.cipher.VerifyDirectLinkToken(token, alias.ID, alias.APIKeyHash) {
		t.Fatal("mail_api_direct_link token is not derived from the created alias and API key hash")
	}
}

func assertExternalAliasCount(t *testing.T, env *httpTestEnv, want int) {
	t.Helper()
	aliases, err := env.store.ListAliases(context.Background())
	if err != nil {
		t.Fatalf("list aliases: %v", err)
	}
	if len(aliases) != want {
		t.Fatalf("alias count = %d, want %d", len(aliases), want)
	}
}
