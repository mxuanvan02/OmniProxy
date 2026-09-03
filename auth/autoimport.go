package auth

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// KiroCliCredentials holds credentials found in kiro-cli SQLite.
type KiroCliCredentials struct {
	RefreshToken string
	AccessToken  string
	ClientID     string
	ClientSecret string
	Region       string
	ProfileArn   string
	ExpiresAt    time.Time
}

// SSOCacheCredentials holds credentials found in ~/.aws/sso/cache.
type SSOCacheCredentials struct {
	RefreshToken string
	Source       string
}

// ImportKiroCli scans the kiro-cli SQLite database(s) for stored credentials and profileArn.
// Returns the first valid set found.
func ImportKiroCli() (*KiroCliCredentials, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}

	candidates := []string{
		filepath.Join(home, ".local", "share", "kiro-cli", "data.sqlite3"),
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		candidates = append(candidates, filepath.Join(appData, "kiro", "storage.db"))
	}

	for _, dbPath := range candidates {
		creds, err := readSQLite(dbPath, "us-east-1")
		if err == nil && creds != nil && creds.RefreshToken != "" {
			return creds, nil
		}
	}
	return nil, fmt.Errorf("no kiro-cli credentials found")
}

// ParseKiroCliFile parses kiro-cli SQLite content from a base64-encoded file upload.
func ParseKiroCliFile(fileContent string, region string) (*KiroCliCredentials, error) {
	data, err := base64.StdEncoding.DecodeString(fileContent)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(fileContent)
		if err != nil {
			return nil, fmt.Errorf("invalid file content: %w", err)
		}
	}
	return ParseKiroCliBytes(data, region)
}

// ParseKiroCliBytes parses kiro-cli SQLite content from raw bytes.
func ParseKiroCliBytes(data []byte, region string) (*KiroCliCredentials, error) {
	tmpFile, err := os.CreateTemp("", "kiro-cli-*.sqlite3")
	if err != nil {
		return nil, fmt.Errorf("temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return nil, fmt.Errorf("write temp: %w", err)
	}
	tmpFile.Close()

	return readSQLite(tmpFile.Name(), region)
}

// sqliteDSN builds the read-only DSN. busy_timeout bounds the wait on a DB the
// running kiro-cli holds locked; modernc's driver takes pragmas via _pragma
// only — the _busy_timeout spelling parses without error and does nothing.
func sqliteDSN(dbPath string) string {
	return dbPath + "?mode=ro&_pragma=busy_timeout(5000)"
}

// readSQLite opens a kiro-cli SQLite database and extracts credentials using real SQL.
func readSQLite(dbPath, region string) (*KiroCliCredentials, error) {
	db, err := sql.Open("sqlite", sqliteDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	// One connection: every read here is sequential, and a single sqlite handle
	// avoids concurrent readers piling up on a locked file.
	db.SetMaxOpenConns(1)
	// Recycles idle connections; this is NOT a query timeout — busy_timeout above
	// is what bounds a blocked read.
	db.SetConnMaxLifetime(5 * time.Second)

	creds := &KiroCliCredentials{}

	tokenKeys := []string{"kirocli:odic:token", "kirocli:oidc:token", "kiro:auth:token"}
	regKeys := []string{"kirocli:odic:device-registration", "kirocli:oidc:device-registration"}
	profileKey := "api.codewhisperer.profile"
	knownTables := []string{"auth_kv", "ItemTable", "storage", "state"}

	// Step 1: discover which tables actually exist in this DB. A partial list
	// would silently turn "table not read" into "key not present".
	available, err := discoverTables(db)
	if err != nil {
		return nil, fmt.Errorf("discover tables: %w", err)
	}

	// Step 2: read tokens (refresh_token + access_token + region).
	var tokenData map[string]interface{}
	for _, key := range tokenKeys {
		for _, table := range knownTables {
			if !available[table] {
				continue
			}
			val, err := readJSONValue(db, table, key)
			if err != nil || val == nil {
				continue
			}
			if rt, _ := val["refresh_token"].(string); rt != "" {
				tokenData = val
				break
			}
		}
		if tokenData != nil {
			break
		}
	}
	if tokenData == nil {
		return nil, fmt.Errorf("no refresh token found in database")
	}

	creds.RefreshToken, _ = tokenData["refresh_token"].(string)
	creds.AccessToken, _ = tokenData["access_token"].(string)
	if r, ok := tokenData["region"].(string); ok && r != "" {
		creds.Region = r
	}
	// Extract expires_at from token data (matches OmniRoute behavior).
	if ea, ok := tokenData["expires_at"].(string); ok && ea != "" {
		parsed, err := time.Parse(time.RFC3339, ea)
		if err == nil {
			creds.ExpiresAt = parsed
		}
	}
	if creds.ExpiresAt.IsZero() {
		if ea, ok := tokenData["expiresAt"].(string); ok && ea != "" {
			parsed, err := time.Parse(time.RFC3339, ea)
			if err == nil {
				creds.ExpiresAt = parsed
			}
		}
	}
	if creds.ExpiresAt.IsZero() {
		creds.ExpiresAt = time.Now().Add(1 * time.Hour)
	}

	// Step 3: read client registration.
	for _, key := range regKeys {
		for _, table := range knownTables {
			if !available[table] {
				continue
			}
			val, err := readJSONValue(db, table, key)
			if err != nil || val == nil {
				continue
			}
			if cid, _ := val["client_id"].(string); cid != "" {
				creds.ClientID = cid
				creds.ClientSecret, _ = val["client_secret"].(string)
				break
			}
		}
		if creds.ClientID != "" {
			break
		}
	}

	// Step 4: read profileArn (enterprise SSO / IDC).
	for _, table := range knownTables {
		if !available[table] {
			continue
		}
		val, err := readJSONValue(db, table, profileKey)
		if err != nil || val == nil {
			continue
		}
		if arn, _ := val["arn"].(string); arn != "" {
			creds.ProfileArn = arn
			break
		}
		if arn, _ := val["profileArn"].(string); arn != "" {
			creds.ProfileArn = arn
			break
		}
	}

	// Step 5: resolve region — caller param wins, then DB value, then default.
	if region == "" {
		region = creds.Region
	}
	if region == "" {
		region = "us-east-1"
	}
	creds.Region = region

	if creds.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh token found in database")
	}
	return creds, nil
}

// ImportSSOCache scans ~/.aws/sso/cache/ for Kiro/AWS SSO token files.
func ImportSSOCache() (*SSOCacheCredentials, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}
	cacheDir := filepath.Join(home, ".aws", "sso", "cache")

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("read sso cache: %w", err)
	}

	preferred := map[string]bool{
		"kiro-auth-token.json":     true,
		"amazon-q-auth-token.json": true,
	}

	var best string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if _, ok := preferred[entry.Name()]; ok {
			best = filepath.Join(cacheDir, entry.Name())
			break
		}
		if best == "" {
			best = filepath.Join(cacheDir, entry.Name())
		}
	}
	if best == "" {
		return nil, fmt.Errorf("no token files found in %s", cacheDir)
	}

	data, err := os.ReadFile(best)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", best, err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", best, err)
	}

	rt, _ := raw["refreshToken"].(string)
	if rt == "" || !strings.HasPrefix(rt, "aorAAAAAG") {
		return nil, fmt.Errorf("no valid refresh token in %s", best)
	}

	return &SSOCacheCredentials{
		RefreshToken: rt,
		Source:       best,
	}, nil
}

// discoverTables returns the set of table names present in the database.
func discoverTables(db *sql.DB) (map[string]bool, error) {
	result := make(map[string]bool)
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		result[name] = true
	}
	return result, rows.Err()
}

// readJSONValue reads a JSON value for a given key from the specified table.
// The table must have a `value` column (the SQLite schema stores JSON text in a value column).
func readJSONValue(db *sql.DB, table, key string) (map[string]interface{}, error) {
	query := fmt.Sprintf("SELECT value FROM %q WHERE key = ?", table)
	row := db.QueryRow(query, key)
	var raw string
	if err := row.Scan(&raw); err != nil {
		return nil, err
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, err
	}
	return data, nil
}

// DeriveKiroConnectionName returns a human-readable label for a Kiro/AWS connection.
// Falls back through: email → profileArn label → provider+region label.
func DeriveKiroConnectionName(email, profileArn, region, targetProvider string) string {
	if email != "" {
		return email
	}
	if region == "" {
		region = "us-east-1"
	}
	if profileArn != "" {
		return "AWS CodeWhisperer (" + region + ")"
	}
	if targetProvider == "amazon-q" {
		return "Amazon Q (" + region + ")"
	}
	return "Kiro (" + region + ")"
}
