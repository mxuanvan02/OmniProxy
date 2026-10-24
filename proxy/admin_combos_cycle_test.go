package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	"strings"
	"testing"
)

// The write-time cycle guard must surface through the admin API as a 400 with
// the cycle message, not a 500 or a silent success. This pins the wiring
// between apiCreateCombo/apiUpdateCombo and the config-layer validation.
func TestAdminCreateComboRejectsCycleAs400(t *testing.T) {
	mustInitConfig(t)
	h := &Handler{}

	create := func(name, model string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]interface{}{"name": name, "models": []string{model}})
		req := httptest.NewRequest(http.MethodPost, "/admin/api/combos", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		h.apiCreateCombo(rec, req)
		return rec
	}

	if rec := create("api-a", "api-b"); rec.Code != http.StatusCreated {
		t.Fatalf("create api-a status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	rec := create("api-b", "api-a")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create api-b->api-a status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cycle") {
		t.Fatalf("error body = %s, want a cycle message", rec.Body.String())
	}

	// The rejected combo must not exist afterwards.
	for _, c := range config.ListCombos() {
		if c.Name == "api-b" {
			t.Fatalf("rejected combo %q was persisted", c.Name)
		}
	}
}

func TestAdminUpdateComboRejectsCycleAs400(t *testing.T) {
	mustInitConfig(t)
	h := &Handler{}

	create := func(name, model string) config.ComboEntry {
		body, _ := json.Marshal(map[string]interface{}{"name": name, "models": []string{model}})
		req := httptest.NewRequest(http.MethodPost, "/admin/api/combos", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		h.apiCreateCombo(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s status = %d, want 201 (body=%s)", name, rec.Code, rec.Body.String())
		}
		var view struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode create response: %v", err)
		}
		return config.ComboEntry{ID: view.ID, Name: name}
	}

	a := create("upd-a", "upd-b")
	b := create("upd-b", "leaf-model")
	t.Cleanup(func() { _ = config.DeleteCombo(a.ID) })
	t.Cleanup(func() { _ = config.DeleteCombo(b.ID) })

	body, _ := json.Marshal(config.ComboUpdateRequest{Models: []string{"upd-a"}})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/combos/"+b.ID, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiUpdateCombo(rec, req, b.ID)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update upd-b->[upd-a] status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cycle") {
		t.Fatalf("error body = %s, want a cycle message", rec.Body.String())
	}

	got := config.GetComboByID(b.ID)
	if got == nil || len(got.Models) != 1 || got.Models[0] != "leaf-model" {
		t.Fatalf("models after rejected update = %+v, want [leaf-model] to survive", got)
	}
}
