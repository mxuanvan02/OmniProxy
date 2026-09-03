package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// awsOIDCRoutes maps a request path to its handler for the fake AWS OIDC /
// CodeWhisperer host. Every builderid and iam_sso call is a POST to a distinct
// path on a hardcoded hostname, so one server plus the rewriting transport
// stands in for all of them.
type awsOIDCRoutes map[string]http.HandlerFunc

// newAWSOIDCServer installs a fake AWS OIDC host as the global auth client's
// only reachable upstream. Unrouted paths fail the test rather than silently
// returning 404, so a request the production code was not expected to make is
// visible instead of being read as an upstream error.
func newAWSOIDCServer(t *testing.T, routes awsOIDCRoutes) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for path, handler := range routes {
		mux.HandleFunc(path, handler)
	}
	if _, ok := routes["/"]; !ok {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("unexpected AWS OIDC request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	installTestAuthClient(t, srv)
	return srv
}

// jsonRoute returns a handler writing a fixed status and body.
func jsonRoute(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(body))
	}
}

// noProfilesRoute answers ListAvailableProfiles with an empty profile set, the
// legitimate response for a Builder ID account.
func noProfilesRoute() http.HandlerFunc {
	return jsonRoute(http.StatusOK, `{"profiles":[]}`)
}

func TestStartBuilderIdLoginStoresSessionFromDeviceAuthorization(t *testing.T) {
	var regPayload, authPayload map[string]interface{}
	newAWSOIDCServer(t, awsOIDCRoutes{
		"/client/register": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&regPayload)
			_, _ = w.Write([]byte(`{"clientId":"bid-client","clientSecret":"bid-secret"}`))
		},
		"/device_authorization": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&authPayload)
			_, _ = w.Write([]byte(`{"deviceCode":"device-code-xxx","userCode":"USER-CODE",
				"verificationUri":"https://device.example/verify",
				"verificationUriComplete":"https://device.example/verify?code=USER-CODE",
				"interval":7,"expiresIn":900}`))
		},
	})

	session, err := StartBuilderIdLogin("")
	if err != nil {
		t.Fatalf("StartBuilderIdLogin: %v", err)
	}
	if session.Region != "us-east-1" {
		t.Fatalf("region = %q, want the us-east-1 default", session.Region)
	}
	if session.ClientID != "bid-client" || session.ClientSecret != "bid-secret" {
		t.Fatalf("client creds = (%q, %q)", session.ClientID, session.ClientSecret)
	}
	if session.DeviceCode != "device-code-xxx" || session.UserCode != "USER-CODE" {
		t.Fatalf("device codes = (%q, %q)", session.DeviceCode, session.UserCode)
	}
	// The complete URI embeds the user code, so it must win over the bare one:
	// the operator otherwise has to type the code by hand.
	if session.VerificationUri != "https://device.example/verify?code=USER-CODE" {
		t.Fatalf("verification URI = %q, want the complete form", session.VerificationUri)
	}
	if session.Interval != 7 {
		t.Fatalf("interval = %d, want the upstream value 7", session.Interval)
	}
	if remaining := time.Until(session.ExpiresAt); remaining < 890*time.Second || remaining > 900*time.Second {
		t.Fatalf("expiry is %s away, want about 900s", remaining)
	}
	if got := regPayload["clientType"]; got != "public" {
		t.Errorf("register clientType = %v, want public", got)
	}
	if got := authPayload["clientId"]; got != "bid-client" {
		t.Errorf("device authorization clientId = %v, want the registered id", got)
	}

	if stored := GetBuilderIdSession(session.ID); stored == nil || stored.DeviceCode != session.DeviceCode {
		t.Fatalf("GetBuilderIdSession = %#v, want the stored session", stored)
	}
	if GetBuilderIdSession("no-such-session") != nil {
		t.Fatal("GetBuilderIdSession returned a session for an unknown ID")
	}
}

// Zero interval/expiry from upstream must become the documented defaults, not a
// zero poll interval (which would hammer the endpoint into slow_down).
func TestStartBuilderIdLoginAppliesPollingDefaults(t *testing.T) {
	newAWSOIDCServer(t, awsOIDCRoutes{
		"/client/register":      jsonRoute(http.StatusOK, `{"clientId":"c","clientSecret":"s"}`),
		"/device_authorization": jsonRoute(http.StatusOK, `{"deviceCode":"d","userCode":"u","verificationUri":"https://device.example/v"}`),
	})

	session, err := StartBuilderIdLogin("eu-central-1")
	if err != nil {
		t.Fatalf("StartBuilderIdLogin: %v", err)
	}
	if session.Interval != 5 {
		t.Fatalf("interval = %d, want the 5s default", session.Interval)
	}
	if remaining := time.Until(session.ExpiresAt); remaining < 590*time.Second || remaining > 600*time.Second {
		t.Fatalf("expiry is %s away, want the 600s default", remaining)
	}
	if session.Region != "eu-central-1" {
		t.Fatalf("region = %q, want the caller's region", session.Region)
	}
	if session.VerificationUri != "https://device.example/v" {
		t.Fatalf("verification URI = %q, want the bare URI fallback", session.VerificationUri)
	}
}

func TestStartBuilderIdLoginSurfacesUpstreamFailures(t *testing.T) {
	t.Run("register rejected", func(t *testing.T) {
		newAWSOIDCServer(t, awsOIDCRoutes{
			"/client/register": jsonRoute(http.StatusForbidden, "not allowed"),
		})
		if _, err := StartBuilderIdLogin("us-east-1"); err == nil ||
			!strings.Contains(err.Error(), "register client failed: 403") {
			t.Fatalf("error = %v, want register client failed: 403", err)
		}
	})

	t.Run("device authorization rejected", func(t *testing.T) {
		newAWSOIDCServer(t, awsOIDCRoutes{
			"/client/register":      jsonRoute(http.StatusOK, `{"clientId":"c","clientSecret":"s"}`),
			"/device_authorization": jsonRoute(http.StatusBadRequest, "bad client"),
		})
		if _, err := StartBuilderIdLogin("us-east-1"); err == nil ||
			!strings.Contains(err.Error(), "device authorization failed: 400") {
			t.Fatalf("error = %v, want device authorization failed: 400", err)
		}
	})

	t.Run("register response is not json", func(t *testing.T) {
		newAWSOIDCServer(t, awsOIDCRoutes{
			"/client/register": jsonRoute(http.StatusOK, `{"clientId":`),
		})
		if _, err := StartBuilderIdLogin("us-east-1"); err == nil ||
			!strings.Contains(err.Error(), "parse register response failed") {
			t.Fatalf("error = %v, want a parse failure", err)
		}
	})
}

// startPolledBuilderIdSession creates a live session whose token endpoint is
// controlled by tokenRoute, so each poll outcome can be driven independently.
func startPolledBuilderIdSession(t *testing.T, tokenRoute http.HandlerFunc, extra awsOIDCRoutes) *BuilderIdSession {
	t.Helper()
	routes := awsOIDCRoutes{
		"/client/register":      jsonRoute(http.StatusOK, `{"clientId":"c","clientSecret":"s"}`),
		"/device_authorization": jsonRoute(http.StatusOK, `{"deviceCode":"d","userCode":"u","verificationUri":"https://device.example/v","interval":5,"expiresIn":600}`),
		"/token":                tokenRoute,
	}
	for path, handler := range extra {
		routes[path] = handler
	}
	newAWSOIDCServer(t, routes)

	session, err := StartBuilderIdLogin("us-east-1")
	if err != nil {
		t.Fatalf("StartBuilderIdLogin: %v", err)
	}
	return session
}

func TestPollBuilderIdAuthCompletesAndConsumesSession(t *testing.T) {
	var tokenPayload map[string]string
	session := startPolledBuilderIdSession(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&tokenPayload)
		_, _ = w.Write([]byte(`{"accessToken":"bid-at","refreshToken":"bid-rt","expiresIn":3600}`))
	}, awsOIDCRoutes{
		"/": jsonRoute(http.StatusOK, `{"profiles":[{"arn":"arn:aws:codewhisperer:us-east-1:1:profile/P"}]}`),
	})

	at, rt, clientID, clientSecret, region, expiresIn, profileArn, status, err := PollBuilderIdAuth(session.ID)
	if err != nil {
		t.Fatalf("PollBuilderIdAuth: %v", err)
	}
	if status != "completed" {
		t.Fatalf("status = %q, want completed", status)
	}
	if at != "bid-at" || rt != "bid-rt" || expiresIn != 3600 {
		t.Fatalf("tokens = (%q, %q, %d)", at, rt, expiresIn)
	}
	if clientID != "c" || clientSecret != "s" || region != "us-east-1" {
		t.Fatalf("session values = (%q, %q, %q)", clientID, clientSecret, region)
	}
	if profileArn != "arn:aws:codewhisperer:us-east-1:1:profile/P" {
		t.Fatalf("profileArn = %q, want the discovered region-matched ARN", profileArn)
	}
	if tokenPayload["grantType"] != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Errorf("grantType = %q", tokenPayload["grantType"])
	}
	if tokenPayload["deviceCode"] != "d" {
		t.Errorf("deviceCode = %q", tokenPayload["deviceCode"])
	}

	// A completed session must be consumed so a replayed poll cannot mint a
	// second account from the same device code.
	if GetBuilderIdSession(session.ID) != nil {
		t.Fatal("completed session was left in the session map")
	}
	if _, _, _, _, _, _, _, _, err := PollBuilderIdAuth(session.ID); err == nil {
		t.Fatal("second poll on a consumed session succeeded")
	}
}

func TestPollBuilderIdAuthReportsPendingWithoutConsumingSession(t *testing.T) {
	session := startPolledBuilderIdSession(t, jsonRoute(http.StatusBadRequest, `{"error":"authorization_pending"}`), nil)

	_, _, _, _, _, _, _, status, err := PollBuilderIdAuth(session.ID)
	if err != nil {
		t.Fatalf("pending poll returned an error: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status = %q, want pending", status)
	}
	if GetBuilderIdSession(session.ID) == nil {
		t.Fatal("a pending poll deleted the session")
	}
}

// slow_down must widen the caller's poll interval; leaving it unchanged keeps
// the client in the rate limit it was just warned about.
func TestPollBuilderIdAuthSlowDownIncreasesInterval(t *testing.T) {
	session := startPolledBuilderIdSession(t, jsonRoute(http.StatusBadRequest, `{"error":"slow_down"}`), nil)
	before := GetBuilderIdSession(session.ID).Interval

	_, _, _, _, _, _, _, status, err := PollBuilderIdAuth(session.ID)
	if err != nil {
		t.Fatalf("slow_down poll returned an error: %v", err)
	}
	if status != "slow_down" {
		t.Fatalf("status = %q, want slow_down", status)
	}
	if after := GetBuilderIdSession(session.ID).Interval; after != before+5 {
		t.Fatalf("interval went from %d to %d, want +5", before, after)
	}
}

func TestPollBuilderIdAuthTerminalErrorsDiscardSession(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"expired device code", `{"error":"expired_token"}`, "device code expired"},
		{"user denied", `{"error":"access_denied"}`, "user denied authorization"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := startPolledBuilderIdSession(t, jsonRoute(http.StatusBadRequest, tc.body), nil)

			_, _, _, _, _, _, _, _, err := PollBuilderIdAuth(session.ID)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			if GetBuilderIdSession(session.ID) != nil {
				t.Fatal("terminal error left the session in the map")
			}
		})
	}
}

func TestPollBuilderIdAuthRejectsUnknownAndExpiredSessions(t *testing.T) {
	if _, _, _, _, _, _, _, _, err := PollBuilderIdAuth("no-such-session"); err == nil ||
		!strings.Contains(err.Error(), "session not found") {
		t.Fatalf("unknown session error = %v, want session not found", err)
	}

	session := startPolledBuilderIdSession(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("expired session must not reach the token endpoint")
	}, nil)
	builderIdMu.Lock()
	builderIdSessions[session.ID].ExpiresAt = time.Now().Add(-time.Second)
	builderIdMu.Unlock()

	if _, _, _, _, _, _, _, _, err := PollBuilderIdAuth(session.ID); err == nil ||
		!strings.Contains(err.Error(), "authorization expired") {
		t.Fatalf("expired session error = %v, want authorization expired", err)
	}
	if GetBuilderIdSession(session.ID) != nil {
		t.Fatal("expired session was not discarded")
	}
}

func TestPollBuilderIdAuthReportsUnexpectedStatus(t *testing.T) {
	session := startPolledBuilderIdSession(t, jsonRoute(http.StatusInternalServerError, "boom"), nil)

	if _, _, _, _, _, _, _, _, err := PollBuilderIdAuth(session.ID); err == nil ||
		!strings.Contains(err.Error(), "unexpected response: 500") {
		t.Fatalf("error = %v, want unexpected response: 500", err)
	}
}

func TestCleanupExpiredBuilderIdSessionsKeepsLiveSessions(t *testing.T) {
	live := startPolledBuilderIdSession(t, noProfilesRoute(), nil)
	expired := &BuilderIdSession{ID: "expired-session", ExpiresAt: time.Now().Add(-time.Minute)}

	builderIdMu.Lock()
	builderIdSessions[expired.ID] = expired
	builderIdMu.Unlock()

	cleanupExpiredBuilderIdSessions()

	if GetBuilderIdSession(expired.ID) != nil {
		t.Fatal("expired session survived cleanup")
	}
	if GetBuilderIdSession(live.ID) == nil {
		t.Fatal("cleanup removed a live session")
	}
}
