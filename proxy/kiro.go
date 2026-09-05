// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"omniproxy/config"
	"omniproxy/logger"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Endpoint configuration (auto-fallback on quota exhaustion).
type kiroEndpoint struct {
	URL       string
	Origin    string
	AmzTarget string
	Name      string
}

// kiroEndpoints holds the default (us-east-1) streaming endpoint templates.
// buildKiroEndpoints derives region-specific URLs from these. Tests override
// this slice to point at a local test server.
var kiroEndpoints = []kiroEndpoint{
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "",
		Name:      "Kiro IDE",
	},
	{
		URL:       "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		Name:      "CodeWhisperer",
	},
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonQDeveloperStreamingService.SendMessage",
		Name:      "AmazonQ",
	},
}

// Global HTTP clients, swappable at runtime to apply proxy reconfiguration without restart.
var kiroHttpStore atomic.Pointer[http.Client]
var kiroRestHttpStore atomic.Pointer[http.Client]

const imageHTTPTimeout = 15 * time.Minute

// proxyClientCache caches http.Client instances keyed by proxy URL for per-account proxy support.
var proxyClientCache sync.Map

func init() {
	InitKiroHttpClient("")
}

// GetClientForProxy returns an http.Client configured for the given proxy URL.
// If proxyURL is empty, returns the global kiro HTTP client.
func GetClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroHttpStore.Load()
	}
	if cached, ok := proxyClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	timeout := config.GetKiroApiTimeout()
	client := &http.Client{
		Timeout:   timeout,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(proxyURL, client)
	return client
}

// GetRestClientForProxy returns a rest http.Client (30s timeout) for the given proxy URL.
// If proxyURL is empty, returns the global kiro REST HTTP client.
func GetRestClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroRestHttpStore.Load()
	}
	cacheKey := "rest:" + proxyURL
	if cached, ok := proxyClientCache.Load(cacheKey); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(cacheKey, client)
	return client
}

// GetImageClientForProxy returns a long-lived REST client for image generation.
// Image generation can spend several minutes before returning its artifact;
// the normal REST client is intentionally short-lived for metadata APIs.
func GetImageClientForProxy(proxyURL string) *http.Client {
	cacheKey := "image:" + proxyURL

	if cached, ok := proxyClientCache.Load(cacheKey); ok {
		return cached.(*http.Client)
	}
	transport := buildKiroTransport(proxyURL)
	transport.ResponseHeaderTimeout = imageHTTPTimeout
	client := &http.Client{
		Timeout:   imageHTTPTimeout,
		Transport: transport,
	}
	actual, _ := proxyClientCache.LoadOrStore(cacheKey, client)
	return actual.(*http.Client)
}

// ResolveAccountProxyURL returns the effective proxy URL for an account.
// Falls back to global config.GetProxyURL() if the account has no per-account proxy.
func ResolveAccountProxyURL(account *config.Account) string {
	if account != nil && account.ProxyURL != "" {
		return account.ProxyURL
	}
	return config.GetProxyURL()
}

// buildKiroTransport constructs an HTTP Transport with optional outbound proxy support.
func buildKiroTransport(proxyURL string) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: initialStreamDataTimeout,
		DisableCompression:    false,
		ForceAttemptHTTP2:     true,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			// Proxied connections cannot negotiate HTTP/2.
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	return t
}

// InitKiroHttpClient initializes (or reinitializes) the HTTP clients used for Kiro API requests.
func InitKiroHttpClient(proxyURL string) {
	timeout := config.GetKiroApiTimeout()
	client := &http.Client{
		Timeout:   timeout,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroHttpStore.Store(client)

	restClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroRestHttpStore.Store(restClient)
}

// ==================== Request Structs ====================

// KiroPayload is the top-level request body sent to the Kiro API.
type KiroPayload struct {
	ConversationState struct {
		AgentContinuationId string `json:"agentContinuationId,omitempty"`
		AgentTaskType       string `json:"agentTaskType,omitempty"`
		ChatTriggerType     string `json:"chatTriggerType"`
		ConversationID      string `json:"conversationId"`
		CurrentMessage      struct {
			UserInputMessage KiroUserInputMessage `json:"userInputMessage"`
		} `json:"currentMessage"`
		History []KiroHistoryMessage `json:"history,omitempty"`
	} `json:"conversationState"`
	ProfileArn      string           `json:"profileArn,omitempty"`
	InferenceConfig *InferenceConfig `json:"inferenceConfig,omitempty"`
	// ToolChoice preserves the client's explicit tool-selection instruction for
	// adapters that support it. Native Kiro does not accept this field.
	ToolChoice interface{} `json:"-"`

	// ToolNameMap maps sanitized tool names (sent to Kiro) back to the
	// original names supplied by the client. Used to restore original names
	// in tool_use responses so the client can match them to its tool registry.
	// Not serialized to the Kiro API request body.
	ToolNameMap map[string]string `json:"-"`

	// OriginalModel preserves the model ID sent to external providers. It may differ
	// from the client-visible model when a request is routed through an internal alias.
	OriginalModel string `json:"-"`

	// PublicModel is the model ID returned to the client. Empty falls back to the
	// routing model for callers that do not need an alias.
	PublicModel string `json:"-"`
}

type KiroUserInputMessage struct {
	Content                 string                   `json:"content"`
	ModelID                 string                   `json:"modelId,omitempty"`
	Origin                  string                   `json:"origin"`
	Images                  []KiroImage              `json:"images,omitempty"`
	UserInputMessageContext *UserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type UserInputMessageContext struct {
	Tools       []KiroToolWrapper `json:"tools,omitempty"`
	ToolResults []KiroToolResult  `json:"toolResults,omitempty"`
}

type KiroToolWrapper struct {
	ToolSpecification struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		InputSchema InputSchema `json:"inputSchema"`
	} `json:"toolSpecification"`
}

type InputSchema struct {
	JSON interface{} `json:"json"`
}

type KiroToolResult struct {
	ToolUseID string              `json:"toolUseId"`
	Content   []KiroResultContent `json:"content"`
	Status    string              `json:"status"`
}

type KiroResultContent struct {
	Text string `json:"text"`
}

type KiroImage struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

type KiroHistoryMessage struct {
	UserInputMessage         *KiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *KiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type KiroAssistantResponseMessage struct {
	Content  string        `json:"content"`
	ToolUses []KiroToolUse `json:"toolUses,omitempty"`
}

type KiroToolUse struct {
	ToolUseID string                 `json:"toolUseId"`
	Name      string                 `json:"name"`
	Input     map[string]interface{} `json:"input"`
}

type InferenceConfig struct {
	MaxTokens       int                   `json:"maxTokens,omitempty"`
	Temperature     float64               `json:"temperature,omitempty"`
	TopP            float64               `json:"topP,omitempty"`
	Thinking        *ClaudeThinkingConfig `json:"-"`
	ReasoningEffort string                `json:"-"`
}

// ==================== Stream Callbacks ====================

// KiroStreamCallback stream response callbacks
type KiroStreamCallback struct {
	OnText    func(text string, isThinking bool)
	OnToolUse func(toolUse KiroToolUse)
	// OnOutput is called as soon as the upstream emits visible output or a
	// tool call, before any downstream buffering. It prevents retrying a
	// partially emitted response and replaying its prefix.
	OnOutput func()
	// HasOutput reports whether the current attempt has emitted output.
	// Retry helpers use it to stop after a retry starts producing a partial
	// response and then fails.
	HasOutput  func() bool
	OnComplete func(inputTokens, outputTokens int)
	// OnReset clears per-attempt adapter state before a non-stream retry.
	OnReset func()
	// OnStopReason reports the normalized upstream terminal reason. The
	// Claude adapter maps this to Anthropic's stop_reason values.
	OnStopReason   func(reason string)
	OnError        func(err error)
	OnCredits      func(credits float64)
	OnContextUsage func(percentage float64)
	// OnCacheRead and OnCacheCreate report usage explicitly returned by the
	// upstream provider. They are the only cache values used for downstream
	// usage, cost, and savings calculations.
	OnCacheRead   func(cachedTokens int)
	OnCacheCreate func(cacheCreateTokens int)
}

// ==================== API Call ====================

func setPayloadProfileArnForAccount(payload *KiroPayload, account *config.Account) {
	if payload == nil {
		return
	}

	payload.ProfileArn = strings.TrimSpace(payload.ProfileArn)
	if account == nil {
		return
	}

	// API-key accounts: only use explicitly stored ARN, never resolve defaults.
	// If empty, leave unset (upstream will 403, which is better than sending
	// a wrong ARN that 403s silently with a misleading message).
	if account.AuthMethod == "api_key" {
		if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
			payload.ProfileArn = profileArn
		}
		return
	}

	if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
		payload.ProfileArn = profileArn
	}
}

// buildKiroEndpoints derives region-specific streaming endpoints. For us-east-1
// it uses the kiroEndpoints templates as-is (and tests override that slice). For
// any other region the legacy codewhisperer.<region> host does NOT exist — only
// q.<region>.amazonaws.com does — so all endpoints point at q.<region>.
// Without this, IdC/enterprise profiles outside us-east-1 (e.g. eu-central-1)
// hit a non-existent host / wrong region and the upstream returns 403.
func buildKiroEndpoints(region string) []kiroEndpoint {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		region = "us-east-1"
	}
	if region == "us-east-1" {
		out := make([]kiroEndpoint, len(kiroEndpoints))
		copy(out, kiroEndpoints)
		return out
	}
	q := fmt.Sprintf("https://q.%s.amazonaws.com/generateAssistantResponse", region)
	return []kiroEndpoint{
		{URL: q, Origin: "AI_EDITOR", AmzTarget: "", Name: "Kiro IDE"},
		{URL: q, Origin: "AI_EDITOR", AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse", Name: "CodeWhisperer"},
		{URL: q, Origin: "AI_EDITOR", AmzTarget: "AmazonQDeveloperStreamingService.SendMessage", Name: "AmazonQ"},
	}
}

// getSortedEndpoints returns endpoints ordered by user preference, with optional fallback.
func getSortedEndpoints(preferred, region string) []kiroEndpoint {
	fallback := config.GetEndpointFallback()
	endpoints := buildKiroEndpoints(region)

	var primary int
	switch preferred {
	case "kiro":
		primary = 0
	case "codewhisperer":
		primary = 1
	case "amazonq":
		primary = 2
	default:
		// "auto": Kiro first, then fallback to others
		return []kiroEndpoint{endpoints[0], endpoints[1], endpoints[2]}
	}

	if !fallback {
		// No fallback: only use the selected endpoint
		return []kiroEndpoint{endpoints[primary]}
	}

	// With fallback: selected first, then others in order
	result := []kiroEndpoint{endpoints[primary]}
	for i, ep := range endpoints {
		if i != primary {
			result = append(result, ep)
		}
	}
	return result
}

// sortEndpointsForAuth reorders endpoints so CodeWhisperer/Q hosts are tried first
// when the account uses API-key auth. The Kiro IDE gateway (kiro.dev) rejects
// tokentype: API_KEY tokens, so the raw CodeWhisperer endpoint must come first.
// Mirrors 9router's getOrderedBaseUrls().
func sortEndpointsForAuth(endpoints []kiroEndpoint, authMethod string, accessToken string, accountRegion string) []kiroEndpoint {
	if authMethod != "api_key" {
		return endpoints
	}
	// ksk_ API keys use Smithy protocol via runtime.kiro.dev with root path.
	// X-Amz-Target header selects the operation. Region is taken from the
	// account (set at import time); default us-east-1 — Kiro API key primary.
	if strings.HasPrefix(accessToken, "ksk_") {
		region := strings.TrimSpace(accountRegion)
		if region == "" {
			region = "us-east-1"
		}
		return []kiroEndpoint{{
			URL:       fmt.Sprintf("https://runtime.%s.kiro.dev/", region),
			Origin:    "KIRO_CLI",
			AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
			Name:      "Kiro Runtime (ksk)",
		}}
	}
	var amazon []kiroEndpoint
	var others []kiroEndpoint
	for _, ep := range endpoints {
		if strings.Contains(ep.URL, "amazonaws.com") {
			amazon = append(amazon, ep)
		} else {
			others = append(others, ep)
		}
	}
	if len(amazon) == 0 {
		return endpoints
	}
	return append(amazon, others...)
}

// CallKiroAPI calls the Kiro streaming API, trying each configured endpoint with automatic fallback.
// ctx carries the client request's lifetime: when the client disconnects the
// upstream request is cancelled so the account is released immediately.
func CallKiroAPI(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	originalProfileArn := ""
	if payload != nil {
		originalProfileArn = payload.ProfileArn
		defer func() {
			payload.ProfileArn = originalProfileArn
		}()
	}
	setPayloadProfileArnForAccount(payload, account)

	if _, err := json.Marshal(payload); err != nil {
		return err
	}

	// Debug: dump full payload for troubleshooting upstream rejections
	if payloadJSON, err := json.Marshal(payload); err == nil {
		logger.Debugf("[KiroAPI] Request payload: %s", string(payloadJSON))
	}

	// Wrap OnToolUse to restore original tool names for the client.
	if callback != nil && callback.OnToolUse != nil && len(payload.ToolNameMap) > 0 {
		originalOnToolUse := callback.OnToolUse
		nameMap := payload.ToolNameMap
		wrapped := *callback
		wrapped.OnToolUse = func(tu KiroToolUse) {
			if original, ok := nameMap[tu.Name]; ok {
				tu.Name = original
			}
			originalOnToolUse(tu)
		}
		callback = &wrapped
	}

	if payload != nil && strings.TrimSpace(payload.ProfileArn) == "" {
		// API-key accounts must never fall back to default profile ARNs — CodeWhisperer
		// rejects a profileArn that doesn't belong to the key's account.
		if account != nil && account.AuthMethod != "api_key" {
			if profileArn, err := ResolveProfileArn(account); err == nil {
				payload.ProfileArn = profileArn
			} else {
				accountEmail := "<nil>"
				if account != nil {
					accountEmail = account.Email
				}
				logger.Warnf("[ProfileArn] Failed to resolve profile ARN for %s: %v", accountEmail, err)
			}
		}
	}

	// Build endpoint list ordered by configuration, then reorder for api-key auth.
	// Use the profile-derived runtime region (regionForAccount), not the raw
	// account.Region — for AWS IdC the profile region (e.g. eu-central-1) can
	// differ from the SSO region stored on the account (e.g. us-east-1). This
	// runs after profile-ARN resolution above, so account.ProfileArn is set.
	endpoints := getSortedEndpoints(config.GetPreferredEndpoint(), regionForAccount(account))
	var authMethod string
	var accessToken string
	if account != nil {
		authMethod = account.AuthMethod
		accessToken = account.AccessToken
	}
	endpoints = sortEndpointsForAuth(endpoints, authMethod, accessToken, account.Region)

	var lastErr error
	for _, ep := range endpoints {
		// Update the origin field for the selected endpoint.
		payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin

		// ksk_ API keys: don't include profileArn in body (server resolves from token)
		isKsk := account != nil && strings.HasPrefix(account.AccessToken, "ksk_")
		if isKsk {
			payload.ProfileArn = ""
		}

		reqBody, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, "POST", ep.URL, bytes.NewReader(reqBody))
		if err != nil {
			lastErr = err
			continue
		}

		host := ""
		if parsedURL, parseErr := url.Parse(ep.URL); parseErr == nil {
			host = parsedURL.Host
		}
		headerValues := buildStreamingHeaderValues(account, host)

		// ksk_ keys use Smithy protocol content-type
		if isKsk {
			req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "*/*")
		if ep.AmzTarget != "" {
			req.Header.Set("X-Amz-Target", ep.AmzTarget)
		}
		applyKiroBaseHeaders(req, account, headerValues)
		if !isKsk {
			req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
		}
		req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
		req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())

		resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
		if err != nil {
			lastErr = err
			logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, err)
			continue
		}

		if resp.StatusCode == 429 {
			resp.Body.Close()
			logger.Warnf("[KiroAPI] Endpoint %s quota exhausted (429), trying next...", ep.Name)
			lastErr = fmt.Errorf("quota exhausted on %s", ep.Name)
			continue
		}

		if resp.StatusCode != 200 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, ep.Name, string(errBody))
			// Authentication errors and payment errors are not retried across endpoints.
			if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 402 {
				return lastErr
			}
			logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, lastErr)
			continue
		}

		err = parseEventStream(newIdleTimeoutReader(resp.Body), callback)
		resp.Body.Close()
		return err
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("all endpoints failed")
}

// ==================== Event Stream Parsing ====================

// parseEventStream decodes an AWS binary Event Stream response body.
func parseEventStream(body io.Reader, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}

	// Read directly without bufio to avoid buffering latency in streaming responses.
	var inputTokens, outputTokens int
	var totalCredits float64
	var currentToolUse *toolUseState
	var lastAssistantContent string
	var lastReasoningContent string

	// KIRO_DEBUG_USAGE=1 dumps every upstream event type plus any token/usage
	// fields it carries, so we can confirm from a live stream whether the
	// upstream actually reports real token counts (vs only credits + context %).
	debugUsage := os.Getenv("KIRO_DEBUG_USAGE") == "1"

	for {
		// Prelude: 12 bytes (total_len + headers_len + crc)
		prelude := make([]byte, 12)
		_, err := io.ReadFull(body, prelude)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		totalLength := int(prelude[0])<<24 | int(prelude[1])<<16 | int(prelude[2])<<8 | int(prelude[3])
		headersLength := int(prelude[4])<<24 | int(prelude[5])<<16 | int(prelude[6])<<8 | int(prelude[7])

		if totalLength < 16 {
			continue
		}

		// Read the remaining message bytes.
		remaining := totalLength - 12
		msgBuf := make([]byte, remaining)
		_, err = io.ReadFull(body, msgBuf)
		if err != nil {
			return err
		}

		if headersLength > len(msgBuf)-4 {
			continue
		}

		eventType := extractEventType(msgBuf[0:headersLength])
		payloadBytes := msgBuf[headersLength : len(msgBuf)-4]
		if len(payloadBytes) == 0 {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal(payloadBytes, &event); err != nil {
			continue
		}

		var cacheUsage upstreamCacheTokenUsage
		inputTokens, outputTokens, cacheUsage = updateTokensAndCacheUsageFromEvent(event, inputTokens, outputTokens)
		if cacheUsage.ReadTokens > 0 && callback.OnCacheRead != nil {
			callback.OnCacheRead(cacheUsage.ReadTokens)
		}
		if cacheUsage.CreateTokens > 0 && callback.OnCacheCreate != nil {
			callback.OnCacheCreate(cacheUsage.CreateTokens)
		}

		if debugUsage {
			// Log the raw event type and, when present, any token/usage-shaped
			// payload so we can see exactly what the upstream emits per request.
			if eventType == "meteringEvent" || eventType == "contextUsageEvent" ||
				hasUsageShape(event) {
				if raw, err := json.Marshal(event); err == nil {
					logger.Infof("[UsageDebug] event=%s running(in=%d out=%d) payload=%s",
						eventType, inputTokens, outputTokens, string(raw))
				}
			} else {
				logger.Infof("[UsageDebug] event=%s running(in=%d out=%d)", eventType, inputTokens, outputTokens)
			}
		}

		// Dispatch by event type.
		switch eventType {
		case "assistantResponseEvent":
			if content, ok := event["content"].(string); ok && content != "" {
				normalized := normalizeChunk(content, &lastAssistantContent)
				if normalized != "" && callback.OnText != nil {
					callback.OnText(normalized, false)
				}
			}
		case "reasoningContentEvent":
			if text, ok := event["text"].(string); ok && text != "" {
				normalized := normalizeChunk(text, &lastReasoningContent)
				if normalized != "" && callback.OnText != nil {
					callback.OnText(normalized, true)
				}
			}
		case "toolUseEvent":
			currentToolUse = handleToolUseEvent(event, currentToolUse, callback)
		case "meteringEvent":
			if usage, ok := event["usage"].(float64); ok {
				totalCredits += usage
			}
		case "contextUsageEvent":
			if pct, ok := event["contextUsagePercentage"].(float64); ok {
				if callback.OnContextUsage != nil {
					callback.OnContextUsage(pct)
				}
			}
		}
	}

	if currentToolUse != nil {
		finishToolUse(currentToolUse, callback)
	}

	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}

	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
	return nil
}

// hasUsageShape reports whether an event (or a nested map) carries any
// token/usage-shaped key. Used only by the KIRO_DEBUG_USAGE capture to surface
// otherwise-unrecognized frames that might hold real upstream token counts.
func hasUsageShape(event map[string]interface{}) bool {
	candidates := []map[string]interface{}{event}
	collectUsageMaps(event, &candidates)
	for _, m := range candidates {
		if m == nil {
			continue
		}
		for k := range m {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "token") || strings.Contains(lk, "usage") {
				return true
			}
		}
	}
	return false
}

func updateTokensFromEvent(event map[string]interface{}, currentInputTokens, currentOutputTokens int) (int, int) {
	in, out, _ := updateTokensAndCacheFromEvent(event, currentInputTokens, currentOutputTokens)
	return in, out
}

type upstreamCacheTokenUsage struct {
	ReadTokens   int
	CreateTokens int
}

// updateTokensAndCacheFromEvent mirrors updateTokensFromEvent but also
// returns the largest cache-read token count observed in any usage-shaped
// map within the event. cacheRead > 0 means the upstream provider reported
// a real prompt-cache hit (Kiro's cacheReadInputTokens or OpenAI's
// cached_tokens), which the handler should forward to the client instead
// of the locally-simulated promptCacheTracker numbers.
func updateTokensAndCacheFromEvent(event map[string]interface{}, currentInputTokens, currentOutputTokens int) (int, int, int) {
	in, out, cacheUsage := updateTokensAndCacheUsageFromEvent(event, currentInputTokens, currentOutputTokens)
	return in, out, cacheUsage.ReadTokens
}

// updateTokensAndCacheUsageFromEvent extracts upstream-confirmed cache reads
// and writes alongside input/output token counts. Keeping this separate from
// promptCacheTracker prevents local predictions from being mistaken for billed
// provider cache usage.
func updateTokensAndCacheUsageFromEvent(event map[string]interface{}, currentInputTokens, currentOutputTokens int) (int, int, upstreamCacheTokenUsage) {
	candidates := []map[string]interface{}{event}
	collectUsageMaps(event, &candidates)

	inputTokens := currentInputTokens
	outputTokens := currentOutputTokens
	cacheUsage := upstreamCacheTokenUsage{}

	for _, usage := range candidates {
		if usage == nil {
			continue
		}

		if v, ok := readTokenNumber(usage,
			"outputTokens", "completionTokens", "totalOutputTokens",
			"output_tokens", "completion_tokens", "total_output_tokens",
		); ok {
			outputTokens = v
		}

		uncached, _ := readTokenNumber(usage, "uncachedInputTokens", "uncached_input_tokens")
		cr, _ := readTokenNumber(usage, "cacheReadInputTokens", "cache_read_input_tokens")
		cacheWrite, _ := readTokenNumber(usage, "cacheWriteInputTokens", "cache_write_input_tokens", "cacheCreationInputTokens", "cache_creation_input_tokens")
		if cr > cacheUsage.ReadTokens {
			cacheUsage.ReadTokens = cr
		}
		if cacheWrite > cacheUsage.CreateTokens {
			cacheUsage.CreateTokens = cacheWrite
		}

		// Read cache fields before handling the aggregate input field. Some
		// OpenAI-compatible usage objects contain input_tokens together with
		// prompt_tokens_details.cached_tokens; the latter must not be skipped.
		if v, ok := readTokenNumber(usage, "cachedTokens", "cached_tokens"); ok && v > cacheUsage.ReadTokens {
			cacheUsage.ReadTokens = v
		}

		if v, ok := readTokenNumber(usage,
			"inputTokens", "promptTokens", "totalInputTokens",
			"input_tokens", "prompt_tokens", "total_input_tokens",
		); ok {
			inputTokens = v
			continue
		}

		if uncached+cr+cacheWrite > 0 {
			inputTokens = uncached + cr + cacheWrite
			continue
		}

		total, ok := readTokenNumber(usage, "totalTokens", "total_tokens")
		if ok && total > 0 {
			candidateOutput := outputTokens
			if v, vok := readTokenNumber(usage,
				"outputTokens", "completionTokens", "totalOutputTokens",
				"output_tokens", "completion_tokens", "total_output_tokens",
			); vok {
				candidateOutput = v
			}
			if total-candidateOutput > 0 {
				inputTokens = total - candidateOutput
			}
		}
	}

	return inputTokens, outputTokens, cacheUsage
}

// getContextWindowSize returns the context window size (in tokens) for a model.
//
// Per Kiro's ListAvailableModels, the 1M-token context window applies to
// Claude 4.6 and newer (and configured Claude 5/Fable 5 models), while 4.5
// and earlier use a 200K window. This value is used to convert the upstream
// contextUsagePercentage into an absolute input-token count that clients rely
// on to decide when to compact; an undersized window under-reports tokens and
// prevents clients from compacting in time.
func getContextWindowSize(model string) int {
	if isLargeContextModel(model) {
		return 1_000_000
	}
	return 200_000
}

// claudeVersionExtractor matches "claude-<family>-<major>[.<minor>]" (dot or
// dash form) and is used to classify 1M-window models by version. The minor
// component is optional because the Claude 5 identifiers are major-only
// (claude-opus-5, claude-fable-5).
var claudeVersionExtractor = regexp.MustCompile(`claude-(?:opus|sonnet|haiku|fable|mythos)-(\d+)(?:[.-](\d+))?`)

func isLargeContextModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if idx := strings.IndexByte(m, '/'); idx >= 0 {
		m = strings.TrimSpace(m[idx+1:])
	}
	// [1m] is a Claude Code capability suffix, not part of the model ID.
	m = stripClaudeContextSuffix(m)

	if match := claudeVersionExtractor.FindStringSubmatch(m); match != nil {
		major, errMaj := strconv.Atoi(match[1])
		// A major-only identifier (claude-opus-5, claude-fable-5) has no minor
		// component; treat it as .0, which is above the 4.6 boundary anyway.
		minor := 0
		if match[2] != "" {
			minor, _ = strconv.Atoi(match[2])
		}
		if errMaj == nil {
			// 1M window for Claude >= 4.6 (4.6, 4.7, 4.8, ...) and any major >= 5.
			if major > 4 {
				return true
			}
			if major == 4 && minor >= 6 {
				return true
			}
			return false
		}
	}
	// Fallback substring checks for non-standard identifiers.
	for _, tag := range []string{"4.6", "4-6", "4.7", "4-7", "4.8", "4-8", "4.9", "4-9"} {
		if strings.Contains(m, tag) {
			return true
		}
	}
	return false
}

func collectUsageMaps(v interface{}, out *[]map[string]interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			lk := strings.ToLower(k)
			if lk == "usage" || lk == "tokenusage" || lk == "token_usage" {
				if m, ok := child.(map[string]interface{}); ok {
					*out = append(*out, m)
				}
			}
			collectUsageMaps(child, out)
		}
	case []interface{}:
		for _, child := range t {
			collectUsageMaps(child, out)
		}
	}
}

func normalizeChunk(chunk string, previous *string) string {
	if chunk == "" {
		return ""
	}

	prev := *previous
	if prev == "" {
		*previous = chunk
		return chunk
	}

	if chunk == prev {
		return ""
	}

	if strings.HasPrefix(chunk, prev) {
		delta := chunk[len(prev):]
		*previous = chunk
		return delta
	}

	if strings.HasPrefix(prev, chunk) {
		return ""
	}

	maxOverlap := 0
	maxLen := len(prev)
	if len(chunk) < maxLen {
		maxLen = len(chunk)
	}
	for i := maxLen; i > 0; i-- {
		if strings.HasSuffix(prev, chunk[:i]) {
			maxOverlap = i
			break
		}
	}

	*previous = chunk
	if maxOverlap > 0 {
		return chunk[maxOverlap:]
	}

	return chunk
}

func readTokenNumber(m map[string]interface{}, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case int64:
			return int(n), true
		case json.Number:
			if parsed, err := n.Int64(); err == nil {
				return int(parsed), true
			}
		case string:
			if parsed, err := strconv.Atoi(n); err == nil {
				return parsed, true
			}
			if parsed, err := strconv.ParseFloat(n, 64); err == nil {
				return int(parsed), true
			}
		}
	}
	return 0, false
}

// ==================== Tool Use Handling ====================

type toolUseState struct {
	ToolUseID   string
	Name        string
	InputBuffer strings.Builder
	GeneratedID bool
}

func handleToolUseEvent(event map[string]interface{}, current *toolUseState, callback *KiroStreamCallback) *toolUseState {
	toolUseID := firstStringField(event, "toolUseId", "toolUseID", "tool_use_id", "id")
	name := firstStringField(event, "name", "toolName", "tool_name")
	isStop := firstBoolField(event, "stop", "isStop", "done")

	if toolUseID != "" && name != "" {
		if current == nil {
			current = &toolUseState{ToolUseID: toolUseID, Name: name}
		} else if current.ToolUseID != toolUseID {
			if current.GeneratedID && current.Name == name {
				current.ToolUseID = toolUseID
				current.GeneratedID = false
			} else {
				finishToolUse(current, callback)
				current = &toolUseState{ToolUseID: toolUseID, Name: name}
			}
		}
	} else if name != "" && current == nil {
		current = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
	} else if name != "" && current != nil && current.Name != name {
		finishToolUse(current, callback)
		current = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
	}

	if current != nil {
		if input, ok := event["input"].(string); ok {
			current.InputBuffer.WriteString(input)
		} else if inputObj, ok := event["input"].(map[string]interface{}); ok {
			data, _ := json.Marshal(inputObj)
			current.InputBuffer.Reset()
			current.InputBuffer.Write(data)
		}
	}

	if isStop && current != nil {
		finishToolUse(current, callback)
		return nil
	}

	return current
}

func finishToolUse(state *toolUseState, callback *KiroStreamCallback) {
	if state == nil || state.Name == "" || callback == nil || callback.OnToolUse == nil {
		return
	}
	if state.ToolUseID == "" {
		state.ToolUseID = "toolu_" + uuid.New().String()
	}
	var input map[string]interface{}
	if state.InputBuffer.Len() > 0 {
		json.Unmarshal([]byte(state.InputBuffer.String()), &input)
	}
	if input == nil {
		input = make(map[string]interface{})
	}
	callback.OnToolUse(KiroToolUse{
		ToolUseID: state.ToolUseID,
		Name:      state.Name,
		Input:     input,
	})
}

func firstStringField(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstBoolField(m map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if v, ok := m[key].(bool); ok {
			return v
		}
	}
	return false
}

// extractEventType extracts the event type string from AWS Event Stream message headers.
func extractEventType(headers []byte) string {
	offset := 0
	for offset < len(headers) {
		if offset >= len(headers) {
			break
		}
		nameLen := int(headers[offset])
		offset++
		if offset+nameLen > len(headers) {
			break
		}
		name := string(headers[offset : offset+nameLen])
		offset += nameLen
		if offset >= len(headers) {
			break
		}
		valueType := headers[offset]
		offset++

		if valueType == 7 { // String
			if offset+2 > len(headers) {
				break
			}
			valueLen := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2
			if offset+valueLen > len(headers) {
				break
			}
			value := string(headers[offset : offset+valueLen])
			offset += valueLen
			if name == ":event-type" {
				return value
			}
			continue
		}

		// Skip other value types by their fixed byte widths.
		skipSizes := map[byte]int{0: 0, 1: 0, 2: 1, 3: 2, 4: 4, 5: 8, 8: 8, 9: 16}
		if valueType == 6 {
			if offset+2 > len(headers) {
				break
			}
			l := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2 + l
		} else if skip, ok := skipSizes[valueType]; ok {
			offset += skip
		} else {
			break
		}
	}
	return ""
}
