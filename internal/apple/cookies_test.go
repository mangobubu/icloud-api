package apple

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestPersistentJarExportImportPreservesCookieScope(t *testing.T) {
	jar, err := NewPersistentJar(nil)
	if err != nil {
		t.Fatal(err)
	}
	setup, _ := url.Parse("https://setup.icloud.com/setup/ws/1/accountLogin")
	jar.SetCookies(setup, []*http.Cookie{
		{Name: "domain-token", Value: "domain-value", Domain: ".icloud.com", Path: "/", Secure: true, HttpOnly: true, Expires: time.Now().Add(time.Hour)},
		{Name: "host-token", Value: "host-value", Secure: true},
	})
	exported := jar.Export()
	if len(exported) != 2 {
		t.Fatalf("exported cookies = %#v", exported)
	}
	encoded, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []PersistentCookie
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	restored, err := NewPersistentJar(decoded)
	if err != nil {
		t.Fatal(err)
	}
	premium, _ := url.Parse("https://p61-maildomainws.icloud.com/v2/hme/list")
	premiumCookies := restored.Cookies(premium)
	if len(premiumCookies) != 1 || premiumCookies[0].Name != "domain-token" || premiumCookies[0].Value != "domain-value" {
		t.Fatalf("premium cookies = %#v", premiumCookies)
	}
	setupCookies := restored.Cookies(setup)
	if len(setupCookies) != 2 {
		t.Fatalf("setup cookies = %#v", setupCookies)
	}

	jar.SetCookies(setup, []*http.Cookie{{Name: "domain-token", Domain: ".icloud.com", Path: "/", MaxAge: -1}})
	remaining := jar.Export()
	if len(remaining) != 1 || remaining[0].Name != "host-token" {
		t.Fatalf("remaining cookies = %#v", remaining)
	}
}

func TestPersistentJarDropsExpiredAndRejectsMalformedCookies(t *testing.T) {
	jar, err := NewPersistentJar([]PersistentCookie{{
		Name: "expired", Value: "value", Domain: "icloud.com", Path: "/", Expires: time.Now().Add(-time.Minute),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if exported := jar.Export(); len(exported) != 0 {
		t.Fatalf("expired cookies exported: %#v", exported)
	}
	for _, malformed := range []PersistentCookie{
		{Name: "", Value: "x", Domain: "icloud.com", Path: "/"},
		{Name: "bad;name", Value: "x", Domain: "icloud.com", Path: "/"},
		{Name: "ok", Value: "bad\r\nvalue", Domain: "icloud.com", Path: "/"},
		{Name: "ok", Value: "x", Domain: "attacker.example/path", Path: "/"},
		{Name: "ok", Value: "x", Domain: "icloud.com", Path: "relative"},
	} {
		if _, err := NewPersistentJar([]PersistentCookie{malformed}); err == nil {
			t.Fatalf("malformed cookie accepted: %#v", malformed)
		}
	}
}

func TestDefaultCookiePath(t *testing.T) {
	tests := map[string]string{
		"":                            "/",
		"/":                           "/",
		"relative":                    "/",
		"/appleauth/auth/signin/init": "/appleauth/auth/signin",
		"/setup/ws/1/accountLogin":    "/setup/ws/1",
	}
	for input, want := range tests {
		if got := defaultCookiePath(input); got != want {
			t.Fatalf("defaultCookiePath(%q) = %q, want %q", input, got, want)
		}
	}
}
