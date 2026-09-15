// Package proxy — Anthropic Messages adapter for external gateway accounts.
//
// Accounts with AuthMethod == "external_openai" normally forward requests in an
// OpenAI dialect. An account may instead select the Messages API
// (config.Account.ExternalAPIDialect == "anthropic"), which is what a resale
// gateway fronting Claude subscriptions serves: it accepts baseUrl + key like
// any other external account, and only the wire dialect differs.
//
// The transport, retries and the blank-output gate are the same ones the other
// external dialects use; this file owns the path, the headers and the choice
// between the two response framings.
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

// defaultExternalAnthropicPath is the Messages API path used unless the account
// overrides it, mirroring defaultExternalChatPath for chat.
const defaultExternalAnthropicPath = "/v1/messages"

// externalAnthropicVersion is the API version every Messages request names. It
// is part of the API contract rather than a client fingerprint, so it is sent
// under the curl profile too.
const externalAnthropicVersion = "2023-06-01"

// Identity headers, shaped like the official Stainless-generated Python SDK.
// Resale gateways in this pool have been observed to reject generic clients, so
// the default profile mimics a real one and the curl profile stays available
// for gateways that reject the SDK fingerprint instead.
const (
	externalAnthropicUserAgent    = "Anthropic/Python 0.69.0"
	externalAnthropicPackageVer   = "0.69.0"
	externalAnthropicRuntimeVer   = "3.12.7"
	externalAnthropicStainlessStr = "python"
	externalAnthropicRuntime      = "CPython"
)

// externalAnthropicPath returns the upstream Messages path for an account: the
// account-level override when set, otherwise the API default.
func externalAnthropicPath(account *config.Account) string {
	if account == nil {
		return defaultExternalAnthropicPath
	}
	if p := strings.TrimSpace(account.AnthropicPath); p != "" {
		return "/" + strings.TrimLeft(p, "/")
	}
	return defaultExternalAnthropicPath
}

// setExternalAnthropicHeaders applies the Messages API headers.
//
// The key goes out twice, as x-api-key and as Authorization: Bearer. The API
// itself reads x-api-key, but the resale gateways in front of it disagree about
// which one they authenticate, and sending both costs a duplicated header
// holding the same secret rather than a 401 on the half that reads the other.
func setExternalAnthropicHeaders(req *http.Request, account *config.Account, apiKey, accept string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("anthropic-version", externalAnthropicVersion)
	if account != nil && strings.EqualFold(strings.TrimSpace(account.ExternalHeaderProfile), "curl") {
		applyExternalCurlIdentity(req)
		return
	}
	req.Header.Set("User-Agent", externalAnthropicUserAgent)
	req.Header.Set("x-stainless-lang", externalAnthropicStainlessStr)
	req.Header.Set("x-stainless-package-version", externalAnthropicPackageVer)
	req.Header.Set("x-stainless-runtime", externalAnthropicRuntime)
	req.Header.Set("x-stainless-runtime-version", externalAnthropicRuntimeVer)
	req.Header.Set("x-stainless-os", externalOpenAIStainlessOS)
	req.Header.Set("x-stainless-arch", externalOpenAIStainlessArch)
	req.Header.Set("x-stainless-retry-count", "0")
}

// CallExternalAnthropic forwards a KiroPayload to an external gateway speaking
// the Anthropic Messages API.
func CallExternalAnthropic(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if account == nil {
		return fmt.Errorf("external anthropic call: account is nil")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if baseURL == "" {
		return fmt.Errorf("external account %s has no baseUrl", account.Email)
	}
	apiKey := strings.TrimSpace(account.AccessToken)
	if apiKey == "" {
		return fmt.Errorf("external account %s has no apiKey", account.Email)
	}

	body, err := kiroPayloadToAnthropicRequest(payload, account)
	if err != nil {
		return fmt.Errorf("external anthropic call build request: %w", err)
	}
	// Always request a stream from the upstream; the handler's non-stream path
	// already buffers through the callback. Usage arrives on message_start and
	// message_delta, so no stream_options equivalent is needed.
	body["stream"] = true

	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("external anthropic call marshal: %w", err)
	}

	endpoint := openAICompatibleEndpoint(baseURL, externalAnthropicPath(account))
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("external anthropic call new request: %w", err)
	}
	setExternalAnthropicHeaders(req, account, apiKey, "text/event-stream")

	client := GetClientForProxy(ResolveAccountProxyURL(account))
	resp, err := doExternalOpenAIRequest(client, req, account)
	if err != nil {
		return fmt.Errorf("external anthropic call %s: %w", account.Email, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		logExternalPayloadSizeRejection(account, payload, "anthropic", len(reqBody), resp, errBody)
		// The status stays in the message so the pool's auth-failure handling can
		// disable the account on 401/403/402, exactly as on the other dialects.
		//
		// There is deliberately no fallback to another dialect: a gateway that
		// answers 404 here was not selected for this dialect by accident, and
		// silently resending as chat would hide a misconfiguration while
		// corrupting the conversation.
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, account.Email, truncateErrBody(errBody))
	}

	// Both parsers gate output themselves — unlike the Responses parser, which
	// is shared with a dialect that does not — so the callback passes through
	// ungated here and the blank-turn check happens where the stream is read.
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return parseExternalAnthropicSSE(resp.Body, callback)
	}
	// Some upstreams omit Content-Type on an SSE body. Peek to tell a JSON
	// fallback from a stream, and hand the buffered reader to whichever parser
	// is chosen — reading resp.Body after Peek would drop the bytes already in
	// the buffer and turn a valid stream into an empty one.
	br := bufio.NewReader(resp.Body)
	first, err := br.Peek(1)
	if err != nil && err != io.EOF {
		return fmt.Errorf("external anthropic peek: %w", err)
	}
	if len(first) == 0 || first[0] != '{' {
		return parseExternalAnthropicSSE(&bufferedReadCloser{Reader: br, Closer: resp.Body}, callback)
	}
	return parseExternalAnthropicJSON(br, callback)
}
