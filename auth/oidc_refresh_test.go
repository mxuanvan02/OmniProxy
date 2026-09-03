package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"omniproxy/config"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installTestAuthClient routes every auth HTTP request to srv regardless of the
// hostname the production code hardcodes, and restores the previous global
// client when the test ends.
func installTestAuthClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	previous := SetGlobalAuthClientForTest(&http.Client{Transport: rewriteAuthRequestTransport{target: target}})
	t.Cleanup(func() { SetGlobalAuthClientForTest(previous) })
}

// useTestOIDCTokenURL points the OIDC refresh endpoint at srv and returns a
// pointer to the region the production code passed to the URL builder.
func useTestOIDCTokenURL(t *testing.T, srv *httptest.Server) *string {
	t.Helper()
	seenRegion := new(string)
	previous := GetOIDCTokenURLForTest()
	SetOIDCTokenURLForTest(func(region string) string {
		*seenRegion = region
		return srv.URL + "/token"
	})
	t.Cleanup(func() { SetOIDCTokenURLForTest(previous) })
	return seenRegion
}

// initTempAuthConfig gives the test its own config file and leaves a fresh one
// behind, so a setting written here cannot leak into a later test.
func initTempAuthConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := config.Init(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Cleanup(func() { _ = config.Init(filepath.Join(dir, "reset.json")) })
}

// authResponse is one canned reply from the fake AWS/kiro.dev endpoint set.
type authResponse struct {
	status int
	body   string
}

func (r authResponse) write(w http.ResponseWriter) {
	if r.status != 0 {
		w.WriteHeader(r.status)
	}
	_, _ = w.Write([]byte(r.body))
}

// authEndpointResponses describes the three upstreams RefreshToken can reach.
type authEndpointResponses struct {
	register authResponse
	oidc     authResponse
	social   authResponse
}

// newAuthEndpointServer serves the OIDC register, OIDC token and social refresh
// endpoints from one httptest server. RefreshToken sends all three through the
// same global auth client, so they must share a host and be told apart by path;
// separate servers would all be rewritten onto whichever one was installed last.
func newAuthEndpointServer(t *testing.T, responses authEndpointResponses) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/client/register", func(w http.ResponseWriter, r *http.Request) {
		responses.register.write(w)
	})
	mux.HandleFunc("/oidc/token", func(w http.ResponseWriter, r *http.Request) {
		responses.oidc.write(w)
	})
	mux.HandleFunc("/social/refreshToken", func(w http.ResponseWriter, r *http.Request) {
		responses.social.write(w)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected auth request to %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	installTestAuthClient(t, srv)

	previousOIDC := GetOIDCTokenURLForTest()
	SetOIDCTokenURLForTest(func(string) string { return srv.URL + "/oidc/token" })
	t.Cleanup(func() { SetOIDCTokenURLForTest(previousOIDC) })

	previousSocial := socialTokenURL
	socialTokenURL = func() string { return srv.URL + "/social/refreshToken" }
	t.Cleanup(func() { socialTokenURL = previousSocial })

	return srv
}

func TestRefreshOIDCTokenParsesSuccessfulResponse(t *testing.T) {
	var payload map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode refresh payload: %v", err)
		}
		_, _ = w.Write([]byte(`{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600,"profileArn":"arn:aws:codewhisperer:us-east-1:123:profile/X"}`))
	}))
	defer srv.Close()
	region := useTestOIDCTokenURL(t, srv)

	at, rt, expiresAt, arn, err := refreshOIDCToken("rt-old", "client-id", "client-secret", "", srv.Client())
	if err != nil {
		t.Fatalf("refreshOIDCToken: %v", err)
	}
	if at != "at-new" || rt != "rt-new" || arn == "" {
		t.Fatalf("tokens = (%q, %q, %q)", at, rt, arn)
	}
	if delta := expiresAt - time.Now().Unix(); delta < 3590 || delta > 3600 {
		t.Fatalf("expiresAt is %ds away, want about 3600s", delta)
	}
	if *region != "us-east-1" {
		t.Fatalf("empty region resolved to %q, want us-east-1", *region)
	}
	want := map[string]string{
		"clientId": "client-id", "clientSecret": "client-secret",
		"refreshToken": "rt-old", "grantType": "refresh_token",
	}
	for k, v := range want {
		if payload[k] != v {
			t.Errorf("payload[%q] = %q, want %q", k, payload[k], v)
		}
	}
}

func TestRefreshOIDCTokenFailureModes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":"invalid_grant"}`, "refresh failed: 401"},
		{"server error", http.StatusInternalServerError, "boom", "refresh failed: 500"},
		{"malformed json", http.StatusOK, `{"accessToken":`, "unexpected EOF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			useTestOIDCTokenURL(t, srv)

			_, _, _, _, err := refreshOIDCToken("rt", "id", "secret", "us-east-1", srv.Client())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// Missing client credentials must fail locally: sending an empty clientId to
// AWS burns a request and returns an error the caller cannot distinguish from a
// revoked token.
func TestRefreshOIDCTokenRejectsMissingClientCredentialsWithoutRequest(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()
	useTestOIDCTokenURL(t, srv)

	_, _, _, _, err := refreshOIDCToken("rt", "", "secret", "us-east-1", srv.Client())
	if err == nil || !strings.Contains(err.Error(), "requires clientId and clientSecret") {
		t.Fatalf("error = %v, want clientId/clientSecret requirement", err)
	}
	if requests != 0 {
		t.Fatalf("upstream was called %d times, want 0", requests)
	}
}

func TestRefreshSocialTokenParsesResponseAndReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode social payload: %v", err)
		}
		if payload["refreshToken"] != "rt-old" {
			t.Errorf("refreshToken = %q", payload["refreshToken"])
		}
		_, _ = w.Write([]byte(`{"accessToken":"at","refreshToken":"rt","expiresIn":120,"profileArn":"arn:profile"}`))
	}))
	defer srv.Close()
	previous := socialTokenURL
	socialTokenURL = func() string { return srv.URL }
	defer func() { socialTokenURL = previous }()

	at, rt, expiresAt, arn, err := refreshSocialToken("rt-old", srv.Client())
	if err != nil {
		t.Fatalf("refreshSocialToken: %v", err)
	}
	if at != "at" || rt != "rt" || arn != "arn:profile" {
		t.Fatalf("social tokens = (%q, %q, %q)", at, rt, arn)
	}
	if delta := expiresAt - time.Now().Unix(); delta < 110 || delta > 120 {
		t.Fatalf("expiresAt is %ds away, want about 120s", delta)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("denied"))
	}))
	defer failing.Close()
	socialTokenURL = func() string { return failing.URL }
	if _, _, _, _, err := refreshSocialToken("rt-old", failing.Client()); err == nil ||
		!strings.Contains(err.Error(), "refresh failed: 403") {
		t.Fatalf("error = %v, want refresh failed: 403", err)
	}
}

func TestRegisterOIDCClientReturnsCredentialsAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client/register" {
			t.Errorf("register path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"clientId":"registered-id","clientSecret":"registered-secret"}`))
	}))
	defer srv.Close()
	installTestAuthClient(t, srv)

	id, secret, err := RegisterOIDCClient("us-east-1")
	if err != nil || id != "registered-id" || secret != "registered-secret" {
		t.Fatalf("RegisterOIDCClient = (%q, %q, %v)", id, secret, err)
	}
}

func TestRegisterOIDCClientReportsHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad tenant"))
	}))
	defer srv.Close()
	installTestAuthClient(t, srv)

	if _, _, err := RegisterOIDCClient("us-east-1"); err == nil ||
		!strings.Contains(err.Error(), "register client failed: 400") {
		t.Fatalf("error = %v, want register client failed: 400", err)
	}
}
