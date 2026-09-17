package proxy

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"omniproxy/config"
)

// svgMatrixAccount is one selectable account plus the models it can serve.
// Shipping the whole membership in a single response is what makes both filter
// directions instant: picking a model narrows the account list, and picking an
// account narrows the model list, with no further round trip per selection.
type svgMatrixAccount struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Provider     string   `json:"provider"`
	Dialect      string   `json:"dialect"`
	CatalogState string   `json:"catalogState"`
	Models       []string `json:"models"`
}

// apiSVGTestMatrix GET /admin/api/test-model-svg/matrix returns every eligible
// chat account with the models it can serve, plus the union model list. The UI
// fetches this once and derives both filters from it locally.
func (h *Handler) apiSVGTestMatrix(w http.ResponseWriter, _ *http.Request) {
	accounts := h.pool.GetAllAccountsFull()
	out := make([]svgMatrixAccount, 0, len(accounts))
	modelSet := map[string]bool{}

	for i := range accounts {
		a := &accounts[i]
		// Service adapters (search/image) have no chat path, so they cannot run
		// an SVG prompt — the same guard the POST endpoint enforces.
		if !a.Enabled || isServiceAccount(a) {
			continue
		}
		catalog := h.pool.GetModelList(a.ID)
		models := servableModels(a, catalog)
		state := string(h.catalogStatusFor(a.ID, len(catalog)).State)
		out = append(out, svgMatrixAccount{
			ID:           a.ID,
			Name:         accountLabel(a),
			Provider:     providerLabelOf(a.Provider),
			Dialect:      externalAPIDialect(a),
			CatalogState: state,
			Models:       models,
		})
		for _, m := range models {
			modelSet[m] = true
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Name < out[j].Name
	})
	models := make([]string, 0, len(modelSet))
	for m := range modelSet {
		models = append(models, m)
	}
	sort.Strings(models)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"accounts": out,
		"models":   models,
		// The UI prefills its prompt box from this so the default it sends is
		// byte-identical to the server default — otherwise the same "default"
		// comparison would fork into two prompt groups.
		"defaultPrompt": defaultSVGPrompt,
	})
}

// servableModels narrows a discovered catalog to the models the account is
// meant to serve. The restriction rule matches the pool and the admin API:
// RestrictModels OR a non-empty allowlist means only allowlisted models count.
//
// This is deliberately not a claim about pool routing. A pinned SVG test
// dispatches straight to the account, bypassing accountHasModel, so the picker
// shows what the provider advertises (the catalog) filtered by the operator's
// stated intent (the allowlist) — not what the load balancer would pick.
func servableModels(a *config.Account, catalog []string) []string {
	allowed := map[string]bool{}
	for _, m := range a.AllowedModels {
		if n := strings.ToLower(strings.TrimSpace(m)); n != "" {
			allowed[n] = true
		}
	}
	restricted := a.RestrictModels || len(allowed) > 0

	out := make([]string, 0, len(catalog))
	for _, m := range catalog {
		n := strings.ToLower(strings.TrimSpace(m))
		if n == "" {
			continue
		}
		if restricted && !allowed[n] {
			continue
		}
		out = append(out, n)
	}
	// A restricted account whose catalog fetch failed still advertises its
	// allowlist: hiding those would leave nothing selectable for an account the
	// operator explicitly configured, which is worse than offering a choice the
	// provider may then reject (and the rejection is stored as a result).
	if restricted {
		for n := range allowed {
			if !containsFold(out, n) {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// apiListSVGTestGroups GET /admin/api/test-model-svg/groups returns stored
// prompt groups (newest first) without SVG payloads, so the history picker stays
// cheap to poll.
func (h *Handler) apiListSVGTestGroups(w http.ResponseWriter, _ *http.Request) {
	groups := listSVGTestGroups(svgTestStoreDir(), 50)
	if groups == nil {
		groups = []svgTestGroupMeta{}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{"groups": groups})
}

// apiGetSVGTestGroup GET /admin/api/test-model-svg/groups/{key} returns every
// stored result for one prompt, SVG markup included, for the comparison grid.
func (h *Handler) apiGetSVGTestGroup(w http.ResponseWriter, _ *http.Request, key string) {
	if !validSVGPromptKey(key) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid prompt key"})
		return
	}
	dir := svgTestStoreDir()
	entries := getSVGTestGroupEntries(dir, key)
	if len(entries) == 0 {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "No results stored for this prompt"})
		return
	}
	meta := loadSVGTestGroup(filepath.Join(dir, key), key)
	if meta == nil {
		meta = &svgTestGroupMeta{PromptKey: key, ResultCount: len(entries)}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{"group": meta, "entries": entries})
}

// apiDeleteSVGTestResult DELETE /admin/api/test-model-svg/groups/{key}/entries
// ?model=&accountId= removes one stored (model, account) result — typically a
// failed attempt the operator no longer wants cluttering the comparison. When it
// was the group's last entry the group directory goes with it, so the history
// never lists a prompt with nothing left to show.
func (h *Handler) apiDeleteSVGTestResult(w http.ResponseWriter, r *http.Request, key string) {
	if !validSVGPromptKey(key) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid prompt key"})
		return
	}
	model := r.URL.Query().Get("model")
	accountID := r.URL.Query().Get("accountId")
	if model == "" || accountID == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Model and account are required"})
		return
	}
	dir := svgTestStoreDir()
	removed, groupEmpty, err := deleteSVGTestEntry(dir, key, model, accountID)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if !removed {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "No such stored result"})
		return
	}
	if groupEmpty {
		_ = os.RemoveAll(filepath.Join(dir, key))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{"deleted": true})
}
