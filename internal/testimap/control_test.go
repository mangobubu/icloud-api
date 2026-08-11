package testimap

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestControlAPIAccountAndPresetLifecycle(t *testing.T) {
	const token = "0123456789abcdef-test-token"
	server := httptest.NewServer(newControlHandler(NewBackend(), token))
	defer server.Close()

	response, err := http.Get(server.URL + "/control/v1/accounts")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		response.Body.Close()
		t.Fatalf("unauthenticated status = %d, want 401", response.StatusCode)
	}
	response.Body.Close()

	accountPayload := `{"username":"ui-test@icloud.test","password":"ui-test-password","forward_address":"ui-test@icloud.test"}`
	response = controlRequest(t, server.URL+"/control/v1/accounts", token, http.MethodPost, accountPayload)
	if response.StatusCode != http.StatusCreated {
		response.Body.Close()
		t.Fatalf("create account status = %d, want 201", response.StatusCode)
	}
	var created struct {
		Data struct {
			Account Account `json:"account"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if created.Data.Account.ID != 1 {
		t.Fatalf("created account ID = %d, want 1", created.Data.Account.ID)
	}

	presetPayload := `{"account_id":1,"alias":"alias@icloud.test"}`
	response = controlRequest(t, server.URL+"/control/v1/presets/verification-code", token, http.MethodPost, presetPayload)
	if response.StatusCode != http.StatusCreated {
		response.Body.Close()
		t.Fatalf("create preset status = %d, want 201", response.StatusCode)
	}
	response.Body.Close()

	response = controlRequest(t, server.URL+"/control/v1/accounts/1/messages", token, http.MethodGet, "")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list messages status = %d, want 200", response.StatusCode)
	}
	var listed struct {
		Data struct {
			Messages []StoredMessage `json:"messages"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data.Messages) != 1 || listed.Data.Messages[0].UID != 1 ||
		listed.Data.Messages[0].Subject != "Your temporary ChatGPT verification code" {
		t.Fatalf("listed messages = %#v", listed.Data.Messages)
	}
}

func controlRequest(t *testing.T, url, token, method, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
