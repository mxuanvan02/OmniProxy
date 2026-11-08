package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMergeCodexConfigPreservesCustomProviderKeys verifies that re-applying
// OmniProxy to an existing config keeps operator-tuned keys inside
// [model_providers.omniproxy] (env_key, retry counts, stream timeouts).
// Replacing them wipes the retry/timeout tuning, and Codex then gives up on
// slow first tokens from large models (e.g. claude-opus-5) instead of waiting
// — the symptom users report as "Codex lags after applying OmniProxy".
func TestMergeCodexConfigPreservesCustomProviderKeys(t *testing.T) {
	tmpDir := t.TempDir()
	codexDir := filepath.Join(tmpDir, ".codex")
	os.MkdirAll(codexDir, 0755)

	existingConfig := `model = "gpt-5.6-terra"
model_provider = "omniproxy"

[model_providers.omniproxy]
name = "OmniProxy"
base_url = "http://127.0.0.1:20131/v1"
wire_api = "responses"
env_key = "OMNIPROXY_API_KEY"
requires_openai_auth = true
request_max_retries = 2
stream_max_retries = 2
stream_idle_timeout_ms = 120000
`

	configPath := filepath.Join(codexDir, "config.toml")
	os.WriteFile(configPath, []byte(existingConfig), 0644)

	err := MergeCodexConfig(tmpDir, "claude-opus-5", "http://127.0.0.1:20131/v1", "claude-opus-5", "high")
	if err != nil {
		t.Fatalf("MergeCodexConfig failed: %v", err)
	}

	data, _ := os.ReadFile(configPath)
	content := string(data)

	for _, want := range []string{
		`env_key = "OMNIPROXY_API_KEY"`,
		`request_max_retries = 2`,
		`stream_max_retries = 2`,
		`stream_idle_timeout_ms = 120000`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("custom key lost after re-apply: %s\n--- merged config ---\n%s", want, content)
		}
	}

	// Managed keys must still point at OmniProxy with the new model.
	if !strings.Contains(content, `model = "claude-opus-5"`) {
		t.Error("new model not applied")
	}
	if !strings.Contains(content, `base_url = "http://127.0.0.1:20131/v1"`) {
		t.Error("base_url not applied")
	}
}
