package proxy

import "strings"

// svgTestDeterministicTemperature is the sampling pin the SVG model test sends
// so repeated runs of the same prompt are comparable instead of each drawing a
// fresh random sample. Zero is deliberate: the dialect builders forward any
// temperature in range, including an explicit greedy pin, so runs become
// reproducible where the upstream allows it.
const svgTestDeterministicTemperature = 0.0

// temperatureOverrideRejected lists model families whose upstream rejects any
// temperature that differs from their fixed default (OpenAI reasoning models
// answer HTTP 400 "Unsupported parameter: temperature" for anything but 1).
// For those the test leaves sampling untouched rather than forcing a 400.
var temperatureOverrideRejected = []string{"gpt-5", "gpt-6", "o1", "o2", "o3", "o4"}

// svgTestTemperature reports the temperature to pin for a model under test.
// The boolean is false for families that reject a temperature override, where
// the caller must omit the field entirely.
func svgTestTemperature(model string) (float64, bool) {
	lower := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range temperatureOverrideRejected {
		if strings.HasPrefix(lower, prefix) {
			return 0, false
		}
	}
	return svgTestDeterministicTemperature, true
}

// buildSVGTestPayload shapes the one request the test sends upstream. It is
// deliberately bare: the operator's prompt goes out exactly as typed, with no
// thinking preamble injected and no reasoning level requested, so what comes
// back reflects how the model chooses to spend its own budget on this prompt.
// That is the measurement — two models handed the same words, one of which
// decides to think hard and one of which does not, and the difference in score
// versus tokens is the result.
//
// The only thing pinned is the temperature, and that pin exists so repeated runs
// of the same prompt are comparable rather than each drawing a fresh random
// sample. It is applied to the payload rather than the OpenAI request, because a
// zero temperature cannot survive that request's own zero-value ambiguity.
// Model families that reject a temperature override keep their own sampling.
func buildSVGTestPayload(openaiReq *OpenAIRequest, actualModel string) *KiroPayload {
	payload := OpenAIToKiro(openaiReq, false)
	if payload.InferenceConfig == nil {
		payload.InferenceConfig = &InferenceConfig{}
	}
	if temp, ok := svgTestTemperature(actualModel); ok {
		payload.InferenceConfig.Temperature = temp
		payload.InferenceConfig.HasTemperature = true
	}
	// Left empty on purpose. Every dialect builder omits its reasoning field when
	// this is blank (external_openai.go, external_openai_responses.go and the
	// anthropic path all gate on it), so no dialect turns reasoning on by itself
	// and none is told how hard to think.
	payload.InferenceConfig.ReasoningEffort = ""
	return payload
}
