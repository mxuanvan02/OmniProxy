package auth

import (
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	"strings"
	"sync/atomic"
	"testing"
)

// RefreshToken dispatches on AuthMethod. An external_idp account must refresh
// against its own IdP endpoint — an IdP-issued token sent to the Kiro OIDC
// endpoint 400s and can burn the rotation cache.
func TestRefreshTokenExternalIdpUsesConfiguredTokenEndpoint(t *testing.T) {
	initTempAuthConfig(t)
	var form atomic.Value
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse IdP form: %v", err)
		}
		form.Store(r.Form.Encode())
		_, _ = w.Write([]byte(`{"access_token":"idp-at","refresh_token":"idp-rt","expires_in":900}`))
	}))
	defer idp.Close()

	oidcCalls := int32(0)
	oidcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&oidcCalls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer oidcSrv.Close()
	useTestOIDCTokenURL(t, oidcSrv)

	at, rt, expiresAt, arn, newID, newSecret, err := RefreshToken(&config.Account{
		AuthMethod:    "external_idp",
		TokenEndpoint: idp.URL,
		ClientID:      "idp-client",
		RefreshToken:  "rt-old",
		Scopes:        "openid profile",
	})
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if at != "idp-at" || rt != "idp-rt" || expiresAt == 0 {
		t.Fatalf("tokens = (%q, %q, %d)", at, rt, expiresAt)
	}
	if arn != "" || newID != "" || newSecret != "" {
		t.Fatalf("external IdP refresh must not report profileArn or new client creds: (%q, %q, %q)", arn, newID, newSecret)
	}
	if n := atomic.LoadInt32(&oidcCalls); n != 0 {
		t.Fatalf("Kiro OIDC endpoint was called %d times for an external_idp account", n)
	}
	encoded, _ := form.Load().(string)
	for _, want := range []string{"grant_type=refresh_token", "client_id=idp-client", "refresh_token=rt-old", "scope=openid+profile"} {
		if !strings.Contains(encoded, want) {
			t.Errorf("IdP form %q missing %q", encoded, want)
		}
	}
}

func TestRefreshViaExternalIdpRejectsEmptyInputsWithoutRequest(t *testing.T) {
	calls := int32(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()

	if _, _, _, err := refreshViaExternalIdp("", srv.URL, "id", "", srv.Client()); err == nil {
		t.Fatal("empty refresh token was accepted")
	}
	if _, _, _, err := refreshViaExternalIdp("rt", "", "id", "", srv.Client()); err == nil {
		t.Fatal("empty token endpoint was accepted")
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("upstream called %d times, want 0", n)
	}
}

// An IdP that answers 200 with an OAuth error object and no access_token must
// surface the error rather than reporting a successful refresh with empty
// credentials — that is what silently 401s every later request.
func TestRefreshViaExternalIdpSurfacesErrorObjectOnHTTP200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	if _, _, _, err := refreshViaExternalIdp("rt", srv.URL, "id", "", srv.Client()); err == nil ||
		!strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("error = %v, want invalid_grant", err)
	}
}

func TestRefreshViaExternalIdpDefaultsMissingExpiresIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"at"}`))
	}))
	defer srv.Close()

	_, _, expiresAt, err := refreshViaExternalIdp("rt", srv.URL, "id", "", srv.Client())
	if err != nil {
		t.Fatalf("refreshViaExternalIdp: %v", err)
	}
	if expiresAt == 0 {
		t.Fatal("missing expires_in produced a zero expiry, want the 3600s default")
	}
}

func TestRefreshViaExternalIdpReportsNon2xxStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	if _, _, _, err := refreshViaExternalIdp("rt", srv.URL, "id", "", srv.Client()); err == nil ||
		!strings.Contains(err.Error(), "status 401") {
		t.Fatalf("error = %v, want status 401", err)
	}
}

// A social account whose kiro.dev refresh fails falls back to registering a
// fresh OIDC client and retrying there. The re-registered credentials must be
// returned so the caller can persist them.
func TestRefreshTokenSocialFallsBackToReregisteredOIDCClient(t *testing.T) {
	initTempAuthConfig(t)

	newAuthEndpointServer(t, authEndpointResponses{
		social:   authResponse{status: http.StatusBadRequest, body: "social refresh not supported for this token"},
		register: authResponse{body: `{"clientId":"fresh-id","clientSecret":"fresh-secret"}`},
		oidc:     authResponse{body: `{"accessToken":"oidc-at","refreshToken":"oidc-rt","expiresIn":600,"profileArn":"arn:fallback"}`},
	})

	at, rt, _, arn, newID, newSecret, err := RefreshToken(&config.Account{
		AuthMethod:   "social",
		RefreshToken: "rt-old",
		Region:       "us-east-1",
	})
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if at != "oidc-at" || rt != "oidc-rt" || arn != "arn:fallback" {
		t.Fatalf("fallback tokens = (%q, %q, %q)", at, rt, arn)
	}
	if newID != "fresh-id" || newSecret != "fresh-secret" {
		t.Fatalf("re-registered client = (%q, %q), want it reported to the caller", newID, newSecret)
	}
}

// When every path fails, RefreshToken must report the original OIDC error
// rather than nil credentials with a nil error.
func TestRefreshTokenReturnsErrorWhenEveryPathFails(t *testing.T) {
	initTempAuthConfig(t)

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("revoked"))
	}))
	defer fail.Close()
	installTestAuthClient(t, fail)
	useTestOIDCTokenURL(t, fail)
	previousSocial := socialTokenURL
	socialTokenURL = func() string { return fail.URL }
	defer func() { socialTokenURL = previousSocial }()

	at, _, _, _, _, _, err := RefreshToken(&config.Account{
		AuthMethod:   "idc",
		RefreshToken: "rt-old",
		ClientID:     "id",
		ClientSecret: "secret",
		Region:       "us-east-1",
	})
	if err == nil {
		t.Fatal("RefreshToken returned nil error after every endpoint failed")
	}
	if at != "" {
		t.Fatalf("access token = %q, want empty on failure", at)
	}
}

// The final fallback: an OIDC-method account whose own endpoint fails can still
// be recovered through the social refresh endpoint.
func TestRefreshTokenOIDCFallsBackToSocialEndpoint(t *testing.T) {
	initTempAuthConfig(t)

	newAuthEndpointServer(t, authEndpointResponses{
		oidc: authResponse{status: http.StatusBadRequest, body: "expired client"},
		// Re-registration also fails, so the social endpoint is the only way out.
		register: authResponse{status: http.StatusBadRequest, body: "cannot register"},
		social:   authResponse{body: `{"accessToken":"social-at","refreshToken":"social-rt","expiresIn":300,"profileArn":"arn:social"}`},
	})

	at, rt, _, arn, _, _, err := RefreshToken(&config.Account{
		AuthMethod:   "idc",
		RefreshToken: "rt-old",
		ClientID:     "id",
		ClientSecret: "secret",
		Region:       "us-east-1",
	})
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if at != "social-at" || rt != "social-rt" || arn != "arn:social" {
		t.Fatalf("social fallback tokens = (%q, %q, %q)", at, rt, arn)
	}
}
