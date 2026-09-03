package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cliConfigHome redirects HOME to a temp dir for the duration of the test. Every
// helper under test resolves its paths through os.UserHomeDir(), so without this
// they would read — and backupToolConfig would write into — the operator's real
// tool configuration.
func cliConfigHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// cliConfigWrite creates a file (and its parents) under the redirected HOME.
func cliConfigWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestGetToolConfigPathsResolvesUnderHomeAndRejectsUnknownTool(t *testing.T) {
	const home = "/tmp/fake-home"
	cases := []struct {
		toolID string
		want   []string
	}{
		{"claude", []string{".claude/settings.json"}},
		{"opencode", []string{".config/opencode/opencode.json"}},
		{"codex", []string{".codex/config.toml", ".codex/auth.json"}},
		{"cline", []string{".cline/data/globalState.json", ".cline/data/secrets.json"}},
		{"hermes", []string{".hermes/config.yaml", ".hermes/.env"}},
		{"droid", []string{".factory/settings.json"}},
		{"openclaw", []string{".openclaw/openclaw.json"}},
	}
	for _, tc := range cases {
		got := getToolConfigPaths(home, tc.toolID)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: %d paths, want %d (%v)", tc.toolID, len(got), len(tc.want), got)
		}
		for i, suffix := range tc.want {
			if want := filepath.Join(home, filepath.FromSlash(suffix)); got[i] != want {
				t.Errorf("%s path %d = %q, want %q", tc.toolID, i, got[i], want)
			}
		}
	}

	// kilo and kilocode are aliases for one config file.
	if a, b := getToolConfigPaths(home, "kilo"), getToolConfigPaths(home, "kilocode"); len(a) != 1 || len(b) != 1 || a[0] != b[0] {
		t.Errorf("kilo/kilocode paths differ: %v vs %v", a, b)
	}
	// An unknown tool must return nothing rather than a path under HOME that a
	// caller would then read or overwrite.
	if got := getToolConfigPaths(home, "not-a-tool"); got != nil {
		t.Errorf("unknown tool paths = %v, want nil", got)
	}
}

// The backup is the operator's only way back to their own configuration, so it
// must exist, hold the original bytes, and leave the original untouched.
func TestBackupToolConfigCopiesOriginalWithoutModifyingIt(t *testing.T) {
	home := cliConfigHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	const original = `{"env":{"ANTHROPIC_BASE_URL":"http://operators-own-proxy"}}`
	cliConfigWrite(t, settings, original)

	backup := backupToolConfig("claude")
	if backup == "" {
		t.Fatal("backupToolConfig returned no path for an existing config")
	}
	if !strings.HasPrefix(backup, settings+".omniproxy.bak.") {
		t.Errorf("backup path = %q, want a sibling of the config", backup)
	}

	saved, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(saved) != original {
		t.Errorf("backup content = %q, want the original bytes", saved)
	}
	if current, err := os.ReadFile(settings); err != nil || string(current) != original {
		t.Errorf("original was modified: %q (err %v)", current, err)
	}
}

// A backup holds whatever the operator had configured, including credentials, so
// it must not be world- or group-readable.
func TestBackupToolConfigWritesOwnerOnlyPermissions(t *testing.T) {
	home := cliConfigHome(t)
	cliConfigWrite(t, filepath.Join(home, ".claude", "settings.json"), `{"env":{}}`)

	backup := backupToolConfig("claude")
	if backup == "" {
		t.Fatal("backupToolConfig returned no path")
	}
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup mode = %04o, want 0600", perm)
	}
}

// Backing up a config OmniProxy already owns would overwrite the last real
// backup with our own output, losing the operator's original for good.
func TestBackupToolConfigSkipsConfigAlreadyPointingAtOmniProxy(t *testing.T) {
	home := cliConfigHome(t)
	dir := filepath.Join(home, ".codex")
	cliConfigWrite(t, filepath.Join(dir, "config.toml"), "model_provider = \"omniproxy\"\n")

	if backup := backupToolConfig("codex"); backup != "" {
		t.Fatalf("backed up an OmniProxy-owned config: %q", backup)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".omniproxy.bak.") {
			t.Errorf("unexpected backup file %q", entry.Name())
		}
	}
}

func TestBackupToolConfigReturnsEmptyForMissingFileAndUnknownTool(t *testing.T) {
	cliConfigHome(t)
	if backup := backupToolConfig("claude"); backup != "" {
		t.Errorf("backup of a missing config = %q, want empty", backup)
	}
	if backup := backupToolConfig("not-a-tool"); backup != "" {
		t.Errorf("backup of an unknown tool = %q, want empty", backup)
	}
}

// A multi-file tool backs up every readable config but reports only the first
// path; both files must actually be written.
// checkToolHasOmniProxy is what the UI shows as "connected". A false positive
// tells the operator a tool is routed through the proxy when it is not.
func TestCheckToolHasOmniProxyDetectsConnectedConfigs(t *testing.T) {
	cases := []struct {
		name    string
		toolID  string
		relPath string
		content string
		want    bool
	}{
		{"claude base url set", "claude", ".claude/settings.json",
			`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8080"}}`, true},
		{"claude base url empty", "claude", ".claude/settings.json",
			`{"env":{"ANTHROPIC_BASE_URL":""}}`, false},
		{"claude no env block", "claude", ".claude/settings.json", `{}`, false},
		{"claude malformed json", "claude", ".claude/settings.json", `{not json`, false},
		{"codex active provider", "codex", ".codex/config.toml",
			"model_provider = \"omniproxy\"\n", true},
		{"codex legacy provider name", "codex", ".codex/config.toml",
			"model_provider = \"superkiro\"\n", true},
		{"codex other provider", "codex", ".codex/config.toml",
			"model_provider = \"openai\"\n", false},
		{"cline base url set", "cline", ".cline/data/globalState.json",
			`{"openAiBaseUrl":"http://127.0.0.1:8080/v1"}`, true},
		{"kilo nested base url", "kilo", ".local/share/kilo/auth.json",
			`{"openai-compatible":{"baseUrl":"http://127.0.0.1:8080/v1"}}`, true},
		{"kilo missing nested entry", "kilo", ".local/share/kilo/auth.json", `{}`, false},
		{"hermes provider block", "hermes", ".hermes/config.yaml",
			"providers:\n  omniproxy:\n    baseUrl: http://127.0.0.1:8080\n", true},
		{"openclaw quoted provider", "openclaw", ".openclaw/openclaw.json",
			`{"providers":{"omniproxy":{}}}`, true},
		{"copilot titled entry", "copilot", ".config/Code/User/chatLanguageModels.json",
			`[{"title":"OmniProxy"}]`, true},
		{"copilot unrelated entry", "copilot", ".config/Code/User/chatLanguageModels.json",
			`[{"title":"Some Other Provider"}]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := cliConfigHome(t)
			cliConfigWrite(t, filepath.Join(home, filepath.FromSlash(tc.relPath)), tc.content)
			if got := checkToolHasOmniProxy(tc.toolID); got != tc.want {
				t.Fatalf("checkToolHasOmniProxy(%q) = %v, want %v", tc.toolID, got, tc.want)
			}
		})
	}
}

func TestBackupToolConfigBacksUpEveryConfiguredFile(t *testing.T) {
	home := cliConfigHome(t)
	dir := filepath.Join(home, ".hermes")
	cliConfigWrite(t, filepath.Join(dir, "config.yaml"), "providers:\n  other: {}\n")
	cliConfigWrite(t, filepath.Join(dir, ".env"), "OTHER_KEY=value\n")

	if backup := backupToolConfig("hermes"); backup == "" {
		t.Fatal("backupToolConfig returned no path")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	backups := 0
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".omniproxy.bak.") {
			backups++
		}
	}
	if backups != 2 {
		t.Errorf("%d backups written, want 2", backups)
	}
}

// TestReadClaudeCliSettingsReportsPersistedEffort covers the round trip: the UI
// re-renders from this response, so an effort already on disk has to come back
// or the next Apply silently downgrades it to the form default.
func TestReadClaudeCliSettingsReportsPersistedEffort(t *testing.T) {
	home := cliConfigHome(t)
	cliConfigWrite(t, filepath.Join(home, ".claude", "settings.json"),
		`{"model":"model-A","effortLevel":"max","env":{"ANTHROPIC_BASE_URL":"http://proxy","ANTHROPIC_API_KEY":"k"}}`)

	settings := readCliToolSettingsFromFile("claude")
	if settings == nil {
		t.Fatal("Claude settings were not read back")
	}
	if settings.Model != "model-A" {
		t.Fatalf("model = %q, want model-A", settings.Model)
	}
	if settings.ReasoningEffort != "max" {
		t.Fatalf("reasoningEffort = %q, want max", settings.ReasoningEffort)
	}
}
