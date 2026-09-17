package proxy

import "strings"

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
// think mode when the caller does not name one. All three external builders
// gate reasoning on a non-empty ReasoningEffort, so one value turns it on
// everywhere.
const svgTestThinkEffort = "high"

// svgTestEffortLevels is the ladder the sweep walks, in ascending spend order.
// "max" is deliberately excluded: the gateways that accept it disagree on
// whether it means anything above "high", so including it would put a
// guaranteed-empty rung on the curve.
var svgTestEffortLevels = []string{"low", "medium", "high"}

// normalizeSVGTestEffort maps a caller-supplied effort onto one of the four
// levels the upstream dialects understand, or "" for anything else. "" means
// "not specified", which think mode reads as its own default rather than as
// zero effort — raw mode is what forces reasoning off.
func normalizeSVGTestEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "medium", "high", "max":
		return strings.ToLower(strings.TrimSpace(effort))
	default:
		return ""
	}
}

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
// regardless of what the model suffix parser decided. The effort argument only
// has meaning in think mode and lets the sweep place several rungs of the same
// pair on one curve.
func buildSVGTestPayload(openaiReq *OpenAIRequest, actualModel, mode, effort string) *KiroPayload {
	payload := OpenAIToKiro(openaiReq, mode == svgTestModeThink)
	if payload.InferenceConfig == nil {
		payload.InferenceConfig = &InferenceConfig{}
	}
	if temp, ok := svgTestTemperature(actualModel); ok {
		payload.InferenceConfig.Temperature = temp
		payload.InferenceConfig.HasTemperature = true
	}
	payload.InferenceConfig.ReasoningEffort = svgTestReasoningEffort(mode, effort)
	return payload
}

// svgTestReasoningEffort is the single place that decides how hard a run
// thinks. Raw mode always returns "" so no dialect can leave reasoning on by
// itself; think mode honours an explicit level and otherwise falls back to the
// default. Keeping it here means a sweep rung and a plain think run cannot
// drift apart.
func svgTestReasoningEffort(mode, effort string) string {
	if mode != svgTestModeThink {
		return ""
	}
	if level := normalizeSVGTestEffort(effort); level != "" {
		return level
	}
	return svgTestThinkEffort
}
