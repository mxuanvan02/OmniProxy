package proxy

import (
	"omniproxy/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandComboModelsFlattensNestedCombosInOrder(t *testing.T) {
	mustInitConfig(t)
	outer, err := config.AddCombo(config.ComboEntry{Name: "outer", Models: []string{"inner", "gpt-5.6-sol"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = config.DeleteCombo(outer.ID) })
	inner, err := config.AddCombo(config.ComboEntry{Name: "inner", Models: []string{"claude-opus-5", "fallback"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = config.DeleteCombo(inner.ID) })

	got, err := expandComboModels("outer")
	if err != nil {
		t.Fatalf("expandComboModels: %v", err)
	}
	want := []string{"claude-opus-5", "fallback", "gpt-5.6-sol"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("expanded models = %v, want %v", got, want)
	}
}

func TestExpandComboModelsDeduplicatesLeavesPreservingFirstOccurrence(t *testing.T) {
	mustInitConfig(t)
	inner, err := config.AddCombo(config.ComboEntry{Name: "dedupe-inner", Models: []string{"shared", "unique"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = config.DeleteCombo(inner.ID) })
	outer, err := config.AddCombo(config.ComboEntry{Name: "dedupe-outer", Models: []string{"shared", "dedupe-inner", "shared"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = config.DeleteCombo(outer.ID) })

	got, err := expandComboModels("dedupe-outer")
	if err != nil {
		t.Fatalf("expandComboModels: %v", err)
	}
	want := []string{"shared", "unique"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("expanded models = %v, want %v", got, want)
	}
}

func TestExpandComboModelsRejectsCycles(t *testing.T) {
	// The runtime guard must still catch cycles that predate validation: a
	// config written before AddCombo/UpdateCombo rejected back-edges can carry
	// a cycle on disk. Build such a config file and load it.
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfgJSON := `{"combos":[
		{"id":"c1","name":"cycle-a","models":["cycle-b"],"strategy":"fallback"},
		{"id":"c2","name":"cycle-b","models":["cycle-a"],"strategy":"fallback"}
	]}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	if _, err := expandComboModels("cycle-a"); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expandComboModels error = %v, want cycle error", err)
	}
}

func TestExpandComboModelsRejectsExcessiveDepth(t *testing.T) {
	mustInitConfig(t)
	ids := make([]string, 0, maxComboExpansionDepth+1)
	for i := 0; i <= maxComboExpansionDepth; i++ {
		name := "depth-" + string(rune('a'+i%26)) + "-" + string(rune('0'+i/26))
		ids = append(ids, name)
		models := []string{"leaf-model"}
		if i < maxComboExpansionDepth {
			models = []string{"depth-" + string(rune('a'+(i+1)%26)) + "-" + string(rune('0'+(i+1)/26))}
		}
		entry, err := config.AddCombo(config.ComboEntry{Name: name, Models: models})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = config.DeleteCombo(entry.ID) })
	}

	if _, err := expandComboModels(ids[0]); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("expandComboModels error = %v, want depth error", err)
	}
}
