package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	"strings"
	"testing"
)

// profileServer answers ListAvailableProfiles with the given ARNs and records
// the headers of the last request, so the Kiro protocol headers the upstream
// requires can be asserted.
func profileServer(t *testing.T, arns ...string) (*httptest.Server, *http.Header) {
	t.Helper()
	lastHeader := new(http.Header)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*lastHeader = r.Header.Clone()
		profiles := make([]map[string]string, 0, len(arns))
		for _, arn := range arns {
			profiles = append(profiles, map[string]string{"arn": arn})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"profiles": profiles})
	}))
	t.Cleanup(srv.Close)
	installTestAuthClient(t, srv)
	return srv, lastHeader
}

func TestDiscoverProfileArnPrefersRegionMatchedProfile(t *testing.T) {
	_, header := profileServer(t,
		"arn:aws:codewhisperer:us-west-2:111:profile/OTHER",
		"arn:aws:codewhisperer:eu-central-1:222:profile/WANTED",
	)

	got := DiscoverProfileArn("test-token-xxx", "eu-central-1", false)
	if got != "arn:aws:codewhisperer:eu-central-1:222:profile/WANTED" {
		t.Fatalf("DiscoverProfileArn = %q, want the eu-central-1 profile", got)
	}
	if target := header.Get("x-amz-target"); target != "AmazonCodeWhispererService.ListAvailableProfiles" {
		t.Errorf("x-amz-target = %q", target)
	}
	if auth := header.Get("Authorization"); auth != "Bearer test-token-xxx" {
		t.Errorf("Authorization = %q", auth)
	}
	if _, set := (*header)["Tokentype"]; set {
		t.Error("TokenType header sent for a non-external-IdP account")
	}
}

// With no region match, the first non-empty ARN is the documented fallback.
func TestDiscoverProfileArnFallsBackToFirstProfile(t *testing.T) {
	profileServer(t, "", "arn:aws:codewhisperer:ap-southeast-2:333:profile/FIRST")

	if got := DiscoverProfileArn("test-token-xxx", "us-east-1", false); got != "arn:aws:codewhisperer:ap-southeast-2:333:profile/FIRST" {
		t.Fatalf("DiscoverProfileArn = %q, want the first non-empty ARN", got)
	}
}

// Enterprise SSO tokens must carry TokenType: EXTERNAL_IDP; without it the
// upstream rejects the token and profile discovery silently returns "".
func TestDiscoverProfileArnSendsExternalIdpTokenType(t *testing.T) {
	_, header := profileServer(t, "arn:aws:codewhisperer:us-east-1:444:profile/IDP")

	if got := DiscoverProfileArn("test-token-xxx", "us-east-1", true); got == "" {
		t.Fatal("DiscoverProfileArn returned empty for an external IdP account")
	}
	if got := header.Get("TokenType"); got != "EXTERNAL_IDP" {
		t.Fatalf("TokenType = %q, want EXTERNAL_IDP", got)
	}
}

// A Builder ID account legitimately has no profile. That is not an error, but it
// must not be reported as one either — "" is the contract.
func TestDiscoverProfileArnReturnsEmptyWithoutProfilesOrOnFailure(t *testing.T) {
	if got := DiscoverProfileArn("", "us-east-1", false); got != "" {
		t.Fatalf("empty access token produced %q, want \"\"", got)
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"profiles":[]}`))
	}))
	defer empty.Close()
	installTestAuthClient(t, empty)
	if got := DiscoverProfileArn("test-token-xxx", "us-east-1", false); got != "" {
		t.Fatalf("empty profile list produced %q, want \"\"", got)
	}
}

func TestDiscoverProfileArnReturnsEmptyOnHTTPErrorAndMalformedBody(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer failing.Close()
	installTestAuthClient(t, failing)
	if got := DiscoverProfileArn("test-token-xxx", "us-east-1", false); got != "" {
		t.Fatalf("HTTP 403 produced %q, want \"\"", got)
	}

	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"profiles":`))
	}))
	defer malformed.Close()
	installTestAuthClient(t, malformed)
	if got := DiscoverProfileArn("test-token-xxx", "us-east-1", false); got != "" {
		t.Fatalf("malformed body produced %q, want \"\"", got)
	}
}

// The legacy codewhisperer host only exists in us-east-1; every other region is
// served by q.<region>. Routing a non-us-east-1 account at the legacy host is a
// silent discovery failure.
func TestKiroProfileHostPerRegion(t *testing.T) {
	cases := map[string]string{
		"":              "https://codewhisperer.us-east-1.amazonaws.com",
		"us-east-1":     "https://codewhisperer.us-east-1.amazonaws.com",
		"US-EAST-1":     "https://codewhisperer.us-east-1.amazonaws.com",
		"eu-central-1":  "https://q.eu-central-1.amazonaws.com",
		" ap-south-1  ": "https://q.ap-south-1.amazonaws.com",
	}
	for region, want := range cases {
		if got := kiroProfileHost(region); got != want {
			t.Errorf("kiroProfileHost(%q) = %q, want %q", region, got, want)
		}
	}
}

// The preferred region is probed first, then the documented profile regions.
func TestDiscoverProfileArnMultiRegionProbesPreferredRegionFirst(t *testing.T) {
	var hosts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.Host)
		// Only answer with a profile once eu-central-1 is probed.
		if len(hosts) < 2 {
			_, _ = w.Write([]byte(`{"profiles":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"profiles":[{"arn":"arn:aws:codewhisperer:eu-central-1:555:profile/X"}]}`))
	}))
	defer srv.Close()
	installTestAuthClient(t, srv)

	if got := DiscoverProfileArnMultiRegion("test-token-xxx", "ap-south-1", false); got == "" {
		t.Fatal("multi-region probe found no profile")
	}
	if len(hosts) < 2 {
		t.Fatalf("probed %d regions, want at least 2", len(hosts))
	}
	if got := DiscoverProfileArnMultiRegion("", "us-east-1", false); got != "" {
		t.Fatalf("empty token produced %q, want \"\"", got)
	}
}

func TestListProfilesReturnsEmptyForEmptyTokenAndOnFailure(t *testing.T) {
	if got := ListProfiles("", "us-east-1", false); got != "" {
		t.Fatalf("empty token produced %q, want \"\"", got)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"profiles":[{"arn":"arn:aws:codewhisperer:us-east-1:666:profile/L"}]}`))
	}))
	defer srv.Close()
	installTestAuthClient(t, srv)
	if got := ListProfiles("test-token-xxx", "", false); got != "arn:aws:codewhisperer:us-east-1:666:profile/L" {
		t.Fatalf("ListProfiles = %q", got)
	}
}

// ResolveProfileArn must refuse locally when it has neither a profile nor the
// credentials to refresh — otherwise the caller cannot tell "no profile" from
// "could not ask".
func TestResolveProfileArnRequiresTokenAndRefreshCredentials(t *testing.T) {
	if _, _, _, _, err := ResolveProfileArn("", "us-east-1", "id", "secret", "", "", "rt"); err == nil ||
		!strings.Contains(err.Error(), "access token is empty") {
		t.Fatalf("error = %v, want empty access token", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"profiles":[]}`))
	}))
	defer srv.Close()
	installTestAuthClient(t, srv)

	_, _, _, _, err := ResolveProfileArn("test-token-xxx", "us-east-1", "", "", "", "", "rt")
	if err == nil || !strings.Contains(err.Error(), "no client credentials to refresh") {
		t.Fatalf("error = %v, want missing client credentials", err)
	}
}

func TestResolveProfileArnReturnsFirstProfileWithoutRefreshing(t *testing.T) {
	initTempAuthConfig(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			t.Error("refresh was attempted even though a profile was found")
		}
		_, _ = w.Write([]byte(`{"profiles":[{"arn":"arn:aws:codewhisperer:us-east-1:777:profile/R"}]}`))
	}))
	defer srv.Close()
	installTestAuthClient(t, srv)

	arn, newAT, newRT, newExp, err := ResolveProfileArn("test-token-xxx", "us-east-1", "id", "secret", "", "", "rt")
	if err != nil {
		t.Fatalf("ResolveProfileArn: %v", err)
	}
	if arn != "arn:aws:codewhisperer:us-east-1:777:profile/R" {
		t.Fatalf("arn = %q", arn)
	}
	if newAT != "" || newRT != "" || newExp != 0 {
		t.Fatalf("no refresh should be reported: (%q, %q, %d)", newAT, newRT, newExp)
	}
}

// The last-resort fallback for IDC tenants: reuse a profile already stored for
// the same start URL and region.
func TestCachedProfileArnForStartURLMatchesOnStartURLAndRegion(t *testing.T) {
	initTempAuthConfig(t)
	accounts := []config.Account{
		{ID: "no-arn", StartUrl: "https://tenant.awsapps.com/start", Region: "us-east-1"},
		{ID: "other-region", StartUrl: "https://tenant.awsapps.com/start", Region: "eu-central-1", ProfileArn: "arn:wrong-region"},
		{ID: "other-tenant", StartUrl: "https://elsewhere.awsapps.com/start", Region: "us-east-1", ProfileArn: "arn:wrong-tenant"},
		{ID: "match", StartUrl: "https://tenant.awsapps.com/start", Region: "US-EAST-1", ProfileArn: "arn:correct"},
	}
	for _, account := range accounts {
		if err := config.AddAccount(account); err != nil {
			t.Fatalf("AddAccount(%s): %v", account.ID, err)
		}
	}

	if got := CachedProfileArnForStartURL("https://tenant.awsapps.com/start", ""); got != "arn:correct" {
		t.Fatalf("CachedProfileArnForStartURL = %q, want arn:correct", got)
	}
	if got := CachedProfileArnForStartURL("", "us-east-1"); got != "" {
		t.Fatalf("empty start URL produced %q, want \"\"", got)
	}
	if got := CachedProfileArnForStartURL("https://unknown.awsapps.com/start", "us-east-1"); got != "" {
		t.Fatalf("unknown tenant produced %q, want \"\"", got)
	}
}

func TestDeriveMachineIDIsStableAndSeedDependent(t *testing.T) {
	first := deriveMachineID("test-token-xxx", "us-east-1")
	if len(first) != 16 {
		t.Fatalf("machine ID length = %d, want 16", len(first))
	}
	if again := deriveMachineID("test-token-xxx", "us-east-1"); again != first {
		t.Fatalf("machine ID not stable: %q then %q", first, again)
	}
	if other := deriveMachineID("test-token-xxx", "eu-central-1"); other == first {
		t.Fatal("machine ID ignored the region component of its seed")
	}
}
