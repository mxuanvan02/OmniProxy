package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"omniproxy/config"
)

// defaultSVGPrompt asks the model to produce a self-contained animated SVG.
// The first sentence is the operator-facing request; the rest pins the output
// format so extractSVG can find the markup without parsing markdown fences or
// an HTML wrapper. The admin UI prefills its prompt box from /matrix, so both
// surfaces send byte-identical text and therefore land in the same prompt group.
const defaultSVGPrompt = `Tạo một tệp HTML với nội dung là hình động 2D vẽ một con bồ nông đang đạp xe đạp bằng SVG. Yêu cầu bắt buộc: SVG hợp lệ có thuộc tính viewBox, dùng thẻ <animate> (SMIL) cho chuyển động (bánh xe đạp quay, thân chim nhấp nhô), không tham chiếu tài nguyên bên ngoài. Chỉ trả về mã SVG thô — không markdown fence, không giải thích, không thẻ bao ngoài.`

// apiTestModelSVG sends a prompt that asks the model to generate an SVG image,
// then extracts and returns the raw SVG markup. This tests whether the model
// can follow complex visual instructions and produce valid structured output —
// something a simple "say ok" health check cannot verify.
func (h *Handler) apiTestModelSVG(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model     string `json:"model"`
		Prompt    string `json:"prompt"`
		AccountID string `json:"accountId"`
		Mode      string `json:"mode"`
		Effort    string `json:"effort"`
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

	// A pinned account is tested directly — the point is to verify what that
	// specific account can do, not what the pool would route to. Without a pin
	// fall back to the same model-aware selection the live proxy uses, so the
	// test hits an account that actually serves the model (and honours cooldown
	// and quota) instead of one that silently rejects it.
	var account *config.Account
	if id := strings.TrimSpace(req.AccountID); id != "" {
		account = h.pool.GetByID(id)
		if account == nil {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"model":   model,
				"error":   "Account not found",
			})
			return
		}
		// Service adapters (search/image) have no chat path; sending their
		// credentials through dispatchChat would misroute them to Kiro/OpenAI.
		if isServiceAccount(account) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"model":   model,
				"error":   "Service accounts cannot run chat-based SVG tests",
			})
			return
		}
	} else {
		account = h.pool.GetNextForModel(model)
		if account == nil {
			w.WriteHeader(503)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"model":   model,
				"error":   "No available account serves this model",
			})
			return
		}
	}

	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, _ := ParseModelAndThinking(model, thinkingCfg.Suffix)

	// The mode, not the model suffix, decides reasoning here: raw forces it off
	// and think forces it on across every dialect, so the comparison isolates
	// rendering ability from reasoning budget. The effort refines think mode for
	// the sweep, where several rungs of one pair must land in one curve.
	mode := resolveSVGTestMode(req.Mode)
	effort := normalizeSVGTestEffort(req.Effort)

	start := time.Now()

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: prompt}},
		MaxTokens: externalAnthropicDefaultMaxTokens,
		Stream:    false,
	}
	kiroPayload := buildSVGTestPayload(openaiReq, actualModel, mode, effort)

	var content string
	var inTok, outTok int
	var stopReason string
	var attemptProduced bool
	callback := &KiroStreamCallback{
		OnText:         func(text string, _ bool) { content += text; attemptProduced = true },
		OnToolUse:      func(_ KiroToolUse) { attemptProduced = true },
		OnComplete:     func(in, out int) { inTok, outTok = in, out },
		OnStopReason:   func(reason string) { stopReason = reason },
		OnError:        func(_ error) {},
		OnCredits:      func(_ float64) {},
		OnContextUsage: func(_ float64) {},
		OnOutput:       func() { attemptProduced = true },
		HasOutput:      func() bool { return attemptProduced },
		OnReset: func() {
			content, inTok, outTok, stopReason, attemptProduced = "", 0, 0, "", false
		},
	}

	// Retried rather than dispatched once: the gateway this test runs through
	// round-robins across backends that disagree on the request shape, so an
	// intermittent 400 is a property of the gateway, not of the model. Without
	// the retry that flakiness is recorded as a model failure and skews the
	// comparison. The retry only fires while nothing has been emitted.
	err := dispatchSVGTestWithRetry(r.Context(), account, kiroPayload, callback, dispatchChat, svgTestMaxAttempts)
	elapsed := time.Since(start).Milliseconds()

	// The prompt, not a client-chosen id, identifies the comparison group: two
	// runs of the same prompt land together even if they were started from
	// different browser tabs or at different times.
	promptKey := svgPromptKey(prompt)

	if err != nil {
		persistSVGTestResult(prompt, model, svgTestEntry{
			AccountID:   account.ID,
			AccountName: accountLabel(account),
			Provider:    providerLabelOf(account.Provider),
			Dialect:     externalAPIDialect(account),
			Mode:        mode,
			Effort:      effort,
			Success:     false,
			Error:       err.Error(),
			ElapsedMs:   elapsed,
			TokensUsed:  inTok + outTok,
		})
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":     false,
			"error":       err.Error(),
			"model":       model,
			"mode":        mode,
			"effort":      effort,
			"promptKey":   promptKey,
			"accountId":   account.ID,
			"accountName": accountLabel(account),
			"elapsedMs":   elapsed,
		})
		return
	}

	svg, failReason := classifySVGReplyWithStop(content, stopReason)
	score, scoreReasons := scoreSVG(svg)

	persistSVGTestResult(prompt, model, svgTestEntry{
		AccountID:   account.ID,
		AccountName: accountLabel(account),
		Provider:    providerLabelOf(account.Provider),
		Dialect:     externalAPIDialect(account),
		Mode:        mode,
		Effort:      effort,
		Success:     svg != "",
		SVG:         svg,
		Error:       failReason,
		ElapsedMs:   elapsed,
		TokensUsed:  inTok + outTok,
	})

	// Score and efficiency are computed here for the live response and again on
	// every read of the stored file, from the same rubric, so what the operator
	// sees immediately and what the archive reports later cannot disagree.
	efficiency, hasEfficiency := svgTestEfficiency(score, inTok+outTok)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":       svg != "",
		"svg":           svg,
		"error":         failReason,
		"rawReply":      content,
		"model":         model,
		"mode":          mode,
		"effort":        effort,
		"promptKey":     promptKey,
		"accountId":     account.ID,
		"accountName":   accountLabel(account),
		"elapsedMs":     elapsed,
		"tokensUsed":    inTok + outTok,
		"score":         score,
		"scoreReasons":  scoreReasons,
		"efficiency":    efficiency,
		"hasEfficiency": hasEfficiency,
	})
}
