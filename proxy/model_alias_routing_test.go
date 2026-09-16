package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// aliasTestUpstream captures the model ID the gateway is asked for and answers
// with a minimal OpenAI SSE turn.
func aliasTestUpstream(t *testing.T, captured *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		*captured, _ = body["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
}

// An account whose gateway only spells the model with a deployment suffix must
// still receive a request for the bare family name, and the outbound wire ID
// must be the variant the gateway actually knows.
func TestOpenAIChatRoutesToDeploymentVariant(t *testing.T) {
	initConfigForTests(t)

	var capturedModel string
	upstream := aliasTestUpstream(t, &capturedModel)
	defer upstream.Close()

	h := newSOTATestHandler(t, "alias-cn-only", upstream.URL, []string{"qwen3.8-max-cn"})

	body := `{"model":"qwen3.8-max","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rec := httptest.NewRecorder()
	h.handleOpenAIChat(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if capturedModel != "qwen3.8-max-cn" {
		t.Fatalf("upstream saw model %q, want the deployment variant qwen3.8-max-cn", capturedModel)
	}
}

// When the exact model is available the rescue must stay out of the way: the
// gateway sees exactly what the client asked for.
func TestOpenAIChatKeepsExactModelWhenAvailable(t *testing.T) {
	initConfigForTests(t)

	var capturedModel string
	upstream := aliasTestUpstream(t, &capturedModel)
	defer upstream.Close()

	h := newSOTATestHandler(t, "alias-exact", upstream.URL, []string{"qwen3.8-max", "qwen3.8-max-cn"})

	body := `{"model":"qwen3.8-max","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rec := httptest.NewRecorder()
	h.handleOpenAIChat(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if capturedModel != "qwen3.8-max" {
		t.Fatalf("upstream saw model %q, want the exact requested ID", capturedModel)
	}
}

// A behaviour variant is a distinct model: with only -agent accounts in the
// pool a bare request must fail rather than be silently upgraded.
func TestOpenAIChatDoesNotSubstituteBehaviourVariant(t *testing.T) {
	initConfigForTests(t)

	var capturedModel string
	upstream := aliasTestUpstream(t, &capturedModel)
	defer upstream.Close()

	h := newSOTATestHandler(t, "alias-agent-only", upstream.URL, []string{"qwen3.8-max-agent"})

	body := `{"model":"qwen3.8-max","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rec := httptest.NewRecorder()
	h.handleOpenAIChat(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code == http.StatusOK {
		t.Fatalf("bare request succeeded against an agent-only pool (upstream saw %q)", capturedModel)
	}
}
