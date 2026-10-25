package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A single observation records what a provider claimed. Both honest and
// mismatched answers are recorded: the honest baseline is what makes the
// mismatch rate readable instead of a pile of warnings.
func TestObserveServedModelRecordsCountsAndMismatch(t *testing.T) {
	resetServedModelStore(t)

	observeServedModel("combo-a", "gpt-5.6-sol", []byte(`{"model":"gpt-5.6-sol"}`))
	observeServedModel("combo-a", "gpt-5.6-sol", []byte(`{"model":"gpt-5.6-sol"}`))
	observeServedModel("combo-a", "gpt-5.6-sol", []byte(`{"model":"gpt-5.6-mini"}`))

	obs := servedModelSnapshot()
	if len(obs) != 2 {
		t.Fatalf("observations = %d entries, want 2 (honest + mismatch)", len(obs))
	}
	honest := findObservation(t, obs, "combo-a", "gpt-5.6-sol", "gpt-5.6-sol")
	if honest.Count != 2 || honest.Mismatch {
		t.Fatalf("honest observation = %+v, want count 2, mismatch false", honest)
	}
	fake := findObservation(t, obs, "combo-a", "gpt-5.6-sol", "gpt-5.6-mini")
	if fake.Count != 1 || !fake.Mismatch {
		t.Fatalf("mismatch observation = %+v, want count 1, mismatch true", fake)
	}
}

// An unparseable body is a gap in evidence, not a mismatch. It must not create
// an observation entry with an empty served model.
func TestObserveServedModelSkipsUnparseableBody(t *testing.T) {
	resetServedModelStore(t)
	observeServedModel("combo-b", "gpt-5.6-sol", []byte(`{"model":`))
	observeServedModel("combo-b", "gpt-5.6-sol", nil)
	if got := len(servedModelSnapshot()); got != 0 {
		t.Fatalf("observations = %d, want 0 for unparseable bodies", got)
	}
}

// The store is bounded: a runaway set of distinct served models must not grow
// memory without limit.
func TestServedModelStoreIsBounded(t *testing.T) {
	resetServedModelStore(t)
	for i := 0; i < maxServedModelObservations+50; i++ {
		observeServedModel("combo-c", "gpt-5.6-sol", []byte(`{"model":"fake-`+itoaForTest(i)+`"}`))
	}
	if got := len(servedModelSnapshot()); got > maxServedModelObservations {
		t.Fatalf("store size = %d, want <= %d", got, maxServedModelObservations)
	}
}

// The admin endpoint exposes the observations as JSON for the dashboard.
func TestApiGetServedModelsReturnsObservations(t *testing.T) {
	resetServedModelStore(t)
	observeServedModel("combo-d", "claude-sonnet-5", []byte(`{"model":"qwen3.8-max"}`))

	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/served-models", nil)
	rec := httptest.NewRecorder()
	h.apiGetServedModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload struct {
		Observations []servedModelObservation `json:"observations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Observations) != 1 {
		t.Fatalf("observations = %+v, want exactly 1", payload.Observations)
	}
	obs := payload.Observations[0]
	if obs.ComboName != "combo-d" || obs.Requested != "claude-sonnet-5" ||
		obs.Served != "qwen3.8-max" || !obs.Mismatch {
		t.Fatalf("observation = %+v, want the seeded mismatch", obs)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content-type = %q, want application/json", rec.Header().Get("Content-Type"))
	}
}

func resetServedModelStore(t *testing.T) {
	t.Helper()
	servedModelsMu.Lock()
	servedModels = make(map[string]*servedModelObservation)
	servedModelsMu.Unlock()
}

func findObservation(t *testing.T, obs []servedModelObservation, combo, requested, served string) servedModelObservation {
	t.Helper()
	for _, o := range obs {
		if o.ComboName == combo && o.Requested == requested && o.Served == served {
			return o
		}
	}
	t.Fatalf("no observation for (%s, %s, %s) in %+v", combo, requested, served, obs)
	return servedModelObservation{}
}

func itoaForTest(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
