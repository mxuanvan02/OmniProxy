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
