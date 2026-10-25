package proxy

import (
	"encoding/json"
	"net/http"
	"sync"
)

// maxServedModelObservations bounds the store. A bounded map keeps a runaway
// set of distinct served model names from growing memory without limit; the
// oldest entries are dropped first, which is acceptable for an observation
// surface that accumulates continuously during normal operation.
const maxServedModelObservations = 512

// servedModelObservation records what a provider claimed to serve for a
// requested combo model. Honest and mismatched claims are both kept: the
// honest baseline is what makes the mismatch rate readable instead of a pile
// of unanchored warnings.
type servedModelObservation struct {
	ComboName string `json:"comboName"`
	Requested string `json:"requested"`
	Served    string `json:"served"`
	Mismatch  bool   `json:"mismatch"`
	Count     int    `json:"count"`
}

var (
	servedModels      = make(map[string]*servedModelObservation)
	servedModelsMu    sync.Mutex
	servedModelsOrder []string
)

func servedModelKey(combo, requested, served string) string {
	return combo + "\x00" + requested + "\x00" + served
}

// observeServedModel records one served-model claim from a combo attempt's
// response body. Unparseable bodies are gaps in evidence, not mismatches, and
// create no entry. Fail-open: this must never affect the response path.
func observeServedModel(comboName, requested string, body []byte) {
	served := extractServedModel(body)
	if served == "" {
		return
	}
	key := servedModelKey(comboName, requested, served)
	mismatch := servedModelMismatch(requested, served)

	servedModelsMu.Lock()
	defer servedModelsMu.Unlock()
	if entry, ok := servedModels[key]; ok {
		entry.Count++
		return
	}
	entry := &servedModelObservation{
		ComboName: comboName,
		Requested: requested,
		Served:    served,
		Mismatch:  mismatch,
		Count:     1,
	}
	servedModels[key] = entry
	servedModelsOrder = append(servedModelsOrder, key)
	for len(servedModels) > maxServedModelObservations && len(servedModelsOrder) > 0 {
		oldest := servedModelsOrder[0]
		servedModelsOrder = servedModelsOrder[1:]
		delete(servedModels, oldest)
	}
}

// servedModelSnapshot returns a stable copy of the observations, mismatches
// first (newest-seen order within each group), so the dashboard's most
// actionable rows are on top.
func servedModelSnapshot() []servedModelObservation {
	servedModelsMu.Lock()
	defer servedModelsMu.Unlock()
	out := make([]servedModelObservation, 0, len(servedModels))
	for _, key := range servedModelsOrder {
		if entry, ok := servedModels[key]; ok {
			out = append(out, *entry)
		}
	}
	// Stable partition: mismatches first, original insertion order preserved
	// within each group.
	mismatched := make([]servedModelObservation, 0, len(out))
	honest := make([]servedModelObservation, 0, len(out))
	for _, o := range out {
		if o.Mismatch {
			mismatched = append(mismatched, o)
		} else {
			honest = append(honest, o)
		}
	}
	return append(mismatched, honest...)
}

// apiGetServedModels handles GET /admin/api/served-models — the dashboard's
// read path for the relay-substitution observation store.
func (h *Handler) apiGetServedModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"observations": servedModelSnapshot(),
	}) //nolint:errcheck
}
