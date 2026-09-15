package proxy

import (
	"net/http"
	"strings"

	"omniproxy/config"
	"omniproxy/logger"
)

// External accounts are deliberately not truncated to a byte budget the way Kiro
// requests are. The Kiro limit measures the serialised KiroPayload, but the body
// actually sent to an external gateway is a different and much smaller shape —
// chat-completions, Responses or Gemini — so applying the Kiro number would be
// guessing at a limit nobody has measured. Guessing wrong either does nothing or
// silently discards conversation history, and a silent truncation is worse than
// a clear failure: the operator never learns the history was dropped.
//
// What replaces the guess is this log. A rejection that names a size is the only
// hard evidence available about where a gateway's real ceiling sits, so it is
// worth a line. If these lines stay absent the decision to leave external
// requests uncut was right; if they appear, the byte counts in them are the data
// needed to set a real threshold per dialect.
//
// The markers matter more than the status. A 400 is the most common status on
// this path and by far the most common cause is a malformed field, so matching
// on the status alone would report size problems that never happened. Every
// marker below names a size limit explicitly; bare "max_tokens" is deliberately
// absent because "max_tokens must be greater than 0" is a validation error, not
// a ceiling.
var externalPayloadSizeMarkers = []string{
	"context_length_exceeded",
	"context length",
	"context limit",
	"context size",
	"maximum context",
	"context window",
	"too many tokens",
	"reduce the length",
	"reduce the size",
	"payload too large",
	"request entity too large",
	"request too large",
	"body too large",
	// The copula forms ("Your request is too large") are as common as the noun
	// forms and match none of them. A false positive here costs one WARN line
	// and changes no control flow, while a miss costs the only evidence this
	// feature exists to collect.
	"is too large",
	"string too long",
	"input is too long",
	"prompt is too long",
	"exceeded the maximum",
	"exceeds the maximum",
	"exceeds the limit",
}

// externalPayloadSizeRejected reports whether an upstream rejection describes an
// oversized request rather than a malformed one.
func externalPayloadSizeRejected(resp *http.Response, body []byte) bool {
	if resp == nil {
		return false
	}
	// 413 names the condition by itself — a gateway that answers with it has
	// already said the request was too large — so a body that repeats the words
	// is not required. Treating it like a 400 would drop the one unambiguous
	// signal this feature exists to collect.
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		return true
	}
	// A 400 is the commonest status on this path and usually means a malformed
	// field, so there the body has to name a ceiling.
	if resp.StatusCode != http.StatusBadRequest {
		return false
	}
	text := strings.ToLower(string(body))
	for _, marker := range externalPayloadSizeMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// logExternalPayloadSizeRejection records the wire size of a request an upstream
// refused as too large. It reports sizes and identifiers only — never message
// content, never credentials — and changes no control flow: the caller returns
// the upstream's own error unchanged.
func logExternalPayloadSizeRejection(account *config.Account, payload *KiroPayload, dialect string, wireBytes int, resp *http.Response, body []byte) {
	if !externalPayloadSizeRejected(resp, body) {
		return
	}
	logger.Warnf("[ExternalPayload] size-exceeded account=%s model=%s dialect=%s bytes=%d status=%d",
		accountLabel(account), externalPayloadModel(payload), dialect, wireBytes, resp.StatusCode)
}

// externalPayloadModel names the model the size log is about, mirroring the
// resolution the chat builder performs: the requested ID, then the Kiro-mapped
// one, then the literal "auto" the builder sends when both are empty. Logging
// "unknown" there would name a model the gateway never saw.
//
// This is the ID as requested, before per-account ModelMappings rewrite it.
// Applying those here would mean duplicating each adapter's own mapping — the
// chat and AgentRouter paths share one helper while Antigravity builds its map
// inline — and a copy that drifts would be worse than a name the operator can
// map back through the account named on the same line.
func externalPayloadModel(payload *KiroPayload) string {
	if payload == nil {
		return "unknown"
	}
	if m := strings.TrimSpace(payload.OriginalModel); m != "" {
		return m
	}
	if m := strings.TrimSpace(payload.ConversationState.CurrentMessage.UserInputMessage.ModelID); m != "" {
		return m
	}
	return "auto"
}
