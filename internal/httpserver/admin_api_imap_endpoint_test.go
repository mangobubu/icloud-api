package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"icloud-api/internal/domain"
)

func TestAdminAPIAccountIMAPEndpointCreateDefaultsAndPersists(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "imap-create-admin", "unused-imap-create-password")

	tests := []struct {
		name     string
		email    string
		host     any
		port     any
		wantHost string
		wantPort int
	}{
		{
			name:     "omitted host and port use iCloud defaults",
			email:    "default-imap-endpoint@icloud.com",
			wantHost: domain.DefaultIMAPHost,
			wantPort: domain.DefaultIMAPPort,
		},
		{
			name:     "omitted host uses iCloud host default",
			email:    "default-imap-host@icloud.com",
			port:     1993,
			wantHost: domain.DefaultIMAPHost,
			wantPort: 1993,
		},
		{
			name:     "omitted port uses iCloud port default",
			email:    "default-imap-port@icloud.com",
			host:     "custom.imap.example",
			wantHost: "custom.imap.example",
			wantPort: domain.DefaultIMAPPort,
		},
		{
			name:     "custom endpoint is normalized",
			email:    "custom-imap-endpoint@icloud.com",
			host:     " IMAP.Example.Test. ",
			port:     1993,
			wantHost: "imap.example.test",
			wantPort: 1993,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := map[string]any{
				"name":          test.name,
				"email":         test.email,
				"imap_username": test.email,
				"imap_password": "app-password-for-imap-endpoint-test",
			}
			if test.host != nil {
				body["imap_host"] = test.host
			}
			if test.port != nil {
				body["imap_port"] = test.port
			}

			response := env.request(
				t,
				http.MethodPost,
				"/admin/api/v1/accounts",
				adminAPITestJSON(t, body),
				"application/json",
				[]*http.Cookie{sessionCookie},
				csrf,
			)
			if response.Code != http.StatusCreated {
				t.Fatalf("create account status = %d; body=%s", response.Code, response.Body.String())
			}

			var payload struct {
				Data adminAPIAccountDTO `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode created account: %v; body=%s", err, response.Body.String())
			}
			if payload.Data.IMAPHost != test.wantHost || payload.Data.IMAPPort != test.wantPort {
				t.Fatalf("created endpoint = %q:%d, want %q:%d", payload.Data.IMAPHost, payload.Data.IMAPPort, test.wantHost, test.wantPort)
			}

			stored, err := env.store.GetAccount(context.Background(), payload.Data.ID)
			if err != nil {
				t.Fatalf("reload created account: %v", err)
			}
			if stored.IMAPHost != test.wantHost || stored.IMAPPort != test.wantPort {
				t.Fatalf("stored endpoint = %q:%d, want %q:%d", stored.IMAPHost, stored.IMAPPort, test.wantHost, test.wantPort)
			}
		})
	}
}

func TestAdminAPIAccountIMAPEndpointUpdatePersistsAndOmissionPreserves(t *testing.T) {
	env := newAdminAPITestEnv(t)
	sessionCookie, csrf, _ := env.createSession(t, "imap-update-admin", "unused-imap-update-password")
	account := createAdminAPIIMAPEndpointAccount(t, env, sessionCookie, csrf, "update-imap-endpoint@icloud.com", "initial.imap.example", 1993)
	accountPath := "/admin/api/v1/accounts/" + strconv.FormatInt(account.ID, 10)

	update := env.request(t, http.MethodPut, accountPath, adminAPITestJSON(t, map[string]any{
		"name":          "Updated endpoint",
		"email":         account.Email,
		"imap_host":     " NEXT.IMAP.EXAMPLE. ",
		"imap_port":     2993,
		"imap_username": account.IMAPUsername,
		"imap_password": "",
		"enabled":       true,
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	assertAdminAPIIMAPEndpointResponse(t, update, http.StatusOK, "next.imap.example", 2993)
	assertStoredAdminAPIIMAPEndpoint(t, env, account.ID, "next.imap.example", 2993)

	tests := []struct {
		name     string
		host     any
		port     any
		wantHost string
		wantPort int
	}{
		{
			name:     "omitting both retains both",
			wantHost: "next.imap.example",
			wantPort: 2993,
		},
		{
			name:     "omitting host retains host",
			port:     3993,
			wantHost: "next.imap.example",
			wantPort: 3993,
		},
		{
			name:     "omitting port retains port",
			host:     "final.imap.example",
			wantHost: "final.imap.example",
			wantPort: 3993,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := map[string]any{
				"name":          test.name,
				"email":         account.Email,
				"imap_username": account.IMAPUsername,
				"imap_password": "",
				"enabled":       true,
			}
			if test.host != nil {
				body["imap_host"] = test.host
			}
			if test.port != nil {
				body["imap_port"] = test.port
			}
			response := env.request(t, http.MethodPut, accountPath, adminAPITestJSON(t, body), "application/json", []*http.Cookie{sessionCookie}, csrf)
			assertAdminAPIIMAPEndpointResponse(t, response, http.StatusOK, test.wantHost, test.wantPort)
			assertStoredAdminAPIIMAPEndpoint(t, env, account.ID, test.wantHost, test.wantPort)
		})
	}
}

func TestAdminAPIAccountRejectsInvalidIMAPHostAndPortWithoutMutation(t *testing.T) {
	tests := []struct {
		name string
		host any
		port any
	}{
		{name: "host includes scheme", host: "imaps://imap.example.test", port: 993},
		{name: "host is empty", host: "", port: 993},
		{name: "host is whitespace", host: " \t ", port: 993},
		{name: "host includes port", host: "imap.example.test:993", port: 993},
		{name: "host has invalid DNS label", host: "imap_example.test", port: 993},
		{name: "port is negative", host: "imap.example.test", port: -1},
		{name: "port is zero", host: "imap.example.test", port: 0},
		{name: "port exceeds maximum", host: "imap.example.test", port: 65536},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newAdminAPITestEnv(t)
			sessionCookie, csrf, _ := env.createSession(t, fmt.Sprintf("imap-invalid-admin-%d", index), "unused-imap-invalid-password")
			email := fmt.Sprintf("invalid-imap-endpoint-%d@icloud.com", index)
			body := map[string]any{
				"name":          test.name,
				"email":         email,
				"imap_host":     test.host,
				"imap_port":     test.port,
				"imap_username": email,
				"imap_password": "app-password-for-invalid-endpoint-test",
			}

			create := env.request(t, http.MethodPost, "/admin/api/v1/accounts", adminAPITestJSON(t, body), "application/json", []*http.Cookie{sessionCookie}, csrf)
			if create.Code != http.StatusBadRequest || adminAPITestErrorCode(t, create) != "VALIDATION_FAILED" {
				t.Fatalf("invalid create response = %d; body=%s", create.Code, create.Body.String())
			}
			if _, err := env.store.GetAccountByEmail(context.Background(), email); err == nil {
				t.Fatal("invalid endpoint account was persisted")
			}

			account := createAdminAPIIMAPEndpointAccount(t, env, sessionCookie, csrf, email, "stable.imap.example", 1993)
			body["imap_password"] = ""
			body["enabled"] = true
			update := env.request(
				t,
				http.MethodPut,
				"/admin/api/v1/accounts/"+strconv.FormatInt(account.ID, 10),
				adminAPITestJSON(t, body),
				"application/json",
				[]*http.Cookie{sessionCookie},
				csrf,
			)
			if update.Code != http.StatusBadRequest || adminAPITestErrorCode(t, update) != "VALIDATION_FAILED" {
				t.Fatalf("invalid update response = %d; body=%s", update.Code, update.Body.String())
			}
			assertStoredAdminAPIIMAPEndpoint(t, env, account.ID, "stable.imap.example", 1993)
		})
	}
}

func TestAdminAPIAccountRejectsNullIMAPEndpointMembers(t *testing.T) {
	tests := []struct {
		name  string
		field string
	}{
		{name: "null host", field: "imap_host"},
		{name: "null port", field: "imap_port"},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newAdminAPITestEnv(t)
			sessionCookie, csrf, _ := env.createSession(t, fmt.Sprintf("imap-null-admin-%d", index), "unused-imap-null-password")
			email := fmt.Sprintf("null-imap-endpoint-%d@icloud.com", index)
			createBody := map[string]any{
				"name":          test.name,
				"email":         email,
				"imap_username": email,
				"imap_password": "app-password-for-null-endpoint-test",
				test.field:      nil,
			}
			create := env.request(t, http.MethodPost, "/admin/api/v1/accounts", adminAPITestJSON(t, createBody), "application/json", []*http.Cookie{sessionCookie}, csrf)
			if create.Code != http.StatusBadRequest || adminAPITestErrorCode(t, create) != "VALIDATION_FAILED" {
				t.Fatalf("null create response = %d; body=%s", create.Code, create.Body.String())
			}
			if _, err := env.store.GetAccountByEmail(context.Background(), email); err == nil {
				t.Fatal("account with null endpoint member was persisted")
			}

			account := createAdminAPIIMAPEndpointAccount(t, env, sessionCookie, csrf, email, "stable.imap.example", 1993)
			updateBody := map[string]any{
				"name":          test.name,
				"email":         account.Email,
				"imap_username": account.IMAPUsername,
				"imap_password": "",
				"enabled":       true,
				test.field:      nil,
			}
			update := env.request(t, http.MethodPut, "/admin/api/v1/accounts/"+strconv.FormatInt(account.ID, 10), adminAPITestJSON(t, updateBody), "application/json", []*http.Cookie{sessionCookie}, csrf)
			if update.Code != http.StatusBadRequest || adminAPITestErrorCode(t, update) != "VALIDATION_FAILED" {
				t.Fatalf("null update response = %d; body=%s", update.Code, update.Body.String())
			}
			assertStoredAdminAPIIMAPEndpoint(t, env, account.ID, "stable.imap.example", 1993)
		})
	}
}

func createAdminAPIIMAPEndpointAccount(
	t *testing.T,
	env *adminAPITestEnv,
	sessionCookie *http.Cookie,
	csrf string,
	email string,
	host string,
	port int,
) domain.Account {
	t.Helper()
	response := env.request(t, http.MethodPost, "/admin/api/v1/accounts", adminAPITestJSON(t, map[string]any{
		"name":          "IMAP endpoint fixture",
		"email":         email,
		"imap_host":     host,
		"imap_port":     port,
		"imap_username": email,
		"imap_password": "app-password-for-imap-endpoint-fixture",
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if response.Code != http.StatusCreated {
		t.Fatalf("create endpoint fixture status = %d; body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Data adminAPIAccountDTO `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode endpoint fixture: %v; body=%s", err, response.Body.String())
	}
	account, err := env.store.GetAccount(context.Background(), payload.Data.ID)
	if err != nil {
		t.Fatalf("reload endpoint fixture: %v", err)
	}
	return account
}

func assertAdminAPIIMAPEndpointResponse(t *testing.T, response interface {
	Result() *http.Response
}, wantStatus int, wantHost string, wantPort int) {
	t.Helper()
	result := response.Result()
	defer result.Body.Close()
	if result.StatusCode != wantStatus {
		t.Fatalf("endpoint response status = %d, want %d", result.StatusCode, wantStatus)
	}
	var payload struct {
		Data adminAPIAccountDTO `json:"data"`
	}
	if err := json.NewDecoder(result.Body).Decode(&payload); err != nil {
		t.Fatalf("decode endpoint response: %v", err)
	}
	if payload.Data.IMAPHost != wantHost || payload.Data.IMAPPort != wantPort {
		t.Fatalf("response endpoint = %q:%d, want %q:%d", payload.Data.IMAPHost, payload.Data.IMAPPort, wantHost, wantPort)
	}
}

func assertStoredAdminAPIIMAPEndpoint(t *testing.T, env *adminAPITestEnv, accountID int64, wantHost string, wantPort int) {
	t.Helper()
	stored, err := env.store.GetAccount(context.Background(), accountID)
	if err != nil {
		t.Fatalf("reload account %d: %v", accountID, err)
	}
	if stored.IMAPHost != wantHost || stored.IMAPPort != wantPort {
		t.Fatalf("stored endpoint = %q:%d, want %q:%d", stored.IMAPHost, stored.IMAPPort, wantHost, wantPort)
	}
}
