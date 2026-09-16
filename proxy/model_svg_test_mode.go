package proxy

// SVG test run modes. The fairness problem they solve: gateways differ in
// whether they enable reasoning on their own, so one fixed config compared
// models on unequal footing. Each pair can now be run in both controlled
// modes — raw (reasoning forced off) and think (reasoning forced on) — at the
// pinned temperature, so a model that only shines in think mode draws its
// strength from reasoning, not from rendering.
//
// A single POST runs exactly one mode. The admin UI expands a "both"
// selection into two POSTs (one per mode), which keeps this handler's response
// single-valued and lets each outcome persist under its own filename.
const (
	svgTestModeRaw   = "raw"
	svgTestModeThink = "think"
)

// svgTestThinkEffort is the reasoning effort forced onto every dialect in
// think mode. All three external builders gate reasoning on a non-empty
// ReasoningEffort, so one value turns it on everywhere.
const svgTestThinkEffort = "high"

// resolveSVGTestMode maps a request's mode onto the one mode this POST runs.
// Empty, "both" and anything unrecognised collapse to raw — the conservative
// single-call baseline — because a bare API call has no UI to expand "both"
// into two requests.
func resolveSVGTestMode(mode string) string {
	if normalizeSVGTestMode(mode) == svgTestModeThink {
		return svgTestModeThink
	}
	return svgTestModeRaw
}

// buildSVGTestPayload builds one mode's upstream payload. The temperature pin
// lives here rather than at the call site so both modes provably share the
// same sampling: the pin is applied on the payload, not the OpenAI request,
// because a zero temperature cannot survive the request's own zero-value
// ambiguity. Model families that reject a temperature override keep their own
// sampling. Reasoning is then forced per mode — off for raw, on for think —
// regardless of what the model suffix parser decided.
func buildSVGTestPayload(openaiReq *OpenAIRequest, actualModel, mode string) *KiroPayload {
	payload := OpenAIToKiro(openaiReq, mode == svgTestModeThink)
	if payload.InferenceConfig == nil {
		payload.InferenceConfig = &InferenceConfig{}
	}
	if temp, ok := svgTestTemperature(actualModel); ok {
		payload.InferenceConfig.Temperature = temp
		payload.InferenceConfig.HasTemperature = true
	}
	if mode == svgTestModeThink {
		payload.InferenceConfig.ReasoningEffort = svgTestThinkEffort
	} else {
		payload.InferenceConfig.ReasoningEffort = ""
	}
	return payload
}
