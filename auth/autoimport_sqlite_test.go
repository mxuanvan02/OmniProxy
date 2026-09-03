package auth

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// buildKiroCliDB creates a minimal kiro-cli-shaped SQLite file: an auth_kv
// key/value table holding the JSON blobs readSQLite looks for.
func buildKiroCliDB(t *testing.T, rows map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kiro-cli.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for k, v := range rows {
		if _, err := db.Exec(`INSERT INTO auth_kv (key, value) VALUES (?, ?)`, k, v); err != nil {
			t.Fatalf("insert %s: %v", k, err)
		}
	}
	return path
}

func TestReadSQLiteExtractsCredentials(t *testing.T) {
	path := buildKiroCliDB(t, map[string]string{
		"kirocli:oidc:token":               `{"refresh_token":"rt-abc","access_token":"at-abc","region":"eu-west-1","expires_at":"2030-01-02T03:04:05Z"}`,
		"kirocli:oidc:device-registration": `{"client_id":"cid-1","client_secret":"sec-1"}`,
		"api.codewhisperer.profile":        `{"arn":"arn:aws:codewhisperer:eu-west-1:1:profile/p"}`,
	})

	creds, err := readSQLite(path, "")
	if err != nil {
		t.Fatalf("readSQLite: %v", err)
	}
	if creds.RefreshToken != "rt-abc" || creds.AccessToken != "at-abc" {
		t.Fatalf("tokens = (%q, %q)", creds.RefreshToken, creds.AccessToken)
	}
	if creds.ClientID != "cid-1" || creds.ClientSecret != "sec-1" {
		t.Fatalf("registration = (%q, %q)", creds.ClientID, creds.ClientSecret)
	}
	if creds.Region != "eu-west-1" {
		t.Fatalf("region = %q, want eu-west-1 from the DB", creds.Region)
	}
	if creds.ProfileArn == "" {
		t.Fatal("profile arn not read")
	}
}

// TestReadSQLiteBusyTimeoutPragmaApplies guards the DSN: modernc's driver strips
// a non-file: query string from the filename but still honours _pragma, so the
// _busy_timeout form is a silent no-op and only _pragma=busy_timeout(...) lands.
func TestReadSQLiteBusyTimeoutPragmaApplies(t *testing.T) {
	path := buildKiroCliDB(t, nil)
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var ms int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&ms); err != nil {
		t.Fatalf("read pragma: %v", err)
	}
	if ms != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000 (DSN pragma did not apply)", ms)
	}
}

func TestDiscoverTablesReportsTablesAndErrors(t *testing.T) {
	path := buildKiroCliDB(t, nil)
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	tables, err := discoverTables(db)
	if err != nil {
		t.Fatalf("discoverTables: %v", err)
	}
	if !tables["auth_kv"] {
		t.Fatalf("auth_kv missing from %v", tables)
	}

	// A closed DB must surface the failure instead of returning an empty set that
	// readSQLite would misread as "no tables in this database".
	db.Close()
	if _, err := discoverTables(db); err == nil {
		t.Fatal("discoverTables on a closed DB returned nil error")
	}
}
