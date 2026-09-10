package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omniproxy/webnext"
)

// The next-gen client is embedded, so a checkout that compiles must also be able
// to serve it. This fails loudly if the build output is ever emptied or the
// embed path drifts from the Vite outDir.
func TestAdminNextIndexIsEmbedded(t *testing.T) {
	index, err := webnext.Index()
	if err != nil {
		t.Fatalf("webnext.Index() error = %v; run npm run build in web-next/", err)
	}
	if !strings.Contains(string(index), webnext.Prefix) {
		t.Fatalf("index.html does not reference %q — Vite base and webnext.Prefix disagree", webnext.Prefix)
	}
}

func TestAdminNextPageServesEntryDocument(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.serveAdminNextPage(rec, httptest.NewRequest(http.MethodGet, webnext.Prefix, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	// The legacy dashboard needs script-src 'unsafe-inline' for its ~90 inline
	// handlers. This client has none, so admitting inline script here would
	// silently give up the only real gain of the rewrite.
	if csp := rec.Header().Get("Content-Security-Policy"); strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("admin-next CSP still allows inline script: %q", csp)
	}
}

// A hashed asset must be served as itself; an extensionless deep link must fall
// back to the SPA entry document instead of 404.
func TestAdminNextAssetRoutingAndSPAFallback(t *testing.T) {
	h := &Handler{}

	index, err := webnext.Index()
	if err != nil {
		t.Fatalf("webnext.Index() error = %v", err)
	}
	asset := assetHrefFromIndex(t, string(index))

	rec := httptest.NewRecorder()
	h.serveAdminNextAsset(rec, httptest.NewRequest(http.MethodGet, asset, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("asset %s status = %d, want 200", asset, rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("asset %s served empty body", asset)
	}

	deep := httptest.NewRecorder()
	h.serveAdminNextAsset(deep, httptest.NewRequest(http.MethodGet, webnext.Prefix+"accounts", nil))
	if deep.Code != http.StatusOK {
		t.Fatalf("deep link status = %d, want 200 (SPA fallback)", deep.Code)
	}
	if ct := deep.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("deep link Content-Type = %q, want text/html", ct)
	}
}

// A traversal attempt must not escape the embedded tree.
func TestAdminNextAssetRejectsTraversal(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, webnext.Prefix+"assets/x.js", nil)
	req.URL.Path = webnext.Prefix + "../data/config.json"
	h.serveAdminNextAsset(rec, req)

	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "accounts") {
		t.Fatalf("traversal served config-like content: status=%d", rec.Code)
	}
}

// assetHrefFromIndex pulls the first /admin-next/assets/... URL out of the entry
// document so the test tracks the real hashed filenames instead of hardcoding
// one that changes on every build.
func assetHrefFromIndex(t *testing.T, html string) string {
	t.Helper()
	needle := webnext.Prefix + "assets/"
	i := strings.Index(html, needle)
	if i < 0 {
		t.Fatalf("no %s reference in index.html", needle)
	}
	rest := html[i:]
	end := strings.IndexAny(rest, `"'`)
	if end < 0 {
		t.Fatalf("unterminated asset URL in index.html")
	}
	return rest[:end]
}
