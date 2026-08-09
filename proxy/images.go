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
	"strings"
	"time"
)

const endpointImage = "image"

const defaultImageModel = "gpt-5.6-luna"

// Codex image generation uses a normal Responses model as the host for the
// image_generation tool. The requested Images API model is therefore not
// sent as the host model; the actual image model is configured separately.
const defaultCodexImageHostModel = "gpt-5.5"
const defaultCodexImageToolModel = "gpt-image-2"

const maxImageResponseBytes = 64 << 20

type imageGenerationRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model,omitempty"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	Quality        string `json:"quality,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	User           string `json:"user,omitempty"`
}

type imageGenerationResponse struct {
	Created int64                 `json:"created"`
	Data    []imageGenerationData `json:"data"`
}

type imageGenerationData struct {
	URL     string `json:"url,omitempty"`
	B64JSON string `json:"b64_json,omitempty"`
}

type unsupportedCapabilityError struct {
	Capability string
	Account    string
	Reason     string
}

func (e *unsupportedCapabilityError) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
	return fmt.Sprintf("account %s does not support %s", e.Account, e.Capability)
}

func imageModelFor(account *config.Account, requested string) string {
	model := strings.TrimSpace(requested)
	if model != "" {
		return model
	}
	if account != nil {
		if model = strings.TrimSpace(account.ImageModel); model != "" {
			return model
		}
		if isCodexAccount(account) {
			if model = strings.TrimSpace(account.CodexImageModel); model != "" {
				return model
			}
		}
	}
	return defaultImageModel
}

func codexImageToolModel(account *config.Account, requested string) string {
	if account != nil {
		if model := strings.TrimSpace(account.CodexImageModel); model != "" {
			return model
		}
	}
	// Permit an explicit image model from clients that use the standard
	// gpt-image-* naming, while keeping OmniProxy's legacy gpt-5.6 aliases
	// from being sent as the tool model.
	if model := strings.TrimSpace(requested); strings.HasPrefix(model, "gpt-image-") {
		return model
	}
	return defaultCodexImageToolModel
}

func codexImageQuality(quality string) string {
	quality = strings.ToLower(strings.TrimSpace(quality))
	switch quality {
	case "low", "medium", "high":
		return quality
	default:
		return "medium"
	}
}

func (h *Handler) handleImageGeneration(w http.ResponseWriter, r *http.Request) {
	var in imageGenerationRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON")
		return
	}
	if strings.TrimSpace(in.Prompt) == "" {
		h.sendOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}
	if in.N <= 0 {
		in.N = 1
	}

	excluded := make(map[string]bool)
	var lastErr error
	var lastAccountID string
	for {
		account := h.pool.GetNextCodex(excluded)
		if account == nil {
			break
		}
		lastAccountID = account.ID
		response, err := callCodexImage(r, account, in)
		if err == nil {
			h.pool.RecordSuccess(account.ID, "image")
			h.recordUsage(apiKeyIDFromContext(r.Context()), account.ID, in.Model, endpointImage, 0, 0, 0, 0, 0, 0)
			writeJSON(w, http.StatusOK, response)
			return
		}
		lastErr = err
		excluded[account.ID] = true
		isQuota := false
		if httpErr, ok := err.(*serviceHTTPError); ok {
			isQuota = httpErr.Status == http.StatusPaymentRequired || httpErr.Status == http.StatusTooManyRequests
		}
		h.pool.RecordError(account.ID, isQuota, "image")
	}

	if lastErr == nil {
		h.sendOpenAIError(w, http.StatusServiceUnavailable, "server_error", "No available image accounts")
		return
	}
	h.recordError(apiKeyIDFromContext(r.Context()), lastAccountID, in.Model, endpointImage, lastErr.Error())
	h.sendOpenAIError(w, serviceErrorStatus(lastErr), "server_error", lastErr.Error())
}

func callOpenRouterImage(parent *http.Request, account *config.Account, in imageGenerationRequest) (imageGenerationResponse, error) {
	model := imageModelFor(account, in.Model)
	body := map[string]interface{}{
		"model":      model,
		"messages":   []map[string]interface{}{{"role": "user", "content": in.Prompt}},
		"modalities": []string{"text", "image"},
	}
	// These fields are understood by OpenRouter's image-capable chat models.
	// response_format is deliberately not forwarded: it belongs to the OpenAI
	// Images API and is not a valid Chat Completions image control upstream.
	if in.N > 0 {
		body["n"] = in.N
	}
	if in.Size != "" {
		body["size"] = in.Size
	}
	if in.Quality != "" {
		body["quality"] = in.Quality
	}
	if in.User != "" {
		body["user"] = in.User
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return imageGenerationResponse{}, err
	}
	endpoint := openRouterImageEndpoint(account)
	req, err := http.NewRequestWithContext(parent.Context(), http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return imageGenerationResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	credential := strings.TrimSpace(account.AccessToken)
	if credential == "" {
		return imageGenerationResponse{}, fmt.Errorf("provider %s has no credential", account.Provider)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, err := GetImageClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return imageGenerationResponse{}, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxImageResponseBytes))
	if readErr != nil {
		return imageGenerationResponse{}, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return imageGenerationResponse{}, &serviceHTTPError{Status: resp.StatusCode, Body: string(raw)}
	}
	result, err := normalizeOpenRouterImage(raw)
	if err != nil {
		return imageGenerationResponse{}, err
	}
	result.Created = time.Now().Unix()
	return result, nil
}

func openRouterImageEndpoint(account *config.Account) string {
	base := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if base == "" {
		return "https://openrouter.ai/api/v1/chat/completions"
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

func normalizeOpenRouterImage(raw []byte) (imageGenerationResponse, error) {
	var upstream struct {
		Choices []struct {
			Message struct {
				Images []struct {
					Type     string `json:"type"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
					B64JSON string `json:"b64_json"`
				} `json:"images"`
				Content interface{} `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &upstream); err != nil {
		return imageGenerationResponse{}, err
	}
	var data []imageGenerationData
	for _, choice := range upstream.Choices {
		for _, image := range choice.Message.Images {
			if image.ImageURL.URL != "" {
				data = append(data, imageGenerationData{URL: image.ImageURL.URL})
			} else if image.B64JSON != "" {
				data = append(data, imageGenerationData{B64JSON: image.B64JSON})
			}
		}
		if parts, ok := choice.Message.Content.([]interface{}); ok {
			for _, part := range parts {
				if item, ok := part.(map[string]interface{}); ok {
					if imageURL, ok := item["image_url"].(map[string]interface{}); ok {
						if value, ok := imageURL["url"].(string); ok && value != "" {
							data = append(data, imageGenerationData{URL: value})
						}
					}
					if value, ok := item["b64_json"].(string); ok && value != "" {
						data = append(data, imageGenerationData{B64JSON: value})
					}
				}
			}
		}
	}
	if len(data) == 0 {
		return imageGenerationResponse{}, fmt.Errorf("image provider returned no image data")
	}
	return imageGenerationResponse{Data: data}, nil
}

// callCodexImage uses the Codex Responses API image_generation tool. Codex
// image generation is tied to the ChatGPT subscription account, so this path
// deliberately never selects a 9router/OpenRouter service account.
func callCodexImage(parent *http.Request, account *config.Account, in imageGenerationRequest) (imageGenerationResponse, error) {
	if account == nil {
		return imageGenerationResponse{}, fmt.Errorf("codex image: account is nil")
	}
	accessToken := strings.TrimSpace(account.AccessToken)
	if accessToken == "" {
		return imageGenerationResponse{}, fmt.Errorf("codex image: account %s has no access token", account.Email)
	}
	if codexTokenNeedsRefresh(account, time.Now().Unix()) {
		if err := refreshCodexAccountToken(account); err != nil {
			return imageGenerationResponse{}, fmt.Errorf("codex image token refresh: %w", err)
		}
		accessToken = strings.TrimSpace(account.AccessToken)
	}
	accountID := strings.TrimSpace(account.ChatGPTAccountID)
	if accountID == "" {
		accountID = auth.ExtractCodexAccountIDPublic(accessToken)
		if accountID != "" {
			account.ChatGPTAccountID = accountID
			_ = config.UpdateAccountChatGPTAccountID(account.ID, accountID)
		}
	}
	if accountID == "" {
		return imageGenerationResponse{}, fmt.Errorf("codex image: account %s has no chatgpt_account_id", account.Email)
	}

	toolModel := codexImageToolModel(account, in.Model)
	size := strings.TrimSpace(in.Size)
	if size == "" {
		size = "1024x1024"
	}
	body := map[string]interface{}{
		"model":        defaultCodexImageHostModel,
		"store":        false,
		"instructions": "You must fulfill the request by using the image_generation tool.",
		"input": []map[string]interface{}{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]interface{}{{"type": "input_text", "text": in.Prompt}},
		}},
		"tools": []map[string]interface{}{{
			"type":           "image_generation",
			"model":          toolModel,
			"size":           size,
			"quality":        codexImageQuality(in.Quality),
			"output_format":  "png",
			"background":     "opaque",
			"partial_images": 1,
		}},
		"tool_choice": map[string]interface{}{
			"type": "allowed_tools", "mode": "required",
			"tools": []map[string]string{{"type": "image_generation"}},
		},
		// The Codex Responses endpoint requires SSE for image_generation.
		"stream": true,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return imageGenerationResponse{}, fmt.Errorf("codex image marshal: %w", err)
	}
	endpoint := codexBaseURL(account) + "/backend-api/codex/responses"
	req, err := http.NewRequestWithContext(parent.Context(), http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return imageGenerationResponse{}, fmt.Errorf("codex image request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("chatgpt-account-id", accountID)
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("User-Agent", "codex_cli_rs/0.0.0 omniproxy/1.0")
	resp, err := GetImageClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return imageGenerationResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxImageResponseBytes))
		if readErr != nil {
			return imageGenerationResponse{}, readErr
		}
		return imageGenerationResponse{}, &serviceHTTPError{Status: resp.StatusCode, Body: string(raw), Headers: serviceResponseHeaders(resp.Header)}
	}
	result, err := readCodexImageResponse(resp.Body, resp.Header.Get("Content-Type"))
	if err != nil {
		return imageGenerationResponse{}, err
	}
	result.Created = time.Now().Unix()
	return result, nil
}

// readCodexImageResponse consumes only the part of an SSE stream needed for
// the Images API response. Some gateways leave the HTTP connection open after
// response.completed; waiting for EOF here turns a completed image into a
// client timeout, so completion is an explicit stop condition.
func readCodexImageResponse(body io.Reader, contentType string) (imageGenerationResponse, error) {
	var watchdog *sseIdleWatchdog
	if closer, ok := body.(io.ReadCloser); ok {
		watchdog = newSSEIdleWatchdog(closer)
		if watchdog != nil {
			watchdog.Start()
			defer watchdog.Stop()
		}
	}

	reader := bufio.NewReader(body)
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream")
	if !isSSE {
		// Some compatible gateways omit Content-Type on a streamed Responses
		// response. Peek without consuming bytes so the SSE parser still sees
		// the complete first event.
		peek, peekErr := reader.Peek(1)
		isSSE = len(peek) == 1 && (peek[0] == 'd' || peek[0] == 'e' || peek[0] == ':')
		if peekErr != nil && len(peek) == 0 && !isSSE {
			if peekErr != io.EOF {
				return imageGenerationResponse{}, peekErr
			}
		}
	}
	if !isSSE {
		raw, err := io.ReadAll(io.LimitReader(reader, maxImageResponseBytes))
		if err != nil {
			return imageGenerationResponse{}, err
		}
		return normalizeCodexImage(raw, contentType)
	}

	var data []imageGenerationData
	var bytesRead int64
	completed := false
	for {
		line, err := reader.ReadString('\n')
		bytesRead += int64(len(line))
		if bytesRead > maxImageResponseBytes {
			return imageGenerationResponse{}, fmt.Errorf("codex image response exceeded %d bytes", maxImageResponseBytes)
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			if watchdog != nil {
				watchdog.DataReceived()
			}
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				break
			}
			if payload != "" {
				var value interface{}
				if decodeErr := json.Unmarshal([]byte(payload), &value); decodeErr != nil {
					return imageGenerationResponse{}, fmt.Errorf("codex image SSE parse: %w", decodeErr)
				}
				collectCodexImageData(value, &data)
				if event, ok := value.(map[string]interface{}); ok && event["type"] == "response.completed" {
					completed = true
					break
				}
			}
		}
		if err != nil {
			if watchdog != nil && watchdog.TimedOut() {
				return imageGenerationResponse{}, ErrStreamIdleTimeout
			}
			if err != io.EOF {
				return imageGenerationResponse{}, fmt.Errorf("codex image SSE read: %w", err)
			}
			break
		}
	}
	if len(data) == 0 {
		return imageGenerationResponse{}, fmt.Errorf("codex image response contained no image data")
	}
	if !completed {
		return imageGenerationResponse{}, fmt.Errorf("codex image SSE stream ended before response.completed")
	}
	return imageGenerationResponse{Data: data}, nil
}

func callOpenAIImages(parent *http.Request, account *config.Account, in imageGenerationRequest) (imageGenerationResponse, error) {
	if account == nil {
		return imageGenerationResponse{}, fmt.Errorf("image: account is nil")
	}
	base := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if base == "" {
		return imageGenerationResponse{}, fmt.Errorf("external image: account %s has no baseUrl", account.Email)
	}
	base = openAICompatibleEndpoint(base, "/v1/images/generations")
	model := imageModelFor(account, in.Model)
	if model == "" {
		model = defaultImageModel
	}
	body := map[string]interface{}{"prompt": in.Prompt}
	body["model"] = model
	if in.N > 0 {
		body["n"] = in.N
	}
	if in.Size != "" {
		body["size"] = in.Size
	}
	if in.Quality != "" {
		body["quality"] = in.Quality
	}
	if in.ResponseFormat != "" {
		body["response_format"] = in.ResponseFormat
	}
	if in.User != "" {
		body["user"] = in.User
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return imageGenerationResponse{}, err
	}
	req, err := http.NewRequestWithContext(parent.Context(), http.MethodPost, base, bytes.NewReader(payload))
	if err != nil {
		return imageGenerationResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token := strings.TrimSpace(account.AccessToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		return imageGenerationResponse{}, fmt.Errorf("external image: account %s has no api key", account.Email)
	}
	resp, err := GetImageClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return imageGenerationResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxImageResponseBytes))
	if err != nil {
		return imageGenerationResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return imageGenerationResponse{}, &serviceHTTPError{Status: resp.StatusCode, Body: string(raw), Headers: serviceResponseHeaders(resp.Header)}
	}
	result, err := normalizeOpenAIImages(raw)
	if err != nil {
		return imageGenerationResponse{}, err
	}
	result.Created = time.Now().Unix()
	return result, nil
}

func normalizeOpenAIImages(raw []byte) (imageGenerationResponse, error) {
	var result imageGenerationResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return imageGenerationResponse{}, fmt.Errorf("image response parse: %w", err)
	}
	if len(result.Data) == 0 {
		return imageGenerationResponse{}, fmt.Errorf("image provider returned no image data")
	}
	return result, nil
}

func callImageGeneration(parent *http.Request, account *config.Account, in imageGenerationRequest) (imageGenerationResponse, error) {
	if account == nil {
		return imageGenerationResponse{}, fmt.Errorf("image: account is nil")
	}
	if isCodexAccount(account) {
		return callCodexImage(parent, account, in)
	}
	if isExternalAccount(account) {
		return callOpenAIImages(parent, account, in)
	}
	if accountHasCapability(account, "image") {
		return callOpenRouterImage(parent, account, in)
	}
	return imageGenerationResponse{}, &unsupportedCapabilityError{
		Capability: "image generation",
		Account:    account.Email,
		Reason:     fmt.Sprintf("account %s/upstream does not expose image generation", firstNonEmpty(account.Email, account.ID)),
	}
}

// normalizeCodexImage accepts both the normal JSON Responses object and SSE
// data emitted by compatible Codex gateways. Image artifacts can arrive as
// raw base64, data URLs, or short-lived HTTPS URLs depending on the upstream.
func normalizeCodexImage(raw []byte, contentType string) (imageGenerationResponse, error) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") || !json.Valid(raw) {
		var data []imageGenerationData
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if line == "" || line == "[DONE]" {
				continue
			}
			var value interface{}
			if json.Unmarshal([]byte(line), &value) == nil {
				collectCodexImageData(value, &data)
			}
		}
		if len(data) == 0 {
			return imageGenerationResponse{}, fmt.Errorf("codex image response contained no image data")
		}
		return imageGenerationResponse{Data: data}, nil
	}
	var value interface{}
	if err := json.Unmarshal(raw, &value); err != nil {
		return imageGenerationResponse{}, fmt.Errorf("codex image response parse: %w", err)
	}
	var data []imageGenerationData
	collectCodexImageData(value, &data)
	if len(data) == 0 {
		return imageGenerationResponse{}, fmt.Errorf("codex image response contained no image data")
	}
	return imageGenerationResponse{Data: data}, nil
}

func collectCodexImageData(value interface{}, data *[]imageGenerationData) {
	switch item := value.(type) {
	case []interface{}:
		for _, child := range item {
			collectCodexImageData(child, data)
		}
	case map[string]interface{}:
		kind, _ := item["type"].(string)
		if kind == "image_generation_call" || kind == "image_generation_result" {
			for _, key := range []string{"result", "image_url", "url", "artifact_url", "b64_json"} {
				if result, ok := item[key]; ok {
					collectCodexImageArtifact(result, data)
				}
			}
		}
		for _, child := range item {
			collectCodexImageData(child, data)
		}
	}
}

func collectCodexImageArtifact(value interface{}, data *[]imageGenerationData) {
	switch artifact := value.(type) {
	case string:
		artifact = strings.TrimSpace(artifact)
		if artifact == "" {
			return
		}
		if strings.HasPrefix(strings.ToLower(artifact), "data:image/") {
			if comma := strings.IndexByte(artifact, ','); comma >= 0 {
				appendCodexImageData(data, imageGenerationData{B64JSON: artifact[comma+1:]})
			}
			return
		}
		if strings.HasPrefix(strings.ToLower(artifact), "http://") || strings.HasPrefix(strings.ToLower(artifact), "https://") {
			appendCodexImageData(data, imageGenerationData{URL: artifact})
			return
		}
		appendCodexImageData(data, imageGenerationData{B64JSON: artifact})
	case []interface{}:
		for _, child := range artifact {
			collectCodexImageArtifact(child, data)
		}
	case map[string]interface{}:
		for _, key := range []string{"url", "image_url", "artifact_url", "b64_json", "result"} {
			if child, ok := artifact[key]; ok {
				collectCodexImageArtifact(child, data)
			}
		}
	}
}

func appendCodexImageData(data *[]imageGenerationData, candidate imageGenerationData) {
	if candidate.URL == "" && candidate.B64JSON == "" {
		return
	}
	for _, existing := range *data {
		if existing == candidate {
			return
		}
	}
	*data = append(*data, candidate)
}
