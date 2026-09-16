package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	accountpool "omniproxy/pool"
	"strings"
	"testing"
)

// End-to-end probe tests. These need a live test server and the pool, so they
// live apart from the pure wire-shape tests in capability_probe_vision_test.go.

// Discovery cannot see vision on these resellers — they publish no input-type
// metadata — so a chat-capable account must be probed for vision anyway.
// Otherwise the probe never runs and the matrix reports nothing forever.
func TestProbeAccountCapabilitiesAddsVisionForChatAccounts(t *testing.T) {
	config.Init("")
	handler := &Handler{pool: accountpool.GetPool()}
	account := &config.Account{
		ID:                     "acc-chat-no-vision",
		AuthMethod:             "external_openai",
		BaseURL:                "http://127.0.0.1:1",
		AccessToken:            "secret",
		Enabled:                true,
		DiscoveredCapabilities: []string{capabilityChat},
	}
	handler.pool.SetModelList(account.ID, []string{"qwen3.8-max"})

	results := handler.probeAccountCapabilities(account, false)
	if _, ok := results[capabilityVision]; !ok {
		t.Errorf("vision was not probed for a chat account; got %v", visionProbeKeys(results))
	}
}

func visionProbeKeys(m map[string]config.CapabilityProbeResult) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A Responses gateway must receive the flat input_image shape, not the
// chat-completions nested one, or the probe 400s and reports no vision.
func TestProbeAccountCapabilityVisionUsesResponsesDialect(t *testing.T) {
	config.Init("")

	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed"}`))
	}))
	defer srv.Close()

	handler := &Handler{pool: accountpool.GetPool()}
	account := &config.Account{
		ID:                     "acc-vision-responses",
		AuthMethod:             "external_openai",
		BaseURL:                srv.URL,
		AccessToken:            "secret",
		Enabled:                true,
		ExternalAPIDialect:     "responses",
		DiscoveredCapabilities: []string{capabilityChat},
	}
	handler.pool.SetModelList(account.ID, []string{"qwen3.8-max"})

	result := handler.probeAccountCapability(account, capabilityVision)
	if !result.OK {
		t.Fatalf("probe not OK: status=%d detail=%q", result.Status, result.Detail)
	}
	if gotPath != "/v1/responses" {
		t.Errorf("probed path = %q, want /v1/responses", gotPath)
	}
	if result.Model != "qwen3.8-max" {
		t.Errorf("probed model = %q, want qwen3.8-max", result.Model)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(gotBody), &decoded); err != nil {
		t.Fatalf("probe body is not valid JSON: %v (%s)", err, gotBody)
	}
	if _, hasMessages := decoded["messages"]; hasMessages {
		t.Errorf("vision probe sent a chat-completions body to a Responses gateway: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"type":"input_image"`) {
		t.Errorf("vision probe body missing the flat input_image part: %s", gotBody)
	}
}
