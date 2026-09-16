package proxy

import "encoding/json"

// probeVisionImageBase64 is a 16x16 solid-colour PNG. Vision providers reject
// degenerate images — a 1x1 PNG fails upstream with "width or height must be
// larger than 10" — so the probe carries the smallest image real gateways
// accept. It is a constant rather than generated per call because every probe
// sends the same bytes and rebuilding a PNG in the probe path only adds noise.
const probeVisionImageBase64 = "iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAIAAACQkWg2AAAAFklEQVR42mO4IydHEmIY1TCqYfhqAACaMxgQdrf9VwAAAABJRU5ErkJggg=="

// probeVisionRequestBody builds the minimal vision request for one dialect.
//
// Vision rides the chat wire, so it shares the chat endpoint and auth, but each
// dialect wraps the image differently and a body built for the wrong one is
// rejected with a 400 that reads as "no vision" on a perfectly capable account.
// The three shapes below mirror exactly what the live adapters emit
// (openAIUserContent, responsesMessageContent, anthropicImageBlock) so the probe
// tests the same bytes a real request would carry.
func probeVisionRequestBody(dialect, model string) []byte {
	dataURL := "data:image/png;base64," + probeVisionImageBase64
	var payload map[string]interface{}
	switch dialect {
	case "responses":
		payload = map[string]interface{}{
			"model": model,
			"input": []map[string]interface{}{{
				"type": "message",
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "input_text", "text": "ping"},
					{"type": "input_image", "image_url": dataURL},
				},
			}},
			// The Responses API floors max_output_tokens at 16.
			"max_output_tokens": 16,
			"stream":            false,
			"store":             false,
		}
	case "anthropic":
		payload = map[string]interface{}{
			"model":      model,
			"max_tokens": 1,
			"messages": []map[string]interface{}{{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "ping"},
					{"type": "image", "source": map[string]interface{}{
						"type":       "base64",
						"media_type": "image/png",
						"data":       probeVisionImageBase64,
					}},
				},
			}},
		}
	default: // chat completions
		payload = map[string]interface{}{
			"model":      model,
			"max_tokens": 1,
			"messages": []map[string]interface{}{{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "ping"},
					{"type": "image_url", "image_url": map[string]interface{}{"url": dataURL}},
				},
			}},
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return body
}
