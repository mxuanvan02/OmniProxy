package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Test parseTomlLine functionality
func TestParseTomlLine(t *testing.T) {
	tests := []struct {
		name           string
		line           string
		section        string
		expectedType   LineType
		expectedKey    string
		expectedValue  string
		expectedActive bool
	}{
		{
			name:           "blank line",
			line:           "",
			section:        "",
			expectedType:   LineBlank,
			expectedActive: false,
		},
		{
			name:           "comment line",
			line:           "# This is a comment",
			section:        "",
			expectedType:   LineComment,
			expectedActive: false,
		},
		{
			name:           "commented key-value",
			line:           "#model = \"gpt-4o\"",
			section:        "",
			expectedType:   LineComment,
			expectedKey:    "model",
			expectedValue:  "gpt-4o",
			expectedActive: false,
		},
		{
			name:           "commented key-value with inline comment",
			line:           "# model = \"gpt-4o\" # old model",
			section:        "",
			expectedType:   LineComment,
			expectedKey:    "model",
			expectedValue:  "gpt-4o",
			expectedActive: false,
		},
		{
			name:           "section header",
			line:           "[model_providers.superkiro]",
			section:        "",
			expectedType:   LineSection,
			expectedKey:    "model_providers.superkiro",
			expectedActive: true,
		},
		{
			name:           "key-value pair",
			line:           `model = "claude-sonnet-4.5"`,
			section:        "",
			expectedType:   LineKeyValue,
			expectedKey:    "model",
			expectedValue:  "claude-sonnet-4.5",
			expectedActive: true,
		},
		{
			name:           "key-value with inline comment",
			line:           `base_url = "http://localhost:8080/v1"  # my server`,
			section:        "",
			expectedType:   LineKeyValue,
			expectedKey:    "base_url",
			expectedValue:  "http://localhost:8080/v1",
			expectedActive: true,
		},
		{
			name:           "hash inside basic string",
			line:           `model = "vendor/model#tag" # pinned`,
			section:        "",
			expectedType:   LineKeyValue,
			expectedKey:    "model",
			expectedValue:  "vendor/model#tag",
			expectedActive: true,
		},
		{
			name:           "hash inside escaped basic string",
			line:           `model = "vendor/\"model#tag\"" # pinned`,
			section:        "",
			expectedType:   LineKeyValue,
			expectedKey:    "model",
			expectedValue:  `vendor/\"model#tag\"`,
			expectedActive: true,
		},
		{
			name:           "hash inside literal string",
			line:           `model = 'vendor/model#tag' # pinned`,
			section:        "",
			expectedType:   LineKeyValue,
			expectedKey:    "model",
			expectedValue:  "vendor/model#tag",
			expectedActive: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseTomlLine(tt.line, tt.section)

			if result.Type != tt.expectedType {
				t.Errorf("Type mismatch: got %v, want %v", result.Type, tt.expectedType)
			}
			if result.Key != tt.expectedKey {
				t.Errorf("Key mismatch: got %q, want %q", result.Key, tt.expectedKey)
			}
			if result.Value != tt.expectedValue {
				t.Errorf("Value mismatch: got %q, want %q", result.Value, tt.expectedValue)
			}
			if result.IsActive != tt.expectedActive {
				t.Errorf("IsActive mismatch: got %v, want %v", result.IsActive, tt.expectedActive)
			}
		})
	}
}

// Test scanConfig functionality
func TestScanConfig(t *testing.T) {
	config := `model = "gpt-4o"
model_provider = "openai"

[model_providers.openai]
base_url = "https://api.openai.com/v1"

[agents.subagent]
model = "claude-sonnet-4"`

	lines := strings.Split(config, "\n")
	state := scanConfig(lines)

	if state.ActiveModel != "gpt-4o" {
		t.Errorf("ActiveModel: got %q, want %q", state.ActiveModel, "gpt-4o")
	}
	if state.ActiveProvider != "openai" {
		t.Errorf("ActiveProvider: got %q, want %q", state.ActiveProvider, "openai")
	}
	if state.SubagentModel != "claude-sonnet-4" {
		t.Errorf("SubagentModel: got %q, want %q", state.SubagentModel, "claude-sonnet-4")
	}
}

// Test MergeCodexConfig with empty config
func TestMergeCodexConfigEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	codexDir := filepath.Join(tmpDir, ".codex")
	os.MkdirAll(codexDir, 0755)

	err := MergeCodexConfig(tmpDir, "claude-sonnet-4.5", "http://localhost:8080/v1", "oc/big-pickle")
	if err != nil {
		t.Fatalf("MergeCodexConfig failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(codexDir, "config.toml"))
	if err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}

	content := string(data)

	// Check required sections exist
	if !strings.Contains(content, `model = "claude-sonnet-4.5"`) {
		t.Error("Missing model setting")
	}
	if !strings.Contains(content, `model_provider = "superkiro"`) {
		t.Error("Missing model_provider setting")
	}
	if !strings.Contains(content, "[model_providers.superkiro]") {
		t.Error("Missing superkiro section")
	}
	if !strings.Contains(content, `base_url = "http://localhost:8080/v1"`) {
		t.Error("Missing base_url")
	}
	if !strings.Contains(content, "[agents.subagent]") {
		t.Error("Missing subagent section")
	}
	if !strings.Contains(content, `model = "oc/big-pickle"`) {
		t.Error("Missing subagent model")
	}
}

// Test MergeCodexConfig with existing config - different values
func TestMergeCodexConfigExistingDifferent(t *testing.T) {
	tmpDir := t.TempDir()
	codexDir := filepath.Join(tmpDir, ".codex")
	os.MkdirAll(codexDir, 0755)

	existingConfig := `model = "gpt-4o"
model_provider = "openai"
approvals_reviewer = "user"

[model_providers.openai]
base_url = "https://api.openai.com/v1"

[projects."/tmp/test"]
trust_level = "trusted"

[agents.subagent]
model = "gpt-4o-mini"
`

	configPath := filepath.Join(codexDir, "config.toml")
	os.WriteFile(configPath, []byte(existingConfig), 0644)

	err := MergeCodexConfig(tmpDir, "claude-sonnet-4.5", "http://localhost:8080/v1", "oc/big-pickle")
	if err != nil {
		t.Fatalf("MergeCodexConfig failed: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}

	content := string(data)

	// Old values should be commented out
	if !strings.Contains(content, `#model = "gpt-4o"`) {
		t.Error("Old model should be commented out")
	}
	if !strings.Contains(content, `#model_provider = "openai"`) {
		t.Error("Old provider should be commented out")
	}

	// New values should be present
	if !strings.Contains(content, `model = "claude-sonnet-4.5"`) {
		t.Error("New model missing")
	}
	if !strings.Contains(content, `model_provider = "superkiro"`) {
		t.Error("New provider missing")
	}

	// Other settings should be preserved
	if !strings.Contains(content, `approvals_reviewer = "user"`) {
		t.Error("approvals_reviewer should be preserved")
	}
	if !strings.Contains(content, `[projects."/tmp/test"]`) {
		t.Error("projects section should be preserved")
	}
	if !strings.Contains(content, `trust_level = "trusted"`) {
		t.Error("trust_level should be preserved")
	}

	// SuperKiro section should exist
	if !strings.Contains(content, "[model_providers.superkiro]") {
		t.Error("SuperKiro section missing")
	}

	// Old subagent should be commented, new one added
	if !strings.Contains(content, `model = "oc/big-pickle"`) {
		t.Error("New subagent model missing")
	}
}

// Test MergeCodexConfig idempotency - same values
func TestMergeCodexConfigIdempotent(t *testing.T) {
	tmpDir := t.TempDir()
	codexDir := filepath.Join(tmpDir, ".codex")
	os.MkdirAll(codexDir, 0755)

	existingConfig := `model = "claude-sonnet-4.5"
model_provider = "superkiro"

[model_providers.superkiro]
name = "SuperKiro"
base_url = "http://localhost:8080/v1"
wire_api = "responses"

[agents.subagent]
model = "oc/big-pickle"
`

	configPath := filepath.Join(codexDir, "config.toml")
	os.WriteFile(configPath, []byte(existingConfig), 0644)

	err := MergeCodexConfig(tmpDir, "claude-sonnet-4.5", "http://localhost:8080/v1", "oc/big-pickle")
	if err != nil {
		t.Fatalf("MergeCodexConfig failed: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}

	content := string(data)

	// No commented lines should be added
	commentCount := strings.Count(content, "#model =")
	if commentCount > 0 {
		t.Error("Should not comment out identical values")
	}
}

// Test MergeCodexConfig with complex existing config
func TestMergeCodexConfigComplex(t *testing.T) {
	tmpDir := t.TempDir()
	codexDir := filepath.Join(tmpDir, ".codex")
	os.MkdirAll(codexDir, 0755)

	existingConfig := `model = "FreeCombo"
model_provider = "9router"
approvals_reviewer = "user"

#1model = "gpt-4o"
#model_provider = "openai"

[model_providers.9router]
name = "9Router"
base_url = "http://127.0.0.1:20128/v1"
wire_api = "responses"

[model_providers.openai]
name = "OpenAI"
base_url = "https://api.openai.com/v1"

[agents.subagent]
model = "oc/big-pickle"

[projects."/mnt/e/test"]
trust_level = "trusted"

[hooks.state."/home/a/.codex/hooks.json:pre_tool_use:0:0"]
trusted_hash = "sha256:abc123"

[tui.model_availability_nux]
"gpt-5.5" = 4
`

	configPath := filepath.Join(codexDir, "config.toml")
	os.WriteFile(configPath, []byte(existingConfig), 0644)

	err := MergeCodexConfig(tmpDir, "claude-sonnet-4.5", "http://localhost:8080/v1", "oc/big-pickle")
	if err != nil {
		t.Fatalf("MergeCodexConfig failed: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("Failed to read config: %v", err)
	}

	content := string(data)

	// Check old comments are preserved
	if !strings.Contains(content, "#1model =") {
		t.Error("Existing comment should be preserved")
	}

	// Check active values are commented out
	if !strings.Contains(content, `#model = "FreeCombo"`) {
		t.Error("Old active model should be commented")
	}
	if !strings.Contains(content, `#model_provider = "9router"`) {
		t.Error("Old active provider should be commented")
	}

	// Check new values exist
	if !strings.Contains(content, `model = "claude-sonnet-4.5"`) {
		t.Error("New model missing")
	}
	if !strings.Contains(content, `model_provider = "superkiro"`) {
		t.Error("New provider missing")
	}

	// Check all other sections preserved
	if !strings.Contains(content, "[model_providers.9router]") {
		t.Error("9router section should be preserved")
	}
	if !strings.Contains(content, "[model_providers.openai]") {
		t.Error("OpenAI section should be preserved")
	}
	if !strings.Contains(content, "[projects.") {
		t.Error("Projects section should be preserved")
	}
	if !strings.Contains(content, "[hooks.state.") {
		t.Error("Hooks section should be preserved")
	}
	if !strings.Contains(content, "[tui.model_availability_nux]") {
		t.Error("TUI section should be preserved")
	}

	// SuperKiro section should exist
	if !strings.Contains(content, "[model_providers.superkiro]") {
		t.Error("SuperKiro section should be added")
	}
}
