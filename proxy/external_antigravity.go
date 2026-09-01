// Package proxy: external_antigravity.go
//
// Google Antigravity (Cloud Code Assist) chat backend.
//
// Antigravity is Google's unified gateway: one Gemini-shaped API in front of
// Gemini, Claude and GPT-OSS models. Generation goes to
// {endpoint}/v1internal:streamGenerateContent?alt=sse, authenticated with the
// account's Google OAuth access token and scoped to the cloudaicompanion
// project that account actually owns.
//
// Google's Antigravity Terms of Service prohibit third-party clients, and
// Google has disabled accounts for using them. OmniProxy sends the protocol
// fields this API requires and nothing beyond them: no randomised client
// fingerprints, no synthetic project ids, no fabricated telemetry. An account
// used through this path can be disabled at any time; classifyAntigravityFailure
// detects that and marks the account BANNED so the pool stops selecting it,
// rather than trying to look like a different client.
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"omniproxy/auth"
	"omniproxy/config"
	"omniproxy/logger"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

const antigravityAuthMethod = "antigravity"

// antigravityDefaultEndpoint is production. The daily/autopush sandboxes exist
// and some third-party proxies default to them, but they run with minimal
// capacity and answer 503 MODEL_CAPACITY_EXHAUSTED on every model, so they are
// not a fallback worth having.
const antigravityDefaultEndpoint = "https://cloudcode-pa.googleapis.com"

// antigravityIDEVersion is reported in the User-Agent. It is a real published
// Antigravity version, kept fixed so the proxy does not present a moving
// target to the upstream.
const antigravityIDEVersion = "1.18.3"

const (
	antigravityStreamAction  = "/v1internal:streamGenerateContent?alt=sse"
	antigravityLoadAction    = "/v1internal:loadCodeAssist"
	antigravityOnboardAction = "/v1internal:onboardUser"
)

// isAntigravityAccount reports whether the account routes to Cloud Code Assist.
func isAntigravityAccount(account *config.Account) bool {
	return account != nil && account.AuthMethod == antigravityAuthMethod
}

// antigravityEndpoint resolves the upstream base URL. A per-account BaseURL
// override exists for operators who must pin a specific environment.
func antigravityEndpoint(account *config.Account) string {
	if account != nil {
		if base := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/"); base != "" {
			return base
		}
	}
	return antigravityDefaultEndpoint
}

// antigravityPlatform maps the host OS onto the platform vocabulary the
// Client-Metadata header uses. It reports the machine OmniProxy actually runs
// on; misreporting it would be a claim about the client, not a protocol need.
func antigravityPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "WINDOWS"
	case "darwin":
		return "MACOS"
	default:
		return "LINUX"
	}
}

// setAntigravityHeaders applies the headers the Cloud Code Assist API requires.
// ideType/pluginType are part of the request contract — the API misroutes calls
// without them — and are sent as fixed values.
func setAntigravityHeaders(req *http.Request, accessToken string) {
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("antigravity/%s %s/%s", antigravityIDEVersion, runtime.GOOS, runtime.GOARCH))
	req.Header.Set("X-Goog-Api-Client", "google-cloud-sdk vscode_cloudshelleditor/0.1")
	req.Header.Set("Client-Metadata", fmt.Sprintf(`{"ideType":"ANTIGRAVITY","platform":%q,"pluginType":"GEMINI"}`, antigravityPlatform()))
}

// ==================== Token refresh ====================

// antigravityTokenNeedsRefresh reports whether the access token is expired or
// close enough to expiry that a long stream would outlive it.
func antigravityTokenNeedsRefresh(account *config.Account, now int64) bool {
	if !isAntigravityAccount(account) {
		return false
	}
	if strings.TrimSpace(account.RefreshToken) == "" {
		return false
	}
	if strings.TrimSpace(account.AccessToken) == "" {
		return true
	}
	return account.ExpiresAt > 0 && account.ExpiresAt-now < 300
}

// refreshAntigravityAccountToken exchanges the stored refresh token for a new
// access token and persists it. Google does not rotate the refresh token on
// this grant, so an empty value in the response keeps the stored one.
func refreshAntigravityAccountToken(account *config.Account) error {
	if account == nil {
		return fmt.Errorf("antigravity refresh: account is nil")
	}
	tokens, err := auth.RefreshAntigravityToken(account.RefreshToken)
	if err != nil {
		return err
	}
	account.AccessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		account.RefreshToken = tokens.RefreshToken
	}
	account.ExpiresAt = tokens.ExpiresAt
	account.TokenRefreshedAt = time.Now().Unix()
	return config.UpdateAccountToken(account.ID, account.AccessToken, account.RefreshToken, account.ExpiresAt)
}

// ==================== Failure classification ====================

type antigravityFailureKind int

const (
	antigravityFailureOther antigravityFailureKind = iota
	// antigravityFailureAuth means the token was rejected and a refresh may fix it.
	antigravityFailureAuth
	// antigravityFailureBanned means Google disabled the service for this
	// account. Using a third-party client is the documented cause, so this is an
	// expected terminal state rather than something to work around.
	antigravityFailureBanned
	// antigravityFailureQuota means the model's capacity for this account is
	// exhausted; another account may still serve the request.
	antigravityFailureQuota
)

// antigravityBanPhrases are the upstream messages that mean the account itself
// has been disabled. They are matched on the response body because the HTTP
// status alone (403) does not separate "disabled account" from "missing scope".
var antigravityBanPhrases = []string{
	"has been disabled",
	"violation of terms of service",
	"violation of the terms of service",
	"account has been suspended",
	"consumer_suspended",
	"billing_disabled",
}

// antigravityAuthPhrases mean the credential is stale rather than revoked.
var antigravityAuthPhrases = []string{
	"invalid authentication credentials",
	"invalid_grant",
	"request had invalid authentication",
	"access token is expired",
	"unauthenticated",
}

func classifyAntigravityFailure(status int, body string) antigravityFailureKind {
	lower := strings.ToLower(body)
	for _, phrase := range antigravityBanPhrases {
		if strings.Contains(lower, phrase) {
			return antigravityFailureBanned
		}
	}
	switch status {
	case http.StatusUnauthorized:
		return antigravityFailureAuth
	case http.StatusTooManyRequests:
		return antigravityFailureQuota
	case http.StatusForbidden:
		for _, phrase := range antigravityAuthPhrases {
			if strings.Contains(lower, phrase) {
				return antigravityFailureAuth
			}
		}
		// A bare PERMISSION_DENIED on an account that authenticated a moment ago
		// is how the disable shows up when no explanatory message is attached.
		return antigravityFailureBanned
	}
	if status == http.StatusServiceUnavailable && strings.Contains(lower, "capacity") {
		return antigravityFailureQuota
	}
	return antigravityFailureOther
}

// markAntigravityBanned records the terminal state so the pool stops selecting
// the account and the admin UI can show why.
func markAntigravityBanned(account *config.Account, body string) {
	if account == nil {
		return
	}
	account.BanStatus = "BANNED"
	account.BanReason = truncateAntigravityReason(body)
	account.BanTime = time.Now().Unix()
	logger.Warnf("[Antigravity] account %s disabled upstream: %s", account.Email, account.BanReason)
	_ = config.SetAccountBanStatus(account.ID, account.BanStatus, account.BanReason)
}

func truncateAntigravityReason(reason string) string {
	reason = strings.TrimSpace(reason)
	const max = 300
	if len(reason) > max {
		return reason[:max] + "..."
	}
	return reason
}

// antigravityRetryDelayPattern reads google.rpc.RetryInfo out of a 429 body so
// the caller can honour the upstream's own backoff instead of guessing.
var antigravityRetryDelayPattern = regexp.MustCompile(`"retryDelay"\s*:\s*"([0-9.]+)s"`)

func antigravityRetryDelay(body string) time.Duration {
	match := antigravityRetryDelayPattern.FindStringSubmatch(body)
	if len(match) != 2 {
		return 0
	}
	var seconds float64
	if _, err := fmt.Sscanf(match[1], "%f", &seconds); err != nil || seconds <= 0 {
		return 0
	}
	if seconds > 300 {
		seconds = 300
	}
	return time.Duration(seconds * float64(time.Second))
}

// ==================== Project discovery ====================

// antigravityProjectTTL bounds how long a resolved project id is trusted before
// it is re-checked. Tier and project assignment are server-side state that can
// change without any local signal.
const antigravityProjectTTL = 24 * time.Hour

// antigravityMetadata is the client descriptor loadCodeAssist and onboardUser
// both take. duetProject is only set once a project is known.
func antigravityMetadata(projectID string) map[string]string {
	metadata := map[string]string{
		"ideType":    "ANTIGRAVITY",
		"platform":   antigravityPlatform(),
		"pluginType": "GEMINI",
	}
	if projectID = strings.TrimSpace(projectID); projectID != "" {
		metadata["duetProject"] = projectID
	}
	return metadata
}

// antigravityLoadResponse is the subset of loadCodeAssist this proxy reads.
// cloudaicompanionProject is returned either as a bare string or as an object,
// depending on the account type, so it is decoded loosely.
type antigravityLoadResponse struct {
	CloudaicompanionProject json.RawMessage `json:"cloudaicompanionProject"`
	CurrentTier             *struct {
		ID string `json:"id"`
	} `json:"currentTier"`
	AllowedTiers []struct {
		ID        string `json:"id"`
		IsDefault bool   `json:"isDefault"`
	} `json:"allowedTiers"`
}

func (r *antigravityLoadResponse) projectID() string {
	if r == nil || len(r.CloudaicompanionProject) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(r.CloudaicompanionProject, &asString) == nil {
		return strings.TrimSpace(asString)
	}
	var asObject struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(r.CloudaicompanionProject, &asObject) == nil {
		return strings.TrimSpace(asObject.ID)
	}
	return ""
}

// defaultTier picks the tier onboardUser should be called with. The upstream
// marks one entry default; the first entry is the documented fallback.
func (r *antigravityLoadResponse) defaultTier() string {
	if r == nil {
		return ""
	}
	if r.CurrentTier != nil && strings.TrimSpace(r.CurrentTier.ID) != "" {
		return strings.TrimSpace(r.CurrentTier.ID)
	}
	for _, tier := range r.AllowedTiers {
		if tier.IsDefault && strings.TrimSpace(tier.ID) != "" {
			return strings.TrimSpace(tier.ID)
		}
	}
	if len(r.AllowedTiers) > 0 {
		return strings.TrimSpace(r.AllowedTiers[0].ID)
	}
	return ""
}

// ensureAntigravityProject resolves the cloudaicompanion project for an account
// and caches it on the account record.
//
// The project id is deliberately never defaulted to a constant. Third-party
// proxies commonly hardcode one shared project id, which sends every user's
// traffic to a project none of them own; that shared id is exactly what appears
// in Google's disabled-project reports. An account whose project cannot be
// resolved is an error here, not something to paper over.
func ensureAntigravityProject(account *config.Account) (string, error) {
	if account == nil {
		return "", fmt.Errorf("antigravity project: account is nil")
	}
	now := time.Now()
	if project := strings.TrimSpace(account.GoogleProjectID); project != "" {
		if account.AntigravityProjectCheckedAt == 0 ||
			now.Sub(time.Unix(account.AntigravityProjectCheckedAt, 0)) < antigravityProjectTTL {
			return project, nil
		}
	}

	loaded, err := antigravityLoadCodeAssist(account, account.GoogleProjectID)
	if err != nil {
		// A stale cached project is better than failing the request outright
		// when discovery itself is what broke.
		if project := strings.TrimSpace(account.GoogleProjectID); project != "" {
			logger.Warnf("[Antigravity] loadCodeAssist for %s failed, keeping cached project: %v", account.Email, err)
			return project, nil
		}
		return "", err
	}

	tier := loaded.defaultTier()
	project := loaded.projectID()
	if project == "" {
		// A account that has never used Code Assist has no project yet;
		// onboardUser provisions the managed one Google assigns to it.
		if tier == "" {
			tier = "FREE"
		}
		project, err = antigravityOnboardUser(account, tier, account.GoogleProjectID)
		if err != nil {
			return "", err
		}
	}
	if project == "" {
		return "", fmt.Errorf("antigravity project: account %s has no cloudaicompanion project (open Antigravity once and sign in)", account.Email)
	}

	account.GoogleProjectID = project
	account.AntigravityTier = tier
	account.AntigravityProjectCheckedAt = now.Unix()
	_ = config.UpdateAccountAntigravityProject(account.ID, project, tier, now.Unix())
	logger.Infof("[Antigravity] resolved project %s (tier %s) for %s", project, tier, account.Email)
	return project, nil
}

func antigravityLoadCodeAssist(account *config.Account, projectID string) (*antigravityLoadResponse, error) {
	body, err := json.Marshal(map[string]interface{}{"metadata": antigravityMetadata(projectID)})
	if err != nil {
		return nil, err
	}
	raw, err := antigravityPostJSON(account, antigravityLoadAction, body)
	if err != nil {
		return nil, fmt.Errorf("antigravity loadCodeAssist: %w", err)
	}
	var parsed antigravityLoadResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("antigravity loadCodeAssist: parse: %w", err)
	}
	return &parsed, nil
}

// antigravityOnboardUser provisions the managed project. The upstream answers a
// long-running operation, so it is polled until done.
func antigravityOnboardUser(account *config.Account, tier, projectID string) (string, error) {
	body, err := json.Marshal(map[string]interface{}{
		"tierId":   tier,
		"metadata": antigravityMetadata(projectID),
	})
	if err != nil {
		return "", err
	}
	for attempt := 0; attempt < 6; attempt++ {
		raw, err := antigravityPostJSON(account, antigravityOnboardAction, body)
		if err != nil {
			return "", fmt.Errorf("antigravity onboardUser: %w", err)
		}
		var parsed struct {
			Done     bool `json:"done"`
			Response struct {
				CloudaicompanionProject struct {
					ID string `json:"id"`
				} `json:"cloudaicompanionProject"`
			} `json:"response"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return "", fmt.Errorf("antigravity onboardUser: parse: %w", err)
		}
		if parsed.Done {
			if id := strings.TrimSpace(parsed.Response.CloudaicompanionProject.ID); id != "" {
				return id, nil
			}
			return strings.TrimSpace(projectID), nil
		}
		time.Sleep(3 * time.Second)
	}
	return "", fmt.Errorf("antigravity onboardUser: provisioning did not finish in time")
}

// antigravityPostJSON performs an authenticated control-plane call, refreshing
// the access token once on an auth failure.
func antigravityPostJSON(account *config.Account, action string, body []byte) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if antigravityTokenNeedsRefresh(account, time.Now().Unix()) {
			if err := refreshAntigravityAccountToken(account); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequest("POST", antigravityEndpoint(account)+action, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		setAntigravityHeaders(req, account.AccessToken)
		req.Header.Set("Accept", "application/json")

		resp, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(req)
		if err != nil {
			return nil, err
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return raw, nil
		}

		text := string(raw)
		switch classifyAntigravityFailure(resp.StatusCode, text) {
		case antigravityFailureBanned:
			markAntigravityBanned(account, text)
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateErrBody(raw))
		case antigravityFailureAuth:
			if attempt == 0 {
				// Force a refresh even when the local expiry looked fine: the
				// upstream is the authority on whether the token still works.
				account.ExpiresAt = 0
				continue
			}
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateErrBody(raw))
	}
	return nil, fmt.Errorf("antigravity request %s exhausted retries", action)
}

// ==================== Request translation ====================

// antigravityUnsupportedSchemaKeys are JSON Schema fields the Cloud Code Assist
// API rejects with HTTP 400. They are documentation/metadata fields with no
// effect on validation here, so stripping them is lossless for the model.
var antigravityUnsupportedSchemaKeys = []string{"$schema", "$id", "$ref", "$defs", "definitions", "default", "examples"}

// sanitizeAntigravitySchema adapts a JSON Schema to what the API accepts:
// unsupported metadata keys are dropped and `const` is rewritten as a
// single-member enum, which is the same constraint expressed in supported
// vocabulary. The input is cloned so other upstreams still see the original.
func sanitizeAntigravitySchema(schema interface{}) interface{} {
	cloned := cloneSchemaValue(schema)
	cleanAntigravitySchema(cloned)
	return cloned
}

func cleanAntigravitySchema(value interface{}) {
	switch current := value.(type) {
	case map[string]interface{}:
		for _, key := range antigravityUnsupportedSchemaKeys {
			delete(current, key)
		}
		// `const: x` is not accepted; `enum: [x]` expresses the same constraint.
		if constValue, exists := current["const"]; exists {
			delete(current, "const")
			if _, hasEnum := current["enum"]; !hasEnum {
				current["enum"] = []interface{}{constValue}
			}
		}
		for _, child := range current {
			cleanAntigravitySchema(child)
		}
	case []interface{}:
		for _, child := range current {
			cleanAntigravitySchema(child)
		}
	}
}

// antigravityToolNamePattern matches the names the API accepts: a leading
// letter or underscore, then letters, digits, underscores, dots, colons or
// dashes. Slashes and spaces are rejected upstream.
var antigravityToolNamePattern = regexp.MustCompile(`[^a-zA-Z0-9_.:-]`)

// sanitizeAntigravityToolName rewrites a tool name into the accepted character
// set. The original name is preserved in the payload's ToolNameMap by the
// caller, so responses still reach the client under the name it sent.
func sanitizeAntigravityToolName(name string) string {
	name = antigravityToolNamePattern.ReplaceAllString(strings.TrimSpace(name), "_")
	if name == "" {
		return "tool"
	}
	if c := name[0]; !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		name = "_" + name
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// antigravityParts builds a Gemini `parts` array from Kiro text + images.
func antigravityParts(text string, images []KiroImage) []map[string]interface{} {
	parts := make([]map[string]interface{}, 0, 1+len(images))
	for _, image := range images {
		format := strings.TrimSpace(image.Format)
		if format == "" {
			format = "png"
		}
		if image.Source.Bytes == "" {
			continue
		}
		parts = append(parts, map[string]interface{}{
			"inlineData": map[string]string{
				"mimeType": "image/" + format,
				"data":     image.Source.Bytes,
			},
		})
	}
	if strings.TrimSpace(text) != "" {
		parts = append(parts, map[string]interface{}{"text": text})
	}
	return parts
}

// kiroPayloadToAntigravityRequest converts a KiroPayload into the wrapped
// Cloud Code Assist request body.
//
// The wire format is Gemini-shaped and differs from OpenAI's in ways that are
// rejected rather than ignored when wrong: the assistant role is "model" (not
// "assistant"), systemInstruction must be an object with `parts` (a plain
// string is a 400), and tool results are `functionResponse` parts carried on a
// user turn rather than a separate role.
func kiroPayloadToAntigravityRequest(payload *KiroPayload, account *config.Account, projectID string) (map[string]interface{}, error) {
	if payload == nil {
		return nil, fmt.Errorf("nil payload")
	}

	modelID := strings.TrimSpace(payload.OriginalModel)
	if modelID == "" {
		modelID = strings.TrimSpace(payload.ConversationState.CurrentMessage.UserInputMessage.ModelID)
	}
	if modelID == "" {
		return nil, fmt.Errorf("antigravity request: no model id")
	}
	modelID = stripInternalModelPrefix(modelID)
	if account != nil {
		if mapped := strings.TrimSpace(account.ModelMappings[modelID]); mapped != "" {
			modelID = mapped
		}
	}

	history := payload.ConversationState.History

	// The translators inject the system prompt as a leading user/assistant
	// priming pair. Cloud Code Assist has a dedicated systemInstruction field,
	// so the pair is lifted out instead of being replayed as a real turn.
	systemInstruction := ""
	if len(history) >= 2 {
		first, second := history[0], history[1]
		if first.UserInputMessage != nil && second.AssistantResponseMessage != nil &&
			strings.Contains(strings.ToLower(strings.TrimSpace(second.AssistantResponseMessage.Content)), "i will follow") {
			systemInstruction = strings.TrimSpace(first.UserInputMessage.Content)
			history = history[2:]
		}
	}
	if systemInstruction == "" && len(history) > 0 && history[0].UserInputMessage != nil {
		if strings.HasPrefix(strings.TrimSpace(history[0].UserInputMessage.Content), "You are ") {
			systemInstruction = strings.TrimSpace(history[0].UserInputMessage.Content)
			history = history[1:]
		}
	}

	contents := make([]map[string]interface{}, 0, len(history)+1)
	appendTurn := func(role string, parts []map[string]interface{}) {
		if len(parts) == 0 {
			return
		}
		contents = append(contents, map[string]interface{}{"role": role, "parts": parts})
	}

	// toolCallNames tracks the sanitized name each tool-use id was issued under.
	// A functionResponse must repeat the same name as its functionCall, and the
	// Kiro tool-result record carries only the id.
	toolCallNames := make(map[string]string)

	appendUserTurn := func(message *KiroUserInputMessage) {
		if message == nil {
			return
		}
		var parts []map[string]interface{}
		if message.UserInputMessageContext != nil {
			for _, result := range message.UserInputMessageContext.ToolResults {
				text := ""
				if len(result.Content) > 0 {
					text = result.Content[0].Text
				}
				name := toolCallNames[result.ToolUseID]
				if name == "" {
					name = "tool"
				}
				response := map[string]interface{}{"output": text}
				if strings.EqualFold(strings.TrimSpace(result.Status), "error") {
					response = map[string]interface{}{"error": text}
				}
				parts = append(parts, map[string]interface{}{
					"functionResponse": map[string]interface{}{
						"id":       result.ToolUseID,
						"name":     name,
						"response": response,
					},
				})
			}
		}
		parts = append(parts, antigravityParts(message.Content, message.Images)...)
		appendTurn("user", parts)
	}

	for _, entry := range history {
		switch {
		case entry.UserInputMessage != nil:
			appendUserTurn(entry.UserInputMessage)
		case entry.AssistantResponseMessage != nil:
			assistant := entry.AssistantResponseMessage
			var parts []map[string]interface{}
			if strings.TrimSpace(assistant.Content) != "" {
				parts = append(parts, map[string]interface{}{"text": assistant.Content})
			}
			for _, use := range assistant.ToolUses {
				name := sanitizeAntigravityToolName(restoreToolName(payload, use.Name))
				toolCallNames[use.ToolUseID] = name
				call := map[string]interface{}{"name": name, "args": use.Input}
				if use.ToolUseID != "" {
					call["id"] = use.ToolUseID
				}
				parts = append(parts, map[string]interface{}{"functionCall": call})
			}
			appendTurn("model", parts)
		}
	}

	current := payload.ConversationState.CurrentMessage.UserInputMessage
	appendUserTurn(&current)

	if len(contents) == 0 {
		return nil, fmt.Errorf("antigravity request: no content to send")
	}

	inner := map[string]interface{}{"contents": contents}
	if systemInstruction != "" {
		inner["systemInstruction"] = map[string]interface{}{
			"parts": []map[string]interface{}{{"text": systemInstruction}},
		}
	}

	generationConfig := map[string]interface{}{}
	if payload.InferenceConfig != nil {
		cfg := payload.InferenceConfig
		if cfg.MaxTokens > 0 {
			generationConfig["maxOutputTokens"] = cfg.MaxTokens
		}
		if cfg.Temperature > 0 {
			generationConfig["temperature"] = cfg.Temperature
		}
		if cfg.TopP > 0 {
			generationConfig["topP"] = cfg.TopP
		}
		if cfg.Thinking != nil && cfg.Thinking.BudgetTokens > 0 {
			budget := cfg.Thinking.BudgetTokens
			// The API requires maxOutputTokens > thinkingBudget; a request that
			// violates it is rejected outright.
			if cfg.MaxTokens > 0 && budget >= cfg.MaxTokens {
				budget = cfg.MaxTokens - 1
			}
			if budget > 0 {
				generationConfig["thinkingConfig"] = map[string]interface{}{
					"thinkingBudget":  budget,
					"includeThoughts": true,
				}
			}
		}
	}
	if len(generationConfig) > 0 {
		inner["generationConfig"] = generationConfig
	}

	if current.UserInputMessageContext != nil && len(current.UserInputMessageContext.Tools) > 0 {
		declarations := make([]map[string]interface{}, 0, len(current.UserInputMessageContext.Tools))
		for _, wrapper := range current.UserInputMessageContext.Tools {
			original := restoreToolName(payload, wrapper.ToolSpecification.Name)
			name := sanitizeAntigravityToolName(original)
			if payload.ToolNameMap == nil {
				payload.ToolNameMap = make(map[string]string)
			}
			if name != original {
				payload.ToolNameMap[name] = original
			}
			declaration := map[string]interface{}{"name": name}
			if description := strings.TrimSpace(wrapper.ToolSpecification.Description); description != "" {
				declaration["description"] = description
			}
			if schema := wrapper.ToolSpecification.InputSchema.JSON; schema != nil {
				declaration["parameters"] = sanitizeAntigravitySchema(schema)
			}
			declarations = append(declarations, declaration)
		}
		inner["tools"] = []map[string]interface{}{{"functionDeclarations": declarations}}
	}

	return map[string]interface{}{
		"project":   projectID,
		"model":     modelID,
		"request":   inner,
		"userAgent": "antigravity",
		"requestId": "agent-" + uuid.New().String(),
	}, nil
}

// ==================== Response parsing ====================

// antigravityEnvelope is one Cloud Code Assist response object. Both the
// non-stream body and every SSE `data:` frame use this shape.
type antigravityEnvelope struct {
	Response struct {
		Candidates []struct {
			Content struct {
				Role  string `json:"role"`
				Parts []struct {
					Text             string                 `json:"text"`
					Thought          bool                   `json:"thought"`
					ThoughtSignature string                 `json:"thoughtSignature"`
					FunctionCall     *struct {
						Name string                 `json:"name"`
						Args map[string]interface{} `json:"args"`
						ID   string                 `json:"id"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount"`
		} `json:"usageMetadata"`
	} `json:"response"`
}

// antigravityUsage accumulates the terminal counters. Gemini-style streams
// report cumulative totals on every frame rather than deltas, so each frame
// overwrites the running value instead of adding to it.
type antigravityUsage struct {
	inputTokens  int
	outputTokens int
	cachedTokens int
	stopReason   string
}

// applyAntigravityEnvelope replays one response object through the callback.
// It reports whether any visible output (text or tool call) was emitted.
func applyAntigravityEnvelope(env *antigravityEnvelope, payload *KiroPayload, callback *KiroStreamCallback, usage *antigravityUsage) bool {
	if env == nil {
		return false
	}
	meta := env.Response.UsageMetadata
	if meta.PromptTokenCount > 0 {
		usage.inputTokens = meta.PromptTokenCount
	}
	// Thinking tokens are billed as output but reported separately, so they are
	// added in rather than left out of the total.
	if total := meta.CandidatesTokenCount + meta.ThoughtsTokenCount; total > 0 {
		usage.outputTokens = total
	}
	if meta.CachedContentTokenCount > 0 {
		usage.cachedTokens = meta.CachedContentTokenCount
	}

	emitted := false
	for _, candidate := range env.Response.Candidates {
		for _, part := range candidate.Content.Parts {
			if part.FunctionCall != nil {
				call := part.FunctionCall
				if callback != nil && callback.OnToolUse != nil {
					id := strings.TrimSpace(call.ID)
					if id == "" {
						// Downstream protocols require an id to pair the result
						// back to the call; Gemini omits it for some models.
						id = "toolu_" + uuid.New().String()
					}
					args := call.Args
					if args == nil {
						args = map[string]interface{}{}
					}
					callback.OnToolUse(KiroToolUse{
						ToolUseID: id,
						Name:      restoreToolName(payload, call.Name),
						Input:     args,
					})
				}
				emitted = true
				continue
			}
			if part.Text == "" {
				continue
			}
			if callback != nil && callback.OnText != nil {
				callback.OnText(part.Text, part.Thought)
			}
			// A thought block is not an answer: retry logic must still be able
			// to rotate accounts after thinking-only output.
			if !part.Thought {
				emitted = true
			}
		}
		if reason := strings.TrimSpace(candidate.FinishReason); reason != "" {
			usage.stopReason = reason
		}
	}
	if emitted && callback != nil && callback.OnOutput != nil {
		callback.OnOutput()
	}
	return emitted
}

// finishAntigravity reports the accumulated usage and terminal reason.
func finishAntigravity(callback *KiroStreamCallback, usage *antigravityUsage) {
	if callback == nil {
		return
	}
	if usage.cachedTokens > 0 && callback.OnCacheRead != nil {
		callback.OnCacheRead(usage.cachedTokens)
	}
	if usage.stopReason != "" && callback.OnStopReason != nil {
		callback.OnStopReason(normalizeAntigravityStopReason(usage.stopReason))
	}
	if callback.OnComplete != nil {
		callback.OnComplete(usage.inputTokens, usage.outputTokens)
	}
}

// normalizeAntigravityStopReason maps Gemini finish reasons onto the vocabulary
// the response adapters already understand.
func normalizeAntigravityStopReason(reason string) string {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII", "RECITATION":
		return "content_filter"
	case "MALFORMED_FUNCTION_CALL":
		return "tool_use"
	default:
		return strings.ToLower(strings.TrimSpace(reason))
	}
}

// parseAntigravitySSE reads a streamGenerateContent SSE stream and replays it
// through the callback. Unlike OpenAI's stream there is no [DONE] sentinel: the
// stream ends when the body closes, and the terminal state is whichever
// finishReason arrived last.
func parseAntigravitySSE(body io.Reader, payload *KiroPayload, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}
	// Whitespace-only output is withheld so a turn that produces nothing real
	// stays retryable instead of reaching the client as a finished empty answer.
	gate := newBlankOutputGate(callback)
	callback = gate.callback()

	var watchdog *sseIdleWatchdog
	if rc, ok := body.(io.ReadCloser); ok {
		if watchdog = newSSEIdleWatchdog(rc); watchdog != nil {
			watchdog.Start()
			defer watchdog.Stop()
		}
	}

	br := bufio.NewReaderSize(body, 32*1024)
	usage := &antigravityUsage{}
	emitted := false

	handle := func(data string) error {
		data = strings.TrimSpace(data)
		if data == "" {
			return nil
		}
		// An upstream error can arrive inside a 200 stream; surface it rather
		// than closing the turn as a successful empty response.
		if message := antigravityErrorMessage([]byte(data)); message != "" {
			return fmt.Errorf("antigravity stream error: %s", message)
		}
		var env antigravityEnvelope
		if err := json.Unmarshal([]byte(data), &env); err != nil {
			// Skip frames we cannot parse instead of failing the whole turn:
			// keepalives and future field additions are not errors.
			return nil
		}
		if applyAntigravityEnvelope(&env, payload, callback, usage) {
			emitted = true
			if watchdog != nil {
				watchdog.DataReceived()
			}
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if watchdog != nil && watchdog.TimedOut() {
				return ErrStreamIdleTimeout
			}
			if err == io.EOF {
				if trimmed := strings.TrimRight(line, "\r\n"); strings.HasPrefix(trimmed, "data:") {
					if handleErr := handle(strings.TrimPrefix(trimmed, "data:")); handleErr != nil {
						return handleErr
					}
				}
				break
			}
			return fmt.Errorf("antigravity SSE read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		if handleErr := handle(strings.TrimPrefix(line, "data:")); handleErr != nil {
			return handleErr
		}
	}

	if !emitted {
		return fmt.Errorf("antigravity stream ended without assistant output")
	}
	if !gate.meaningful {
		return blankTurnError(normalizeAntigravityStopReason(usage.stopReason))
	}
	finishAntigravity(callback, usage)
	return nil
}

// parseAntigravityJSON handles the non-stream generateContent body, used when
// the upstream ignores the stream action and answers with a single object.
func parseAntigravityJSON(body io.Reader, payload *KiroPayload, callback *KiroStreamCallback) error {
	raw, err := io.ReadAll(io.LimitReader(body, maxAntigravityResponseBytes))
	if err != nil {
		return fmt.Errorf("antigravity response read: %w", err)
	}
	if message := antigravityErrorMessage(raw); message != "" {
		return fmt.Errorf("antigravity error: %s", message)
	}
	var env antigravityEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("antigravity response parse: %w", err)
	}
	gate := newBlankOutputGate(callback)
	gated := gate.callback()
	usage := &antigravityUsage{}
	if !applyAntigravityEnvelope(&env, payload, gated, usage) {
		return fmt.Errorf("antigravity response carried no assistant output")
	}
	if !gate.meaningful {
		return blankTurnError(normalizeAntigravityStopReason(usage.stopReason))
	}
	finishAntigravity(gated, usage)
	return nil
}

// CallExternalAntigravity forwards a KiroPayload to Cloud Code Assist and
// replays the response through callback. It mirrors CallKiroAPI's contract: nil
// on success, error on failure, with 401/403 surfaced so the caller can refresh
// or disable the account.
func CallExternalAntigravity(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if account == nil {
		return fmt.Errorf("antigravity call: account is nil")
	}
	if err := ensureAntigravityToken(account); err != nil {
		return err
	}
	projectID, err := ensureAntigravityProject(account)
	if err != nil {
		return err
	}

	body, err := kiroPayloadToAntigravityRequest(payload, account, projectID)
	if err != nil {
		return fmt.Errorf("antigravity call build request: %w", err)
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("antigravity call marshal: %w", err)
	}

	endpoint := antigravityEndpoint(account) + antigravityStreamAction
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("antigravity call new request: %w", err)
	}
	setAntigravityHeaders(req, account.AccessToken)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return fmt.Errorf("antigravity call %s: %w", account.Email, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		text := string(errBody)
		switch classifyAntigravityFailure(resp.StatusCode, text) {
		case antigravityFailureBanned:
			// Google disables the service on an account for third-party client
			// use. That is terminal, so stop selecting the account rather than
			// retrying it on every request.
			markAntigravityBanned(account, text)
		case antigravityFailureAuth:
			logger.Warnf("[Antigravity] auth failure for %s: HTTP %d", account.Email, resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, account.Email, truncateErrBody(errBody))
	}

	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return parseAntigravitySSE(resp.Body, payload, callback)
	}
	// Distinguish a JSON fallback from SSE without losing buffered bytes: the
	// same reader must be handed to whichever parser is chosen.
	br := bufio.NewReader(resp.Body)
	first, peekErr := br.Peek(1)
	if peekErr != nil && peekErr != io.EOF {
		return fmt.Errorf("antigravity response peek: %w", peekErr)
	}
	if len(first) > 0 && first[0] == '{' {
		return parseAntigravityJSON(br, payload, callback)
	}
	return parseAntigravitySSE(&antigravityBufferedBody{Reader: br, Closer: resp.Body}, payload, callback)
}

// antigravityBufferedBody keeps the original Close method reachable after the
// body has been wrapped for peeking, so the SSE idle watchdog can still abort
// the underlying connection.
type antigravityBufferedBody struct {
	io.Reader
	io.Closer
}

// maxAntigravityResponseBytes bounds a buffered non-stream response. Cloud Code
// Assist replies are text plus usage metadata, so 32 MiB is far above any real
// answer while still refusing a runaway body.
const maxAntigravityResponseBytes = 32 << 20

// ensureAntigravityToken refreshes the access token when it is missing or about
// to expire. It runs before every call because a Google access token lives one
// hour and a long agent turn can outlast one issued mid-session.
func ensureAntigravityToken(account *config.Account) error {
	if strings.TrimSpace(account.AccessToken) == "" && strings.TrimSpace(account.RefreshToken) == "" {
		return fmt.Errorf("antigravity call: account %s has no credentials (re-login required)", account.Email)
	}
	if !antigravityTokenNeedsRefresh(account, time.Now().Unix()) {
		return nil
	}
	if err := refreshAntigravityAccountToken(account); err != nil {
		// invalid_grant means the grant itself is gone — revoked, or the account
		// was disabled. Retrying cannot recover it, so record the terminal state.
		if strings.Contains(strings.ToLower(err.Error()), "invalid_grant") {
			markAntigravityBanned(account, "Google refused the refresh token (invalid_grant); re-login required")
		}
		return fmt.Errorf("antigravity token refresh for %s: %w", account.Email, err)
	}
	return nil
}

// antigravityErrorMessage extracts a google.rpc.Status message from a response
// body, returning "" when the payload is not an error. Cloud Code Assist can
// answer HTTP 200 with an error envelope inside the stream, so the body has to
// be inspected rather than trusting the status line alone.
func antigravityErrorMessage(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	// A streamed error may arrive as a single-element array of envelopes.
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var envelopes []struct {
			Error *struct {
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		if json.Unmarshal(trimmed, &envelopes) == nil {
			for _, envelope := range envelopes {
				if envelope.Error != nil && strings.TrimSpace(envelope.Error.Message) != "" {
					return envelope.Error.Message
				}
			}
		}
		return ""
	}
	var envelope struct {
		Error *struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(trimmed, &envelope) != nil || envelope.Error == nil {
		return ""
	}
	if message := strings.TrimSpace(envelope.Error.Message); message != "" {
		return message
	}
	return strings.TrimSpace(envelope.Error.Status)
}

// ==================== Model catalog ====================

const antigravityModelsAction = "/v1internal:fetchAvailableModels"

// antigravityFallbackModels is the catalog used when fetchAvailableModels is
// unavailable. It is a floor, not a claim of completeness: the live catalog is
// preferred whenever the upstream answers, because model availability is tied to
// the account's tier and changes without any local signal.
func antigravityFallbackModels() []ModelInfo {
	specs := []struct {
		id, name, desc      string
		maxInput, maxOutput int
	}{
		{"gemini-3-pro-high", "Gemini 3 Pro (High)", "Gemini 3 Pro, high reasoning effort", 1048576, 65536},
		{"gemini-3-pro-low", "Gemini 3 Pro (Low)", "Gemini 3 Pro, low reasoning effort", 1048576, 65536},
		{"claude-sonnet-4-6", "Claude Sonnet 4.6", "Claude Sonnet via the Antigravity gateway", 200000, 64000},
		{"claude-opus-4-6-thinking", "Claude Opus 4.6 Thinking", "Claude Opus with extended thinking", 200000, 64000},
		{"gpt-oss-120b-medium", "GPT-OSS 120B (Medium)", "Open-weight GPT-OSS 120B", 131072, 32768},
	}
	out := make([]ModelInfo, 0, len(specs))
	for _, spec := range specs {
		model := ModelInfo{
			ModelId:        spec.id,
			ModelName:      spec.name,
			Description:    spec.desc,
			InputTypes:     []string{"text", "image"},
			RateMultiplier: 1.0,
			Provider:       "antigravity",
		}
		model.TokenLimits = &struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		}{MaxInputTokens: spec.maxInput, MaxOutputTokens: spec.maxOutput}
		out = append(out, model)
	}
	return out
}

// fetchAntigravityModels asks the gateway for the account's own catalog and
// falls back to the static list when the endpoint is unavailable. It never
// returns an empty catalog, so routing always has candidates.
func fetchAntigravityModels(account *config.Account) ([]ModelInfo, error) {
	if err := ensureAntigravityToken(account); err != nil {
		return nil, err
	}
	projectID, err := ensureAntigravityProject(account)
	if err != nil {
		// A tier/project problem must not leave the account with no catalog.
		logger.Warnf("[Antigravity] catalog for %s falling back to static list: %v", account.Email, err)
		return antigravityFallbackModels(), nil
	}

	body, _ := json.Marshal(map[string]interface{}{
		"project":  projectID,
		"metadata": antigravityMetadata(projectID),
	})
	raw, err := antigravityPostJSON(account, antigravityModelsAction, body)
	if err != nil {
		logger.Warnf("[Antigravity] fetchAvailableModels for %s failed, using static list: %v", account.Email, err)
		return antigravityFallbackModels(), nil
	}

	models := parseAntigravityModels(raw)
	if len(models) == 0 {
		return antigravityFallbackModels(), nil
	}
	return models, nil
}

// parseAntigravityModels reads the fetchAvailableModels payload. The field names
// vary between gateway versions, so each known spelling is accepted rather than
// failing the whole catalog on an unexpected key.
func parseAntigravityModels(raw []byte) []ModelInfo {
	var payload struct {
		Models []struct {
			Name            string `json:"name"`
			ModelID         string `json:"modelId"`
			ID              string `json:"id"`
			DisplayName     string `json:"displayName"`
			Description     string `json:"description"`
			InputTokenLimit int    `json:"inputTokenLimit"`
			OutputTokenLimit int   `json:"outputTokenLimit"`
		} `json:"models"`
		AvailableModels []struct {
			Name            string `json:"name"`
			ModelID         string `json:"modelId"`
			ID              string `json:"id"`
			DisplayName     string `json:"displayName"`
			Description     string `json:"description"`
			InputTokenLimit int    `json:"inputTokenLimit"`
			OutputTokenLimit int   `json:"outputTokenLimit"`
		} `json:"availableModels"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	entries := payload.Models
	if len(entries) == 0 {
		entries = payload.AvailableModels
	}

	out := make([]ModelInfo, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		id := firstNonEmpty(entry.ModelID, entry.ID, entry.Name)
		id = strings.TrimPrefix(strings.TrimSpace(id), "models/")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(entry.DisplayName)
		if name == "" {
			name = id
		}
		model := ModelInfo{
			ModelId:        id,
			ModelName:      name,
			Description:    strings.TrimSpace(entry.Description),
			InputTypes:     []string{"text", "image"},
			RateMultiplier: 1.0,
			Provider:       "antigravity",
		}
		if entry.InputTokenLimit > 0 || entry.OutputTokenLimit > 0 {
			model.TokenLimits = &struct {
				MaxInputTokens  int `json:"maxInputTokens"`
				MaxOutputTokens int `json:"maxOutputTokens"`
			}{MaxInputTokens: entry.InputTokenLimit, MaxOutputTokens: entry.OutputTokenLimit}
		}
		out = append(out, model)
	}
	return out
}
