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
	"context"
	"fmt"
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

// CallExternalOpenAIResponses forwards a KiroPayload to an external
// OpenAI-compatible provider using the Responses API dialect.
//
// Implemented in phase 03.
func CallExternalOpenAIResponses(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	return fmt.Errorf("external responses dialect not implemented yet for %s", accountLabel(account))
}
