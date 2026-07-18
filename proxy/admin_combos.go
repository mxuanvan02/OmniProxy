package proxy

import (
	"encoding/json"
	"omniproxy/config"
	"net/http"
	"strings"
)

// comboView is the API response shape for a combo entry.
type comboView struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Models   []string `json:"models"`
	Strategy string   `json:"strategy,omitempty"`
}

func toComboView(e config.ComboEntry) comboView {
	return comboView{
		ID:       e.ID,
		Name:     e.Name,
		Models:   e.Models,
		Strategy: e.Strategy,
	}
}

// apiListCombos handles GET /admin/api/combos
func (h *Handler) apiListCombos(w http.ResponseWriter, r *http.Request) {
	entries := config.ListCombos()
	out := make([]comboView, len(entries))
	for i, e := range entries {
		out[i] = toComboView(e)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"combos": out})
}

// apiGetCombo handles GET /admin/api/combos/:id
func (h *Handler) apiGetCombo(w http.ResponseWriter, r *http.Request, id string) {
	entry := config.GetComboByID(id)
	if entry == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "combo not found"})
		return
	}
	json.NewEncoder(w).Encode(toComboView(*entry))
}

// apiCreateCombo handles POST /admin/api/combos
func (h *Handler) apiCreateCombo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string   `json:"name"`
		Models   []string `json:"models"`
		Strategy string   `json:"strategy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "name is required"})
		return
	}
	if strings.Contains(req.Name, "/") {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "combo name must not contain '/'"})
		return
	}
	if len(req.Models) < 1 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "combo must have at least 1 model"})
		return
	}
	entry := config.ComboEntry{
		Name:     req.Name,
		Models:   req.Models,
		Strategy: req.Strategy,
	}
	created, err := config.AddCombo(entry)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(toComboView(created))
}

// apiUpdateCombo handles PUT /admin/api/combos/:id
func (h *Handler) apiUpdateCombo(w http.ResponseWriter, r *http.Request, id string) {
	var req config.ComboUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name != nil {
		*req.Name = strings.TrimSpace(*req.Name)
		if strings.Contains(*req.Name, "/") {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "combo name must not contain '/'"})
			return
		}
	}
	if err := config.UpdateCombo(id, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	updated := config.GetComboByID(id)
	if updated == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "combo not found"})
		return
	}
	json.NewEncoder(w).Encode(toComboView(*updated))
}

// apiDeleteCombo handles DELETE /admin/api/combos/:id
func (h *Handler) apiDeleteCombo(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteCombo(id); err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

// apiGetComboSettings handles GET /admin/api/combo-settings
func (h *Handler) apiGetComboSettings(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"strategy":             config.GetComboStrategy(),
		"stickyRoundRobinLimit": config.GetComboStickyRoundRobinLimit(),
	})
}

// apiUpdateComboSettings handles POST /admin/api/combo-settings
func (h *Handler) apiUpdateComboSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Strategy             string `json:"strategy"`
		StickyRoundRobinLimit int    `json:"stickyRoundRobinLimit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
		return
	}
	if err := config.UpdateComboSettings(req.Strategy, req.StickyRoundRobinLimit); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.apiGetComboSettings(w, r)
}
