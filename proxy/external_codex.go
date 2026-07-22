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
	cryptoRand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

// codexCacheKey derives a stable, opaque cache-routing key from the
// system prompt (instructions). The key is shared across all conversations
// that use the same instructions, so the backend's prompt cache can serve
// hits regardless of which conversation is making the request.
//
// This is critical for multi-agent scenarios: 10 agents using the same
// system prompt share 1 cache entry per account instead of 10 separate
// entries. Warming cost drops from O(accounts × conversations) to
// O(accounts × system_prompts).
//
// The key is used for:
//   - prompt_cache_key in the request body (cache prefix matching)
//   - session-id / thread-id headers (sticky routing to same machine)
//   - cacheSticky map in AccountPool (pin to same account)
//
// Returns empty string when instructions are empty (cache won't work
// without a system prompt — the backend only caches the instructions
// field, not input content).
func codexCacheKey(instructions string) string {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return ""
	}
	// Normalize: trim + collapse whitespace so minor formatting
	// differences (extra spaces, line endings) don't create separate
	// cache entries for semantically identical prompts.
	normalized := strings.Join(strings.Fields(instructions), " ")
	h := sha256.Sum256([]byte("codex-cache:" + normalized))
	return hex.EncodeToString(h[:16]) // 32-char hex, stable per instructions
}

// codexSessionKey derives a per-conversation routing key for the
// session-id / thread-id headers. This is separate from the cache key
// so that conversations with the same system prompt share cache but
// still get distinct session routing (avoids backend conflating
// separate conversations on the same machine).
func codexSessionKey(conversationID string) string {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return ""
	}
	h := sha256.Sum256([]byte("codex-session:" + conversationID))
	return hex.EncodeToString(h[:16])
}

// WarmCodexCache sends a minimal warmup request to the Codex backend for
// the given account + cache key so the upstream prompt cache writes the
// instructions prefix. After this call, a subsequent real request with the
// same instructions + prompt_cache_key will hit the cache instead of
// re-processing the full prefix.
//
// The warmup request reuses the same instructions and prompt_cache_key as
// the real request but sends a trivial user message ("hi") with a tiny
// max_output_tokens budget. This minimises token cost while still writing
// the instructions prefix to the account's cache.
//
// Returns nil on success (cache warmed), or the error from the upstream.
// Errors are non-fatal — the caller should proceed with the real request
// regardless (it will just be a cache miss).
func WarmCodexCache(account *config.Account, instructions, modelID, cacheKey string) error {
	if account == nil || cacheKey == "" {
		return nil
	}
	accessToken := strings.TrimSpace(account.AccessToken)
	if accessToken == "" {
		return fmt.Errorf("codex warmup: account %s has no access token", account.Email)
	}
	accountID := strings.TrimSpace(account.ChatGPTAccountID)
	if accountID == "" {
		return fmt.Errorf("codex warmup: account %s has no chatgpt_account_id", account.Email)
	}

	// Minimal body: same instructions + prompt_cache_key so the cache
	// router groups this with the real request. Tiny input to minimise
	// token spend. We omit max_output_tokens because the ChatGPT
	// subscription backend rejects it ("Unsupported parameter"); the
	// backend caps the response automatically for "hi".
	body := map[string]interface{}{
		"model":            modelID,
		"instructions":     instructions,
		"input":            []map[string]interface{}{{"type": "message", "role": "user", "content": "hi"}},
		"stream":           true,
		"store":            false,
		"prompt_cache_key": cacheKey,
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("codex warmup marshal: %w", err)
	}

	endpoint := codexBaseURL(account) + "/backend-api/codex/responses"
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("codex warmup new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("chatgpt-account-id", accountID)
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("User-Agent", "codex_cli_rs/0.0.0 omniproxy/1.0")
	req.Header.Set("session-id", cacheKey)
	req.Header.Set("thread-id", cacheKey)

	client := GetClientForProxy(ResolveAccountProxyURL(account))
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("codex warmup request: %w", err)
	}
	defer resp.Body.Close()

	// Drain the SSE stream to completion so the backend records the
	// cache write. We don't care about the content — only that the
	// response.completed event fires (which triggers the cache write).
	if resp.StatusCode >= 400 {
		bodySnippet := make([]byte, 256)
		n, _ := resp.Body.Read(bodySnippet)
		return fmt.Errorf("codex warmup: HTTP %d: %s", resp.StatusCode, string(bodySnippet[:n]))
	}

	// Read until EOF or timeout. The stream is small (max_output_tokens=1)
	// so this completes in ~1-3s.
	io.Copy(io.Discard, resp.Body)
	return nil
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

	// prompt_cache_key: derived from the instructions (system prompt) so
	// that all conversations using the same system prompt share a single
	// cache entry per account. This is critical for multi-agent scenarios
	// where many agents/conversations use the same instructions — they
	// all benefit from the same warmed cache instead of each needing
	// their own warmup.
	//
	// When instructions are empty (no system prompt), the backend won't
	// cache anyway (it only caches the instructions field, not input
	// content), so we skip setting the key.
	if instr, ok := body["instructions"].(string); ok && instr != "" {
		body["prompt_cache_key"] = codexCacheKey(instr)
	}

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

	// Prompt-cache sticky routing: Codex CLI sends session-id and thread-id
	// headers so the backend can route consecutive turns from the same
	// conversation to the same inference machine (required for prompt cache
	// hits). Without these, every request lands on a random machine and
	// cached_tokens is always 0.
	//
	// session-id / thread-id: per-conversation (hash of conversation ID)
	// so turns within the same conversation route to the same machine.
	// This is separate from prompt_cache_key (hash of instructions) so
	// that conversations sharing the same system prompt can still have
	// distinct session routing.
	if payload != nil {
		convID := strings.TrimSpace(payload.ConversationState.ConversationID)
		if convID != "" {
			sessionKey := codexSessionKey(convID)
			req.Header.Set("session-id", sessionKey)
			req.Header.Set("thread-id", sessionKey)
		}
	}

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
		// "event:" or "data:" — SSE stream. Pass resp.Body (io.ReadCloser)
		// so the SSE idle watchdog can close it on timeout.
		return parseCodexResponsesSSE(resp.Body, callback)
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
	// Credits headers are only sent by Codex pay-as-you-go backends.
	// ChatGPT Plus subscription responses omit them entirely — detect
	// header presence so we don't clobber a previously-captured balance
	// with a misleading "0 / not unlimited" snapshot.
	creditsBalanceHdr := hdr.Get("x-codex-credits-balance")
	creditsUnlimitedHdr := hdr.Get("x-codex-credits-unlimited")
	creditsKnown := creditsBalanceHdr != "" || creditsUnlimitedHdr != ""
	creditsBalance := atoiSafe(creditsBalanceHdr)
	creditsUnlimited := creditsUnlimitedHdr == "True" ||
		creditsUnlimitedHdr == "true"

	// Only persist if we got at least the plan type (indicates headers present)
	if planType == "" && activeLimit == "" && primaryPct == 0 {
		logger.Debugf("[Codex] no usage headers for %s (planType=%q activeLimit=%q primaryPct=%d)",
			account.Email, planType, activeLimit, primaryPct)
		return
	}
	logger.Infof("[Codex] captured usage for %s: plan=%s limit=%s primary=%d%% credits=%d (known=%v)",
		account.Email, planType, activeLimit, primaryPct, creditsBalance, creditsKnown)
	_ = config.UpdateAccountCodexUsage(
		account.ID, planType, activeLimit,
		primaryPct, secondaryPct, primaryWindow,
		primaryResetAt, secondaryResetAt,
		creditsBalance, creditsUnlimited, creditsKnown,
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
		// NOTE: temperature and top_p are intentionally NOT forwarded
		// to the ChatGPT backend. The Codex/ChatGPT responses API rejects
		// these parameters with HTTP 400 "Unsupported parameter: temperature"
		// for GPT-5.x reasoning models. Only reasoning.effort is supported.
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

	// SSE idle watchdog: kills the connection when no ``data:`` line arrives
	// within the configured idle window. Catches "200 OK but silent" hangs
	// that a byte-level idle reader cannot detect (upstream keepalive
	// comments reset byte-level timers without carrying payload). The
	// watchdog closes the underlying body to unblock the pending ReadString.
	var watchdog *sseIdleWatchdog
	if rc, ok := body.(io.ReadCloser); ok {
		watchdog = newSSEIdleWatchdog(rc)
		if watchdog != nil {
			watchdog.Start()
			defer watchdog.Stop()
		}
	}

	var inputTokens, outputTokens int
	var totalCredits float64
	// toolAccums accumulates arguments per call_id; emitted on
	// response.output_item.done with type=function_call.
	toolAccums := make(map[string]*codexToolAccum)

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if watchdog != nil && watchdog.TimedOut() {
				return ErrStreamIdleTimeout
			}
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
		// Real data line — reset the SSE idle watchdog timer.
		if watchdog != nil {
			watchdog.DataReceived()
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
				// OpenAI Responses API emits input_tokens_details.cached_tokens
				// when the upstream prompt cache served part of the input.
				// Forwarding it lets the handler report real cache hits to the
				// client instead of the locally-simulated promptCacheTracker.
				InputTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"input_tokens_details,omitempty"`
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
			// Forward upstream prompt-cache hit so the handler reports real
			// cached_tokens to the client instead of the simulated tracker.
			if evt.Response.Usage.InputTokensDetails != nil &&
				evt.Response.Usage.InputTokensDetails.CachedTokens > 0 &&
				callback.OnCacheRead != nil {
				callback.OnCacheRead(evt.Response.Usage.InputTokensDetails.CachedTokens)
			}
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
			InputTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details,omitempty"`
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
	// Forward upstream prompt-cache hit (non-stream path). Fired
	// independently of OnComplete so a callback that only cares about
	// cache numbers still receives them.
	if resp.Usage != nil &&
		resp.Usage.InputTokensDetails != nil &&
		resp.Usage.InputTokensDetails.CachedTokens > 0 &&
		callback.OnCacheRead != nil {
		callback.OnCacheRead(resp.Usage.InputTokensDetails.CachedTokens)
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
		OnText:         c.onText,
		OnToolUse:      c.onToolUse,
		OnComplete:     c.onComplete,
		OnCredits:      target.OnCredits,
		OnContextUsage: target.OnContextUsage,
		OnCacheRead:    target.OnCacheRead,
		OnError:        c.onError,
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

// fetchCodexUsage sends a minimal /backend-api/codex/responses request to
// capture the x-codex-* rate-limit / usage headers. Codex doesn't expose a
// dedicated usage endpoint — usage info comes back as response headers on
// every chat request. We send a tiny "say ok" prompt with max_tokens=1 so
// the cost is negligible, then discard the body and keep only the headers.
func fetchCodexUsage(account *config.Account) error {
	if account == nil || !isCodexAccount(account) {
		return fmt.Errorf("not a codex account")
	}
	accessToken := strings.TrimSpace(account.AccessToken)
	if accessToken == "" {
		return fmt.Errorf("no access token")
	}
	accountID := strings.TrimSpace(account.ChatGPTAccountID)
	if accountID == "" {
		accountID = auth.ExtractCodexAccountIDPublic(accessToken)
		if accountID == "" {
			return fmt.Errorf("no chatgpt_account_id")
		}
	}

	body := map[string]interface{}{
		"model": "gpt-5.6-luna",
		"instructions": "You are a helpful assistant.",
		"input": []map[string]interface{}{
			{
				"type":    "message",
				"role":    "user",
				"content": "ok",
			},
		},
		"stream": true,
		"store":  false,
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	endpoint := codexBaseURL(account) + "/backend-api/codex/responses"
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("chatgpt-account-id", accountID)
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("User-Agent", "codex_cli_rs/0.0.0 omniproxy/1.0")

	client := GetClientForProxy(ResolveAccountProxyURL(account))
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()

	// Capture usage headers on ANY response (200, 429, 402, etc.) — Codex
	// sends x-codex-* rate-limit headers even on rate-limited responses,
	// which is exactly when we need them most.
	captureCodexUsageHeaders(account, resp.Header)

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		// 429 "usage_limit_reached" is not a hard error — we still captured
		// the usage headers, so return nil to signal "usage fetched".
		if resp.StatusCode == 429 {
			logger.Infof("[Codex] %s rate-limited but usage headers captured", account.Email)
			return nil
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateErrBody(errBody))
	}

	// Drain the body so the connection can be reused.
	io.Copy(io.Discard, resp.Body)

	// Also fetch the bank-reset credits available count from the wham/usage
	// endpoint. This is a separate GET (no chat cost) that returns
	// rate_limit_reset_credits.available_count. We cache it on the account
	// so the Quota page can display it without making per-poll upstream
	// calls. Errors are non-fatal — we just leave the cached value as-is.
	if avail, err := codexResetCreditsAvailable(account); err == nil {
		if account.CodexResetCreditsAvailable != avail {
			account.CodexResetCreditsAvailable = avail
			_ = config.UpdateAccount(account.ID, *account)
		}
	}
	return nil
}

// codexSubscriptionModels returns the canonical model list exposed by
// OpenAI's Codex backend for ChatGPT subscription logins. These are the
// model IDs the upstream /v1/responses endpoint accepts. The proxy seeds
// its routing cache with this list so claude-cli / openai clients can
// request any of them by name.
//
// Source: Codex CLI model registry (openai/codex GitHub) + official
// model guide at developers.openai.com/codex/models + observed
// /v1/responses accept-list. Updated when OpenAI ships new models.
//
// Note: The upstream /backend-api/codex/models endpoint exists but
// returns an empty list for most accounts because model visibility is
// gated by Statsig feature flags (see openai/codex#31873). The models
// are still callable via -m / model field, so we hardcode the full
// list here as a reliable fallback.
func codexSubscriptionModels() []ModelInfo {
	type lim struct {
		MaxInputTokens  int
		MaxOutputTokens int
	}
	specs := []struct {
		id, name, desc string
		lim
	}{
		// ── GPT-5.6 family (current flagship) ──
		{"gpt-5.6", "GPT-5.6", "GPT-5.6 alias (routes to Sol)", lim{300000, 128000}},
		{"gpt-5.6-sol", "GPT-5.6 Sol", "Flagship GPT-5.6 — hardest coding & reasoning", lim{300000, 128000}},
		{"gpt-5.6-terra", "GPT-5.6 Terra", "Balanced GPT-5.6 — everyday workhorse", lim{272000, 128000}},
		{"gpt-5.6-luna", "GPT-5.6 Luna", "Fast & affordable GPT-5.6 — high-throughput", lim{200000, 100000}},
		// ── GPT-5.5 (previous default) ──
		{"gpt-5.5", "GPT-5.5", "Previous default reasoning model", lim{272000, 128000}},
		// ── GPT-5.4 family ──
		{"gpt-5.4", "GPT-5.4", "Older default reasoning model", lim{272000, 128000}},
		{"gpt-5.4-mini", "GPT-5.4 Mini", "Lower-cost testing & lighter workflows", lim{200000, 100000}},
		{"gpt-5.4-nano", "GPT-5.4 Nano", "High-throughput simple tasks", lim{200000, 100000}},
		// ── GPT-5.1 / GPT-5 ──
		{"gpt-5.1", "GPT-5.1", "GPT-5.1 reasoning model", lim{272000, 128000}},
		{"gpt-5.1-codex-mini", "GPT-5.1 Codex Mini", "Cheaper coding workflows", lim{200000, 100000}},
		{"gpt-5", "GPT-5", "GPT-5 base model", lim{272000, 128000}},
		// ── Codex-specialized ──
		{"gpt-5.3-codex-spark", "GPT-5.3 Codex Spark", "Agentic coding (spark)", lim{200000, 100000}},
		{"codex-mini-latest", "Codex Mini", "Codex mini (latest)", lim{200000, 100000}},
		// ── o-series reasoning ──
		{"o4", "o4", "OpenAI o4 reasoning", lim{200000, 100000}},
		{"o3", "o3", "OpenAI o3 reasoning", lim{200000, 100000}},
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

// ─── Codex Bank Reset Quota (rate-limit-reset-credits) ──────────────────
//
// ChatGPT/Codex accounts occasionally get a "Bank Reset Quota" credit —
// a one-shot token that resets the account's rate-limit windows. 9router
// exposes this as the "Codex reset credit available" button. The flow is:
//
//  1. GET /backend-api/wham/usage → response.rate_limit_reset_credits
//     contains { available_count: N, ... }.
//  2. If available_count > 0, the operator can consume a credit:
//     POST /backend-api/wham/rate-limit-reset-credits/consume
//     body: { redeem_request_id: <random uuid> }
//     → response: { code: "reset"|"no_credit", windows_reset: N }
//
// On success, the account's primary/secondary usage counters are reset
// upstream, and we clear our local cached counters so the pool picks the
// account immediately.

// codexResetCreditsAvailable queries the upstream wham/usage endpoint and
// returns the number of available bank-reset credits (0 if none or error).
func codexResetCreditsAvailable(account *config.Account) (int, error) {
	if account == nil || !isCodexAccount(account) {
		return 0, fmt.Errorf("not a codex account")
	}
	accessToken := strings.TrimSpace(account.AccessToken)
	if accessToken == "" {
		return 0, fmt.Errorf("no access token")
	}
	accountID := strings.TrimSpace(account.ChatGPTAccountID)
	if accountID == "" {
		accountID = auth.ExtractCodexAccountIDPublic(accessToken)
		if accountID == "" {
			return 0, fmt.Errorf("no chatgpt_account_id")
		}
	}
	endpoint := codexBaseURL(account) + "/backend-api/wham/usage"
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("chatgpt-account-id", accountID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/0.0.0 omniproxy/1.0")

	client := GetClientForProxy(ResolveAccountProxyURL(account))
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateErrBody(body))
	}
	var parsed struct {
		RateLimitResetCredits struct {
			AvailableCount int `json:"available_count"`
		} `json:"rate_limit_reset_credits"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}
	return parsed.RateLimitResetCredits.AvailableCount, nil
}

// codexConsumeResetCredit consumes one bank-reset credit upstream. On
// success (code == "reset" and windows_reset > 0), the caller should
// clear local Codex usage counters so the pool picks the account.
//
// Returns (windowsReset, error). windowsReset == 0 with nil error means
// the upstream reported "no_credit" — the operator has no credits left.
func codexConsumeResetCredit(account *config.Account) (int, error) {
	if account == nil || !isCodexAccount(account) {
		return 0, fmt.Errorf("not a codex account")
	}
	accessToken := strings.TrimSpace(account.AccessToken)
	if accessToken == "" {
		return 0, fmt.Errorf("no access token")
	}
	accountID := strings.TrimSpace(account.ChatGPTAccountID)
	if accountID == "" {
		accountID = auth.ExtractCodexAccountIDPublic(accessToken)
		if accountID == "" {
			return 0, fmt.Errorf("no chatgpt_account_id")
		}
	}
	// Generate a unique redeem_request_id (UUID v4 shape). The upstream
	// dedupes on this id, so a retry with the same id is idempotent.
	redeemID := generateCodexRedeemID()
	reqBody, _ := json.Marshal(map[string]string{"redeem_request_id": redeemID})
	endpoint := codexBaseURL(account) + "/backend-api/wham/rate-limit-reset-credits/consume"
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return 0, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("chatgpt-account-id", accountID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/0.0.0 omniproxy/1.0")

	client := GetClientForProxy(ResolveAccountProxyURL(account))
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateErrBody(body))
	}
	var parsed struct {
		Code         string `json:"code"`
		WindowsReset int    `json:"windows_reset"`
		Message      string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}
	if parsed.Code == "no_credit" {
		return 0, nil // not an error — operator knows no credits left
	}
	if parsed.Code != "reset" || parsed.WindowsReset == 0 {
		msg := parsed.Message
		if msg == "" {
			msg = string(body)
		}
		return 0, fmt.Errorf("reset failed: code=%s windows_reset=%d msg=%s", parsed.Code, parsed.WindowsReset, msg)
	}
	return parsed.WindowsReset, nil
}

// generateCodexRedeemID returns a UUID v4-shaped string for the
// redeem_request_id field. We use crypto/rand so the id is unpredictable
// (the upstream dedupes on it).
func generateCodexRedeemID() string {
	var b [16]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		// Fallback to time-based — extremely unlikely.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	// Set version (4) and variant bits per RFC 4122.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
