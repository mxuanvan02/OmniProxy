package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestDB writes a minimal 9router-shaped db.json to a temp dir and
// returns its path. Used by the import tests.
func writeTestDB(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write db.json: %v", err)
	}
	return path
}

func TestReadNineRouterDB_CodexAndKiro(t *testing.T) {
	db := `{
  "providerConnections": [
    {
      "id": "codex-1",
      "provider": "codex",
      "authType": "oauth",
      "name": "My Codex",
      "accessToken": "eyJhbGciOiJSUzI1NiIsImtpZCI6IjE5MzQ0ZTY1In0.eyJzdWIiOiJhYmMxMjMifQ.sig",
      "refreshToken": "rt_test123",
      "expiresAt": "2026-05-24T09:12:38.945Z",
      "providerSpecificData": {
        "chatgptAccountId": "2c4d3890-577e-45a0-a5c9-132ce0c0b690",
        "chatgptPlanType": "plus"
      }
    },
    {
      "id": "kiro-1",
      "provider": "kiro",
      "authType": "oauth",
      "name": "My Kiro",
      "accessToken": "aoa_test",
      "refreshToken": "aor_test",
      "expiresAt": "2026-06-14T16:38:19.427Z",
      "providerSpecificData": {
        "profileArn": "arn:aws:codewhisperer:us-east-1:699475941385:profile/EHGA3GRVQMUK"
      }
    },
    {
      "id": "qwen-1",
      "provider": "qwen",
      "authType": "oauth",
      "name": "Qwen",
      "accessToken": "qwen_token"
    },
    {
      "id": "openrouter-1",
      "provider": "openrouter",
      "authType": "apikey",
      "name": "OR",
      "apiKey": "sk-or-v1-test"
    }
  ]
}`
	path := writeTestDB(t, db)
	t.Setenv("NINEROUTER_DB", path)

	result, err := ReadNineRouterDB()
	if err != nil {
		t.Fatalf("ReadNineRouterDB: %v", err)
	}
	if len(result.Codex) != 1 {
		t.Fatalf("expected 1 codex account, got %d", len(result.Codex))
	}
	if len(result.Kiro) != 1 {
		t.Fatalf("expected 1 kiro account, got %d", len(result.Kiro))
	}
	codex := result.Codex[0]
	if codex.Name != "My Codex" {
		t.Errorf("codex name = %q, want %q", codex.Name, "My Codex")
	}
	if codex.RefreshToken != "rt_test123" {
		t.Errorf("codex refresh = %q, want %q", codex.RefreshToken, "rt_test123")
	}
	if codex.ChatGPTAccountID != "2c4d3890-577e-45a0-a5c9-132ce0c0b690" {
		t.Errorf("codex account id = %q", codex.ChatGPTAccountID)
	}
	if codex.PlanType != "plus" {
		t.Errorf("codex plan = %q, want plus", codex.PlanType)
	}
	if codex.ExpiresAt == 0 {
		t.Error("codex expiresAt should be parsed")
	}
	// Verify the parsed expiry matches the expected time.
	wantExpiry := time.Date(2026, 5, 24, 9, 12, 38, 945000000, time.UTC).Unix()
	if codex.ExpiresAt != wantExpiry {
		t.Errorf("codex expiresAt = %d, want %d", codex.ExpiresAt, wantExpiry)
	}

	kiro := result.Kiro[0]
	if kiro.Name != "My Kiro" {
		t.Errorf("kiro name = %q", kiro.Name)
	}
	if kiro.RefreshToken != "aor_test" {
		t.Errorf("kiro refresh = %q", kiro.RefreshToken)
	}
	if kiro.ProfileArn != "arn:aws:codewhisperer:us-east-1:699475941385:profile/EHGA3GRVQMUK" {
		t.Errorf("kiro profileArn = %q", kiro.ProfileArn)
	}

	// Skipped providers should include qwen + openrouter.
	if len(result.Skipped) != 2 {
		t.Fatalf("expected 2 skipped providers, got %d (%v)", len(result.Skipped), result.Skipped)
	}
}

func TestReadNineRouterDB_EmptyFile(t *testing.T) {
	path := writeTestDB(t, `{"providerConnections": []}`)
	t.Setenv("NINEROUTER_DB", path)

	result, err := ReadNineRouterDB()
	if err != nil {
		t.Fatalf("ReadNineRouterDB: %v", err)
	}
	if len(result.Codex) != 0 || len(result.Kiro) != 0 {
		t.Errorf("expected 0 accounts, got codex=%d kiro=%d", len(result.Codex), len(result.Kiro))
	}
}

func TestReadNineRouterDB_MissingFile(t *testing.T) {
	t.Setenv("NINEROUTER_DB", "/nonexistent/path/db.json")
	_, err := ReadNineRouterDB()
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestReadNineRouterDB_CodexMissingToken(t *testing.T) {
	// A codex connection with no access token should be dropped
	// (parseNineRouterCodex returns nil).
	db := `{
  "providerConnections": [
    {"id": "codex-bad", "provider": "codex", "authType": "oauth", "name": "Bad", "refreshToken": "rt_only"}
  ]
}`
	path := writeTestDB(t, db)
	t.Setenv("NINEROUTER_DB", path)

	result, err := ReadNineRouterDB()
	if err != nil {
		t.Fatalf("ReadNineRouterDB: %v", err)
	}
	if len(result.Codex) != 0 {
		t.Errorf("expected 0 codex accounts (no access token), got %d", len(result.Codex))
	}
}

func TestReadNineRouterDB_KiroMissingRefresh(t *testing.T) {
	// A kiro connection with no refresh token should be dropped.
	db := `{
  "providerConnections": [
    {"id": "kiro-bad", "provider": "kiro", "authType": "oauth", "name": "Bad", "accessToken": "aoa_only"}
  ]
}`
	path := writeTestDB(t, db)
	t.Setenv("NINEROUTER_DB", path)

	result, err := ReadNineRouterDB()
	if err != nil {
		t.Fatalf("ReadNineRouterDB: %v", err)
	}
	if len(result.Kiro) != 0 {
		t.Errorf("expected 0 kiro accounts (no refresh token), got %d", len(result.Kiro))
	}
}

func TestParseNineRouterExpiry(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"not-a-date", 0},
		{"2026-05-24T09:12:38.945Z", time.Date(2026, 5, 24, 9, 12, 38, 945000000, time.UTC).Unix()},
		{"2026-05-24T09:12:38Z", time.Date(2026, 5, 24, 9, 12, 38, 0, time.UTC).Unix()},
	}
	for _, c := range cases {
		got := parseNineRouterExpiry(c.in)
		if got != c.want {
			t.Errorf("parseNineRouterExpiry(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
