package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// seedSVGTestEntry writes one entry through the real save path so delete tests
// operate on exactly what the writer produces rather than on a file named by
// hand. A pair is now the whole identity of a result, so seeding two accounts
// for one model is enough to prove a delete is scoped.
func seedSVGTestEntry(t *testing.T, dir, key, model, accountID string) {
	t.Helper()
	if err := saveSVGTestEntry(dir, svgTestEntry{
		PromptKey: key, Model: model, AccountID: accountID, Success: false,
	}); err != nil {
		t.Fatalf("seed entry %s/%s: %v", model, accountID, err)
	}
}

func TestDeleteSVGTestEntryRemovesOneKeepsOthers(t *testing.T) {
	dir := t.TempDir()
	seedSVGTestEntry(t, dir, "p-key1", "gemini-3.8-flash", "acct-a")
	seedSVGTestEntry(t, dir, "p-key1", "gemini-3.8-flash", "acct-b")

	removed, groupEmpty, err := deleteSVGTestEntry(dir, "p-key1", "gemini-3.8-flash", "acct-a")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !removed || groupEmpty {
		t.Fatalf("removed=%v groupEmpty=%v, want true/false", removed, groupEmpty)
	}
	left := getSVGTestGroupEntries(dir, "p-key1")
	if len(left) != 1 || left[0].AccountID != "acct-b" {
		t.Fatalf("left = %+v, want only acct-b", left)
	}
}

func TestDeleteSVGTestEntryLastEntryReportsGroupEmpty(t *testing.T) {
	dir := t.TempDir()
	seedSVGTestEntry(t, dir, "p-key2", "glm-5.3", "acct-a")

	removed, groupEmpty, err := deleteSVGTestEntry(dir, "p-key2", "glm-5.3", "acct-a")
	if err != nil || !removed || !groupEmpty {
		t.Fatalf("removed=%v groupEmpty=%v err=%v, want true/true/nil", removed, groupEmpty, err)
	}
	if len(getSVGTestGroupEntries(dir, "p-key2")) != 0 {
		t.Fatal("group still reports entries after deleting its only one")
	}
}

func TestDeleteSVGTestEntryMissingIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	seedSVGTestEntry(t, dir, "p-key3", "glm-5.3", "acct-a")

	removed, _, err := deleteSVGTestEntry(dir, "p-key3", "glm-5.3", "never-stored")
	if err != nil {
		t.Fatalf("missing delete must not error, got %v", err)
	}
	if removed {
		t.Fatal("removed=true for an entry that was never stored")
	}
}

func TestDeleteSVGTestEntryRejectsInvalidArgs(t *testing.T) {
	dir := t.TempDir()
	seedSVGTestEntry(t, dir, "p-key4", "glm-5.3", "acct-a")

	for _, tc := range []struct{ key, model, account string }{
		{"../evil", "glm-5.3", "acct-a"},
		{"p-key4", "", "acct-a"},
		{"p-key4", "glm-5.3", ""},
	} {
		removed, _, err := deleteSVGTestEntry(dir, tc.key, tc.model, tc.account)
		if err != nil || removed {
			t.Fatalf("invalid args %+v: removed=%v err=%v, want false/nil", tc, removed, err)
		}
	}
	if len(getSVGTestGroupEntries(dir, "p-key4")) != 1 {
		t.Fatal("invalid delete arguments must not touch stored entries")
	}
}

// A hostile model id must not let the delete reach outside the group dir: the
// sanitised stem collapses ../ to underscores, so the real entry survives.
func TestDeleteSVGTestEntryCannotEscapeGroup(t *testing.T) {
	dir := t.TempDir()
	seedSVGTestEntry(t, dir, "p-key5", "glm-5.3", "acct-a")
	outside := filepath.Join(dir, "outside.json")
	if err := os.WriteFile(outside, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}

	if _, _, err := deleteSVGTestEntry(dir, "p-key5", "../../outside", "acct-a"); err != nil {
		t.Fatalf("escape attempt errored: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file was deleted: %v", err)
	}
	if len(getSVGTestGroupEntries(dir, "p-key5")) != 1 {
		t.Fatal("escape attempt removed the real entry")
	}
}

// Deleting a model the group never held must not take a different model's result
// with it: the operator drops one bad cell from a comparison and the rest of the
// grid has to survive intact.
func TestDeleteSVGTestEntryIsScopedToItsModel(t *testing.T) {
	dir := t.TempDir()
	seedSVGTestEntry(t, dir, "p-key6", "glm-5.3", "acct-a")
	seedSVGTestEntry(t, dir, "p-key6", "qwen3.8-max", "acct-a")

	removed, groupEmpty, err := deleteSVGTestEntry(dir, "p-key6", "glm-5.3", "acct-a")
	if err != nil || !removed || groupEmpty {
		t.Fatalf("removed=%v groupEmpty=%v err=%v, want true/false/nil", removed, groupEmpty, err)
	}
	left := getSVGTestGroupEntries(dir, "p-key6")
	if len(left) != 1 || left[0].Model != "qwen3.8-max" {
		t.Fatalf("left = %+v, want only the qwen3.8-max entry", left)
	}
}
