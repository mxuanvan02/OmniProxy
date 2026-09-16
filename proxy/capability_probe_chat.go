// Package proxy — dialect-aware chat probe for external accounts.
//
// A Responses or Messages gateway answers a chat-completions POST with 404/400
// even when its chat capability is healthy, and that failure reads on the
// account matrix as "no chat, no vision". The probe therefore sends the shape
// and path of the dialect the account actually speaks.
package proxy

import (
	"encoding/json"
	"net/http"

	"omniproxy/config"
)

// externalDialectProbePath returns the endpoint a chat probe should hit for one
// account, honouring its per-dialect path override.
func externalDialectProbePath(account *config.Account, dialect string) string {
	switch dialect {
	case "responses":
		return externalResponsesPath(account)
	case "anthropic":
		return externalAnthropicPath(account)
	default:
		return externalChatPath(account)
	}
}

// probeChatRequestBody builds the minimal valid chat-shaped request for one
// dialect. max_output_tokens has a floor of 16 on the Responses API, so the
// probe uses that there; the other two dialects accept 1.
func probeChatRequestBody(dialect, model string) []byte {
	var payload map[string]interface{}
	switch dialect {
	case "responses":
		payload = map[string]interface{}{
			"model":             model,
			"input":             "ping",
			"max_output_tokens": 16,
			"stream":            false,
			"store":             false,
		}
	default:
		payload = map[string]interface{}{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return body
}

// probeChatEndpoint returns the base URL a chat probe should POST to for one
// account, honouring its per-dialect path override.
func probeChatEndpoint(account *config.Account, dialect string) string {
	return openAICompatibleEndpoint(account.BaseURL, externalDialectProbePath(account, dialect))
}

// applyChatProbeAuth sets the credential headers the dialect's own adapter sets.
// The Messages API authenticates with x-api-key and rejects a request with no
// anthropic-version, so it cannot share the Bearer-only shape the two OpenAI
// dialects use.
func applyChatProbeAuth(req *http.Request, account *config.Account, credential, dialect string) {
	if dialect == "anthropic" {
		setExternalAnthropicHeaders(req, account, credential, "*/*")
		return
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
}
