package proxy

import (
	"encoding/json"
	"omniproxy/config"
	"path/filepath"
	"testing"
)

// The OpenAI-protocol path used to drop reasoning_effort silently: OpenAIRequest
// had no field for it, so external OpenAI-compatible upstreams (VSLLM/Qwen) always
// ran with their server-default thinking budget. Simple chats paid full reasoning.
//
// This test was authored on a non-git host tree and is merged here so the fix
// lives under version control with the passthrough work that carries the same
// parameter by a different mechanism (typed field + whitelist, rather than the
// generic verbatim Extra forward).
func TestNormalizeReasoningEffort(t *testing.T) {
	cases := map[string]string{
		"low":      "low",
		"HIGH":     "high",
		" medium ": "medium",
		"none":     "none",
		"minimal":  "minimal",
		"xhigh":    "xhigh",
		"max":      "max",
		"":         "",
		"ultra":    "",
		"bogus":    "",
	}
	for in, want := range cases {
		if got := normalizeReasoningEffort(in); got != want {
			t.Errorf("normalizeReasoningEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenAIToKiroForwardsReasoningEffort(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	req := &OpenAIRequest{
		Model:           "qwen3.8-max-0902",
		Messages:        []OpenAIMessage{{Role: "user", Content: "hello"}},
		MaxTokens:       100,
		ReasoningEffort: "low",
	}
	payload := OpenAIToKiro(req, false)
	if payload.InferenceConfig == nil {
		t.Fatal("InferenceConfig not built")
	}
	if payload.InferenceConfig.ReasoningEffort != "low" {
		t.Errorf("ReasoningEffort = %q, want low", payload.InferenceConfig.ReasoningEffort)
	}
}

// An effort-only request (no max_tokens/temperature/top_p) must still build the
// InferenceConfig so the level reaches the upstream.
func TestOpenAIToKiroEffortAloneBuildsConfig(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	req := &OpenAIRequest{
		Model:           "qwen3.8-max-0902",
		Messages:        []OpenAIMessage{{Role: "user", Content: "hello"}},
		ReasoningEffort: "none",
	}
	payload := OpenAIToKiro(req, false)
	if payload.InferenceConfig == nil {
		t.Fatal("InferenceConfig not built for effort-only request")
	}
	if payload.InferenceConfig.ReasoningEffort != "none" {
		t.Errorf("ReasoningEffort = %q, want none", payload.InferenceConfig.ReasoningEffort)
	}
}

// Unknown levels must be dropped, not forwarded: an upstream gateway may 400 on
// a vocabulary it does not know.
func TestOpenAIToKiroDropsUnknownEffort(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	req := &OpenAIRequest{
		Model:           "qwen3.8-max-0902",
		Messages:        []OpenAIMessage{{Role: "user", Content: "hello"}},
		MaxTokens:       50,
		ReasoningEffort: "ultra",
	}
	payload := OpenAIToKiro(req, false)
	if payload.InferenceConfig == nil {
		t.Fatal("InferenceConfig not built")
	}
	if payload.InferenceConfig.ReasoningEffort != "" {
		t.Errorf("unknown effort forwarded: %q", payload.InferenceConfig.ReasoningEffort)
	}
}

// The merge invariant: reasoning_effort must travel the typed/whitelisted path
// exactly once, never ALSO through the generic Extra passthrough. If it rode both
// ways an unknown level ("ultra") would be dropped by the whitelist yet still
// leak upstream verbatim via Extra, defeating the whole point of validating it.
func TestReasoningEffortNotDoubleForwardedViaExtra(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
	  "max_tokens":16,"reasoning_effort":"ultra"}`)
	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Declared, so it must NOT be in Extra.
	if _, leaked := req.Extra["reasoning_effort"]; leaked {
		t.Fatal("reasoning_effort captured into Extra; it is a declared typed field " +
			"and must not ride the verbatim passthrough too")
	}
	// And the whitelist dropped the bogus level.
	payload := OpenAIToKiro(&req, false)
	if payload.InferenceConfig != nil && payload.InferenceConfig.ReasoningEffort != "" {
		t.Errorf("unknown effort survived: %q", payload.InferenceConfig.ReasoningEffort)
	}
}

// A valid effort must reach the outbound upstream body, proving the typed path
// actually forwards (not just stores on the IR).
func TestReasoningEffortReachesUpstreamBody(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
	  "max_tokens":16,"reasoning_effort":"high"}`)
	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	payload := OpenAIToKiro(&req, false)
	payload.OriginalModel = "m"
	account := &config.Account{ID: "a", Email: "a@e.com", AuthMethod: externalAuthMethod,
		AccessToken: "k", BaseURL: "https://x/"}
	upstream, err := kiroPayloadToOpenAIRequest(payload, account)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := upstream["reasoning_effort"]; got != "high" {
		t.Errorf("upstream reasoning_effort = %v, want high", got)
	}
}
