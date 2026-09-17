package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"testing"
)

// These cover the walk itself: the picker alone proves nothing until a refused
// model actually changes what the probe reports.

// probeRequestModel pulls the model out of whatever body the probe sent.
func probeRequestModel(r *http.Request) string {
	raw, _ := io.ReadAll(r.Body)
	var decoded struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &decoded)
	return decoded.Model
}

func probeRetryAccount(id, baseURL string) *config.Account {
	return &config.Account{
		ID:                     id,
		AuthMethod:             "external_openai",
		BaseURL:                baseURL,
		AccessToken:            "secret",
		Enabled:                true,
		DiscoveredCapabilities: []string{capabilityChat},
	}
}

// The regression this guards: VIBE10's catalog lists qwen3.8-max-thinking-agent
// next to qwen3.8-max and answers the first one asked with 403 "no access to
// model". A probe that tried exactly one model reported no vision on a capable
// account. Here the plain model is the one refused, so the walk has to reach the
// variant to get a real answer.
func TestProbeAccountCapabilityRetriesPastDeniedModel(t *testing.T) {
	config.Init("")

	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := probeRequestModel(r)
		asked = append(asked, model)
		w.Header().Set("Content-Type", "application/json")
		if model == "qwen3.8-max" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"This token has no access to model ` + model + `","type":"new_api_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	handler := &Handler{pool: accountpool.GetPool()}
	account := probeRetryAccount("acc-denied-variant", srv.URL)
	handler.pool.SetModelList(account.ID, []string{"qwen3.8-max-thinking-agent", "qwen3.8-max"})

	result := handler.probeAccountCapability(account, capabilityVision)
	if !result.OK {
		t.Fatalf("probe failed although a working model exists: status=%d detail=%q", result.Status, result.Detail)
	}
	if result.Model != "qwen3.8-max-thinking-agent" {
		t.Errorf("recorded model = %q, want the one that actually answered", result.Model)
	}
	if len(asked) != 2 {
		t.Errorf("probe asked %d model(s) %v, want both candidates", len(asked), asked)
	}
}

// The pool caches each catalog as a set, so GetModelList returns map order. A
// probe that walked that order as-is would ask a different model each run and
// could report vision on Monday and no vision on Tuesday for the same account.
func TestProbeCandidateModelsIsDeterministic(t *testing.T) {
	config.Init("")
	h := &Handler{pool: accountpool.GetPool()}
	account := &config.Account{ID: "acc-determinism", Enabled: true}
	catalog := []string{
		"qwen3.8-max-thinking-agent", "qwen3.8-max-fast-agent", "qwen3.8-max",
		"deepseek-v4-flash", "Qwen3-Embedding-8B", "glm-5.3-cn",
	}
	h.pool.SetModelList(account.ID, catalog)

	first, reason := probeCandidateModels(h, account, capabilityChat)
	if reason != "" {
		t.Fatalf("catalog walk reported a reason although chat models exist: %s", reason)
	}
	for i := 0; i < 200; i++ {
		again, _ := probeCandidateModels(h, account, capabilityChat)
		if len(again) != len(first) {
			t.Fatalf("run %d returned %d models, first run returned %d", i, len(again), len(first))
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("run %d order %v differs from first run %v: probe outcome would depend on map iteration", i, again, first)
			}
		}
	}
}

// When every candidate is refused at the model level the probe must report
// skipped, not failed. It never got a clean shot at the capability, so calling
// the endpoint broken would be the same false negative in a different hat.
func TestProbeAccountCapabilityAllModelsDeniedIsSkipped(t *testing.T) {
	config.Init("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := probeRequestModel(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"This token has no access to model ` + model + `"}}`))
	}))
	defer srv.Close()

	handler := &Handler{pool: accountpool.GetPool()}
	account := probeRetryAccount("acc-all-denied", srv.URL)
	handler.pool.SetModelList(account.ID, []string{"model-a", "model-b"})

	result := handler.probeAccountCapability(account, capabilityVision)
	if result.OK {
		t.Fatal("probe reported OK although every model was denied")
	}
	if !result.Skipped {
		t.Errorf("all-denied probe = failed, want skipped: it is not evidence about the endpoint")
	}
	if result.SkippedReason == "" {
		t.Error("skipped probe carries no reason for the operator to read")
	}
}

// A body the upstream rejects is a real verdict about the capability and must
// stop the walk, otherwise retrying across the catalog would hide it.
func TestProbeAccountCapabilityStopsOnRealVerdict(t *testing.T) {
	config.Init("")

	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"images are not supported by this model"}}`))
	}))
	defer srv.Close()

	handler := &Handler{pool: accountpool.GetPool()}
	account := probeRetryAccount("acc-real-verdict", srv.URL)
	handler.pool.SetModelList(account.ID, []string{"model-a", "model-b", "model-c"})

	result := handler.probeAccountCapability(account, capabilityVision)
	if result.OK || result.Skipped {
		t.Fatalf("a 400 must be recorded as a failure, got ok=%v skipped=%v", result.OK, result.Skipped)
	}
	if asked != 1 {
		t.Errorf("probe asked %d models after a 400, want 1: a body verdict is not model-specific", asked)
	}
}

// Far more candidates than the budget allows: one diagnostic must not become a
// long run of billed requests against a gateway that denies everything.
func TestProbeAccountCapabilityRespectsRetryBudget(t *testing.T) {
	config.Init("")

	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"request_quota_exceeded"}}`))
	}))
	defer srv.Close()

	handler := &Handler{pool: accountpool.GetPool()}
	account := probeRetryAccount("acc-budget", srv.URL)
	many := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		many = append(many, "model-"+string(rune('a'+i)))
	}
	handler.pool.SetModelList(account.ID, many)

	result := handler.probeAccountCapability(account, capabilityVision)
	if asked != maxProbeModelRetries {
		t.Errorf("probe made %d requests, want the budget of %d", asked, maxProbeModelRetries)
	}
	if !result.Skipped {
		t.Errorf("rate-limited probe across every candidate = failed, want skipped")
	}
}
