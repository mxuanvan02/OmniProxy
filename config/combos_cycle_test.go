package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// mustInitConfig starts a fresh, empty config for one test. Each test gets its
// own tempdir so config globals never bleed across cases.
func mustInitConfigForComboCycle(t *testing.T) {
	t.Helper()
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
}

// A combo that names itself as one of its own models would expand forever if
// accepted. Reject it at write time, not at request time.
func TestAddComboRejectsSelfReference(t *testing.T) {
	mustInitConfigForComboCycle(t)
	if _, err := AddCombo(ComboEntry{Name: "self", Models: []string{"self"}}); err == nil ||
		!strings.Contains(err.Error(), "cycle") {
		t.Fatalf("AddCombo error = %v, want a cycle error", err)
	}
}

// Two combos pointing at each other form a cycle. The second add must fail.
func TestAddComboRejectsCycle(t *testing.T) {
	mustInitConfigForComboCycle(t)
	a, err := AddCombo(ComboEntry{Name: "a", Models: []string{"b"}})
	if err != nil {
		t.Fatalf("add a: %v", err)
	}
	t.Cleanup(func() { _ = DeleteCombo(a.ID) })

	if _, err := AddCombo(ComboEntry{Name: "b", Models: []string{"a"}}); err == nil ||
		!strings.Contains(err.Error(), "cycle") {
		t.Fatalf("AddCombo(b->a) error = %v, want a cycle error", err)
	}
}

// A longer cycle (a -> b -> c -> a) is just as bad. Adds that do not yet close
// the cycle succeed; the add that closes it must be rejected.
func TestAddComboRejectsLongCycle(t *testing.T) {
	mustInitConfigForComboCycle(t)
	a, err := AddCombo(ComboEntry{Name: "a", Models: []string{"b"}})
	if err != nil {
		t.Fatalf("add a: %v", err)
	}
	t.Cleanup(func() { _ = DeleteCombo(a.ID) })
	b, err := AddCombo(ComboEntry{Name: "b", Models: []string{"c"}})
	if err != nil {
		t.Fatalf("add b: %v", err)
	}
	t.Cleanup(func() { _ = DeleteCombo(b.ID) })

	if _, err := AddCombo(ComboEntry{Name: "c", Models: []string{"a"}}); err == nil ||
		!strings.Contains(err.Error(), "cycle") {
		t.Fatalf("AddCombo(c->a) error = %v, want a cycle error", err)
	}
}

// Updating a combo so that it now creates a cycle must be rejected too, and the
// previous definition must survive unchanged.
func TestUpdateComboRejectsCycle(t *testing.T) {
	mustInitConfigForComboCycle(t)
	a, err := AddCombo(ComboEntry{Name: "a", Models: []string{"b"}})
	if err != nil {
		t.Fatalf("add a: %v", err)
	}
	t.Cleanup(func() { _ = DeleteCombo(a.ID) })
	b, err := AddCombo(ComboEntry{Name: "b", Models: []string{"claude-opus-5"}})
	if err != nil {
		t.Fatalf("add b: %v", err)
	}
	t.Cleanup(func() { _ = DeleteCombo(b.ID) })

	models := []string{"a"}
	if err := UpdateCombo(b.ID, ComboUpdateRequest{Models: models}); err == nil ||
		!strings.Contains(err.Error(), "cycle") {
		t.Fatalf("UpdateCombo(b->[a]) error = %v, want a cycle error", err)
	}

	got := GetComboByID(b.ID)
	if got == nil {
		t.Fatal("combo b disappeared after a rejected update")
	}
	if len(got.Models) != 1 || got.Models[0] != "claude-opus-5" {
		t.Fatalf("models = %v, want the previous [claude-opus-5] to survive", got.Models)
	}
}

// A legal chain (a -> b, where b is a leaf model) must still be accepted, so
// the new check does not reject ordinary nested combos.
func TestAddComboAllowsValidNestedChain(t *testing.T) {
	mustInitConfigForComboCycle(t)
	inner, err := AddCombo(ComboEntry{Name: "inner", Models: []string{"claude-opus-5"}})
	if err != nil {
		t.Fatalf("add inner: %v", err)
	}
	t.Cleanup(func() { _ = DeleteCombo(inner.ID) })
	outer, err := AddCombo(ComboEntry{Name: "outer", Models: []string{"inner", "gpt-5.6-sol"}})
	if err != nil {
		t.Fatalf("add outer: %v", err)
	}
	t.Cleanup(func() { _ = DeleteCombo(outer.ID) })
}
