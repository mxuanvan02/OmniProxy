package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"omniproxy/config"
)

// buildAntigravityPayload assembles a payload with a system priming pair, a
// completed tool round-trip, and a current user turn — the shape the translators
// actually produce.
func buildAntigravityPayload() *KiroPayload {
	payload := &KiroPayload{OriginalModel: "gemini-3-pro-high"}
	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "You are a helpful assistant."}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "I will follow these instructions."}},
		{UserInputMessage: &KiroUserInputMessage{Content: "What is the weather?"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{
			ToolUses: []KiroToolUse{{
				ToolUseID: "call-1",
				Name:      "get_weather",
				Input:     map[string]interface{}{"city": "Hanoi"},
			}},
		}},
		{UserInputMessage: &KiroUserInputMessage{
			UserInputMessageContext: &UserInputMessageContext{
				ToolResults: []KiroToolResult{{
					ToolUseID: "call-1",
					Status:    "success",
					Content:   []KiroResultContent{{Text: "32C"}},
				}},
			},
		}},
	}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "Thanks, and tomorrow?",
	}
	return payload
}

func TestKiroPayloadToAntigravityRequestShape(t *testing.T) {
	payload := buildAntigravityPayload()
	body, err := kiroPayloadToAntigravityRequest(payload, &config.Account{}, "proj-1")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	if body["project"] != "proj-1" {
		t.Errorf("project = %v, want proj-1", body["project"])
	}
	if body["model"] != "gemini-3-pro-high" {
		t.Errorf("model = %v, want gemini-3-pro-high", body["model"])
	}
	if body["userAgent"] != "antigravity" {
		t.Errorf("userAgent = %v, want antigravity", body["userAgent"])
	}
	if id, _ := body["requestId"].(string); !strings.HasPrefix(id, "agent-") {
		t.Errorf("requestId = %q, want agent- prefix", id)
	}

	inner, ok := body["request"].(map[string]interface{})
	if !ok {
		t.Fatalf("request is %T, want map", body["request"])
	}

	// systemInstruction must be an object with parts; a plain string is a 400.
	system, ok := inner["systemInstruction"].(map[string]interface{})
	if !ok {
		t.Fatalf("systemInstruction is %T, want object", inner["systemInstruction"])
	}
	if _, ok := system["parts"].([]map[string]interface{}); !ok {
		t.Errorf("systemInstruction.parts is %T, want array", system["parts"])
	}

	contents, ok := inner["contents"].([]map[string]interface{})
	if !ok {
		t.Fatalf("contents is %T, want array", inner["contents"])
	}
	// The priming pair is lifted into systemInstruction, so it must not be
	// replayed as a turn: user, model(tool call), user(tool result), user.
	if len(contents) != 4 {
		t.Fatalf("contents length = %d, want 4: %+v", len(contents), contents)
	}
	wantRoles := []string{"user", "model", "user", "user"}
	for i, want := range wantRoles {
		if got := contents[i]["role"]; got != want {
			t.Errorf("contents[%d].role = %v, want %v", i, got, want)
		}
	}
	// "assistant" is not a valid role for this API.
	for i, turn := range contents {
		if turn["role"] == "assistant" {
			t.Errorf("contents[%d] uses role assistant; Antigravity requires model", i)
		}
	}
}

func TestKiroPayloadToAntigravityRequestPairsToolResultByName(t *testing.T) {
	payload := buildAntigravityPayload()
	body, err := kiroPayloadToAntigravityRequest(payload, &config.Account{}, "proj-1")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	inner := body["request"].(map[string]interface{})
	contents := inner["contents"].([]map[string]interface{})

	call := contents[1]["parts"].([]map[string]interface{})[0]["functionCall"].(map[string]interface{})
	if call["name"] != "get_weather" {
		t.Fatalf("functionCall.name = %v, want get_weather", call["name"])
	}

	// A functionResponse must repeat the name of the call it answers; the Kiro
	// tool-result record carries only the id, so the name is recovered from the
	// earlier functionCall.
	response := contents[2]["parts"].([]map[string]interface{})[0]["functionResponse"].(map[string]interface{})
	if response["name"] != "get_weather" {
		t.Errorf("functionResponse.name = %v, want get_weather", response["name"])
	}
	if response["id"] != "call-1" {
		t.Errorf("functionResponse.id = %v, want call-1", response["id"])
	}
}

func TestKiroPayloadToAntigravityRequestThinkingBudgetStaysBelowMaxTokens(t *testing.T) {
	payload := buildAntigravityPayload()
	payload.InferenceConfig = &InferenceConfig{
		MaxTokens: 1000,
		Thinking:  &ClaudeThinkingConfig{BudgetTokens: 4000},
	}
	body, err := kiroPayloadToAntigravityRequest(payload, &config.Account{}, "proj-1")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	generation := body["request"].(map[string]interface{})["generationConfig"].(map[string]interface{})
	thinking, ok := generation["thinkingConfig"].(map[string]interface{})
	if !ok {
		t.Fatalf("thinkingConfig is %T, want object", generation["thinkingConfig"])
	}
	budget, _ := thinking["thinkingBudget"].(int)
	if budget >= 1000 {
		t.Errorf("thinkingBudget = %d, must be below maxOutputTokens 1000", budget)
	}
}

func TestSanitizeAntigravitySchemaStripsRejectedKeys(t *testing.T) {
	raw := map[string]interface{}{
		"$schema":  "https://json-schema.org/draft/2020-12/schema",
		"$id":      "urn:tool",
		"type":     "object",
		"default":  map[string]interface{}{},
		"examples": []interface{}{"x"},
		"properties": map[string]interface{}{
			"kind": map[string]interface{}{
				"type":  "string",
				"const": "email",
			},
			"nested": map[string]interface{}{
				"type":  "object",
				"$ref":  "#/$defs/other",
				"items": map[string]interface{}{"default": 1, "type": "string"},
			},
		},
		"$defs": map[string]interface{}{"other": map[string]interface{}{"type": "string"}},
	}
	cleaned := sanitizeAntigravitySchema(raw).(map[string]interface{})

	for _, key := range []string{"$schema", "$id", "default", "examples", "$defs"} {
		if _, exists := cleaned[key]; exists {
			t.Errorf("%s survived sanitization; the API rejects it", key)
		}
	}
	properties := cleaned["properties"].(map[string]interface{})
	kind := properties["kind"].(map[string]interface{})
	if _, exists := kind["const"]; exists {
		t.Error("const survived; the API rejects it")
	}
	// const must become enum:[value] rather than being dropped, or the
	// constraint is silently lost.
	enum, ok := kind["enum"].([]interface{})
	if !ok || len(enum) != 1 || enum[0] != "email" {
		t.Errorf("kind.enum = %v, want [email]", kind["enum"])
	}
	nested := properties["nested"].(map[string]interface{})
	if _, exists := nested["$ref"]; exists {
		t.Error("$ref survived in nested schema")
	}
	if _, exists := nested["items"].(map[string]interface{})["default"]; exists {
		t.Error("default survived in nested items")
	}

	// The original must not be mutated: the same payload is reused for other
	// upstreams that accept these keys.
	if _, exists := raw["$schema"]; !exists {
		t.Error("sanitization mutated the caller's schema")
	}
}

func TestSanitizeAntigravityToolName(t *testing.T) {
	cases := map[string]string{
		"get_weather":       "get_weather",
		"mcp:mongodb.query": "mcp:mongodb.query",
		"read-file":         "read-file",
		"mcp/query":         "mcp_query",
		"123_tool":          "_123_tool",
		"has space":         "has_space",
	}
	for input, want := range cases {
		if got := sanitizeAntigravityToolName(input); got != want {
			t.Errorf("sanitizeAntigravityToolName(%q) = %q, want %q", input, got, want)
		}
	}
	long := strings.Repeat("a", 100)
	if got := sanitizeAntigravityToolName(long); len(got) != 64 {
		t.Errorf("long name length = %d, want 64", len(got))
	}
}

func TestClassifyAntigravityFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   antigravityFailureKind
	}{
		{
			// The documented ToS-enforcement message. It must outrank the plain
			// 403 mapping, or a disabled account is retried forever.
			name:   "tos disable",
			status: http.StatusForbidden,
			body:   `{"error":{"message":"This service has been disabled in this account for violation of Terms of Service"}}`,
			want:   antigravityFailureBanned,
		},
		{
			name:   "expired token",
			status: http.StatusUnauthorized,
			body:   `{"error":{"message":"Request had invalid authentication credentials"}}`,
			want:   antigravityFailureAuth,
		},
		{
			name:   "stale credential surfaced as 403",
			status: http.StatusForbidden,
			body:   `{"error":{"message":"Invalid authentication credentials"}}`,
			want:   antigravityFailureAuth,
		},
		{
			name:   "rate limited",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"status":"RESOURCE_EXHAUSTED"}}`,
			want:   antigravityFailureQuota,
		},
		{
			name:   "capacity exhausted",
			status: http.StatusServiceUnavailable,
			body:   `{"error":{"message":"MODEL_CAPACITY_EXHAUSTED: no capacity"}}`,
			want:   antigravityFailureQuota,
		},
		{
			name:   "transient",
			status: http.StatusInternalServerError,
			body:   `{"error":{"message":"internal"}}`,
			want:   antigravityFailureOther,
		},
		{
			// Google asks the account owner to finish a verification step. It
			// arrives as the same 403 PERMISSION_DENIED a terminated account
			// produces, so without an explicit case it was recorded as a
			// permanent ban and the account was switched off.
			name:   "owner verification required",
			status: http.StatusForbidden,
			body: `{"error":{"code":403,"message":"Verify your account to continue.","status":"PERMISSION_DENIED",` +
				`"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"VALIDATION_REQUIRED",` +
				`"domain":"cloudcode-pa.googleapis.com"}]}}`,
			want: antigravityFailureValidation,
		},
		{
			// The ban phrases stay authoritative: an explicit termination wins
			// even when the body also mentions verification.
			name:   "termination outranks verification",
			status: http.StatusForbidden,
			body:   `{"error":{"message":"This service has been disabled in this account. Verify your account to continue."}}`,
			want:   antigravityFailureBanned,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAntigravityFailure(tc.status, tc.body); got != tc.want {
				t.Errorf("classify(%d) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// TestAntigravityValidationDoesNotBan drives the real adapter against a fake
// upstream and pins the consequence of the classification: an owner who still
// has a verification step outstanding must leave the account enabled and
// un-banned, because the remedy is a human action on the Google account.
//
// A terminated account returns the same 403, and that one must still be
// recorded — otherwise the fix would have turned the guard into a no-op.
func TestAntigravityValidationDoesNotBan(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantBanned bool
	}{
		{
			name:   "verification required",
			status: http.StatusForbidden,
			body: `{"error":{"code":403,"message":"Verify your account to continue.","status":"PERMISSION_DENIED",` +
				`"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"VALIDATION_REQUIRED"}]}}`,
			wantBanned: false,
		},
		{
			name:       "account disabled",
			status:     http.StatusForbidden,
			body:       `{"error":{"message":"This service has been disabled in this account for violation of Terms of Service"}}`,
			wantBanned: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// markAntigravityBanned persists through config, so the store has to
			// be initialised for the ban path to be exercised at all.
			initConfigForTests(t)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()

			account := &config.Account{
				ID:              "ag",
				Email:           "owner@example.com",
				AuthMethod:      "antigravity",
				Enabled:         true,
				BaseURL:         upstream.URL,
				AccessToken:     "token",
				ExpiresAt:       time.Now().Add(time.Hour).Unix(),
				GoogleProjectID: "aicode-consumers",
				// Set to now so the cached project is trusted and no
				// loadCodeAssist round-trip is attempted.
				AntigravityProjectCheckedAt: time.Now().Unix(),
			}

			err := CallExternalAntigravity(context.Background(), account,
				buildAntigravityPayload(), &KiroStreamCallback{})
			if err == nil {
				t.Fatal("expected the 403 to surface as an error")
			}

			if tc.wantBanned {
				if account.BanStatus != "BANNED" {
					t.Fatalf("BanStatus = %q, want BANNED for a disabled account", account.BanStatus)
				}
				return
			}
			if account.BanStatus != "" {
				t.Errorf("BanStatus = %q, want empty: owner verification is not a ban", account.BanStatus)
			}
			if !account.Enabled {
				t.Error("account was disabled, want it left enabled")
			}
		})
	}
}

func TestAntigravityRetryDelayReadsRetryInfo(t *testing.T) {
	body := `{"error":{"code":429,"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"3.957525076s"}]}}`
	delay := antigravityRetryDelay(body)
	if delay <= 0 {
		t.Fatalf("delay = %v, want positive", delay)
	}
	if delay.Seconds() < 3.9 || delay.Seconds() > 4.0 {
		t.Errorf("delay = %v, want ~3.96s", delay)
	}
	if antigravityRetryDelay(`{"error":{}}`) != 0 {
		t.Error("delay should be zero when RetryInfo is absent")
	}
}

func TestParseAntigravitySSEEmitsTextToolsAndUsage(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"thinking..."}]}}]}}`,
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}]}}`,
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Hanoi"},"id":"toolu_1"}}]},"finishReason":"STOP"}],` +
			`"usageMetadata":{"promptTokenCount":16,"candidatesTokenCount":4,"thoughtsTokenCount":6,"cachedContentTokenCount":8}}}`,
		"",
	}, "\n\n")

	var text, thinking strings.Builder
	var tools []KiroToolUse
	var inTok, outTok, cacheRead int
	var stopReason string
	callback := &KiroStreamCallback{
		OnText: func(chunk string, isThinking bool) {
			if isThinking {
				thinking.WriteString(chunk)
				return
			}
			text.WriteString(chunk)
		},
		OnToolUse:    func(tu KiroToolUse) { tools = append(tools, tu) },
		OnComplete:   func(in, out int) { inTok, outTok = in, out },
		OnCacheRead:  func(n int) { cacheRead = n },
		OnStopReason: func(reason string) { stopReason = reason },
	}

	if err := parseAntigravitySSE(strings.NewReader(stream), &KiroPayload{}, callback); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if text.String() != "Hello" {
		t.Errorf("text = %q, want Hello", text.String())
	}
	if thinking.String() != "thinking..." {
		t.Errorf("thinking = %q, want thinking...", thinking.String())
	}
	if len(tools) != 1 || tools[0].Name != "get_weather" || tools[0].ToolUseID != "toolu_1" {
		t.Errorf("tools = %+v, want one get_weather/toolu_1", tools)
	}
	if inTok != 16 {
		t.Errorf("inputTokens = %d, want 16", inTok)
	}
	// Thinking tokens are billed as output, so they belong in the total.
	if outTok != 10 {
		t.Errorf("outputTokens = %d, want 10 (candidates 4 + thoughts 6)", outTok)
	}
	if cacheRead != 8 {
		t.Errorf("cacheRead = %d, want 8", cacheRead)
	}
	if stopReason != "stop" {
		t.Errorf("stopReason = %q, want stop", stopReason)
	}
}

func TestParseAntigravitySSEUsageIsNotAccumulatedAcrossFrames(t *testing.T) {
	// Gemini-style streams report cumulative totals on every frame. Adding them
	// up would multiply the reported usage by the number of frames.
	stream := strings.Join([]string{
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1}}}`,
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"b"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}}`,
		"",
	}, "\n\n")

	var inTok, outTok int
	callback := &KiroStreamCallback{
		OnText:     func(string, bool) {},
		OnComplete: func(in, out int) { inTok, outTok = in, out },
	}
	if err := parseAntigravitySSE(strings.NewReader(stream), &KiroPayload{}, callback); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if inTok != 10 {
		t.Errorf("inputTokens = %d, want 10 (not summed)", inTok)
	}
	if outTok != 2 {
		t.Errorf("outputTokens = %d, want 2 (not summed)", outTok)
	}
}

func TestParseAntigravitySSEThinkingOnlyIsNotSuccess(t *testing.T) {
	// A turn that only produced reasoning has no answer in it. Reporting success
	// would hand the client an empty response instead of rotating accounts.
	stream := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"reasoning"}]},"finishReason":"STOP"}]}}` + "\n\n"
	callback := &KiroStreamCallback{OnText: func(string, bool) {}}
	if err := parseAntigravitySSE(strings.NewReader(stream), &KiroPayload{}, callback); err == nil {
		t.Fatal("expected an error for a thinking-only stream")
	}
}

func TestParseAntigravitySSESurfacesInlineError(t *testing.T) {
	stream := `data: {"error":{"code":429,"message":"You have exhausted your capacity on this model.","status":"RESOURCE_EXHAUSTED"}}` + "\n\n"
	callback := &KiroStreamCallback{OnText: func(string, bool) {}}
	err := parseAntigravitySSE(strings.NewReader(stream), &KiroPayload{}, callback)
	if err == nil {
		t.Fatal("expected an error for an inline error frame")
	}
	if !strings.Contains(err.Error(), "exhausted your capacity") {
		t.Errorf("error = %v, want the upstream message", err)
	}
}

func TestParseAntigravitySSESkipsUnparseableFrames(t *testing.T) {
	// Keepalives and unknown future fields are not failures.
	stream := strings.Join([]string{
		`data: not-json`,
		`: keepalive`,
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`,
		"",
	}, "\n\n")
	var text strings.Builder
	callback := &KiroStreamCallback{OnText: func(chunk string, _ bool) { text.WriteString(chunk) }}
	if err := parseAntigravitySSE(strings.NewReader(stream), &KiroPayload{}, callback); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if text.String() != "ok" {
		t.Errorf("text = %q, want ok", text.String())
	}
}

func TestAntigravityLoadResponseProjectAndTier(t *testing.T) {
	// The project arrives either as a bare string or as an object with an id.
	var asString antigravityLoadResponse
	if err := json.Unmarshal([]byte(`{"cloudaicompanionProject":"proj-string"}`), &asString); err != nil {
		t.Fatalf("unmarshal string form: %v", err)
	}
	if got := asString.projectID(); got != "proj-string" {
		t.Errorf("projectID (string form) = %q, want proj-string", got)
	}

	var asObject antigravityLoadResponse
	if err := json.Unmarshal([]byte(`{"cloudaicompanionProject":{"id":"proj-object"}}`), &asObject); err != nil {
		t.Fatalf("unmarshal object form: %v", err)
	}
	if got := asObject.projectID(); got != "proj-object" {
		t.Errorf("projectID (object form) = %q, want proj-object", got)
	}

	var tiers antigravityLoadResponse
	if err := json.Unmarshal([]byte(`{"allowedTiers":[{"id":"LEGACY"},{"id":"STANDARD","isDefault":true}]}`), &tiers); err != nil {
		t.Fatalf("unmarshal tiers: %v", err)
	}
	if got := tiers.defaultTier(); got != "STANDARD" {
		t.Errorf("defaultTier = %q, want STANDARD (the isDefault entry)", got)
	}
}

func TestNormalizeAntigravityStopReason(t *testing.T) {
	cases := map[string]string{
		"STOP":                    "stop",
		"MAX_TOKENS":              "length",
		"SAFETY":                  "content_filter",
		"PROHIBITED_CONTENT":      "content_filter",
		"MALFORMED_FUNCTION_CALL": "tool_use",
	}
	for input, want := range cases {
		if got := normalizeAntigravityStopReason(input); got != want {
			t.Errorf("normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAntigravityHeadersCarryRequiredClientMetadata(t *testing.T) {
	req, err := http.NewRequest("POST", "https://cloudcode-pa.googleapis.com/v1internal:generateContent", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	setAntigravityHeaders(req, "tok")

	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", got)
	}
	// ideType/pluginType are part of the request contract; the API misroutes
	// calls that omit them.
	metadata := req.Header.Get("Client-Metadata")
	var parsed map[string]string
	if err := json.Unmarshal([]byte(metadata), &parsed); err != nil {
		t.Fatalf("Client-Metadata is not JSON (%q): %v", metadata, err)
	}
	if parsed["ideType"] != "ANTIGRAVITY" || parsed["pluginType"] != "GEMINI" {
		t.Errorf("Client-Metadata = %v, want ideType ANTIGRAVITY and pluginType GEMINI", parsed)
	}
	if parsed["platform"] == "" {
		t.Error("Client-Metadata is missing platform")
	}
}

// TestAntigravityPlatformIsAValidClientMetadataEnum pins the platform
// vocabulary to the proto enum
// google.internal.cloud.code.v1internal.ClientMetadata.Platform. The API
// validates metadata.platform against that enum and answers
// INVALID_ARGUMENT for anything else, so a host-OS name like "MACOS" — or the
// bare "DARWIN" — makes loadCodeAssist fail and leaves the account without a
// project. The enum is architecture-qualified, so the value has to carry the
// suffix too.
func TestAntigravityPlatformIsAValidClientMetadataEnum(t *testing.T) {
	valid := map[string]bool{
		"PLATFORM_UNSPECIFIED": true,
		"DARWIN_AMD64":         true,
		"DARWIN_ARM64":         true,
		"LINUX_AMD64":          true,
		"LINUX_ARM64":          true,
		"WINDOWS_AMD64":        true,
	}
	got := antigravityPlatform()
	if !valid[got] {
		t.Errorf("antigravityPlatform() = %q, not a ClientMetadata.Platform enum value", got)
	}
	if meta := antigravityMetadata("")["platform"]; !valid[meta] {
		t.Errorf("metadata.platform = %q, not a ClientMetadata.Platform enum value", meta)
	}
}

// TestAntigravityPlatformTracksHostOSAndArch checks the mapping itself, not
// just membership: the enum is per-OS per-arch, so a hardcoded "DARWIN_ARM64"
// would be wrong on Linux and on Intel Macs. Windows has no ARM64 member, so
// every Windows host reports AMD64.
func TestAntigravityPlatformTracksHostOSAndArch(t *testing.T) {
	got := antigravityPlatform()
	var wantPrefix, wantSuffix string
	switch runtime.GOOS {
	case "darwin":
		wantPrefix = "DARWIN_"
	case "windows":
		wantPrefix, wantSuffix = "WINDOWS_", "AMD64"
	default:
		wantPrefix = "LINUX_"
	}
	if wantSuffix == "" {
		if runtime.GOARCH == "arm64" {
			wantSuffix = "ARM64"
		} else {
			wantSuffix = "AMD64"
		}
	}
	if want := wantPrefix + wantSuffix; got != want {
		t.Errorf("antigravityPlatform() = %q, want %q for %s/%s", got, want, runtime.GOOS, runtime.GOARCH)
	}
}

// TestAntigravityPlatformIsSharedByHeaderAndBody keeps the Client-Metadata
// header and the request metadata on one vocabulary. They are the same proto
// on the server side, so a value that is valid in one and not the other is a
// bug in this client, not a protocol difference.
func TestAntigravityPlatformIsSharedByHeaderAndBody(t *testing.T) {
	req, err := http.NewRequest("POST", "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	setAntigravityHeaders(req, "tok")
	var header map[string]string
	if err := json.Unmarshal([]byte(req.Header.Get("Client-Metadata")), &header); err != nil {
		t.Fatalf("Client-Metadata is not JSON: %v", err)
	}
	if want, got := antigravityMetadata("")["platform"], header["platform"]; got != want {
		t.Errorf("header platform = %q, metadata platform = %q; they must match", got, want)
	}
}

func TestAntigravityEndpointDefaultsToProduction(t *testing.T) {
	// The daily/autopush sandboxes answer 503 MODEL_CAPACITY_EXHAUSTED, so
	// production must be the default rather than a fallback.
	if got := antigravityEndpoint(&config.Account{}); got != "https://cloudcode-pa.googleapis.com" {
		t.Errorf("default endpoint = %q, want production", got)
	}
	if strings.Contains(antigravityEndpoint(&config.Account{}), "sandbox") {
		t.Error("default endpoint must not be a sandbox environment")
	}
	override := &config.Account{BaseURL: "https://example.test/"}
	if got := antigravityEndpoint(override); got != "https://example.test" {
		t.Errorf("override endpoint = %q, want https://example.test", got)
	}
}

func TestAntigravityTokenNeedsRefresh(t *testing.T) {
	const now int64 = 1_000_000
	cases := []struct {
		name    string
		account config.Account
		want    bool
	}{
		{
			name:    "fresh token",
			account: config.Account{AuthMethod: "antigravity", AccessToken: "a", RefreshToken: "r", ExpiresAt: now + 3600},
			want:    false,
		},
		{
			name:    "near expiry",
			account: config.Account{AuthMethod: "antigravity", AccessToken: "a", RefreshToken: "r", ExpiresAt: now + 60},
			want:    true,
		},
		{
			name:    "missing access token",
			account: config.Account{AuthMethod: "antigravity", RefreshToken: "r"},
			want:    true,
		},
		{
			// Without a refresh token there is nothing to exchange; reporting
			// "needs refresh" would only produce a guaranteed failure.
			name:    "no refresh token",
			account: config.Account{AuthMethod: "antigravity", ExpiresAt: now - 10},
			want:    false,
		},
		{
			name:    "other provider",
			account: config.Account{AuthMethod: "codex", AccessToken: "a", RefreshToken: "r", ExpiresAt: now - 10},
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := tc.account
			if got := antigravityTokenNeedsRefresh(&account, now); got != tc.want {
				t.Errorf("needsRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAntigravityFallbackModelsAreChatCapable(t *testing.T) {
	models := antigravityFallbackModels()
	if len(models) == 0 {
		t.Fatal("fallback catalog is empty")
	}
	for _, model := range models {
		if model.ModelId == "" || model.ModelName == "" {
			t.Errorf("model %+v is missing an id or name", model)
		}
		if model.TokenLimits == nil || model.TokenLimits.MaxInputTokens <= 0 {
			t.Errorf("model %s has no usable token limits", model.ModelId)
		}
	}
}

// TestParseAntigravityModelsReadsTheMapShape pins the wire shape of
// FetchAvailableModelsResponse: `models` is a map keyed by model id
// (ModelsEntry → ModelDetails), and the context window is maxTokens, not
// inputTokenLimit. Parsing it as an array made Unmarshal fail and the caller
// silently fall back to the static catalog, so every account reported no live
// models.
func TestParseAntigravityModelsReadsTheMapShape(t *testing.T) {
	raw := []byte(`{
		"models": {
			"gemini-3-pro-high": {
				"displayName": "Gemini 3 Pro (High)",
				"description": "Thinking model",
				"maxTokens": 1000000,
				"maxOutputTokens": 65536
			},
			"claude-sonnet-4-5": {
				"displayName": "Claude Sonnet 4.5",
				"maxTokens": 200000,
				"maxOutputTokens": 64000
			}
		}
	}`)

	models := parseAntigravityModels(raw)
	if len(models) != 2 {
		t.Fatalf("parsed %d models, want 2: %+v", len(models), models)
	}

	byID := make(map[string]ModelInfo, len(models))
	for _, model := range models {
		byID[model.ModelId] = model
	}

	pro, ok := byID["gemini-3-pro-high"]
	if !ok {
		t.Fatalf("model ids = %v, want gemini-3-pro-high among them", byID)
	}
	if pro.ModelName != "Gemini 3 Pro (High)" {
		t.Errorf("ModelName = %q, want the displayName", pro.ModelName)
	}
	if pro.Description != "Thinking model" {
		t.Errorf("Description = %q, want the description", pro.Description)
	}
	if pro.TokenLimits == nil || pro.TokenLimits.MaxInputTokens != 1000000 || pro.TokenLimits.MaxOutputTokens != 65536 {
		t.Errorf("TokenLimits = %+v, want 1000000/65536 from maxTokens/maxOutputTokens", pro.TokenLimits)
	}
	if pro.Provider != "antigravity" {
		t.Errorf("Provider = %q, want antigravity", pro.Provider)
	}

	// Models missing maxTokens keep a nil limit rather than reporting 0/0.
	if sonnet := byID["claude-sonnet-4-5"]; sonnet.TokenLimits == nil || sonnet.TokenLimits.MaxInputTokens != 200000 {
		t.Errorf("TokenLimits = %+v, want 200000/64000", sonnet.TokenLimits)
	}

	// The order is stable, so a refresh does not reshuffle the model picker.
	if models[0].ModelId != "claude-sonnet-4-5" || models[1].ModelId != "gemini-3-pro-high" {
		t.Errorf("order = %q,%q, want claude-sonnet-4-5 then gemini-3-pro-high",
			models[0].ModelId, models[1].ModelId)
	}
}

// TestParseAntigravityModelsHandlesEmptyAndInvalid covers the paths that must
// report "no catalog" so the caller can fall back instead of publishing an
// empty list over a working one.
func TestParseAntigravityModelsHandlesEmptyAndInvalid(t *testing.T) {
	// A model id already carrying the "models/" prefix, and one with no
	// displayName, both have to come out usable.
	models := parseAntigravityModels([]byte(`{"models":{"models/gemini-2.5-flash":{}}}`))
	if len(models) != 1 {
		t.Fatalf("parsed %d models, want 1", len(models))
	}
	if models[0].ModelId != "gemini-2.5-flash" {
		t.Errorf("ModelId = %q, want the models/ prefix stripped", models[0].ModelId)
	}
	if models[0].ModelName != "gemini-2.5-flash" {
		t.Errorf("ModelName = %q, want the id when displayName is absent", models[0].ModelName)
	}

	for _, raw := range []string{
		``,
		`not json`,
		`{"models":[]}`,
		`{"models":{}}`,
		`{"availableModels":[{"modelId":"gemini-2.5-pro"}]}`,
	} {
		if got := parseAntigravityModels([]byte(raw)); len(got) != 0 {
			t.Errorf("parseAntigravityModels(%q) = %+v, want nil so the caller falls back", raw, got)
		}
	}
}
