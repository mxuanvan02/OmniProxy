package proxy

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"omniproxy/config"
)

// TestClassifyCodexAuthFailureSeparatesBanFromSessionExpiry pins the core
// distinction the dashboard depends on: a dead session must offer "Log in
// again", while an upstream account termination must surface as BANNED.
//
// Before this classifier existed, every 401 was flattened into
// REAUTH_REQUIRED, so a banned ChatGPT account was indistinguishable from a
// logged-out one.
func TestClassifyCodexAuthFailureSeparatesBanFromSessionExpiry(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want codexAuthFailureKind
	}{
		// Real upstream bodies observed in data/omniproxy.log.
		{
			name: "token_invalidated is a dead session",
			err:  fmt.Errorf(`HTTP 401: {"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","code":"token_invalidated"}}`),
			want: codexAuthFailureReauth,
		},
		{
			name: "refresh_token_invalidated is a dead session",
			err:  fmt.Errorf(`codex refresh: codex token: HTTP 401: {"error":{"message":"Your session has ended. Please log in again.","code":"refresh_token_invalidated"}}`),
			want: codexAuthFailureReauth,
		},
		{
			name: "token_revoked is a dead session",
			err:  fmt.Errorf(`HTTP 401: {"error":{"code":"token_revoked"}}`),
			want: codexAuthFailureReauth,
		},
		// Ban vocabulary must win even though the status code is identical.
		{
			name: "account_deactivated is a ban",
			err:  fmt.Errorf(`HTTP 401: {"error":{"message":"Your account was deactivated.","code":"account_deactivated"}}`),
			want: codexAuthFailureBanned,
		},
		{
			name: "403 account suspended is a ban",
			err:  fmt.Errorf(`HTTP 403: {"error":{"message":"Your account has been suspended."}}`),
			want: codexAuthFailureBanned,
		},
		{
			name: "policy violation is a ban",
			err:  fmt.Errorf(`HTTP 403: {"error":{"message":"Access terminated for violating our usage policies."}}`),
			want: codexAuthFailureBanned,
		},
		{
			name: "unusual activity is a ban",
			err:  fmt.Errorf(`HTTP 403: {"error":{"message":"We detected unusual activity on this account."}}`),
			want: codexAuthFailureBanned,
		},
		// Non-credential failures must not touch account state at all.
		{
			name: "429 rate limit is not a credential failure",
			err:  fmt.Errorf("HTTP 429: rate limit exceeded"),
			want: codexAuthFailureNone,
		},
		{
			name: "502 upstream is not a credential failure",
			err:  fmt.Errorf("HTTP 502: bad gateway"),
			want: codexAuthFailureNone,
		},
		{
			name: "dns failure is not a credential failure",
			err:  fmt.Errorf("dial tcp: lookup chatgpt.com: no such host"),
			want: codexAuthFailureNone,
		},
		{
			name: "nil error",
			err:  nil,
			want: codexAuthFailureNone,
		},
		// A ban reported alongside a rate limit must still be a ban.
		{
			name: "429 plus ban vocabulary is a ban",
			err:  fmt.Errorf("HTTP 429: rate limit; account suspended"),
			want: codexAuthFailureBanned,
		},
		// Bare 401 with no vocabulary falls back to the recoverable reading.
		{
			name: "bare 401 defaults to re-login",
			err:  fmt.Errorf("HTTP 401: unauthorized"),
			want: codexAuthFailureReauth,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyCodexAuthFailure(c.err); got != c.want {
				t.Fatalf("classifyCodexAuthFailure(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// TestMarkCodexAuthFailurePersistsBannedForTerminatedAccount proves the banned
// state actually reaches persisted config, so the dashboard renders BANNED
// instead of a "Log in again" button that can never recover the account.
func TestMarkCodexAuthFailurePersistsBannedForTerminatedAccount(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	banned := config.Account{
		ID:          "codex-banned",
		Email:       "banned@example.test",
		AuthMethod:  codexAuthMethod,
		AccessToken: "access-token",
		Enabled:     true,
	}
	if err := config.AddAccount(banned); err != nil {
		t.Fatalf("add banned account: %v", err)
	}

	markCodexAuthFailure(&banned, fmt.Errorf(`HTTP 403: {"error":{"message":"Your account has been suspended."}}`))

	got := findAccountByID(t, banned.ID)
	if got.BanStatus != "BANNED" {
		t.Fatalf("banStatus = %q, want BANNED", got.BanStatus)
	}
	if got.Enabled {
		t.Fatal("banned account must be disabled")
	}
	if !strings.Contains(strings.ToLower(got.BanReason), "suspended") {
		t.Fatalf("banReason = %q, want upstream suspension text", got.BanReason)
	}
	if got.BanTime == 0 {
		t.Fatal("banTime must be recorded so the UI can show when it happened")
	}
}

// TestMarkCodexAuthFailureNeverDowngradesBanToReauth guards the ordering bug:
// once OpenAI reports a termination, a later bare 401 from a background
// refresh must not overwrite BANNED with REAUTH_REQUIRED and hide the ban.
func TestMarkCodexAuthFailureNeverDowngradesBanToReauth(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	account := config.Account{
		ID:          "codex-downgrade",
		Email:       "downgrade@example.test",
		AuthMethod:  codexAuthMethod,
		AccessToken: "access-token",
		Enabled:     true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	markCodexAuthFailure(&account, fmt.Errorf(`HTTP 403: {"error":{"code":"account_deactivated"}}`))
	if got := findAccountByID(t, account.ID); got.BanStatus != "BANNED" {
		t.Fatalf("setup failed: banStatus = %q, want BANNED", got.BanStatus)
	}

	// Background refresh later sees only a session error.
	markCodexAuthFailure(&account, fmt.Errorf(`HTTP 401: {"error":{"code":"token_invalidated"}}`))

	got := findAccountByID(t, account.ID)
	if got.BanStatus != "BANNED" {
		t.Fatalf("banStatus was downgraded to %q; ban must survive later 401s", got.BanStatus)
	}
}

// TestMarkCodexAuthFailureIgnoresNonCredentialErrors ensures transient
// upstream failures never disable an otherwise healthy account.
func TestMarkCodexAuthFailureIgnoresNonCredentialErrors(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	account := config.Account{
		ID:          "codex-healthy",
		Email:       "healthy@example.test",
		AuthMethod:  codexAuthMethod,
		AccessToken: "access-token",
		Enabled:     true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	for _, err := range []error{
		fmt.Errorf("HTTP 429: rate limit exceeded"),
		fmt.Errorf("HTTP 503: service unavailable"),
		fmt.Errorf("context deadline exceeded"),
	} {
		markCodexAuthFailure(&account, err)
	}

	got := findAccountByID(t, account.ID)
	if got.BanStatus != "" || !got.Enabled {
		t.Fatalf("healthy account was mutated: banStatus=%q enabled=%v", got.BanStatus, got.Enabled)
	}
}

func findAccountByID(t *testing.T, id string) config.Account {
	t.Helper()
	for _, a := range config.GetAccounts() {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("account %q not found", id)
	return config.Account{}
}
