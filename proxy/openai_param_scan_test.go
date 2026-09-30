package proxy

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// The scanner is hand-written, so the only real proof it agrees with the
// reference decoder is to compare them. For every body below, scanTopLevelParams
// must produce the same key set and the same raw values as
// json.Unmarshal into map[string]json.RawMessage — after removing the keys the
// scanner intentionally skips (declared + blocked).
//
// The cases are chosen for the things a byte scanner gets wrong: braces and
// brackets inside strings, escaped quotes, escaped backslashes, nested
// containers, whitespace variants, and unicode escapes.
func TestScanTopLevelParamsAgreesWithEncodingJSON(t *testing.T) {
	bodies := []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"stop":["}"], "seed": 7}`,
		`{"stop":["]"],"extra":{"]":"}"}}`,
		`{"content":"a } b { c ] d [","flag":true}`,
		`{"quote":"he said \"stop\"","n2":2}`,
		`{"backslash":"a\\\\b","x":1}`,
		`{"esc_quote_after_backslash":"a\\\\\"b","y":2}`,
		`{"nested":{"deep":{"deeper":[1,2,{"k":"v"}]}},"top":"t"}`,
		`{"arr":[[1],[2,[3]]],"s":9}`,
		`{"unicode":"\u007b\u007d","z":3}`,
		"{" + `"tab":"a\tb"` + "}",
		`{ "a" : 1 , "b" : [ ] , "c" : { } }`,
		`{"nullval":null,"zero":0,"neg":-1.5,"exp":1e10}`,
		`{"empty_obj":{},"empty_arr":[],"empty_str":""}`,
		`{"response_format":{"type":"json_object"},"seed":1}`,
		`{"some_future_openai_param":{"nested":[1,2,3]}}`,
		`{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7,"h":8,"i":9,"j":10}`,
		`{"key with spaces":1,"key\"quote":2}`,
		`{"messages":[{"content":"{\"json\":\"in a string\"}"}],"seed":5}`,
		`{"stop":["\n\nUser:"],"user":"x"}`,
	}

	for _, body := range bodies {
		body := body
		t.Run(body[:min(40, len(body))], func(t *testing.T) {
			if !json.Valid([]byte(body)) {
				t.Skipf("test body is not valid JSON, skip: %s", body)
			}

			// Reference answer from the standard decoder.
			var ref map[string]json.RawMessage
			if err := json.Unmarshal([]byte(body), &ref); err != nil {
				t.Fatalf("reference decode: %v", err)
			}
			want := map[string]string{}
			for k, v := range ref {
				if openAIParamDeclared[k] || openAIParamBlocked[k] {
					continue
				}
				want[k] = string(v)
			}

			got := scanTopLevelParams([]byte(body))
			if len(got) != len(want) {
				gotKeys := make([]string, 0, len(got))
				for k := range got {
					gotKeys = append(gotKeys, k)
				}
				sort.Strings(gotKeys)
				wantKeys := make([]string, 0, len(want))
				for k := range want {
					wantKeys = append(wantKeys, k)
				}
				sort.Strings(wantKeys)
				t.Fatalf("key count = %d %v, want %d %v\nbody=%s",
					len(got), gotKeys, len(want), wantKeys, body)
			}
			for k, wv := range want {
				gv, ok := got[k]
				if !ok {
					t.Errorf("scanner missed key %q (body=%s)", k, body)
					continue
				}
				if string(gv) != wv {
					t.Errorf("key %q value = %s, want %s (body=%s)", k, string(gv), wv, body)
				}
				// The value must still be individually valid JSON, i.e. the
				// scanner sliced on a real boundary.
				if !json.Valid(gv) {
					t.Errorf("key %q produced invalid JSON slice %q", k, string(gv))
				}
			}
		})
	}
}

// Declared and blocked keys must never enter Extra: declared ones come from
// their typed field, and blocked ones are owned by the proxy.
func TestScanTopLevelParamsSkipsDeclaredAndBlocked(t *testing.T) {
	body := `{"model":"m","messages":[],"max_tokens":5,"temperature":0,"top_p":0.9,
	  "stream":true,"stream_options":{"include_usage":true},"tools":[],"tool_choice":"auto",
	  "n":3,"logprobs":true,"top_logprobs":2,
	  "stop":["x"],"seed":1}`
	got := scanTopLevelParams([]byte(body))

	for k := range openAIParamDeclared {
		if _, ok := got[k]; ok {
			t.Errorf("declared key %q must be handled by its typed field, not Extra", k)
		}
	}
	for k := range openAIParamBlocked {
		if _, ok := got[k]; ok {
			t.Errorf("blocked key %q must not be forwarded", k)
		}
	}
	// And the genuinely undeclared ones are still there.
	for _, k := range []string{"stop", "seed"} {
		if _, ok := got[k]; !ok {
			t.Errorf("undeclared key %q was not captured", k)
		}
	}
}

// Malformed input must not panic or invent keys. The struct decode already
// succeeded before this runs, so in practice the body is well formed — but a
// hand-written scanner that panics on odd input is a latent outage.
func TestScanTopLevelParamsMalformedIsSafe(t *testing.T) {
	for _, body := range []string{
		``,
		`{`,
		`{"a"`,
		`{"a":`,
		`{"a":1,`,
		`[]`,
		`"just a string"`,
		`null`,
		`{"a":"unterminated`,
		`{"a":}`,
		`}`,
		`{"a":1}}`,
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic on %q: %v", body, r)
				}
			}()
			_ = scanTopLevelParams([]byte(body))
		}()
	}
	// Non-object input must yield nothing rather than garbage.
	if got := scanTopLevelParams([]byte(`[1,2,3]`)); len(got) != 0 {
		t.Errorf("array body produced %v, want nothing", got)
	}
	if got := scanTopLevelParams([]byte(`{"a"`)); len(got) != 0 {
		t.Errorf("truncated body produced %v, want nothing", got)
	}
}

// A large realistic agent body: the scanner must still find the small scalar
// parameters buried after a huge messages array and tool list, since those
// precede nothing — the params can appear anywhere in the object.
func TestScanTopLevelParamsFindsParamsAfterHugeMessages(t *testing.T) {
	filler := strings.Repeat("repository context line. ", 4000) // ~100 KB
	fillerJSON, _ := json.Marshal(filler)
	body := `{"model":"qwen3.8-max-0902","messages":[{"role":"system","content":` +
		string(fillerJSON) + `},{"role":"user","content":"go"}],` +
		`"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{"p":{"type":"string"}}}}}],` +
		`"max_tokens":64,"temperature":0,"stop":["END"],"seed":42,"response_format":{"type":"json_object"}}`

	if !json.Valid([]byte(body)) {
		t.Fatalf("test body invalid, len=%d", len(body))
	}
	got := scanTopLevelParams([]byte(body))
	for _, k := range []string{"stop", "seed", "response_format"} {
		v, ok := got[k]
		if !ok {
			t.Errorf("key %q not found in a %d-byte body", k, len(body))
			continue
		}
		t.Logf("  %s = %s", k, string(v))
	}
	if _, ok := got["messages"]; ok {
		t.Error("messages must not be captured (it is a declared field)")
	}
	if _, ok := got["tools"]; ok {
		t.Error("tools must not be captured (it is a declared field)")
	}
}
