package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAdminAPIDualEntryPoints exercises the complete API login flow through
// the real Server.Router. The installation-specific path is the canonical
// entry point, while /admin/api/v1 remains available for existing clients.
func TestAdminAPIDualEntryPoints(t *testing.T) {
	const randomAdminPath = "/0123456789abcdef0123456789abcdef/admin"
	const fixedAPIPath = "/admin/api/v1"

	tests := []struct {
		name              string
		loginPath         string
		sessionCookiePath string
	}{
		{name: "login through random path", loginPath: randomAdminPath + "/api/v1", sessionCookiePath: randomAdminPath},
		{name: "login through fixed path", loginPath: fixedAPIPath, sessionCookiePath: "/admin"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newAdminAPITestEnv(t)
			env.server.cfg.AdminPath = randomAdminPath
			router, err := env.server.Router()
			if err != nil {
				t.Fatalf("build router: %v", err)
			}
			admin := env.createAdmin(t, "dual-entry-"+strings.ReplaceAll(test.name, " ", "-"), "correct-password")

			loginCSRFResponse := serveAdminRouterRequest(router, http.MethodGet, test.loginPath+"/auth/csrf", nil, nil)
			if loginCSRFResponse.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d; body=%s", test.loginPath+"/auth/csrf", loginCSRFResponse.Code, loginCSRFResponse.Body.String())
			}
			loginCSRFToken := adminAPITestCSRFToken(t, loginCSRFResponse)
			loginCSRFCookies := cookiesByName(loginCSRFResponse.Result().Cookies(), adminAPILoginCSRFCookie)
			if len(loginCSRFCookies) != 1 {
				t.Fatalf("GET %s Set-Cookie = %#v, want one login CSRF cookie", test.loginPath+"/auth/csrf", loginCSRFCookies)
			}
			wantCSRFCookiePath := test.loginPath + "/auth"
			if loginCSRFCookies[0].Path != wantCSRFCookiePath {
				t.Fatalf("GET %s login CSRF cookie Path = %q, want %q", test.loginPath+"/auth/csrf", loginCSRFCookies[0].Path, wantCSRFCookiePath)
			}

			loginBody := adminAPITestJSON(t, map[string]string{
				"username": admin.Username,
				"password": "correct-password",
			})
			loginResponse := serveAdminRouterRequest(router, http.MethodPost, test.loginPath+"/auth/login", loginBody, map[string]string{
				"Content-Type":     "application/json",
				"Origin":           "http://admin.example.test",
				adminAPICSRFHeader: loginCSRFToken,
			}, loginCSRFCookies[0])
			if loginResponse.Code != http.StatusOK {
				t.Fatalf("POST %s status = %d; body=%s", test.loginPath+"/auth/login", loginResponse.Code, loginResponse.Body.String())
			}
			var loginPayload struct {
				Data adminAPISessionDTO `json:"data"`
			}
			if err := json.Unmarshal(loginResponse.Body.Bytes(), &loginPayload); err != nil {
				t.Fatalf("decode login response: %v; body=%s", err, loginResponse.Body.String())
			}
			if loginPayload.Data.Admin.Username != admin.Username || loginPayload.Data.CSRFToken == "" {
				t.Fatalf("login session payload = %#v", loginPayload.Data)
			}
			sessionCookies := cookiesByName(loginResponse.Result().Cookies(), sessionCookie)
			if len(sessionCookies) != 2 {
				t.Fatalf("login Set-Cookie = %#v, want session cookies for both admin paths", sessionCookies)
			}
			wantSessionPaths := map[string]bool{randomAdminPath: false, "/admin": false}
			sessionByPath := make(map[string]*http.Cookie, len(wantSessionPaths))
			var rawSessionToken string
			for _, cookie := range sessionCookies {
				if cookie.Value == "" {
					t.Fatalf("login session cookie is empty: %#v", cookie)
				}
				if rawSessionToken != "" && cookie.Value != rawSessionToken {
					t.Fatalf("login session cookies use different tokens")
				}
				rawSessionToken = cookie.Value
				if _, ok := wantSessionPaths[cookie.Path]; !ok {
					t.Fatalf("login session cookie Path = %q; want %v", cookie.Path, wantSessionPaths)
				}
				if wantSessionPaths[cookie.Path] {
					t.Fatalf("login emitted duplicate session cookie Path %q", cookie.Path)
				}
				wantSessionPaths[cookie.Path] = true
				sessionByPath[cookie.Path] = cookie
			}
			for path, seen := range wantSessionPaths {
				if !seen {
					t.Fatalf("login did not emit session cookie Path %q", path)
				}
			}

			// The same session must authenticate through both API prefixes using
			// the cookie whose Path applies to that prefix.
			for _, entry := range []struct {
				apiPath    string
				cookiePath string
			}{
				{apiPath: randomAdminPath + "/api/v1", cookiePath: randomAdminPath},
				{apiPath: fixedAPIPath, cookiePath: "/admin"},
			} {
				sessionResponse := serveAdminRouterRequest(router, http.MethodGet, entry.apiPath+"/auth/session", nil, nil, sessionByPath[entry.cookiePath])
				if sessionResponse.Code != http.StatusOK {
					t.Fatalf("GET %s/auth/session status = %d; body=%s", entry.apiPath, sessionResponse.Code, sessionResponse.Body.String())
				}
				var payload struct {
					Data adminAPISessionDTO `json:"data"`
				}
				if err := json.Unmarshal(sessionResponse.Body.Bytes(), &payload); err != nil {
					t.Fatalf("decode %s session response: %v", entry.apiPath, err)
				}
				if payload.Data.Admin.Username != admin.Username || payload.Data.CSRFToken != loginPayload.Data.CSRFToken {
					t.Fatalf("session through %s = %#v; login = %#v", entry.apiPath, payload.Data, loginPayload.Data)
				}
			}

			// Mutations through either prefix must advertise a Location rooted at
			// the prefix that received the request.
			accountBody := adminAPITestJSON(t, map[string]string{
				"name":          "Dual entry account",
				"email":         "dual-entry-" + strings.ReplaceAll(test.name, " ", "-") + "@icloud.com",
				"imap_username": "dual-entry@example.com",
				"imap_password": "app-specific-password",
			})
			accountResponse := serveAdminRouterRequest(router, http.MethodPost, randomAdminPath+"/api/v1/accounts", accountBody, map[string]string{
				"Content-Type":     "application/json",
				"Origin":           "http://admin.example.test",
				adminAPICSRFHeader: loginPayload.Data.CSRFToken,
			}, sessionByPath[randomAdminPath])
			if accountResponse.Code != http.StatusCreated {
				t.Fatalf("POST random accounts status = %d; body=%s", accountResponse.Code, accountResponse.Body.String())
			}
			var accountPayload struct {
				Data adminAPIAccountDTO `json:"data"`
			}
			if err := json.Unmarshal(accountResponse.Body.Bytes(), &accountPayload); err != nil {
				t.Fatalf("decode account response: %v", err)
			}
			wantAccountLocation := randomAdminPath + "/api/v1/accounts/" + strconvFormatInt(accountPayload.Data.ID)
			if got := accountResponse.Header().Get("Location"); got != wantAccountLocation {
				t.Fatalf("random account Location = %q, want %q", got, wantAccountLocation)
			}

			aliasBody := adminAPITestJSON(t, map[string]string{
				"address": "dual-alias-" + strings.ReplaceAll(test.name, " ", "-") + "@icloud.com",
				"label":   "dual entry",
			})
			aliasResponse := serveAdminRouterRequest(router, http.MethodPost,
				fixedAPIPath+"/accounts/"+strconvFormatInt(accountPayload.Data.ID)+"/aliases", aliasBody, map[string]string{
					"Content-Type":     "application/json",
					"Origin":           "http://admin.example.test",
					adminAPICSRFHeader: loginPayload.Data.CSRFToken,
				}, sessionByPath["/admin"])
			if aliasResponse.Code != http.StatusCreated {
				t.Fatalf("POST fixed aliases status = %d; body=%s", aliasResponse.Code, aliasResponse.Body.String())
			}
			var aliasPayload struct {
				Data struct {
					Alias adminAPIAliasDTO `json:"alias"`
				} `json:"data"`
			}
			if err := json.Unmarshal(aliasResponse.Body.Bytes(), &aliasPayload); err != nil {
				t.Fatalf("decode alias response: %v", err)
			}
			wantAliasLocation := fixedAPIPath + "/aliases/" + strconvFormatInt(aliasPayload.Data.Alias.ID)
			if got := aliasResponse.Header().Get("Location"); got != wantAliasLocation {
				t.Fatalf("fixed alias Location = %q, want %q", got, wantAliasLocation)
			}

			logoutResponse := serveAdminRouterRequest(router, http.MethodPost, test.loginPath+"/auth/logout", nil, map[string]string{
				"Origin":           "http://admin.example.test",
				adminAPICSRFHeader: loginPayload.Data.CSRFToken,
			}, sessionByPath[test.sessionCookiePath])
			if logoutResponse.Code != http.StatusNoContent {
				t.Fatalf("POST %s/auth/logout status = %d; body=%s", test.loginPath, logoutResponse.Code, logoutResponse.Body.String())
			}
			logoutCookies := cookiesByName(logoutResponse.Result().Cookies(), sessionCookie)
			if len(logoutCookies) != 2 {
				t.Fatalf("logout Set-Cookie = %#v, want both session paths cleared", logoutCookies)
			}
			clearedPaths := map[string]bool{randomAdminPath: false, "/admin": false}
			for _, cookie := range logoutCookies {
				if cookie.MaxAge >= 0 || cookie.Value != "" {
					t.Fatalf("logout cookie was not expired: %#v", cookie)
				}
				if _, ok := clearedPaths[cookie.Path]; !ok {
					t.Fatalf("logout cleared unexpected Path %q", cookie.Path)
				}
				clearedPaths[cookie.Path] = true
			}
			for path, cleared := range clearedPaths {
				if !cleared {
					t.Fatalf("logout did not clear session Path %q", path)
				}
			}
		})
	}
}

func serveAdminRouterRequest(router http.Handler, method, target string, body []byte, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://admin.example.test"+target, strings.NewReader(string(body)))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	for _, cookie := range cookies {
		if cookie != nil {
			request.AddCookie(cookie)
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func adminAPITestCSRFToken(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode login CSRF response: %v; body=%s", err, response.Body.String())
	}
	if payload.Data.CSRFToken == "" {
		t.Fatalf("login CSRF response omitted token: %s", response.Body.String())
	}
	return payload.Data.CSRFToken
}

func cookiesByName(cookies []*http.Cookie, name string) []*http.Cookie {
	result := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie.Name == name {
			result = append(result, cookie)
		}
	}
	return result
}
