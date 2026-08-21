package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omniproxy/config"
)

// newBankResetTestAccount builds a minimal Codex account pointed at a test
// server. The access token is a syntactically valid JWT-ish string; we set
// ChatGPTAccountID explicitly so no token parsing is needed.
func newBankResetTestAccount(t *testing.T, baseURL string) *config.Account {
	t.Helper()
	config.Init(t.TempDir() + "/config.json")
	acc := config.Account{
		ID:               "bank-reset-test-account",
		Nickname:         "Bank Reset Test",
		Email:            "bankreset@example.test",
		AuthMethod:       "codex",
		AccessToken:      "test-access-token",
		ChatGPTAccountID: "acct_test_123",
		BaseURL:          baseURL,
		Enabled:          true,
	}
	if err := config.AddAccount(acc); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	stored := config.GetAccounts()
	for i := range stored {
		if stored[i].ID == acc.ID {
			return &stored[i]
		}
	}
	t.Fatalf("account not found after AddAccount")
	return nil
}

// TestCodexConsumeResetCredit_Success verifies the happy path: a "reset"
// response returns windows_reset and clears the pending redeem id so a later
// click starts a fresh redemption.
func TestCodexConsumeResetCredit_Success(t *testing.T) {
	var seenRedeemIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			RedeemRequestID string `json:"redeem_request_id"`
		}
		_ = json.Unmarshal(body, &payload)
		seenRedeemIDs = append(seenRedeemIDs, payload.RedeemRequestID)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"reset","windows_reset":2}`))
	}))
	defer srv.Close()

	acc := newBankResetTestAccount(t, srv.URL)
	windows, err := codexConsumeResetCredit(acc)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if windows != 2 {
		t.Fatalf("windowsReset = %d, want 2", windows)
	}
	if len(seenRedeemIDs) != 1 {
		t.Fatalf("upstream received %d requests, want 1", len(seenRedeemIDs))
	}
	if strings.TrimSpace(seenRedeemIDs[0]) == "" {
		t.Fatalf("redeem_request_id was empty")
	}
	if acc.CodexResetRedeemID != "" {
		t.Fatalf("pending redeem id should be cleared on success, got %q", acc.CodexResetRedeemID)
	}
}

// TestCodexConsumeResetCredit_NoCredit verifies "no_credit" is a business
// outcome (nil error, 0 windows), not a transport error, and that the pending
// redeem id is cleared.
func TestCodexConsumeResetCredit_NoCredit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"no_credit","windows_reset":0}`))
	}))
	defer srv.Close()

	acc := newBankResetTestAccount(t, srv.URL)
	windows, err := codexConsumeResetCredit(acc)
	if err != nil {
		t.Fatalf("no_credit must not be an error, got: %v", err)
	}
	if windows != 0 {
		t.Fatalf("windowsReset = %d, want 0", windows)
	}
	if acc.CodexResetRedeemID != "" {
		t.Fatalf("pending redeem id should be cleared on no_credit, got %q", acc.CodexResetRedeemID)
	}
}

// TestCodexConsumeResetCredit_IdempotentRetry is the regression test for the
// bug this patch fixes: after an inconclusive attempt (HTTP 503), the retry
// must reuse the SAME redeem_request_id so the upstream dedupes it instead of
// burning a second non-refundable credit.
func TestCodexConsumeResetCredit_IdempotentRetry(t *testing.T) {
	var seenRedeemIDs []string
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			RedeemRequestID string `json:"redeem_request_id"`
		}
		_ = json.Unmarshal(body, &payload)
		seenRedeemIDs = append(seenRedeemIDs, payload.RedeemRequestID)
		attempt++
		if attempt == 1 {
			// Inconclusive: upstream may or may not have applied the redemption.
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"reset","windows_reset":1}`))
	}))
	defer srv.Close()

	acc := newBankResetTestAccount(t, srv.URL)

	if _, err := codexConsumeResetCredit(acc); err == nil {
		t.Fatalf("expected error on HTTP 503 attempt")
	}
	pending := acc.CodexResetRedeemID
	if pending == "" {
		t.Fatalf("pending redeem id must be retained after an inconclusive 503")
	}

	windows, err := codexConsumeResetCredit(acc)
	if err != nil {
		t.Fatalf("retry should succeed, got: %v", err)
	}
	if windows != 1 {
		t.Fatalf("windowsReset = %d, want 1", windows)
	}
	if len(seenRedeemIDs) != 2 {
		t.Fatalf("upstream received %d requests, want 2", len(seenRedeemIDs))
	}
	if seenRedeemIDs[0] != seenRedeemIDs[1] {
		t.Fatalf("retry used a different redeem id (%q vs %q) — not idempotent, a second credit would be burned",
			seenRedeemIDs[0], seenRedeemIDs[1])
	}
	if acc.CodexResetRedeemID != "" {
		t.Fatalf("pending redeem id should be cleared after conclusive success, got %q", acc.CodexResetRedeemID)
	}
}

// TestCodexConsumeResetCredit_DefinitiveRejectionClearsID verifies a 4xx
// (other than 429) is treated as "never redeemed", so the pending id is
// dropped and the next attempt starts fresh.
func TestCodexConsumeResetCredit_DefinitiveRejectionClearsID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer srv.Close()

	acc := newBankResetTestAccount(t, srv.URL)
	if _, err := codexConsumeResetCredit(acc); err == nil {
		t.Fatalf("expected error on HTTP 400")
	}
	if acc.CodexResetRedeemID != "" {
		t.Fatalf("pending redeem id should be cleared on definitive 4xx rejection, got %q", acc.CodexResetRedeemID)
	}
}

// TestGenerateCodexRedeemID_ShapeAndUniqueness checks the redeem id is a
// distinct RFC 4122 v4 UUID each time it is generated.
func TestGenerateCodexRedeemID_ShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := generateCodexRedeemID()
		parts := strings.Split(id, "-")
		if len(parts) != 5 {
			t.Fatalf("id %q does not have 5 dash-separated groups", id)
		}
		if len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
			t.Fatalf("id %q does not match UUID group lengths", id)
		}
		if parts[2][0] != '4' {
			t.Fatalf("id %q is not version 4 (got %q)", id, parts[2][0:1])
		}
		switch parts[3][0] {
		case '8', '9', 'a', 'b':
		default:
			t.Fatalf("id %q has wrong RFC 4122 variant nibble %q", id, parts[3][0:1])
		}
		if seen[id] {
			t.Fatalf("duplicate redeem id generated: %q", id)
		}
		seen[id] = true
	}
}
