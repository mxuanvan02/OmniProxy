package auth

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestBuildAuthTransportUsesExplicitProxyURL(t *testing.T) {
	transport := buildAuthTransport("http://proxy.local:8080")
	req := &http.Request{URL: mustParseURL(t, "https://oidc.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://proxy.local:8080")
}

func TestBuildAuthTransportFallsBackToEnvironmentProxy(t *testing.T) {
	const helperEnv = "GO_WANT_AUTH_ENV_PROXY_HELPER"
	const wantProxy = "http://env-proxy.local:2323"

	if os.Getenv(helperEnv) == "1" {
		transport := buildAuthTransport("")
		req := &http.Request{URL: mustParseURL(t, "https://oidc.us-east-1.amazonaws.com")}

		got, err := transport.Proxy(req)
		if err != nil {
			t.Fatalf("unexpected proxy error: %v", err)
		}
		assertProxyURL(t, got, wantProxy)
		return
	}

	// http.ProxyFromEnvironment caches its first environment lookup per
	// process. Run this assertion in a fresh test process so other auth tests
	// cannot make its result depend on execution order.
	cmd := exec.Command(os.Args[0], "-test.run=^TestBuildAuthTransportFallsBackToEnvironmentProxy$")
	cmd.Env = append(withoutProxyEnvironment(os.Environ()),
		helperEnv+"=1",
		"HTTPS_PROXY="+wantProxy,
		"HTTP_PROXY=",
		"ALL_PROXY=",
		"NO_PROXY=",
		"https_proxy="+wantProxy,
		"http_proxy=",
		"all_proxy=",
		"no_proxy=",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("environment proxy helper failed: %v\n%s", err, output)
	}
}

func withoutProxyEnvironment(environ []string) []string {
	filtered := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToLower(key) {
		case "http_proxy", "https_proxy", "all_proxy", "no_proxy":
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("invalid test URL: %v", err)
	}
	return parsed
}

func assertProxyURL(t *testing.T, got *url.URL, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("expected proxy URL %q, got nil", want)
	}
	if got.String() != want {
		t.Fatalf("expected proxy URL %q, got %q", want, got.String())
	}
}
