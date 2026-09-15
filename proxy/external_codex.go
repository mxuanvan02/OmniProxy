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
	"context"
	cryptoRand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"omniproxy/auth"
	"omniproxy/config"
	"omniproxy/logger"
	"omniproxy/pool"
	"strings"
	"time"
)

// codexAuthMethod marks an account that authenticates via ChatGPT
// subscription OAuth and routes through OpenAI's Codex /v1/responses backend.
const codexAuthMethod = "codex"

// codexReauthRequiredStatus means the OAuth credentials were rejected by
// OpenAI and the account needs a new interactive login. It is intentionally
// distinct from BANNED, which represents an upstream account suspension.
const codexReauthRequiredStatus = "REAUTH_REQUIRED"

// codexAuthFailureKind classifies why OpenAI rejected a Codex account.
//
// The distinction matters operationally: a re-login fixes a dead session, but
// nothing the operator does locally fixes an upstream account termination.
// Collapsing both into REAUTH_REQUIRED hides bans behind a "Log in again"
// button that can never succeed.
type codexAuthFailureKind int

const (
	// codexAuthFailureNone means the error is not an account-credential
	// problem (rate limit, 5xx, network, model routing, ...).
	codexAuthFailureNone codexAuthFailureKind = iota
	// codexAuthFailureReauth means the session/token is dead but the account
	// itself is intact — an interactive login restores it.
	codexAuthFailureReauth
	// codexAuthFailureBanned means OpenAI terminated, deactivated or
	// suspended the account. Re-login cannot recover it.
	codexAuthFailureBanned
)

// codexBanPhrases are the upstream vocabulary that indicates the ChatGPT
// account itself was terminated/deactivated/suspended, as opposed to a merely
// expired or revoked session token.
//
// Matching is phrase-based, never status-code-based: a bare 401/403 can also
// come from a stale token, a regional block or an edge proxy, none of which
// prove a ban.
var codexBanPhrases = []string{
	"account_deactivated",
	"account deactivated",
	"account was deactivated",
	"account has been deactivated",
	"account_suspended",
	"account suspended",
	"account has been suspended",
	"temporarily_suspended",
	"access_terminated",
	"access was terminated",
	"access has been terminated",
	"account_terminated",
	"account terminated",
	"account_disabled",
	"account disabled",
	"disabled your account",
	"banned",
	"unusual activity",
	"suspicious activity",
	"violat", // covers "violation" / "violating our policies" / "violates"
	"terms of use",
	"usage policies",
}

// codexReauthPhrases are upstream signals that the credential is dead while
// the account remains usable after an interactive login.
var codexReauthPhrases = []string{
	"invalid_grant",
	"bad credentials",
	"invalid token",
	"token revoked",
	"token_revoked",
	"token invalidated",
	"token_invalidated",
	"refresh_token_invalidated",
	"session has ended",
	"please log in again",
	"please try signing in again",
}

func containsAnyPhrase(s string, phrases []string) bool {
	for _, phrase := range phrases {
		if strings.Contains(s, phrase) {
			return true
		}
	}
	return false
}

// classifyCodexAuthFailure decides whether an upstream error means the Codex
// account is banned, merely needs a fresh login, or is unrelated to auth.
//
// Ban detection runs BEFORE re-login detection on purpose: OpenAI returns a
// 401/403 for terminated accounts too, so a status-code-first ordering would
// misfile every ban as a session expiry — which is exactly the bug this
// function exists to prevent.
func classifyCodexAuthFailure(err error) codexAuthFailureKind {
	if err == nil {
		return codexAuthFailureNone
	}
	lower := strings.ToLower(err.Error())

	// Quota/rate limits are health signals, not credential failures.
	if isQuotaErrorMessage(lower) && !containsAnyPhrase(lower, codexBanPhrases) {
		return codexAuthFailureNone
	}

	if containsAnyPhrase(lower, codexBanPhrases) {
		return codexAuthFailureBanned
	}

	if containsAnyPhrase(lower, codexReauthPhrases) {
		return codexAuthFailureReauth
	}

	// A standalone 401 with no recognised vocabulary is treated as a dead
	// session: it is the recoverable interpretation, and Test/refresh will
	// re-classify it if the upstream later reports a ban.
	if hasStatusToken(lower, "401") {
		return codexAuthFailureReauth
	}

	return codexAuthFailureNone
}

// isCodexReauthRequiredError reports whether the error means the account needs
// a fresh interactive login (and specifically NOT that it was banned).
func isCodexReauthRequiredError(err error) bool {
	return classifyCodexAuthFailure(err) == codexAuthFailureReauth
}

// isCodexBannedError reports whether the error proves OpenAI terminated,
// deactivated or suspended the account.
func isCodexBannedError(err error) bool {
	return classifyCodexAuthFailure(err) == codexAuthFailureBanned
}

// markCodexAuthFailure persists the correct terminal state for a Codex
// credential failure: BANNED when the upstream says the account is gone,
// REAUTH_REQUIRED when only the session died, and nothing at all otherwise.
func markCodexAuthFailure(account *config.Account, err error) {
	if !isCodexAccount(account) {
		return
	}
	kind := classifyCodexAuthFailure(err)
	var status string
	switch kind {
	case codexAuthFailureBanned:
		status = "BANNED"
	case codexAuthFailureReauth:
		status = codexReauthRequiredStatus
	default:
		return
	}

	// Never downgrade a known ban into a re-login prompt. Once OpenAI has
	// reported a termination, a later bare 401 must not hide it.
	if status == codexReauthRequiredStatus && account.BanStatus == "BANNED" {
		logger.Warnf("[Codex] Keeping %s BANNED; ignoring re-login signal: %v", account.Email, err)
		return
	}

	account.BanStatus = status
	account.BanReason = truncateErrBody([]byte(err.Error()))
	account.BanTime = time.Now().Unix()
	account.Enabled = false
	// Field-scoped: a wholesale write would restore every counter from the
	// snapshot this account was read into, discarding whatever live requests
	// recorded via UpdateAccountStats while the refresh pass was running.
	if persistErr := config.UpdateAccountBanStatus(account.ID, account.BanStatus, account.BanReason, account.BanTime, account.Enabled); persistErr != nil {
		logger.Errorf("[Codex] Failed to persist %s status for %s: %v", status, account.Email, persistErr)
		return
	}
	logger.Warnf("[Codex] Marked %s as %s: %v", account.Email, status, err)
}

// markCodexReauthRequired is retained for call sites that only ever expect a
// session failure. It delegates to markCodexAuthFailure so ban vocabulary is
// still classified correctly instead of being flattened into REAUTH_REQUIRED.
func markCodexReauthRequired(account *config.Account, err error) {
	markCodexAuthFailure(account, err)
}

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
// This is critical for multi-agent scenarios: agents using the exact same
// system prompt share one cache entry per account instead of separate entries.
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
	if strings.TrimSpace(instructions) == "" {
		return ""
	}
	// Cache routing must not collapse whitespace: indentation, line endings,
	// and fenced code can be semantically meaningful instructions. The upstream
	// still receives this exact value, so the key must match it exactly too.
	h := sha256.Sum256([]byte("codex-cache:" + instructions))
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

// CallExternalCodex forwards a KiroPayload to the Codex /v1/responses
// endpoint and replays the response through callback. Mirrors
// CallKiroAPI/CallExternalOpenAI's contract: nil on success, error on
// failure. HTTP 401/403 are surfaced directly so the pool's auth-failure
// handling can refresh or disable the account.
//
// Token refresh + chatgpt-account-id re-extraction is handled here (not in
// ensureValidToken) because the account_id is JWT-bound and may rotate
// when OpenAI re-issues the access token.
func CallExternalCodex(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
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

	// prompt_cache_key: derived from the system prompt when there is one, so
	// that all conversations sharing the same instructions share a single cache
	// entry per account. This matters for multi-agent use, where many
	// conversations run identical instructions and would otherwise each need
	// their own warmup.
	//
	// For GPT-5.6 and later the documented behaviour is that prompt_cache_key
	// is required to get the reliable prefix matching: without it a request may
	// still land an automatic hit, but only via the weaker path. The key is
	// therefore always sent, falling back to the conversation prefix when there
	// is no system prompt — previously the field was omitted entirely in that
	// case, which was the weakest possible configuration on the priciest model.
	if key := payloadCacheKey(payload); key != "" {
		body["prompt_cache_key"] = key
	}

	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("codex call marshal: %w", err)
	}

	endpoint := codexBaseURL(account) + "/backend-api/codex/responses"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
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
		logExternalPayloadSizeRejection(account, payload, "codex", len(reqBody), resp, errBody)
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
		return parseResponsesSSE(resp.Body, callback)
	}
	// Peek through a buffered reader to distinguish a non-stream JSON fallback
	// from SSE. The buffered reader must also be passed to the chosen parser:
	// reading directly from resp.Body after Peek loses the bytes already held in
	// bufio's buffer, which can turn a valid SSE response into an empty stream.
	br := bufio.NewReader(resp.Body)
	first, err := br.Peek(1)
	if err != nil && err != io.EOF {
		return fmt.Errorf("codex response peek: %w", err)
	}
	if len(first) == 0 || first[0] != '{' {
		// Stream=true is the normal path. Treat any non-JSON payload as SSE so
		// comment-prefixed keepalives are also handled correctly. Preserve both
		// bufio's buffered bytes and the original Close method for the idle
		// watchdog.
		return parseResponsesSSE(&bufferedReadCloser{Reader: br, Closer: resp.Body}, callback)
	}
	// Non-SSE fallback: a single JSON Responses object.
	return parseResponsesJSON(br, callback)
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
	usageUpdate, err := config.UpdateAccountCodexUsage(
		account.ID, planType, activeLimit,
		primaryPct, secondaryPct, primaryWindow,
		primaryResetAt, secondaryResetAt,
		creditsBalance, creditsUnlimited, creditsKnown,
	)
	if err != nil {
		logger.Warnf("[Codex] persist usage for %s: %v", account.Email, err)
		return
	}
	if usageUpdate.PrimaryWindowReset {
		pool.GetPool().ResetCodexPrimaryWindowTokens(account.ID, primaryResetAt)
	} else if usageUpdate.PrimaryWindowChanged || usageUpdate.BootstrapCurrentWindowTokens {
		pool.GetPool().SyncCodexPrimaryWindow(account.ID, usageUpdate.PrimaryResetAt, usageUpdate.BootstrapCurrentWindowTokens)
	}
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

// defaultCodexResponsesModel is the model substituted when a Codex-bound payload
// carries no model id.
const defaultCodexResponsesModel = "gpt-5.6-sol"

// codexResponsesOptions returns the dialect options for the ChatGPT Codex
// subscription backend: a hard model default, no sampling parameters (the
// backend rejects them for GPT-5.x reasoning models), and the CLI lifecycle
// guidance injected into every tool description.
func codexResponsesOptions() responsesDialectOptions {
	return responsesDialectOptions{
		DefaultModel:          defaultCodexResponsesModel,
		ForwardSamplingParams: false,
		ToolDescription:       codexToolDescription,
	}
}

// kiroPayloadToCodexResponsesRequest is the Codex-flavoured entry point. It is
// kept as a named wrapper because callers and tests address the Codex dialect
// directly, while the translation itself lives in responses_upstream.go.
func kiroPayloadToCodexResponsesRequest(payload *KiroPayload, account *config.Account) (map[string]interface{}, error) {
	return kiroPayloadToResponsesRequest(payload, account, codexResponsesOptions())
}

const taskStopLifecycleGuidance = "Lifecycle constraint: call TaskStop only for a task that is currently running. A completed, failed, or stopped notification is terminal and requires no cleanup. If that task is later resumed, it may be stopped again until its next terminal notification. Never retry an ID after a 'No task found' result."

func codexToolDescription(name, description string) string {
	if name != "TaskStop" {
		return description
	}
	if strings.TrimSpace(description) == "" {
		return taskStopLifecycleGuidance
	}
	return strings.TrimSpace(description) + "\n\n" + taskStopLifecycleGuidance
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
	target      *KiroStreamCallback
	textBuf     strings.Builder
	thinkingBuf strings.Builder
	lastFlush   time.Time
	tick        *time.Timer
	tickCh      <-chan time.Time
	flushed     bool
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
		OnOutput:       c.onOutput,
		HasOutput:      target.HasOutput,
		OnComplete:     c.onComplete,
		OnReset:        c.onReset,
		OnStopReason:   c.onStopReason,
		OnCredits:      target.OnCredits,
		OnContextUsage: target.OnContextUsage,
		OnCacheRead:    target.OnCacheRead,
		OnCacheCreate:  target.OnCacheCreate,
		OnError:        c.onError,
	}
}

func (c *coalescer) onStopReason(reason string) {
	c.flush()
	if c.target.OnStopReason != nil {
		c.target.OnStopReason(reason)
	}
}

// onText buffers text and flushes when the tick interval or byte budget
// is reached. Thinking and non-thinking text are flushed independently.
func (c *coalescer) onText(text string, isThinking bool) {
	if text == "" {
		return
	}
	if c.target.OnOutput != nil {
		c.target.OnOutput()
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
	if c.target.OnOutput != nil {
		c.target.OnOutput()
	}
	c.flush()
	if c.target.OnToolUse != nil {
		c.target.OnToolUse(tu)
	}
}

func (c *coalescer) onOutput() {
	if c.target.OnOutput != nil {
		c.target.OnOutput()
	}
}

func (c *coalescer) onReset() {
	c.textBuf.Reset()
	c.thinkingBuf.Reset()
	c.lastFlush = time.Now()
	if c.target.OnReset != nil {
		c.target.OnReset()
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
func dispatchCodex(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if isCodexAccount(account) {
		return CallExternalCodex(ctx, account, payload, callback)
	}
	return dispatchChat(ctx, account, payload, callback)
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

// refreshCodexAccountToken commits a Codex OAuth rotation only after the
// upstream call returns a complete, valid token response. In particular, a
// failed refresh must never replace an existing token with an empty value or
// alter the account status.
func refreshCodexAccountToken(account *config.Account) error {
	if account == nil || !isCodexAccount(account) {
		return fmt.Errorf("not a codex account")
	}
	if strings.TrimSpace(account.RefreshToken) == "" {
		return fmt.Errorf("no codex refresh token")
	}

	newAccessToken, newRefreshToken, newExpiresAt, _, _, _, err := auth.RefreshAccountToken(account)
	if err != nil {
		return err
	}
	if strings.TrimSpace(newAccessToken) == "" || newExpiresAt <= 0 {
		return fmt.Errorf("codex refresh returned incomplete token data")
	}
	if newRefreshToken == "" {
		newRefreshToken = account.RefreshToken
	}

	// Persist first. If persistence fails, leave both the in-memory account and
	// the old refresh token untouched so the operator can retry safely.
	if err := config.UpdateAccountToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt); err != nil {
		return fmt.Errorf("persist codex token: %w", err)
	}
	account.AccessToken = newAccessToken
	account.RefreshToken = newRefreshToken
	account.ExpiresAt = newExpiresAt
	pool.GetPool().UpdateToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
	refreshCodexAccountID(account)
	return nil
}

// codexTokenNeedsRefresh checks both the persisted expiry and the JWT expiry.
// The JWT is useful when an imported account has stale/missing ExpiresAt.
func codexTokenNeedsRefresh(account *config.Account, now int64) bool {
	if account == nil || !isCodexAccount(account) {
		return false
	}
	if account.ExpiresAt > 0 && now >= account.ExpiresAt-tokenRefreshSkewSeconds {
		return true
	}
	info := auth.ExtractCodexJWTInfoPublic(account.AccessToken)
	return info.ExpiresAt > 0 && now >= info.ExpiresAt-tokenRefreshSkewSeconds
}

// fetchCodexUsage sends a minimal /backend-api/codex/responses request to
// capture the x-codex-* rate-limit / usage headers. Codex doesn't expose a
// dedicated usage endpoint — usage info comes back as response headers on
// every chat request. We send a tiny "say ok" prompt with max_tokens=1 so
// the cost is negligible, then discard the body and keep only the headers.
func fetchCodexUsage(account *config.Account) error {
	err := fetchCodexUsageAttempt(account)
	if err == nil || !isCodexAuthError(err) {
		return err
	}

	// A single refresh/retry handles an expired or invalidated access token.
	// Do not loop: a failed refresh must leave the account intact and visible.
	if refreshErr := refreshCodexAccountToken(account); refreshErr != nil {
		return fmt.Errorf("%w; codex token refresh failed: %v", err, refreshErr)
	}
	return fetchCodexUsageAttempt(account)
}

func isCodexAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 401") || strings.Contains(msg, "http status 401")
}

func fetchCodexUsageAttempt(account *config.Account) error {
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
		"model":        "gpt-5.6-luna",
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

	// Refresh the bank-reset credit count here, BEFORE the status-code
	// branching below, and not on the success path only.
	//
	// This used to sit after the 200-only path, which inverted the feature: a
	// bank-reset credit exists precisely to rescue an account whose quota is
	// spent, but a spent account answers this probe with 429 and the old code
	// returned early — so the count was never read, stayed at 0, and the UI
	// disabled the very button that would have fixed the account. Verified
	// live: the probe POST returns 429 while GET wham/usage returns 200 with
	// available_count=2 for the same account.
	refreshCodexResetCreditCache(account)

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		// 429 "usage_limit_reached" is not a hard error — we still captured
		// the usage headers, so return nil to signal "usage fetched".
		if resp.StatusCode == 429 {
			logger.Infof("[Codex] %s rate-limited but usage headers captured (bank-reset credits: %d)",
				account.Email, account.CodexResetCreditsAvailable)
			return nil
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateErrBody(errBody))
	}

	// Drain the body so the connection can be reused.
	io.Copy(io.Discard, resp.Body)

	return nil
}

// refreshCodexResetCreditCache reads the bank-reset credit count from the
// upstream wham/usage endpoint and caches it on the account so the Quota page
// can render it without a per-poll upstream call.
//
// Separate GET, no chat cost, and it answers even when the account is
// rate-limited — which is the case that matters, since that is when an operator
// goes looking for a reset credit. Failures are deliberately non-fatal: the
// previously cached value is more useful than a zero, and a 401 here will be
// followed by a token refresh and a second attempt by the caller.
func refreshCodexResetCreditCache(account *config.Account) {
	avail, err := codexResetCreditsAvailable(account)
	if err != nil {
		logger.Debugf("[Codex] %s bank-reset credit lookup failed (keeping cached %d): %v",
			account.Email, account.CodexResetCreditsAvailable, err)
		return
	}
	if account.CodexResetCreditsAvailable == avail {
		return
	}
	account.CodexResetCreditsAvailable = avail
	if err := config.UpdateAccountPreservingCredentials(account.ID, *account); err != nil {
		logger.Warnf("[Codex] Failed to persist bank-reset credit count for %s: %v", account.Email, err)
	}
}

// codexSubscriptionModels returns the canonical model list exposed by
// OpenAI's Codex backend for ChatGPT subscription logins. These are the
// model IDs the upstream /v1/responses endpoint accepts. The proxy seeds
// its routing cache with this list so claude-cli / openai clients can
// request any of them by name.
//
// Source for GPT-5.6 operating context: openai/codex
// codex-rs/models-manager/models.json (context_window), revision
// 6751b54cae32b23786001e2414d749a9916201e1. This matches the
// /backend-api/codex/responses route used by subscription accounts.
// Source for GPT-5.6 max output: the official public model pages at
// https://developers.openai.com/api/docs/models/gpt-5.6-{sol,terra,luna}.md.
// The Codex registry does not separately publish a max-output field.
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
		// ── GPT-6 family (current flagship) ──
		{"gpt-6-astra", "GPT-6 Astra", "Flagship GPT-6 model for complex coding and reasoning", lim{272000, 128000}},
		// ── GPT-5.6 family (current flagship) ──
		{"gpt-5.6", "GPT-5.6", "GPT-5.6 alias (routes to Sol)", lim{272000, 128000}},
		{"gpt-5.6-sol", "GPT-5.6 Sol", "Flagship GPT-5.6 — hardest coding & reasoning", lim{272000, 128000}},
		{"gpt-5.6-terra", "GPT-5.6 Terra", "Balanced GPT-5.6 — everyday workhorse", lim{272000, 128000}},
		{"gpt-5.6-luna", "GPT-5.6 Luna", "Fast & affordable GPT-5.6 — high-throughput", lim{272000, 128000}},
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
			Provider:       "openai-codex",
		}
		m.TokenLimits = &ModelTokenLimits{MaxInputTokens: s.MaxInputTokens, MaxOutputTokens: s.MaxOutputTokens}
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
	// redeem_request_id: the upstream dedupes on this id, so a retry with the
	// SAME id is idempotent and does not burn a second credit. We therefore
	// reuse a pending id left over from an inconclusive attempt (network
	// error, timeout, 5xx) instead of generating a fresh one every call.
	redeemID := strings.TrimSpace(account.CodexResetRedeemID)
	reusedRedeemID := redeemID != ""
	if !reusedRedeemID {
		redeemID = generateCodexRedeemID()
		// Persist BEFORE the POST: if the process dies mid-request we must
		// still know which id was (possibly) already redeemed upstream.
		account.CodexResetRedeemID = redeemID
		if err := config.UpdateAccountPreservingCredentials(account.ID, *account); err != nil {
			logger.Errorf("[codexConsumeResetCredit] Failed to persist pending redeem id for %s: %v", account.Email, err)
		}
	} else {
		logger.Infof("[codexConsumeResetCredit] Reusing pending redeem id for %s (idempotent retry)", account.Email)
	}
	// clearPendingRedeemID drops the stored id once the outcome is conclusive.
	clearPendingRedeemID := func() {
		if account.CodexResetRedeemID == "" {
			return
		}
		account.CodexResetRedeemID = ""
		if err := config.UpdateAccountPreservingCredentials(account.ID, *account); err != nil {
			logger.Errorf("[codexConsumeResetCredit] Failed to clear pending redeem id for %s: %v", account.Email, err)
		}
	}
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
		// INCONCLUSIVE: the POST may or may not have reached the upstream.
		// Keep the pending redeem id so the next attempt is idempotent.
		return 0, fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		// 5xx / 429: upstream may have applied the redemption before failing
		// to answer — keep the pending id. 4xx (except 429) is a definitive
		// rejection, so the id was never redeemed and can be dropped.
		if resp.StatusCode < 500 && resp.StatusCode != 429 {
			clearPendingRedeemID()
		}
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateErrBody(body))
	}
	var parsed struct {
		Code         string `json:"code"`
		WindowsReset int    `json:"windows_reset"`
		Message      string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		// HTTP 200 with an unparseable body: the redemption most likely did
		// happen upstream, so keep the pending id rather than risk a second one.
		return 0, fmt.Errorf("parse: %w", err)
	}
	if parsed.Code == "no_credit" {
		clearPendingRedeemID()
		return 0, nil // not an error — operator knows no credits left
	}
	if parsed.Code != "reset" || parsed.WindowsReset == 0 {
		clearPendingRedeemID()
		msg := parsed.Message
		if msg == "" {
			msg = string(body)
		}
		return 0, fmt.Errorf("reset failed: code=%s windows_reset=%d msg=%s", parsed.Code, parsed.WindowsReset, msg)
	}
	clearPendingRedeemID()
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
