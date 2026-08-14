package httpserver

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testAdminSPAIndex = `<!doctype html><html><head><base href="./"></head><body><div id="spa-test-marker"></div></body></html>`

func TestAdminSPARandomPathAssetsAndAPIBoundaries(t *testing.T) {
	const adminPath = "/0123456789abcdef0123456789abcdef/admin"
	env := newAdminAPITestEnv(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte(testAdminSPAIndex), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app-test.js"), []byte("window.__SPA_TEST__ = true;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spa, err := loadAdminSPA(root)
	if err != nil {
		t.Fatalf("load test SPA: %v", err)
	}
	env.server.adminSPA = spa
	env.server.cfg.AdminPath = adminPath
	router, err := env.server.Router()
	if err != nil {
		t.Fatalf("build SPA router: %v", err)
	}

	rootResponse := serveAdminRouterRequest(router, http.MethodGet, "/", nil, nil)
	if rootResponse.Code != http.StatusFound || rootResponse.Header().Get("Location") != "/docs/" {
		t.Fatalf("GET / = %d Location=%q", rootResponse.Code, rootResponse.Header().Get("Location"))
	}
	redirect := serveAdminRouterRequest(router, http.MethodGet, adminPath, nil, nil)
	if redirect.Code != http.StatusPermanentRedirect || redirect.Header().Get("Location") != adminPath+"/" {
		t.Fatalf("GET admin root = %d Location=%q", redirect.Code, redirect.Header().Get("Location"))
	}
	for _, target := range []string{adminPath + "/", adminPath + "/accounts/42", adminPath + "/aliases"} {
		response := serveAdminRouterRequest(router, http.MethodGet, target, nil, nil)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "spa-test-marker") ||
			!strings.Contains(response.Body.String(), `<base href="`+adminPath+`/">`) {
			t.Fatalf("GET %s = %d body=%s", target, response.Code, response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store, private" {
			t.Fatalf("GET %s Cache-Control=%q", target, response.Header().Get("Cache-Control"))
		}
	}
	head := serveAdminRouterRequest(router, http.MethodHead, adminPath+"/accounts/42", nil, nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD SPA route = %d body=%q", head.Code, head.Body.String())
	}
	asset := serveAdminRouterRequest(router, http.MethodGet, adminPath+"/assets/app-test.js", nil, nil)
	if asset.Code != http.StatusOK || asset.Body.String() != "window.__SPA_TEST__ = true;\n" ||
		asset.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("SPA asset = %d headers=%v body=%q", asset.Code, asset.Header(), asset.Body.String())
	}
	for _, target := range []string{adminPath + "/assets", adminPath + "/assets/missing.js"} {
		response := serveAdminRouterRequest(router, http.MethodGet, target, nil, nil)
		if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "spa-test-marker") {
			t.Fatalf("GET %s = %d body=%s", target, response.Code, response.Body.String())
		}
	}
	apiMissing := serveAdminRouterRequest(router, http.MethodGet, adminPath+"/api/v1/not-found", nil, nil)
	if apiMissing.Code != http.StatusNotFound || !strings.HasPrefix(apiMissing.Header().Get("Content-Type"), "application/json") ||
		!strings.Contains(apiMissing.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("missing random API = %d headers=%v body=%s", apiMissing.Code, apiMissing.Header(), apiMissing.Body.String())
	}
	fixedPage := serveAdminRouterRequest(router, http.MethodGet, "/admin", nil, nil)
	if fixedPage.Code != http.StatusNotFound || strings.Contains(fixedPage.Body.String(), "spa-test-marker") {
		t.Fatalf("fixed /admin unexpectedly served UI: %d body=%s", fixedPage.Code, fixedPage.Body.String())
	}
	fixedAPI := serveAdminRouterRequest(router, http.MethodGet, legacyAdminAPIBasePath+"/auth/csrf", nil, nil)
	if fixedAPI.Code != http.StatusOK || !strings.HasPrefix(fixedAPI.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("fixed admin API = %d type=%q body=%s", fixedAPI.Code, fixedAPI.Header().Get("Content-Type"), fixedAPI.Body.String())
	}
}

func TestRouterWithoutWebRootDoesNotExposeAdminUI(t *testing.T) {
	const adminPath = "/fedcba9876543210fedcba9876543210/admin"
	env := newAdminAPITestEnv(t)
	env.server.adminSPA = nil
	env.server.cfg.AdminPath = adminPath
	router, err := env.server.Router()
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{adminPath, adminPath + "/", adminPath + "/login", "/admin"} {
		response := serveAdminRouterRequest(router, http.MethodGet, target, nil, nil)
		if response.Code != http.StatusNotFound || strings.Contains(response.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("GET %s = %d type=%q body=%s", target, response.Code, response.Header().Get("Content-Type"), response.Body.String())
		}
	}
	fixedAPI := serveAdminRouterRequest(router, http.MethodGet, legacyAdminAPIBasePath+"/auth/csrf", nil, nil)
	if fixedAPI.Code != http.StatusOK {
		t.Fatalf("fixed admin API without web root = %d body=%s", fixedAPI.Code, fixedAPI.Body.String())
	}
}

func TestLoadAdminSPARequiresCompleteBuild(t *testing.T) {
	root := t.TempDir()
	if _, err := loadAdminSPA(root); err == nil || !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("loadAdminSPA without index error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte(testAdminSPAIndex), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAdminSPA(root); err == nil || !strings.Contains(err.Error(), "assets") {
		t.Fatalf("loadAdminSPA without assets error = %v", err)
	}
}
