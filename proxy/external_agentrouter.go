// Package proxy — AgentRouter AI Gateway provider adapter.
//
// Accounts with AuthMethod == "agentrouter" forward chat-completion requests
// to an upstream AgentRouter endpoint using the account's AccessToken.
//
// AgentRouter's edge validates the OpenAI Node SDK/Stainless request
// fingerprint. The documented Anthropic route exists, but the compatible chat
// route below is the one verified against the live service for this adapter.
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"omniproxy/config"
	"strings"
)

// agentRouterAuthMethod is the AuthMethod value marking an AgentRouter account.
const agentRouterAuthMethod = "agentrouter"

// AgentRouter's current public API is available at the backup domain.
const (
	defaultAgentRouterBaseURL          = "https://ps.air-outer.com"
	agentRouterUserAgent               = "QwenCode/0.2.0 (linux; x64)"
	agentRouterStainlessPackageVersion = "6.34.0"
	agentRouterStainlessRuntimeVersion = "node/26.4.0"
	agentRouterTestModel               = "claude-opus-4-8"
	agentRouterTestPrompt              = "Say OK"
	agentRouterTestMaxTokens           = 20
)

// isAgentRouterAccount reports whether the account routes to AgentRouter.
func isAgentRouterAccount(account *config.Account) bool {
	if account == nil {
		return false
	}
	m := strings.ToLower(strings.TrimSpace(account.AuthMethod))
	return m == agentRouterAuthMethod || m == "external_agentrouter"
}

// CallExternalAgentRouter forwards a KiroPayload through AgentRouter's verified
// OpenAI-compatible chat-completions route.
func CallExternalAgentRouter(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if account == nil {
		return fmt.Errorf("agentrouter call: account is nil")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultAgentRouterBaseURL
	}
	apiKey := strings.TrimSpace(account.AccessToken)
	if apiKey == "" {
		return fmt.Errorf("agentrouter account %s has no apiKey/accessToken", account.Email)
	}

	return callAgentRouterOpenAI(account, agentRouterRootURL(baseURL), apiKey, payload, callback)
}

func callAgentRouterOpenAI(account *config.Account, baseURL, apiKey string, payload *KiroPayload, callback *KiroStreamCallback) error {
	endpoint := openAICompatibleEndpoint(baseURL, "/v1/chat/completions")

	body, err := kiroPayloadToOpenAIRequest(payload, account)
	if err != nil {
		return fmt.Errorf("agentrouter build request: %w", err)
	}
	body["stream"] = true
	body["stream_options"] = map[string]bool{"include_usage": true}

	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("agentrouter marshal: %w", err)
	}

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("agentrouter new request: %w", err)
	}

	setAgentRouterHeaders(req, apiKey, "text/event-stream")

	client := GetClientForProxy(ResolveAccountProxyURL(account))
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("agentrouter call %s: %w", account.Email, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		err := fmt.Errorf("HTTP %d from AgentRouter (%s): %s", resp.StatusCode, account.Email, truncateErrBody(errBody))
		if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 402 {
			return err
		}
		return err
	}

	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return parseExternalOpenAISSE(resp.Body, callback)
	}
	return parseExternalOpenAIJSON(resp.Body, callback)
}

// CallAgentRouterTest performs the fixed non-streaming OpenAI-compatible probe
// whose wire format has been verified against AgentRouter's live edge.
func CallAgentRouterTest(account *config.Account) (string, error) {
	if account == nil {
		return "", fmt.Errorf("agentrouter test: account is nil")
	}
	apiKey := strings.TrimSpace(account.AccessToken)
	if apiKey == "" {
		return "", fmt.Errorf("agentrouter account %s has no apiKey/accessToken", account.Email)
	}

	baseURL := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultAgentRouterBaseURL
	}
	endpoint := openAICompatibleEndpoint(agentRouterRootURL(baseURL), "/v1/chat/completions")

	reqBody, err := json.Marshal(struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}{
		Model:     agentRouterTestModel,
		MaxTokens: agentRouterTestMaxTokens,
		Stream:    false,
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{{Role: "user", Content: agentRouterTestPrompt}},
	})
	if err != nil {
		return "", fmt.Errorf("agentrouter test marshal: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("agentrouter test new request: %w", err)
	}
	setAgentRouterHeaders(req, apiKey, "application/json")

	client := GetClientForProxy(ResolveAccountProxyURL(account))
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("agentrouter test call %s: %w", account.Email, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d from AgentRouter (%s): %s", resp.StatusCode, account.Email, truncateErrBody(errBody))
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("agentrouter test read response: %w", err)
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		var encoded string
		if json.Unmarshal(responseBody, &encoded) != nil || json.Unmarshal([]byte(encoded), &response) != nil {
			return "", fmt.Errorf("agentrouter test decode response: %w", err)
		}
	}
	if len(response.Choices) == 0 {
		return "", fmt.Errorf("agentrouter test response has no choices")
	}
	return response.Choices[0].Message.Content, nil
}

func agentRouterRootURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, suffix := range []string{"/v1/chat/completions", "/chat/completions", "/v1/messages", "/messages", "/v1"} {
		baseURL = strings.TrimSuffix(baseURL, suffix)
	}
	return baseURL
}

func setAgentRouterHeaders(req *http.Request, apiKey, accept string) {
	req.Header.Set("Accept", accept)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", agentRouterUserAgent)
	req.Header.Set("x-stainless-arch", "arm64")
	req.Header.Set("x-stainless-lang", "js")
	req.Header.Set("x-stainless-os", "MacOS")
	req.Header.Set("x-stainless-package-version", agentRouterStainlessPackageVersion)
	req.Header.Set("x-stainless-retry-count", "0")
	req.Header.Set("x-stainless-runtime", "node")
	req.Header.Set("x-stainless-runtime-version", agentRouterStainlessRuntimeVersion)
}

func fetchAgentRouterModels(account *config.Account) ([]ModelInfo, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultAgentRouterBaseURL
	}
	apiKey := strings.TrimSpace(account.AccessToken)

	endpoint := openAICompatibleEndpoint(agentRouterRootURL(baseURL), "/v1/models")
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	setAgentRouterHeaders(req, apiKey, "application/json")

	client := GetClientForProxy(ResolveAccountProxyURL(account))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch models HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	var models []ModelInfo
	for _, m := range parsed.Data {
		models = append(models, ModelInfo{
			ModelId:   m.ID,
			ModelName: m.ID,
		})
	}
	return models, nil
}
