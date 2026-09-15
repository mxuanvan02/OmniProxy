// Package proxy — external OpenAI-compatible provider adapter, Responses dialect.
//
// Accounts with AuthMethod == "external_openai" normally forward chat-completion
// requests to {BaseURL}/v1/chat/completions. An account may instead select the
// Responses API dialect (config.Account.ExternalAPIDialect == "responses"), which
// is required by gateways whose backend only speaks Responses — a reseller of
// ChatGPT Codex subscription capacity, for example. Such a gateway has to
// translate chat completions into Responses internally, and that translation
// loses the reasoning items the model would otherwise carry between turns.
//
// The wire translation is shared with the Codex adapter and lives in
// responses_upstream.go; this file owns only the account-level selection and the
// transport, which is the same one the chat dialect uses.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"omniproxy/config"
)

// defaultExternalResponsesPath is the OpenAI Responses path used unless the
// account overrides it, mirroring defaultExternalChatPath for chat.
const defaultExternalResponsesPath = "/v1/responses"

// externalAPIDialect reports which outbound OpenAI dialect an account uses:
// "responses" when explicitly selected, otherwise "chat". Unrecognised values
// fall back to chat rather than erroring, so a hand-edited config can never take
// an account offline over a typo.
func externalAPIDialect(account *config.Account) string {
	if account == nil {
		return "chat"
	}
	if strings.EqualFold(strings.TrimSpace(account.ExternalAPIDialect), "responses") {
		return "responses"
	}
	return "chat"
}

// externalResponsesPath returns the upstream Responses path for an account: the
// account-level override when set, otherwise the OpenAI default.
func externalResponsesPath(account *config.Account) string {
	if account == nil {
		return defaultExternalResponsesPath
	}
	if p := strings.TrimSpace(account.ResponsesPath); p != "" {
		return "/" + strings.TrimLeft(p, "/")
	}
	return defaultExternalResponsesPath
}

// externalResponsesOptions returns the dialect options for a generic
// OpenAI-compatible gateway that speaks the Responses API.
//
// Unlike the ChatGPT Codex backend, such a gateway normally accepts temperature
// and top_p, so they are forwarded. DefaultModel is deliberately left empty: a
// gateway has no sensible model to guess, and substituting a wrong one is worse
// than the error the builder returns. ToolDescription is nil for the same
// reason — the Codex lifecycle guidance describes Codex CLI tooling and would
// mislead a different client.
func externalResponsesOptions() responsesDialectOptions {
	return responsesDialectOptions{
		ForwardSamplingParams: true,
	}
}

// CallExternalOpenAIResponses forwards a KiroPayload to an external
// OpenAI-compatible provider using the Responses API dialect.
//
// A gateway that only speaks Responses has to translate chat completions into
// Responses internally, and that translation loses the reasoning items the model
// would otherwise carry between turns. Sending the Responses shape directly
// avoids the lossy hop.
func CallExternalOpenAIResponses(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if account == nil {
		return fmt.Errorf("external responses call: account is nil")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if baseURL == "" {
		return fmt.Errorf("external account %s has no baseUrl", account.Email)
	}
	apiKey := strings.TrimSpace(account.AccessToken)
	if apiKey == "" {
		return fmt.Errorf("external account %s has no apiKey", account.Email)
	}

	body, err := kiroPayloadToResponsesRequest(payload, account, externalResponsesOptions())
	if err != nil {
		return fmt.Errorf("external responses call build request: %w", err)
	}
	// Always request a stream from the upstream; the handler's non-stream path
	// already buffers through the callback. The Responses API has no
	// stream_options equivalent — usage arrives on the response.completed event.
	body["stream"] = true

	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("external responses call marshal: %w", err)
	}

	endpoint := openAICompatibleEndpoint(baseURL, externalResponsesPath(account))
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("external responses call new request: %w", err)
	}
	setExternalOpenAIHeaders(req, account, apiKey, "text/event-stream")

	client := GetClientForProxy(ResolveAccountProxyURL(account))
	resp, err := doExternalOpenAIRequest(client, req, account)
	if err != nil {
		return fmt.Errorf("external responses call %s: %w", account.Email, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		logExternalPayloadSizeRejection(account, payload, "responses", len(reqBody), resp, errBody)
		// The status stays in the message so the pool's auth-failure handling can
		// disable the account on 401/403/402, exactly as on the chat dialect.
		//
		// There is deliberately no fallback to the chat path: a gateway answering
		// 404 here means the dialect is misconfigured, and silently retrying as
		// chat would hide that while corrupting the token accounting this dialect
		// exists to improve.
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, account.Email, truncateErrBody(errBody))
	}

	// Withhold whitespace-only text so a turn that never produces real content
	// stays retryable instead of reaching the client as a finished, empty answer.
	// Applied here rather than inside the shared parser because the Codex dialect
	// does not use the gate.
	gate := newBlankOutputGate(callback)
	gated := gate.callback()

	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return parseResponsesSSE(resp.Body, gated)
	}
	// Some upstreams omit Content-Type on an SSE body. Peek to tell a JSON
	// fallback from a stream, and hand the buffered reader to whichever parser is
	// chosen — reading resp.Body after Peek would drop the bytes already in the
	// buffer and turn a valid stream into an empty one.
	br := bufio.NewReader(resp.Body)
	first, err := br.Peek(1)
	if err != nil && err != io.EOF {
		return fmt.Errorf("external responses peek: %w", err)
	}
	if len(first) == 0 || first[0] != '{' {
		return parseResponsesSSE(&bufferedReadCloser{Reader: br, Closer: resp.Body}, gated)
	}
	return parseResponsesJSON(br, gated)
}
