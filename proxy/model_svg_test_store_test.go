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
	a := svgEntryFileName("gpt-4o", "acc-1", "", "")
	b := svgEntryFileName("claude-opus-5", "acc-1", "", "")
	if a == b {
		t.Errorf("two models on one account collided: %q == %q", a, b)
	}
	if !strings.HasSuffix(a, ".json") || strings.ContainsAny(a, "/\\ ") {
		t.Errorf("entry filename %q is not a safe single path element", a)
	}
}

// An empty mode must keep the legacy two-part name so results written before
// modes existed still resolve; a non-empty mode appends a third segment. Model
// ids pass through sanitizeIDForFile, so dots become underscores.
func TestSVGEntryFileNameModeSuffix(t *testing.T) {
	legacy := svgEntryFileName("glm-5.3", "acc-1", "", "")
	raw := svgEntryFileName("glm-5.3", "acc-1", "raw", "")
	think := svgEntryFileName("glm-5.3", "acc-1", "think", "")
	if legacy != "glm-5_3--acc-1.json" {
		t.Errorf("legacy name = %q, want glm-5_3--acc-1.json", legacy)
	}
	if raw != "glm-5_3--acc-1--raw.json" {
		t.Errorf("raw name = %q, want glm-5_3--acc-1--raw.json", raw)
	}
	// The two modes of one pair must never collide, or one run overwrites the
	// other and the raw-vs-think comparison the feature exists for is lost.
	if raw == think || raw == legacy || think == legacy {
		t.Errorf("mode names collided: legacy=%q raw=%q think=%q", legacy, raw, think)
	}
}

// A sweep runs one pair at several reasoning efforts, and every rung must land in
// its own file. If the effort were dropped from the name the last rung would
// overwrite the rest and the curve would collapse to a single point — exactly
// the comparison the sweep exists to produce.
func TestSVGEntryFileNameEffortSuffix(t *testing.T) {
	base := svgEntryFileName("qwen3.8-max-cn", "acc-1", "think", "")
	low := svgEntryFileName("qwen3.8-max-cn", "acc-1", "think", "low")
	medium := svgEntryFileName("qwen3.8-max-cn", "acc-1", "think", "medium")
	high := svgEntryFileName("qwen3.8-max-cn", "acc-1", "think", "high")

	if base != "qwen3_8-max-cn--acc-1--think.json" {
		t.Errorf("pre-sweep think name = %q, want qwen3_8-max-cn--acc-1--think.json", base)
	}
	if low != "qwen3_8-max-cn--acc-1--think--low.json" {
		t.Errorf("low rung name = %q, want qwen3_8-max-cn--acc-1--think--low.json", low)
	}
	seen := map[string]string{base: "base", low: "low", medium: "medium", high: "high"}
	if len(seen) != 4 {
		t.Errorf("sweep rungs collided: %v", seen)
	}
	// Effort only has meaning in think mode — raw forces reasoning off, so an
	// effort there would imply a lever the run did not pull. It is dropped from
	// the name to keep raw results comparable across sweeps.
	if got := svgEntryFileName("qwen3.8-max-cn", "acc-1", "raw", "high"); got != "qwen3_8-max-cn--acc-1--raw.json" {
		t.Errorf("raw with effort = %q, want the plain raw name qwen3_8-max-cn--acc-1--raw.json", got)
	}
	// An unrecognised effort must not invent a segment: the plain think file and
	// the one for a bogus effort have to be the same file, or a typo silently
	// forks a group the UI cannot address.
	if got := svgEntryFileName("qwen3.8-max-cn", "acc-1", "think", "extreme"); got != base {
		t.Errorf("unknown effort = %q, want the pre-sweep name %q", got, base)
	}
}

func TestNormalizeSVGTestMode(t *testing.T) {
	cases := map[string]string{
		"raw": "raw", "RAW": "raw", "  raw ": "raw",
		"think": "think", "Think": "think",
		"": "", "both": "", "bogus": "", "rawx": "",
	}
	for in, want := range cases {
		if got := normalizeSVGTestMode(in); got != want {
			t.Errorf("normalizeSVGTestMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveSVGTestMode(t *testing.T) {
	// A bare API call has no UI to expand "both" into two requests, so empty,
	// "both" and anything unknown all collapse to the raw single-call baseline.
	cases := map[string]string{
		"think": "think", "Think": "think",
		"raw": "raw", "": "raw", "both": "raw", "bogus": "raw",
	}
	for in, want := range cases {
		if got := resolveSVGTestMode(in); got != want {
			t.Errorf("resolveSVGTestMode(%q) = %q, want %q", in, got, want)
		}
	}
}
