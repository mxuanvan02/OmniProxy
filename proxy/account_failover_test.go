package proxy

import (
	"errors"
	"testing"

	"omniproxy/config"
	accountpool "omniproxy/pool"
)

func TestNetworkErrorClassifier(t *testing.T) {
	tests := []struct {
		msg string
		exp bool
	}{
		{msg: "dial tcp 127.0.0.1:8080: connect: connection refused", exp: true},
		{msg: "dial tcp: lookup localhost: no such host", exp: true},
		{msg: "dial tcp 127.0.0.1:8080: i/o timeout", exp: true},
		{msg: "read: connection reset by peer", exp: true},
		{msg: "write: broken pipe", exp: true},
		{msg: "EOF", exp: true},
		{msg: "external SSE read: stream error: stream ID 57; INTERNAL_ERROR; received from peer", exp: true},
		{msg: "external SSE read: stream error: stream ID 19; REFUSED_STREAM; received from peer", exp: true},
		{msg: "external SSE read: stream error: malformed event payload", exp: false},
		{msg: "upstream returned INTERNAL_ERROR without an HTTP/2 stream reset", exp: false},
		{msg: "HTTP 503 from Kiro IDE: upstream unavailable", exp: false},
		{msg: "HTTP 401 from Kiro IDE: unauthorized", exp: false},
		{msg: "HTTP 429: quota exhausted", exp: false},
		{msg: "no available Kiro profile", exp: false},
		// Both transport shapes that used to reach the default branch and park
		// a healthy account. See TestHeaderTimeoutDoesNotCoolDownTheOnlyAccount.
		{msg: `external call example-provider: Post "https://api.example.com/v1/chat/completions": ` +
			`http2: timeout awaiting response headers`, exp: true},
		{msg: `external call example-provider: Post "https://api.example.com/v1/chat/completions": ` +
			`read tcp 192.0.2.10:54102->192.0.2.20:443: read: can't assign requested address`, exp: true},
	}

	for _, tc := range tests {
		got := isNetworkError(tc.msg)
		if got != tc.exp {
			t.Errorf("isNetworkError(%q) = %v, want %v", tc.msg, got, tc.exp)
		}
	}
}

func TestAccountFailureClassifiers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) bool
		msg  string
	}{
		{name: "quota", fn: isQuotaErrorMessage, msg: "HTTP 429: quota exhausted"},
		{name: "rate limit quota", fn: isQuotaErrorMessage, msg: `HTTP 403 from 10k: {"error":{"type":"rate_limit_error"}}`},
		{name: "overage", fn: isOverageErrorMessage, msg: "HTTP 402 from Kiro IDE: OVERAGE limit exceeded"},
		{name: "suspension", fn: isSuspensionErrorMessage, msg: "Your User ID temporarily is suspended"},
		{name: "profile", fn: isProfileUnavailableErrorMessage, msg: "no available Kiro profile"},
		{name: "auth", fn: isAuthErrorMessage, msg: "Authentication failed - token invalid or expired"},
	}

	for _, tc := range tests {
		if !tc.fn(tc.msg) {
			t.Fatalf("%s classifier did not match %q", tc.name, tc.msg)
		}
	}
}

func TestRateLimit403IsNotAuthenticationFailure(t *testing.T) {
	msg := `HTTP 403 from 10k: {"error":{"type":"rate_limit_error"}}`
	if isAuthErrorMessage(msg) {
		t.Fatalf("rate-limit response must not trigger token refresh: %s", msg)
	}
}

// TestHeaderTimeoutDoesNotCoolDownTheOnlyAccount pins what an operator actually
// saw: requests to a model only one provider serves kept aborting instantly
// ("no account found after 0 attempts") instead of failing once and recovering.
//
// The chain was: the transport's response-header deadline fired, the message
// matched no classifier, handleAccountFailure's default branch charged a
// cooldown, and three consecutive stalls parked the account for a minute. During
// that minute every request for the model found no candidate at all.
func TestHeaderTimeoutDoesNotCoolDownTheOnlyAccount(t *testing.T) {
	initConfigForTests(t)
	const id = "ext-header-timeout"
	if err := config.AddAccount(config.Account{
		ID:          id,
		Email:       "timeout@example.com",
		AuthMethod:  externalAuthMethod,
		Enabled:     true,
		AccessToken: "key",
		BaseURL:     "https://api.example.com",
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	p.SetModelList(id, []string{"qwen3.8-max"})

	h := &Handler{pool: p, catalogStatus: newCatalogStatusStore()}
	err := errors.New(`external call timeout@example.com: Post "https://api.example.com/v1/chat/completions": ` +
		`http2: timeout awaiting response headers`)

	// Three consecutive failures is what trips the strike counter.
	for i := 0; i < 3; i++ {
		h.handleAccountFailure(p.GetByID(id), err, "qwen3.8-max")
	}

	if !p.HasAvailableAccountForModel("qwen3.8-max") {
		t.Fatal("a stalled response header parked the only account for the model; " +
			"the next request aborts with \"no account found\" instead of retrying")
	}
}

// AgentRouter's keyword scanner rejects a payload with HTTP 500 and
// code "sensitive_words_detected". That status is neither one of the
// 502/503/504 transient tokens nor an auth/quota shape, so before this was
// classified the error reached handleAccountFailure's default branch and
// charged a cooldown to an account that was working correctly — then the
// failover loop replayed the identical payload across the rest of the pool.
func TestSensitiveWordsRejectionIsContentBlockedNotAccountFault(t *testing.T) {
	msg := `AgentRouter (Backup Domain) HTTP 500 from AgentRouter (AgentRouter-Opus5): ` +
		`{"error":{"message":"sensitive words detected (request id: 20260912101558810317806g8n5vTAILW1QK)",` +
		`"type":"new_api_error","param":"","code":"sensitive_words_detected"}}`

	if !isContentBlockedErrorMessage(msg) {
		t.Fatalf("sensitive-words rejection must be classified content-blocked: %s", msg)
	}
	// Guard the misclassifications that made this bug expensive.
	if isAuthErrorMessage(msg) {
		t.Fatal("payload refusal must not be treated as an auth failure")
	}
	if isQuotaErrorMessage(msg) {
		t.Fatal("payload refusal must not be treated as quota exhaustion")
	}
	if isNetworkError(msg) {
		t.Fatal("payload refusal must not be treated as a transport error")
	}
}

// The proxy-side classifier delegates to pool.IsContentBlockedError so the two
// cannot drift again. Both spellings and both grammatical numbers must match.
func TestContentBlockedClassifierMatchesPoolMarkers(t *testing.T) {
	blocked := []string{
		"upstream 400: content-blocked",
		"CONTENT_BLOCKER triggered",
		"request rejected: content blocked by policy",
		`{"code":"sensitive_words_detected"}`,
		"sensitive word detected",
		"finishReason: PROHIBITED_CONTENT",
	}
	for _, msg := range blocked {
		if !isContentBlockedErrorMessage(msg) {
			t.Errorf("isContentBlockedErrorMessage(%q) = false, want true", msg)
		}
	}

	notBlocked := []string{
		"HTTP 401 unauthorized",
		"HTTP 429 too many requests",
		"context deadline exceeded",
		"blocked account",
		"",
	}
	for _, msg := range notBlocked {
		if isContentBlockedErrorMessage(msg) {
			t.Errorf("isContentBlockedErrorMessage(%q) = true, want false", msg)
		}
	}
}

// TestGenericBanAppliesExcludesSelfClassifyingProviders pins which accounts the
// shared Kiro-shaped ban branches are allowed to act on.
//
// The message tests they are paired with are deliberately broad:
// isAuthErrorMessage matches any text carrying a standalone 403. That is right
// for Kiro, where a 403 means the account was rejected — and wrong for a
// provider that classifies its own failures, because the same status also
// covers states an operator can clear. An Antigravity account whose owner still
// had a verification step outstanding was banned and disabled by the generic
// branch, and "Test & Recover" re-applied the ban on every attempt.
func TestGenericBanAppliesExcludesSelfClassifyingProviders(t *testing.T) {
	tosDisable403 := `HTTP 403 from owner@example.com: {"error":{"code":403,"message":"Verify your account to continue.",` +
		`"status":"PERMISSION_DENIED","details":[{"reason":"VALIDATION_REQUIRED"}]}}`

	// The message really does trip the broad auth test; the guard is the only
	// thing standing between it and a ban.
	if !isAuthErrorMessage(tosDisable403) {
		t.Fatal("isAuthErrorMessage did not match a 403 body — this test no longer covers the bug")
	}

	cases := []struct {
		name    string
		account *config.Account
		want    bool
	}{
		{"kiro account", &config.Account{ID: "k", AuthMethod: "builderid"}, true},
		{"antigravity account", &config.Account{ID: "ag", AuthMethod: "antigravity"}, false},
		{"external openai account", &config.Account{ID: "e", AuthMethod: "external_openai"}, false},
		{"codex account", &config.Account{ID: "c", AuthMethod: codexAuthMethod}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := genericBanApplies(tc.account); got != tc.want {
				t.Errorf("genericBanApplies(%s) = %v, want %v", tc.account.AuthMethod, got, tc.want)
			}
		})
	}
}
