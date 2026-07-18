// Package proxy — Codex (ChatGPT subscription) Responses API adapter.
//
// Accounts with AuthMethod == "codex" forward requests to OpenAI's Codex
// backend at https://chatgpt.com/backend-api/codex/responses using the
// /v1/responses (Responses API) endpoint. Authentication uses the OAuth
// access_token (Bearer) plus the chatgpt-account-id header extracted from
// the JWT.
//
// The KiroPayload intermediate representation is translated into the
// Responses API "input" array shape, and the upstream SSE response
// (response.output_text.delta / response.reasoning.delta /
// response.output_item.done tool_call / response.completed) is replayed
// through the same KiroStreamCallback used by CallKiroAPI, so the existing
// Claude/OpenAI response handlers work unchanged.
//
// Downstream coalescing: token deltas are buffered and flushed on a 24ms
// tick (or when buffer reaches ~4KB) to cut per-token syscall + JSON
// marshal overhead by ~50-100x. This is what makes concurrent claude-cli
// sessions against reasoning models (gpt-5.6-sol) not lag the proxy.
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"omniproxy/auth"
	"omniproxy/config"
	"omniproxy/logger"
	"time"
)

// codexAuthMethod marks an account that authenticates via ChatGPT
// subscription OAuth and routes through OpenAI's Codex /v1/responses backend.
const codexAuthMethod = "codex"

// codexDefaultBaseURL is the upstream endpoint Codex CLI uses for
// ChatGPT-subscription logins. The full request path is
// {BaseURL}/backend-api/codex/responses.
const codexDefaultBaseURL = "https://chatgpt.com"

// isCodexAccount reports whether the account routes to the Codex
// /v1/responses backend via ChatGPT subscription OAuth.
func isCodexAccount(account *config.Account) bool {
	return account != nil && account.AuthMethod == codexAuthMethod
}

// codexBaseURL resolves the upstream Codex endpoint. Per-account BaseURL
// overrides the default (e.g. for testing or routing via a regional proxy).
func codexBaseURL(account *config.Account) string {
	if account != nil && strings.TrimSpace(account.BaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	}
	return codexDefaultBaseURL
}

// CallExternalCodex forwards a KiroPayload to the Codex /v1/responses
// endpoint and replays the response through callback. Mirrors
// CallKiroAPI/CallExternalOpenAI's contract: nil on success, error on
// failure. HTTP 401/403 are surfaced directly so the pool's auth-failure
// handling can refresh or disable the account.
//
// Token refresh + chatgpt-account-id re-extraction is handled here (not in
// ensureValidToken) because the account_id is JWT-bound and may rotate
// when OpenAI re-issues the access token.
func CallExternalCodex(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if account == nil {
		return fmt.Errorf("codex call: account is nil")
	}
	accessToken := strings.TrimSpace(account.AccessToken)
	if accessToken == "" {
		return fmt.Errorf("codex call: account %s has no access token", account.Email)
	}
	accountID := strings.TrimSpace(account.ChatGPTAccountID)
	if accountID == "" {
		// Lazy-extract from current access token if missing (e.g. account
		// imported via credentials import without going through OAuth).
		accountID = auth.ExtractCodexAccountIDPublic(accessToken)
		if accountID != "" {
			account.ChatGPTAccountID = accountID
			_ = config.UpdateAccountChatGPTAccountID(account.ID, accountID)
		}
	}
	if accountID == "" {
		return fmt.Errorf("codex call: account %s has no chatgpt_account_id (re-login via OAuth)", account.Email)
	}

	body, err := kiroPayloadToCodexResponsesRequest(payload, account)
	if err != nil {
		return fmt.Errorf("codex call build request: %w", err)
	}
	// Always stream — the non-stream handler buffers via the callback.
	body["stream"] = true

	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("codex call marshal: %w", err)
	}

	endpoint := codexBaseURL(account) + "/backend-api/codex/responses"
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("codex call new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("chatgpt-account-id", accountID)
	// Codex CLI sends these headers; matching them keeps the upstream
	// sticky-routing and turn-state logic happy.
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("User-Agent", "codex_cli_rs/0.0.0 omniproxy/1.0")

	client := GetClientForProxy(ResolveAccountProxyURL(account))
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("codex call %s: %w", account.Email, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		err := fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, account.Email, truncateErrBody(errBody))
		// 401/403 → caller refreshes token and retries; 402/429 → caller
		// rotates account. Other 5xx are transient.
		return err
	}

	// Capture Codex rate-limit / usage headers before consuming the body.
	// These are returned on every /v1/responses response and let the admin
	// UI show real-time usage %, plan type, and reset time per account.
	captureCodexUsageHeaders(account, resp.Header)

	contentType := resp.Header.Get("Content-Type")
	// We always send stream=true, so the upstream returns SSE. Some
	// upstreams (chatgpt.com) omit Content-Type or return a generic
	// type; detect SSE by peeking at the first bytes instead.
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return parseCodexResponsesSSE(resp.Body, callback)
	}
	// Peek at the first byte to distinguish SSE ("event:...") from JSON ("{").
	br := bufio.NewReader(resp.Body)
	first, err := br.Peek(1)
	if err != nil && err != io.EOF {
		return fmt.Errorf("codex response peek: %w", err)
	}
	if len(first) > 0 && (first[0] == 'e' || first[0] == 'd') {
		// "event:" or "data:" — SSE stream
		return parseCodexResponsesSSE(br, callback)
	}
	// Non-SSE fallback: a single JSON Responses object.
	return parseCodexResponsesJSON(br, callback)
}

// captureCodexUsageHeaders reads x-codex-* headers from the upstream
// response and persists them to the account record. Called on every
// successful Codex request so the admin UI always has fresh usage data.
func captureCodexUsageHeaders(account *config.Account, hdr http.Header) {
	if account == nil || !isCodexAccount(account) {
		return
	}
	planType := hdr.Get("x-codex-plan-type")
	activeLimit := hdr.Get("x-codex-active-limit")
	primaryPct := atoiSafe(hdr.Get("x-codex-primary-used-percent"))
	secondaryPct := atoiSafe(hdr.Get("x-codex-secondary-used-percent"))
	primaryWindow := atoiSafe(hdr.Get("x-codex-primary-window-minutes"))
	primaryResetAt := atoi64Safe(hdr.Get("x-codex-primary-reset-at"))
	secondaryResetAt := atoi64Safe(hdr.Get("x-codex-secondary-reset-at"))
	creditsBalance := atoiSafe(hdr.Get("x-codex-credits-balance"))
	creditsUnlimited := hdr.Get("x-codex-credits-unlimited") == "True" ||
		hdr.Get("x-codex-credits-unlimited") == "true"

	// Only persist if we got at least the plan type (indicates headers present)
	if planType == "" && activeLimit == "" && primaryPct == 0 {
		logger.Debugf("[Codex] no usage headers for %s (planType=%q activeLimit=%q primaryPct=%d)",
			account.Email, planType, activeLimit, primaryPct)
		return
	}
	logger.Infof("[Codex] captured usage for %s: plan=%s limit=%s primary=%d%% credits=%d",
		account.Email, planType, activeLimit, primaryPct, creditsBalance)
	_ = config.UpdateAccountCodexUsage(
		account.ID, planType, activeLimit,
		primaryPct, secondaryPct, primaryWindow,
		primaryResetAt, secondaryResetAt,
		creditsBalance, creditsUnlimited,
	)
}

// atoiSafe parses an integer, returning 0 on error.
func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// atoi64Safe parses an int64, returning 0 on error.
func atoi64Safe(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// kiroPayloadToCodexResponsesRequest converts the KiroPayload into a
// /v1/responses request body. The Responses API uses an "input" array of
// typed items (message / function_call / function_call_output) rather than
// chat-completions "messages".
func kiroPayloadToCodexResponsesRequest(payload *KiroPayload, account *config.Account) (map[string]interface{}, error) {
	if payload == nil {
		return nil, fmt.Errorf("nil payload")
	}

	modelID := strings.TrimSpace(payload.OriginalModel)
	if modelID == "" {
		modelID = strings.TrimSpace(payload.ConversationState.CurrentMessage.UserInputMessage.ModelID)
	}
	if modelID == "" {
		modelID = "gpt-5.6-sol"
	}
	modelID = stripInternalModelPrefix(modelID)
	if account != nil {
		modelID = resolveExternalModelID(account, modelID)
	}

	input := make([]map[string]interface{}, 0, 8)
	history := payload.ConversationState.History

	// Detect & extract the system priming pair injected by the translators
	// (history[0]=user(systemPrompt), history[1]=assistant("I will follow...")).
	// Responses API has a top-level "instructions" field for system priming.
	instructions := ""
	if len(history) >= 2 {
		first := history[0]
		second := history[1]
		if first.UserInputMessage != nil && second.AssistantResponseMessage != nil &&
			strings.Contains(strings.ToLower(strings.TrimSpace(second.AssistantResponseMessage.Content)), "i will follow") {
			instructions = strings.TrimSpace(first.UserInputMessage.Content)
			history = history[2:]
		}
	}

	// If no priming pair was detected, look for a leading user-only system
	// prompt (some translators inject it that way for non-Claude clients).
	if instructions == "" && len(history) > 0 {
		if history[0].UserInputMessage != nil && strings.HasPrefix(strings.TrimSpace(history[0].UserInputMessage.Content), "You are ") {
			instructions = strings.TrimSpace(history[0].UserInputMessage.Content)
			history = history[1:]
		}
	}

	for _, h := range history {
		if h.UserInputMessage != nil {
			um := h.UserInputMessage
			// Tool results → function_call_output items.
			if um.UserInputMessageContext != nil && len(um.UserInputMessageContext.ToolResults) > 0 {
				for _, tr := range um.UserInputMessageContext.ToolResults {
					text := ""
					if len(tr.Content) > 0 {
						text = tr.Content[0].Text
					}
					input = append(input, map[string]interface{}{
						"type": "function_call_output",
						"call_id": tr.ToolUseID,
						"output":  text,
					})
				}
			}
			if strings.TrimSpace(um.Content) != "" || len(um.Images) > 0 {
				input = append(input, map[string]interface{}{
					"type":    "message",
					"role":    "user",
					"content": codexMessageContent(um.Content, um.Images),
				})
			}
		} else if h.AssistantResponseMessage != nil {
			am := h.AssistantResponseMessage
			// Assistant text → message item. Tool uses → function_call items
			// (separate from the message; Responses API models them as
			// parallel output items in the same turn).
			if strings.TrimSpace(am.Content) != "" {
				input = append(input, map[string]interface{}{
					"type":    "message",
					"role":    "assistant",
					"content": am.Content,
				})
			}
			for _, tu := range am.ToolUses {
				args, _ := json.Marshal(tu.Input)
				input = append(input, map[string]interface{}{
					"type":       "function_call",
					"call_id":    tu.ToolUseID,
					"name":       restoreToolName(payload, tu.Name),
					"arguments":  string(args),
				})
			}
		}
	}

	// Current user message + tool results.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) > 0 {
		for _, tr := range cur.UserInputMessageContext.ToolResults {
			text := ""
			if len(tr.Content) > 0 {
				text = tr.Content[0].Text
			}
			input = append(input, map[string]interface{}{
				"type": "function_call_output",
				"call_id": tr.ToolUseID,
				"output":  text,
			})
		}
	}
	if strings.TrimSpace(cur.Content) != "" || len(cur.Images) > 0 {
		input = append(input, map[string]interface{}{
			"type":    "message",
			"role":    "user",
			"content": codexMessageContent(cur.Content, cur.Images),
		})
	}

	body := map[string]interface{}{
		"model":  modelID,
		"input":  input,
		"stream": true,
		"store":  false,
	}
	if instructions != "" {
		body["instructions"] = instructions
	}

	// Tools — Responses API uses a flat shape (name/description/parameters
	// at top level, not nested under "function").
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(cur.UserInputMessageContext.Tools))
		for _, tw := range cur.UserInputMessageContext.Tools {
			tools = append(tools, map[string]interface{}{
				"type":        "function",
				"name":        restoreToolName(payload, tw.ToolSpecification.Name),
				"description": tw.ToolSpecification.Description,
				"parameters":  tw.ToolSpecification.InputSchema.JSON,
			})
		}
		body["tools"] = tools
	}

	if payload.InferenceConfig != nil {
		if payload.InferenceConfig.Temperature > 0 {
			body["temperature"] = payload.InferenceConfig.Temperature
		}
		if payload.InferenceConfig.TopP > 0 {
			body["top_p"] = payload.InferenceConfig.TopP
		}
		if payload.InferenceConfig.ReasoningEffort != "" {
			body["reasoning"] = map[string]string{"effort": payload.InferenceConfig.ReasoningEffort}
		}
	}

	return body, nil
}

// codexMessageContent builds the Responses API "content" value for a
// message item. Plain text → string; with images → array of
// {type:"input_text"|"input_image"} parts.
func codexMessageContent(text string, images []KiroImage) interface{} {
	if len(images) == 0 {
		return text
	}
	parts := make([]map[string]interface{}, 0, len(images)+1)
	if strings.TrimSpace(text) != "" {
		parts = append(parts, map[string]interface{}{
			"type": "input_text",
			"text": text,
		})
	}
	for _, image := range images {
		format := strings.TrimSpace(image.Format)
		if format == "" {
			format = "png"
		}
		parts = append(parts, map[string]interface{}{
			"type": "input_image",
			"image_url": "data:image/" + format + ";base64," + image.Source.Bytes,
		})
	}
	return parts
}

// parseCodexResponsesSSE parses the Codex Responses API SSE stream and
// drives the KiroStreamCallback. Output text/reasoning deltas are
// accumulated and emitted to callback.OnText; tool calls are accumulated
// per call_id and emitted via callback.OnToolUse on completion.
//
// Per-token overhead is bounded by reading line-by-line with bufio.Reader
// (one JSON parse per SSE event — same as OpenAI chat-completions path).
// Downstream coalescing happens at the handler side, not here.
func parseCodexResponsesSSE(body io.Reader, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}
	br := bufio.NewReader(body)

	var inputTokens, outputTokens int
	var totalCredits float64
	// toolAccums accumulates arguments per call_id; emitted on
	// response.output_item.done with type=function_call.
	toolAccums := make(map[string]*codexToolAccum)

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if strings.TrimSpace(line) != "" {
					processCodexSSELine(line, callback, toolAccums, &inputTokens, &outputTokens)
				}
				break
			}
			return fmt.Errorf("codex SSE read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// event: / id: / comment lines — ignore (event type is
			// duplicated inside the JSON payload's "type" field).
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		processCodexSSELine("data: "+data, callback, toolAccums, &inputTokens, &outputTokens)
	}

	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}
	return nil
}

// codexToolAccum buffers function_call arguments across multiple
// response.function_call_arguments.delta events until the final
// response.output_item.done arrives.
type codexToolAccum struct {
	ID   string
	Name string
	Args strings.Builder
}

// processCodexSSELine parses one Responses API SSE data line and dispatches
// to the callback. Kept separate from parseCodexResponsesSSE for testability.
func processCodexSSELine(line string, callback *KiroStreamCallback, toolAccums map[string]*codexToolAccum, inputTokens, outputTokens *int) {
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "[DONE]" {
		return
	}
	var evt struct {
		Type string `json:"type"`
		// output_text.delta
		Delta string `json:"delta"`
		// response.reasoning.delta (some Codex builds use "delta" too)
		Text  string `json:"text"`
		// function_call output item
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		// response.completed / response.in_progress usage
		Response struct {
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
		// item (for response.output_item.done with function_call)
		Item struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(data), &evt); err != nil {
		return
	}

	switch evt.Type {
	case "response.output_text.delta":
		if evt.Delta != "" && callback.OnText != nil {
			callback.OnText(evt.Delta, false)
		}
	case "response.reasoning.delta":
		// Codex emits reasoning as "delta" or "text" depending on build.
		text := evt.Delta
		if text == "" {
			text = evt.Text
		}
		if text != "" && callback.OnText != nil {
			callback.OnText(text, true)
		}
	case "response.output_text.done":
		// Final text — already streamed via deltas. No-op.
	case "response.output_item.added":
		// A function_call item started — begin accumulating arguments.
		if evt.Item.Type == "function_call" && evt.Item.CallID != "" {
			toolAccums[evt.Item.CallID] = &codexToolAccum{
				ID:   evt.Item.CallID,
				Name: evt.Item.Name,
			}
		}
	case "response.function_call_arguments.delta":
		if evt.CallID != "" {
			acc, ok := toolAccums[evt.CallID]
			if !ok {
				acc = &codexToolAccum{ID: evt.CallID}
				toolAccums[evt.CallID] = acc
			}
			acc.Args.WriteString(evt.Delta)
		}
	case "response.output_item.done":
		// Final tool call — emit via callback.
		if evt.Item.Type == "function_call" && evt.Item.CallID != "" {
			acc, ok := toolAccums[evt.Item.CallID]
			if !ok {
				acc = &codexToolAccum{ID: evt.Item.CallID, Name: evt.Item.Name}
			}
			args := acc.Args.String()
			if evt.Item.Arguments != "" {
				args = evt.Item.Arguments
			}
			name := evt.Item.Name
			if name == "" {
				name = acc.Name
			}
			var input map[string]interface{}
			if args != "" {
				_ = json.Unmarshal([]byte(args), &input)
			}
			if input == nil {
				input = map[string]interface{}{}
			}
			if callback.OnToolUse != nil {
				callback.OnToolUse(KiroToolUse{
					ToolUseID: evt.Item.CallID,
					Name:      name,
					Input:     input,
				})
			}
			delete(toolAccums, evt.Item.CallID)
		}
	case "response.completed":
		if evt.Response.Usage != nil {
			*inputTokens = evt.Response.Usage.InputTokens
			*outputTokens = evt.Response.Usage.OutputTokens
		}
	case "response.failed", "error":
		// Surface error to caller via OnError if attached.
		if callback.OnError != nil {
			callback.OnError(fmt.Errorf("codex stream error: %s", data))
		}
	}
}

// parseCodexResponsesJSON handles the rare non-SSE Codex response (when
// stream=true was ignored upstream). Reads a single Responses API JSON
// object and drives the callback.
func parseCodexResponsesJSON(body io.Reader, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("codex JSON read: %w", err)
	}
	var resp struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("codex JSON parse: %w", err)
	}
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Text != "" && callback.OnText != nil {
					callback.OnText(c.Text, false)
				}
			}
		case "function_call":
			var input map[string]interface{}
			if item.Arguments != "" {
				_ = json.Unmarshal([]byte(item.Arguments), &input)
			}
			if input == nil {
				input = map[string]interface{}{}
			}
			if callback.OnToolUse != nil {
				callback.OnToolUse(KiroToolUse{
					ToolUseID: item.CallID,
					Name:      item.Name,
					Input:     input,
				})
			}
		}
	}
	if callback.OnComplete != nil {
		in, out := 0, 0
		if resp.Usage != nil {
			in = resp.Usage.InputTokens
			out = resp.Usage.OutputTokens
		}
		callback.OnComplete(in, out)
	}
	return nil
}

// ==================== Downstream coalescing ====================
//
// coalesceCodexDeltas wraps a KiroStreamCallback so that rapid OnText
// calls (per-token from Codex SSE) are batched into larger chunks before
// reaching the underlying callback. This cuts the downstream
// json.Marshal + Flush syscall count by ~50-100x for reasoning models
// (gpt-5.6-sol emits 10k-50k tokens per request).
//
// Coalesce rules:
//   - Flush immediately when buffer reaches coalesceMaxBytes (4 KB).
//   - Flush on a 24ms tick (coalesceTickInterval).
//   - Flush immediately on OnToolUse / OnComplete / OnError (terminal
//     events must not be delayed).
//   - Thinking and non-thinking text are kept in separate buffers so the
//     handler's thinking-block framing stays correct.
//
// The wrapper is safe for concurrent use only from a single goroutine
// (the streaming loop); it is not safe for shared use across requests.
type coalescer struct {
	target           *KiroStreamCallback
	textBuf          strings.Builder
	thinkingBuf      strings.Builder
	lastFlush        time.Time
	tick             *time.Timer
	tickCh           <-chan time.Time
	flushed          bool
}

const (
	coalesceTickInterval = 24 * time.Millisecond
	coalesceMaxBytes     = 4 * 1024
)

// newCodexCoalescer wraps a target callback with downstream coalescing.
// The returned callback must be driven from a single goroutine.
func newCodexCoalescer(target *KiroStreamCallback) *KiroStreamCallback {
	if target == nil {
		target = &KiroStreamCallback{}
	}
	c := &coalescer{
		target:    target,
		lastFlush: time.Now(),
	}
	// Pre-arm a lazy tick: we don't allocate a timer per token. Instead
	// we check elapsed time on each OnText and flush if the tick has
	// elapsed. This avoids goroutine/timer overhead per request.
	return &KiroStreamCallback{
		OnText:          c.onText,
		OnToolUse:       c.onToolUse,
		OnComplete:      c.onComplete,
		OnCredits:       target.OnCredits,
		OnContextUsage:  target.OnContextUsage,
		OnError:         c.onError,
	}
}

// onText buffers text and flushes when the tick interval or byte budget
// is reached. Thinking and non-thinking text are flushed independently.
func (c *coalescer) onText(text string, isThinking bool) {
	if text == "" {
		return
	}
	if isThinking {
		c.thinkingBuf.WriteString(text)
	} else {
		c.textBuf.WriteString(text)
	}
	// Flush on byte budget or tick.
	now := time.Now()
	if c.textBuf.Len() >= coalesceMaxBytes || c.thinkingBuf.Len() >= coalesceMaxBytes || now.Sub(c.lastFlush) >= coalesceTickInterval {
		c.flush()
	}
}

func (c *coalescer) onToolUse(tu KiroToolUse) {
	c.flush()
	if c.target.OnToolUse != nil {
		c.target.OnToolUse(tu)
	}
}

func (c *coalescer) onComplete(inTok, outTok int) {
	c.flush()
	if c.target.OnComplete != nil {
		c.target.OnComplete(inTok, outTok)
	}
}

func (c *coalescer) onError(err error) {
	c.flush()
	if c.target.OnError != nil {
		c.target.OnError(err)
	}
}

// flush emits any buffered text/thinking to the target callback. Safe to
// call when buffers are empty (no-op).
func (c *coalescer) flush() {
	if c.textBuf.Len() > 0 && c.target.OnText != nil {
		c.target.OnText(c.textBuf.String(), false)
		c.textBuf.Reset()
	}
	if c.thinkingBuf.Len() > 0 && c.target.OnText != nil {
		c.target.OnText(c.thinkingBuf.String(), true)
		c.thinkingBuf.Reset()
	}
	c.lastFlush = time.Now()
	c.flushed = true
}

// dispatchCodex routes a request to the Codex backend when the account is
// a ChatGPT-subscription Codex account, otherwise falls through to the
// existing external/Kiro dispatch. Called from the same dispatchChat site
// the handlers already use.
func dispatchCodex(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if isCodexAccount(account) {
		return CallExternalCodex(account, payload, callback)
	}
	return dispatchChat(account, payload, callback)
}

// ==================== Admin: import / refresh ====================

// extractCodexAccountIDForImport is a thin wrapper so handler.go can
// extract the chatgpt_account_id from a freshly-imported access token
// without importing the auth package directly (handler already imports
// auth, but this keeps the call site self-documenting).
func extractCodexAccountIDForImport(accessToken string) string {
	return auth.ExtractCodexAccountIDPublic(accessToken)
}

// refreshCodexAccountID re-extracts chatgpt_account_id from the current
// access token after a refresh and persists it. Also refreshes the JWT
// profile fields (email, name, plan_type) in case the user upgraded or
// the account rotated. Called by handler.go after a successful Codex
// token refresh.
func refreshCodexAccountID(account *config.Account) {
	if account == nil || account.AuthMethod != codexAuthMethod {
		return
	}
	info := auth.ExtractCodexJWTInfoPublic(account.AccessToken)
	if info.AccountID != "" && info.AccountID != account.ChatGPTAccountID {
		account.ChatGPTAccountID = info.AccountID
		_ = config.UpdateAccountChatGPTAccountID(account.ID, info.AccountID)
		logger.Infof("[Codex] Refreshed chatgpt_account_id for %s", account.Email)
	}
	// Refresh profile fields if they changed (e.g. plan upgrade).
	if info.Email != "" || info.Name != "" || info.PlanType != "" {
		changed := info.Email != account.CodexEmail ||
			info.Name != account.CodexName ||
			info.PlanType != account.CodexPlanType
		if changed {
			account.CodexEmail = info.Email
			account.CodexName = info.Name
			account.CodexPlanType = info.PlanType
			_ = config.UpdateAccountCodexProfile(account.ID, info.Email, info.Name, info.PlanType)
		}
	}
}

// codexSubscriptionModels returns the canonical model list exposed by
// OpenAI's Codex backend for ChatGPT subscription logins. These are the
// model IDs the upstream /v1/responses endpoint accepts. The proxy seeds
// its routing cache with this list so claude-cli / openai clients can
// request any of them by name.
//
// Source: Codex CLI default model registry + observed /v1/responses
// accept-list. Updated when OpenAI ships new subscription-tier models.
func codexSubscriptionModels() []ModelInfo {
	type lim struct {
		MaxInputTokens  int
		MaxOutputTokens int
	}
	specs := []struct {
		id, name, desc string
		lim
	}{
		{"gpt-5.6-sol", "GPT-5.6 Sol", "Codex reasoning model (default)", lim{300000, 128000}},
		{"gpt-5.1", "GPT-5.1", "Codex fast model", lim{272000, 128000}},
		{"gpt-5", "GPT-5", "GPT-5 base", lim{272000, 128000}},
		{"o4", "o4", "OpenAI o4 reasoning", lim{200000, 100000}},
		{"o3", "o3", "OpenAI o3 reasoning", lim{200000, 100000}},
		{"codex-mini-latest", "Codex Mini", "Codex mini (latest)", lim{200000, 100000}},
	}
	out := make([]ModelInfo, 0, len(specs))
	for _, s := range specs {
		m := ModelInfo{
			ModelId:        s.id,
			ModelName:      s.name,
			Description:    s.desc,
			InputTypes:     []string{"text", "image"},
			RateMultiplier: 1.0,
		}
		m.TokenLimits = &struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		}{MaxInputTokens: s.MaxInputTokens, MaxOutputTokens: s.MaxOutputTokens}
		out = append(out, m)
	}
	return out
}
