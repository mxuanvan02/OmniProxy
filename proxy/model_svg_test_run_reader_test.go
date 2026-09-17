package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// readSVGTestEntry recomputes the score from the stored SVG on every read rather
// than trusting a persisted value. This is the property that lets results written
// before scoring existed get a grade with no migration, and it is the only reason
// the rubric can be improved later without the archive going stale. A stored
// Score field must therefore be ignored, not honoured.
func TestReadSVGTestEntryScoresOnRead(t *testing.T) {
	dir := t.TempDir()
	key := svgPromptKey("draw a pelican")
	groupDir := filepath.Join(dir, key)
	if err := os.MkdirAll(groupDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	good := svgScoreFixture(60, "cx", "opacity")
	wantScore, wantReasons := scoreSVG(good)

	// Write an entry by hand with a deliberately wrong stored score, to prove the
	// reader recomputes instead of echoing what is on disk.
	raw, err := json.Marshal(map[string]interface{}{
		"promptKey": key, "model": "qwen3.8-max-cn", "accountId": "acc-1",
		"success": true, "svg": good, "tokensUsed": 2000, "score": 13,
		"scoreReasons": []string{"stale persisted value"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(groupDir, "qwen3_8-max-cn--acc-1.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	entry := readSVGTestEntry(path)
	if entry == nil {
		t.Fatal("readSVGTestEntry returned nil for a valid entry")
	}
	if entry.Score != wantScore {
		t.Errorf("score = %d, want the recomputed %d (stored 13 must be ignored)", entry.Score, wantScore)
	}
	if len(entry.ScoreReasons) != len(wantReasons) {
		t.Errorf("scoreReasons = %v, want the recomputed %v (stale reasons must be dropped)", entry.ScoreReasons, wantReasons)
	}
	if eff, ok := svgTestEfficiency(entry.Score, entry.TokensUsed); !ok || eff <= 0 {
		t.Errorf("efficiency = (%v, %v), want a positive computable value", eff, ok)
	}
}

// A failed run stores no SVG, so it must score zero with no reasons rather than
// an empty-document grade: a failure is not a low-quality drawing, it is no
// drawing at all, and the efficiency column shows "n/a" for it.
func TestReadSVGTestEntryScoresFailureAsZero(t *testing.T) {
	dir := t.TempDir()
	key := svgPromptKey("draw a pelican")
	groupDir := filepath.Join(dir, key)
	if err := os.MkdirAll(groupDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	raw := `{"promptKey":"` + key + `","model":"qwen3.8-max","accountId":"acc-1","success":false,"error":"HTTP 400 from host: Bad Request","tokensUsed":0}`
	path := filepath.Join(groupDir, "qwen3_8-max--acc-1.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	entry := readSVGTestEntry(path)
	if entry == nil {
		t.Fatal("readSVGTestEntry returned nil for a failure entry")
	}
	if entry.Score != 0 || len(entry.ScoreReasons) != 0 {
		t.Errorf("failure scored %d with reasons %v, want 0 and none", entry.Score, entry.ScoreReasons)
	}
	if _, ok := svgTestEfficiency(entry.Score, entry.TokensUsed); ok {
		t.Error("a failure with no tokens must report efficiency as unavailable")
	}
}

// readSVGTestEntry must keep skipping the metadata file and non-JSON files so the
// score-on-read pass never tries to grade the group's _meta.json.
func TestReadSVGTestEntrySkipsNonEntries(t *testing.T) {
	dir := t.TempDir()
	if got := readSVGTestEntry(filepath.Join(dir, "_meta.json")); got != nil {
		t.Errorf("_meta.json produced %+v, want nil", got)
	}
	if got := readSVGTestEntry(filepath.Join(dir, "notes.txt")); got != nil {
		t.Errorf("non-JSON produced %+v, want nil", got)
	}
	if got := readSVGTestEntry(filepath.Join(dir, "missing.json")); got != nil {
		t.Errorf("missing file produced %+v, want nil", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write broken.json: %v", err)
	}
	if got := readSVGTestEntry(filepath.Join(dir, "broken.json")); got != nil {
		t.Errorf("malformed JSON produced %+v, want nil", got)
	}
}

// The group summary is what the history list renders, so it must carry the score
// too — computed on read, matching the detail view.
func TestLoadSVGTestGroupSummariesCarryScore(t *testing.T) {
	dir := t.TempDir()
	key := svgPromptKey("draw a pelican")
	if err := saveSVGTestMeta(dir, key, "draw a pelican"); err != nil {
		t.Fatalf("saveSVGTestMeta: %v", err)
	}
	good := svgScoreFixture(60, "cx", "opacity")
	wantScore, _ := scoreSVG(good)
	if err := saveSVGTestEntry(dir, svgTestEntry{
		PromptKey: key, Model: "qwen3.8-max-cn", AccountID: "acc-1", AccountName: "beta@example.com",
		Success: true, SVG: good, TokensUsed: 2000,
	}); err != nil {
		t.Fatalf("saveSVGTestEntry: %v", err)
	}

	meta := loadSVGTestGroup(filepath.Join(dir, key), key)
	if meta == nil || len(meta.Results) != 1 {
		t.Fatalf("meta = %+v, want one result", meta)
	}
	if meta.Results[0].Score != wantScore {
		t.Errorf("summary score = %d, want %d", meta.Results[0].Score, wantScore)
	}
	if !meta.Results[0].HasSVG || !meta.Results[0].Success {
		t.Errorf("summary flags = hasSVG=%v success=%v, want both true", meta.Results[0].HasSVG, meta.Results[0].Success)
	}
}

// A sweep stores one pair at several efforts, and getSVGTestGroupEntries must
// return them low-to-high so the curve is already ordered and the grid groups a
// pair's rungs together.
func TestGetSVGTestGroupEntriesOrdersSweepRungs(t *testing.T) {
	dir := t.TempDir()
	key := svgPromptKey("draw a pelican")
	if err := saveSVGTestMeta(dir, key, "draw a pelican"); err != nil {
		t.Fatalf("saveSVGTestMeta: %v", err)
	}
	// Seeded out of order and with a raw rung, to prove mode sorts before effort
	// and effort sorts by spend rather than alphabetically.
	order := []struct{ mode, effort string }{
		{"think", "high"}, {"raw", ""}, {"think", "low"}, {"think", "medium"},
	}
	for _, o := range order {
		if err := saveSVGTestEntry(dir, svgTestEntry{
			PromptKey: key, Model: "qwen3.8-max-cn", AccountID: "acc-1", AccountName: "beta@example.com",
			Mode: o.mode, Effort: o.effort, Success: true, SVG: svgScoreFixture(30, "cx"), TokensUsed: 1000,
		}); err != nil {
			t.Fatalf("seed %s/%s: %v", o.mode, o.effort, err)
		}
	}

	entries := getSVGTestGroupEntries(dir, key)
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4", len(entries))
	}
	// raw sorts before think ("" < "raw" < "think"); think rungs follow low→high.
	got := make([]string, len(entries))
	for i, e := range entries {
		got[i] = e.Mode + "/" + e.Effort
	}
	want := []string{"raw/", "think/low", "think/medium", "think/high"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q (full order %v)", i, got[i], want[i], got)
		}
	}
}

func TestSVGTestEffortRank(t *testing.T) {
	cases := map[string]int{"": 0, "bogus": 0, "low": 1, "medium": 2, "high": 3, "max": 4}
	for in, want := range cases {
		if got := svgTestEffortRank(in); got != want {
			t.Errorf("svgTestEffortRank(%q) = %d, want %d", in, got, want)
		}
	}
}
