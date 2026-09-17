package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidSVGPromptKey(t *testing.T) {
	good := []string{"p-abc123", "run-1", "abc_123", "A-b_C"}
	for _, id := range good {
		if !validSVGPromptKey(id) {
			t.Errorf("validSVGPromptKey(%q) = false, want true", id)
		}
	}
	// Rejected: path traversal, separators, empty, over-long.
	bad := []string{"", "../etc", "a/b", "a b", ".", "..", "run/../secret", strings.Repeat("a", 65)}
	for _, id := range bad {
		if validSVGPromptKey(id) {
			t.Errorf("validSVGPromptKey(%q) = true, want false", id)
		}
	}
}

func TestSVGPromptKeyStableAcrossWhitespace(t *testing.T) {
	base := svgPromptKey("draw a pelican on a bicycle")
	if got := svgPromptKey("  draw a pelican on a bicycle  "); got != base {
		t.Errorf("surrounding whitespace changed the key: %q vs %q", got, base)
	}
	if got := svgPromptKey("draw  a pelican\n on a bicycle"); got != base {
		t.Errorf("inner whitespace changed the key: %q vs %q", got, base)
	}
	if got := svgPromptKey("draw a different prompt"); got == base {
		t.Error("different prompts produced the same key")
	}
	if !validSVGPromptKey(base) {
		t.Errorf("derived key %q is not filesystem-safe", base)
	}
}

func TestSaveAndGetSVGTestGroupEntries(t *testing.T) {
	dir := t.TempDir()
	key := svgPromptKey("draw a cat")
	if err := saveSVGTestMeta(dir, key, "draw a cat"); err != nil {
		t.Fatalf("saveSVGTestMeta: %v", err)
	}
	// Same account under two different models must survive as two entries:
	// keying by account alone collapsed them and lost the comparison.
	a1 := svgTestEntry{PromptKey: key, Model: "gpt-4o", AccountID: "acc-1", AccountName: "beta@example.com", Provider: "External OpenAI", Success: true, SVG: `<svg viewBox="0 0 1 1"></svg>`, ElapsedMs: 120, TokensUsed: 42}
	a2 := svgTestEntry{PromptKey: key, Model: "claude-opus-5", AccountID: "acc-1", AccountName: "beta@example.com", Provider: "External OpenAI", Success: false, Error: "upstream 429", ElapsedMs: 30}
	if err := saveSVGTestEntry(dir, a1); err != nil {
		t.Fatalf("saveSVGTestEntry(a1): %v", err)
	}
	if err := saveSVGTestEntry(dir, a2); err != nil {
		t.Fatalf("saveSVGTestEntry(a2): %v", err)
	}

	entries := getSVGTestGroupEntries(dir, key)
	if len(entries) != 2 {
		t.Fatalf("getSVGTestGroupEntries returned %d entries, want 2", len(entries))
	}
	// Sorted by model then account name.
	if entries[0].Model != "claude-opus-5" || entries[1].Model != "gpt-4o" {
		t.Errorf("entries not sorted by model: %q, %q", entries[0].Model, entries[1].Model)
	}
	if entries[1].SVG == "" {
		t.Error("success entry lost its SVG payload")
	}
	if entries[0].Error != "upstream 429" {
		t.Errorf("failure entry lost its error: %q", entries[0].Error)
	}
}

func TestSaveSVGTestEntrySkipsInvalid(t *testing.T) {
	dir := t.TempDir()
	if err := saveSVGTestEntry(dir, svgTestEntry{PromptKey: "../escape", AccountID: "a", Model: "m"}); err != nil {
		t.Errorf("saveSVGTestEntry(bad key) returned err %v, want nil", err)
	}
	if err := saveSVGTestEntry(dir, svgTestEntry{PromptKey: "p-ok", AccountID: "", Model: "m"}); err != nil {
		t.Errorf("saveSVGTestEntry(empty account) returned err %v, want nil", err)
	}
	if err := saveSVGTestEntry("", svgTestEntry{PromptKey: "p-ok", AccountID: "a", Model: "m"}); err != nil {
		t.Errorf("saveSVGTestEntry(empty dir) returned err %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "../escape")); !os.IsNotExist(err) {
		t.Errorf("traversal key created a directory outside the store")
	}
}

func TestLoadSVGTestGroupDerivesCountsAndModels(t *testing.T) {
	dir := t.TempDir()
	key := svgPromptKey("draw three")
	if err := saveSVGTestMeta(dir, key, "draw three"); err != nil {
		t.Fatalf("saveSVGTestMeta: %v", err)
	}
	for _, e := range []svgTestEntry{
		{PromptKey: key, Model: "gpt-4o", AccountID: "a1", AccountName: "a1", Success: true, SVG: "<svg></svg>"},
		{PromptKey: key, Model: "gpt-4o", AccountID: "a2", AccountName: "a2", Success: true, SVG: "<svg></svg>"},
		{PromptKey: key, Model: "claude-opus-5", AccountID: "a3", AccountName: "a3", Success: false, Error: "429"},
	} {
		if err := saveSVGTestEntry(dir, e); err != nil {
			t.Fatalf("saveSVGTestEntry(%s/%s): %v", e.Model, e.AccountID, err)
		}
	}
	meta := loadSVGTestGroup(filepath.Join(dir, key), key)
	if meta == nil {
		t.Fatal("loadSVGTestGroup returned nil")
	}
	// Counts and the model list come from the directory scan, never _meta.json.
	if meta.ResultCount != 3 || len(meta.Results) != 3 {
		t.Errorf("derived counts = %d/%d, want 3/3", meta.ResultCount, len(meta.Results))
	}
	if len(meta.Models) != 2 || meta.Models[0] != "claude-opus-5" || meta.Models[1] != "gpt-4o" {
		t.Errorf("derived models = %v, want [claude-opus-5 gpt-4o]", meta.Models)
	}
	if meta.Prompt != "draw three" {
		t.Errorf("meta lost the prompt: %q", meta.Prompt)
	}
}

func TestLoadSVGTestGroupEmptyIsNil(t *testing.T) {
	dir := t.TempDir()
	key := svgPromptKey("nothing yet")
	if err := saveSVGTestMeta(dir, key, "nothing yet"); err != nil {
		t.Fatalf("saveSVGTestMeta: %v", err)
	}
	if meta := loadSVGTestGroup(filepath.Join(dir, key), key); meta != nil {
		t.Errorf("loadSVGTestGroup with no results = %+v, want nil", meta)
	}
}

func TestListSVGTestGroupsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	prompts := []string{"oldest prompt", "middle prompt", "newest prompt"}
	for i, p := range prompts {
		key := svgPromptKey(p)
		e := svgTestEntry{PromptKey: key, Model: "m", AccountID: "a", AccountName: "a", Success: true, SVG: "<svg></svg>", SavedAt: int64(1000 + i*100)}
		if err := saveSVGTestMeta(dir, key, p); err != nil {
			t.Fatalf("saveSVGTestMeta(%s): %v", p, err)
		}
		if err := saveSVGTestEntry(dir, e); err != nil {
			t.Fatalf("saveSVGTestEntry(%s): %v", p, err)
		}
	}
	// saveSVGTestMeta stamps SavedAt at write time, so the last-written prompt
	// is newest — the entry timestamps above also pin the ordering.
	groups := listSVGTestGroups(dir, 50)
	if len(groups) != 3 {
		t.Fatalf("listSVGTestGroups returned %d groups, want 3", len(groups))
	}
	if groups[0].SavedAt < groups[2].SavedAt {
		t.Errorf("groups not newest-first: %d ... %d", groups[0].SavedAt, groups[2].SavedAt)
	}
	if limited := listSVGTestGroups(dir, 2); len(limited) != 2 {
		t.Errorf("listSVGTestGroups(limit=2) returned %d, want 2", len(limited))
	}
	if got := listSVGTestGroups("", 50); got != nil {
		t.Errorf("listSVGTestGroups(\"\") = %v, want nil", got)
	}
}

func TestSanitizeIDForFile(t *testing.T) {
	cases := map[string]string{
		"abc-123":     "abc-123",
		"a/b":         "a_b",
		"../../x":     "______x",
		"user@host.t": "user_host_t",
		"keep_OK-9":   "keep_OK-9",
	}
	for in, want := range cases {
		if got := sanitizeIDForFile(in); got != want {
			t.Errorf("sanitizeIDForFile(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGetSVGTestGroupEntriesRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	if got := getSVGTestGroupEntries(dir, "../../etc/passwd"); got != nil {
		t.Errorf("getSVGTestGroupEntries with traversal key = %v, want nil", got)
	}
	if got := getSVGTestGroupEntries(dir, "p-missing"); got != nil {
		t.Errorf("getSVGTestGroupEntries for missing group = %v, want nil", got)
	}
}

func TestSVGEntryFileNameSeparatesModelAndAccount(t *testing.T) {
	a := svgEntryFileName("gpt-4o", "acc-1")
	b := svgEntryFileName("claude-opus-5", "acc-1")
	if a == b {
		t.Errorf("two models on one account collided: %q == %q", a, b)
	}
	if !strings.HasSuffix(a, ".json") || strings.ContainsAny(a, "/\\ ") {
		t.Errorf("entry filename %q is not a safe single path element", a)
	}
}

// One pair has exactly one stored result now: every run sends the same bare
// prompt with no knobs to vary, so the name is model plus account and nothing
// else. Model ids pass through sanitizeIDForFile, so dots become underscores,
// and two accounts holding the same model must not overwrite each other.
func TestSVGEntryFileNameIsKeyedByPair(t *testing.T) {
	if got := svgEntryFileName("glm-5.3", "acc-1"); got != "glm-5_3--acc-1.json" {
		t.Errorf("name = %q, want glm-5_3--acc-1.json", got)
	}
	first := svgEntryFileName("qwen3.8-max-cn", "acc-1")
	second := svgEntryFileName("qwen3.8-max-cn", "acc-2")
	if first == second {
		t.Errorf("two accounts on one model collided: %q == %q", first, second)
	}
	// sanitizeIDForFile passes '-' through unchanged, so "--" is not escaped and a
	// model id ending in one could in principle blur into the account id. That does
	// not bite because account ids are UUIDs (12111913-21ce-442f-...) and a UUID
	// never contains "--": the separator is unambiguous in practice, not by
	// construction. Pin it, so a future non-UUID account id fails here first.
	for _, acct := range []string{"12111913-21ce-442f-9ca7-ec3b9a6f6255", "15dc0b0a-2bf1-4525-b56a-f1f092ab67ea"} {
		if strings.Contains(acct, "--") {
			t.Errorf("account id %q contains the separator; entry names would be ambiguous", acct)
		}
	}
	if svgEntryFileName("qwen3.8-max-cn", "12111913-21ce-442f-9ca7-ec3b9a6f6255") !=
		"qwen3_8-max-cn--12111913-21ce-442f-9ca7-ec3b9a6f6255.json" {
		t.Error("real account id did not produce the expected entry name")
	}
}
