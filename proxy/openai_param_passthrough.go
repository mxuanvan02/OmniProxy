package proxy

import (
	"encoding/json"
)

// Chat Completions parameter passthrough.
//
// The route is client JSON -> OpenAIRequest -> KiroPayload (IR) -> rebuilt
// upstream body. The IR exists because native Kiro/CodeWhisperer understands a
// different schema, but an external OpenAI-compatible gateway understands
// exactly what the client sent, so every parameter the IR could not express
// used to vanish silently at the first hop.
//
// The fix is deliberately NOT a list of the twelve parameters observed missing.
// OpenAI adds parameters and gateways accept vendor extensions; an enumerated
// allowlist reopens the same hole the first time that happens, and it fails
// invisibly because the request still succeeds with the upstream's defaults. So
// the contract is inverted: everything the client sent is forwarded unless it is
// explicitly claimed by the proxy or already modelled by a typed struct field.

// openAIParamDeclared covers keys the OpenAIRequest struct models with a typed
// field. Those are forwarded from their typed field, so capture must not also
// put them in Extra — the same key would otherwise be emitted twice, once from
// the typed value and once as raw JSON, and the two could disagree.
var openAIParamDeclared = map[string]bool{
	"model":       true,
	"messages":    true,
	"max_tokens":  true,
	"temperature": true,
	"top_p":       true,
	"stream":      true,
	"tools":       true,
	"tool_choice": true,
}

// openAIParamBlocked covers keys the proxy must own or must not promise.
//
//   - stream / stream_options: the streaming path always asks upstream for a
//     stream and always sets stream_options itself, because usage accounting
//     depends on the terminal chunk carrying include_usage. A client value
//     would override that and break billing.
//   - n: the response parser walks every choice and merges their deltas into a
//     single stream. Forwarding n>1 would bill the client for n completions and
//     hand back one interleaved mess.
//   - logprobs / top_logprobs: the parser reads no logprob field, so the client
//     would pay for tokens it can never see.
//
// These are blocked rather than forwarded because forwarding them would make
// the proxy promise something it cannot deliver.
var openAIParamBlocked = map[string]bool{
	"stream":         true,
	"stream_options": true,
	"n":              true,
	"logprobs":       true,
	"top_logprobs":   true,
}

// captureExtraParams records every top-level request parameter that is neither
// declared on the struct nor claimed by the proxy, verbatim as raw JSON.
//
// Raw JSON rather than a decoded value: a parameter this code has never seen has
// no known shape, and round-tripping it through interface{} would rewrite number
// formatting for no benefit. Storing the original bytes means a gateway that
// accepts a vendor extension gets exactly what the client wrote.
//
// Note what this deliberately does NOT handle: temperature. That field is a
// *float64 on the struct, so the shadow-type decode in UnmarshalJSON already
// distinguishes "pinned to 0" from "absent", and openAIParamDeclared keeps it out
// of Extra. Capturing it again here would create a second source of truth for the
// same value.
//
// The scan is a single pass over the top-level keys, not a second
// json.Unmarshal of the whole body: re-parsing would also re-parse messages and
// tool schemas, which are the bulk of an agent request. See scanTopLevelParams
// for the measured cost difference.
//
// Called from UnmarshalJSON, so no call site can forget it. Idempotent.
func (r *OpenAIRequest) captureExtraParams(data []byte) {
	extra := scanTopLevelParams(data)
	if len(extra) == 0 {
		// The common case — a plain chat request with no undeclared
		// parameters — allocates nothing.
		return
	}
	if r.Extra == nil {
		r.Extra = make(map[string]json.RawMessage, len(extra))
	}
	for k, v := range extra {
		r.Extra[k] = v
	}
}

// UnmarshalJSON decodes the request and captures everything the struct does not
// declare. The shadow type breaks the recursion a plain json.Unmarshal(data, r)
// inside this method would cause.
func (r *OpenAIRequest) UnmarshalJSON(data []byte) error {
	type openAIRequestShadow OpenAIRequest
	var s openAIRequestShadow
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	*r = OpenAIRequest(s)
	r.captureExtraParams(data)
	return nil
}

// MarshalJSON re-emits the captured parameters alongside the declared fields.
//
// This exists for the combo and adaptive-routing paths, which re-marshal the
// decoded request (json.Marshal(req) in handleOpenAIChat) and hand the result to
// another handler. Without it a combo sub-request would lose every undeclared
// parameter even though the original request carried it.
//
// Declared fields win over Extra on a key collision, so a hand-built Extra entry
// can never shadow a value the struct actually parsed.
func (r OpenAIRequest) MarshalJSON() ([]byte, error) {
	type openAIRequestShadow OpenAIRequest
	b, err := json.Marshal(openAIRequestShadow(r))
	if err != nil {
		return nil, err
	}
	if len(r.Extra) == 0 {
		return b, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(b, &merged); err != nil {
		return nil, err
	}
	for k, v := range r.Extra {
		if _, exists := merged[k]; exists {
			continue
		}
		merged[k] = v
	}
	return json.Marshal(merged)
}

// SetTemperature records an explicit temperature, including zero. Assigning the
// field directly works too, but this keeps the "present even when zero"
// invariant named at the call sites that build a request in code rather than by
// decoding one (the Responses route).
func (r *OpenAIRequest) SetTemperature(v float64) {
	r.Temperature = &v
}

// forwardedParams returns the client parameters that must reach an
// OpenAI-compatible upstream: everything captured verbatim by
// captureExtraParams.
//
// There is no typed re-encoding step because nothing else needs these values —
// they are forwarded as the client wrote them. The parameters that DO need
// typed handling (temperature, top_p, max_tokens, tools, tool_choice) are
// declared struct fields and are written by the body builder itself.
//
// Returns nil when the client sent nothing undeclared, so a plain chat request
// costs one nil check on the hot path.
func (r *OpenAIRequest) forwardedParams() map[string]json.RawMessage {
	if len(r.Extra) == 0 {
		return nil
	}
	return r.Extra
}

// applyClientParams writes forwarded parameters onto an outbound upstream body.
// Keys the builder already set are left alone, so this can never clobber the
// model, the rebuilt messages, a resolved max_tokens, or the proxy's own
// stream/stream_options settings.
func applyClientParams(body map[string]interface{}, params map[string]json.RawMessage) {
	for k, v := range params {
		if _, exists := body[k]; exists {
			continue
		}
		body[k] = v
	}
}
