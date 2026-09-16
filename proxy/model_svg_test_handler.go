package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"omniproxy/config"
)

// defaultSVGPrompt asks the model to produce a self-contained SVG illustration.
// The instruction is explicit about output format so extractSVG can find the
// markup without parsing markdown fences.
const defaultSVGPrompt = `Create a simple, colorful SVG illustration of a cute animal riding a bicycle in a sunny landscape with clouds and grass. The SVG must be valid XML with a viewBox attribute and no external resources. Output ONLY the raw SVG code — no markdown fences, no explanation, no wrapping tags.`

// apiTestModelSVG sends a prompt that asks the model to generate an SVG image,
// then extracts and returns the raw SVG markup. This tests whether the model
// can follow complex visual instructions and produce valid structured output —
// something a simple "say ok" health check cannot verify.
func (h *Handler) apiTestModelSVG(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "claude-sonnet-4"
	}

	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		prompt = defaultSVGPrompt
	}

	// Route through the same model-aware pool the live proxy uses, so the test
	// hits an account that actually serves this model (and honours cooldown and
	// quota). Picking "first enabled account" would land on an upstream that
	// silently rejects the model, making the capability result meaningless.
	account := h.pool.GetNextForModel(model)
	if account == nil {
		w.WriteHeader(503)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"model":   model,
			"error":   "No available account serves this model",
		})
		return
	}

	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(model, thinkingCfg.Suffix)

	start := time.Now()

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: prompt}},
		MaxTokens: 4096,
		Stream:    false,
	}
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	var content string
	var inTok, outTok int
	callback := &KiroStreamCallback{
		OnText:     func(text string, _ bool) { content += text },
		OnToolUse:  func(_ KiroToolUse) {},
		OnComplete: func(in, out int) { inTok, outTok = in, out },
		OnError:    func(_ error) {},
		OnCredits:  func(_ float64) {},
		OnContextUsage: func(_ float64) {},
	}

	err := dispatchChat(r.Context(), account, kiroPayload, callback)
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   false,
			"error":     err.Error(),
			"model":     model,
			"elapsedMs": elapsed,
		})
		return
	}

	svg := extractSVG(content)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    svg != "",
		"svg":        svg,
		"rawReply":   content,
		"model":      model,
		"elapsedMs":  elapsed,
		"tokensUsed": inTok + outTok,
	})
}

// extractSVG finds the first <svg ...>...</svg> block in s, stripping any
// surrounding markdown fences the model may have added despite the prompt.
func extractSVG(s string) string {
	// Strip common markdown fence wrappers.
	s = strings.TrimSpace(s)
	for _, prefix := range []string{"```svg", "```xml", "```html", "```"} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
			s = strings.TrimSpace(s)
			break
		}
	}
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}

	lower := strings.ToLower(s)
	start := strings.Index(lower, "<svg")
	if start < 0 {
		return ""
	}
	end := strings.LastIndex(lower, "</svg>")
	if end < 0 || end <= start {
		return ""
	}
	return strings.TrimSpace(s[start : end+len("</svg>")])
}
