package proxy

import (
	"bytes"
	"encoding/json"
	"omniproxy/config"
	"strings"
	"testing"
)

// A representative agent-harness body: everything a real client sends, including
// the parameters the previous struct did not declare. Sizes are realistic rather
// than minimal so the benchmark measures the cost the hot path actually pays.
func richOpenAIBody(t *testing.T, fillerKB int) []byte {
	t.Helper()
	filler := strings.Repeat("repository context line. ", fillerKB*1024/26)
	body := `{
      "model": "qwen3.8-max-0902",
      "messages": [
        {"role": "system", "content": ` + mustQuote(filler) + `},
        {"role": "user", "content": "List two prime numbers."}
      ],
      "max_tokens": 64,
      "temperature": 0,
      "top_p": 0.9,
      "stream": true,
      "stream_options": {"include_usage": true},
      "stop": ["\n\nUser:"],
      "seed": 12345,
      "response_format": {"type": "json_object"},
      "parallel_tool_calls": false,
      "reasoning_effort": "high",
      "frequency_penalty": 0.3,
      "presence_penalty": 0.4,
      "user": "hermes-session-abc",
      "n": 2,
      "logprobs": true,
      "top_logprobs": 3,
      "tools": [{"type":"function","function":{"name":"get_weather","description":"d",
        "parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
      "tool_choice": "auto",
      "some_future_openai_param": {"nested": [1,2,3]}
    }`
	if !json.Valid([]byte(body)) {
		t.Fatalf("test body is not valid JSON")
	}
	return []byte(body)
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The core anti-drift guarantee: a parameter the struct does not declare still
// reaches upstream. Listing 12 field names would fix today's gap and silently
// reopen it the next time OpenAI ships a parameter, so the contract is
// "undeclared survives", not "these twelve survive".
func TestUndeclaredParamsSurviveDecodeAndReachUpstream(t *testing.T) {
	initConfigForTests(t)

	var req OpenAIRequest
	body := richOpenAIBody(t, 1)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	req.captureExtraParams(body)

	payload := OpenAIToKiro(&req, false)
	payload.OriginalModel = "qwen3.8-max-0902"

	account := &config.Account{
		ID: "audit", Email: "audit@example.com",
		AuthMethod: externalAuthMethod, AccessToken: "sk-x",
		BaseURL: "https://vsllm.com/",
	}
	upstream, err := kiroPayloadToOpenAIRequest(payload, account)
	if err != nil {
		t.Fatalf("kiroPayloadToOpenAIRequest: %v", err)
	}

	mustForward := []string{
		"stop", "seed", "response_format", "parallel_tool_calls",
		"frequency_penalty", "presence_penalty", "user",
		"some_future_openai_param", // proves the rule, not the list
	}
	for _, k := range mustForward {
		if _, ok := upstream[k]; !ok {
			t.Errorf("upstream body lost %q — an undeclared client parameter was dropped", k)
		}
	}

	// reasoning_effort arrives too. It is no longer a typed field: it travels as
	// raw JSON like every other forwarded parameter, which is why it is read as
	// `"high"` (a quoted JSON string) rather than the Go string "high".
	if got := compactJSON(t, upstream["reasoning_effort"]); got != `"high"` {
		t.Errorf("reasoning_effort = %s, want \"high\"", got)
	}

	// Values must survive byte-identical, not merely be present.
	if got := compactJSON(t, upstream["response_format"]); got != `{"type":"json_object"}` {
		t.Errorf("response_format = %s, want the client's exact object", got)
	}
	// A literal false is the case most likely to be lost to a truthiness check.
	if got := compactJSON(t, upstream["parallel_tool_calls"]); got != `false` {
		t.Errorf("parallel_tool_calls = %s, want false (a false value must not be dropped)", got)
	}
	// stop is an array of strings and must keep its shape.
	if got := compactJSON(t, upstream["stop"]); got != `["\n\nUser:"]` {
		t.Errorf("stop = %s, want the client's array", got)
	}
}

// compactJSON renders a forwarded parameter value the way it will go on the
// wire. Forwarded parameters are json.RawMessage, so marshaling reproduces the
// client's original bytes rather than a Go-typed value.
func compactJSON(t *testing.T, v interface{}) string {
	t.Helper()
	if v == nil {
		return "<absent>"
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return string(b)
}

// The proxy owns these. Forwarding them would either fight its own required
// settings or promise the client something the response parser cannot deliver.
func TestProxyOwnedParamsAreNotForwarded(t *testing.T) {
	initConfigForTests(t)

	var req OpenAIRequest
	body := richOpenAIBody(t, 1)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	req.captureExtraParams(body)

	payload := OpenAIToKiro(&req, false)
	payload.OriginalModel = "qwen3.8-max-0902"
	account := &config.Account{
		ID: "audit", Email: "a@e.com", AuthMethod: externalAuthMethod,
		AccessToken: "sk-x", BaseURL: "https://vsllm.com/",
	}
	upstream, err := kiroPayloadToOpenAIRequest(payload, account)
	if err != nil {
		t.Fatalf("kiroPayloadToOpenAIRequest: %v", err)
	}
	// This is what the production streaming path does next.
	upstream["stream"] = true
	upstream["stream_options"] = map[string]bool{"include_usage": true}

	for _, k := range []string{"n", "logprobs", "top_logprobs"} {
		if _, ok := upstream[k]; ok {
			t.Errorf("%q was forwarded: the response parser merges every choice into "+
				"one stream and drops logprobs, so the client would pay for output it "+
				"can never receive", k)
		}
	}
	// The proxy's own stream settings must win over the client's.
	so, ok := upstream["stream_options"].(map[string]bool)
	if !ok || !so["include_usage"] {
		t.Errorf("stream_options = %v, want the proxy's include_usage:true (usage accounting depends on it)",
			upstream["stream_options"])
	}
}

// temperature 0 is a greedy-decoding pin, not "unset". The old float field could
// not tell the two apart, so a client asking for deterministic output silently
// got the upstream default temperature instead.
func TestTemperatureZeroReachesUpstream(t *testing.T) {
	initConfigForTests(t)

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
	  "max_tokens":16,"temperature":0}`)
	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Temperature == nil {
		t.Fatal("temperature 0 decoded as absent; the pin is lost")
	}
	if *req.Temperature != 0 {
		t.Fatalf("temperature = %v, want 0", *req.Temperature)
	}

	payload := OpenAIToKiro(&req, false)
	payload.OriginalModel = "m"
	account := &config.Account{ID: "a", Email: "a@e.com", AuthMethod: externalAuthMethod,
		AccessToken: "k", BaseURL: "https://x/"}
	upstream, err := kiroPayloadToOpenAIRequest(payload, account)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, ok := upstream["temperature"]
	if !ok {
		t.Fatal("temperature was dropped: the client asked for greedy decoding and " +
			"upstream will use its own default instead")
	}
	if f, _ := got.(float64); f != 0 {
		t.Errorf("temperature = %v, want 0", got)
	}
}

// An absent temperature must stay absent — forwarding a zero the client never
// sent would pin every request to greedy decoding.
func TestAbsentTemperatureIsNotInvented(t *testing.T) {
	initConfigForTests(t)

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Temperature != nil {
		t.Fatalf("temperature = %v, want nil when the client sent none", *req.Temperature)
	}
	payload := OpenAIToKiro(&req, false)
	payload.OriginalModel = "m"
	account := &config.Account{ID: "a", Email: "a@e.com", AuthMethod: externalAuthMethod,
		AccessToken: "k", BaseURL: "https://x/"}
	upstream, err := kiroPayloadToOpenAIRequest(payload, account)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := upstream["temperature"]; ok {
		t.Error("temperature was invented for a request that never carried one")
	}
}

// The combo and adaptive-routing paths re-marshal the decoded request
// (json.Marshal(req) in handleOpenAIChat) and hand it to another handler. If the
// captured parameters do not survive that round trip, a combo sub-request loses
// everything the struct does not declare.
func TestExtraParamsSurviveMarshalRoundTrip(t *testing.T) {
	initConfigForTests(t)

	body := richOpenAIBody(t, 1)
	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	req.captureExtraParams(body)

	out, err := json.Marshal(&req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, k := range []string{"stop", "seed", "response_format", "some_future_openai_param"} {
		if !bytes.Contains(out, []byte(`"`+k+`"`)) {
			t.Errorf("re-marshaled request lost %q; combo sub-requests would drop it", k)
		}
	}
	// Declared fields must still be there, and not duplicated.
	for _, k := range []string{"model", "messages", "max_tokens", "temperature", "tools"} {
		if n := bytes.Count(out, []byte(`"`+k+`"`)); n != 1 {
			t.Errorf("key %q appears %d times in the re-marshaled request, want 1", k, n)
		}
	}
}

// The same temperature-0 bug existed on the Claude/Messages route: both
// translators derived HasTemperature from "Temperature != 0". Fixed in both, so
// pinned in both — fixing one translator and not its sibling is the shape this
// bug actually took.
//
// Note this pins the DECODE layer (request struct -> InferenceConfig). The
// InferenceConfig -> Anthropic body layer already had coverage
// (external_anthropic_params_test.go asserts Temperature 0 + HasTemperature).
func TestClaudeToKiroPreservesGreedyTemperaturePin(t *testing.T) {
	initConfigForTests(t)

	body := []byte(`{"model":"claude-opus-5","max_tokens":16,"temperature":0,
	  "messages":[{"role":"user","content":"hi"}]}`)
	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Temperature == nil {
		t.Fatal("temperature 0 decoded as absent; the greedy pin is lost")
	}
	payload := ClaudeToKiro(&req, false)
	if payload.InferenceConfig == nil {
		t.Fatal("no InferenceConfig built")
	}
	if !payload.InferenceConfig.HasTemperature {
		t.Error("HasTemperature = false for an explicit temperature 0: upstream " +
			"would apply its own default instead of greedy decoding")
	}
	if payload.InferenceConfig.Temperature != 0 {
		t.Errorf("Temperature = %v, want 0", payload.InferenceConfig.Temperature)
	}
}

// And the inverse on the Claude route: no temperature in the request must not
// invent one.
func TestClaudeToKiroDoesNotInventTemperature(t *testing.T) {
	initConfigForTests(t)

	body := []byte(`{"model":"claude-opus-5","max_tokens":16,
	  "messages":[{"role":"user","content":"hi"}]}`)
	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Temperature != nil {
		t.Fatalf("temperature = %v, want nil when the client sent none", *req.Temperature)
	}
	payload := ClaudeToKiro(&req, false)
	if payload.InferenceConfig != nil && payload.InferenceConfig.HasTemperature {
		t.Error("HasTemperature was set for a request that carried no temperature")
	}
}

// captureExtraParams runs on every chat request, including ones with no
// undeclared parameters at all — the common case. It must not allocate for them.
func TestCaptureExtraParamsNoAllocationWhenNothingExtra(t *testing.T) {
	initConfigForTests(t)

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
	  "max_tokens":16,"temperature":0.5,"stream":true}`)
	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	req.captureExtraParams(body)
	if req.Extra != nil {
		t.Errorf("Extra = %v, want nil so the common path allocates nothing", req.Extra)
	}
}

// The perf claim has to be measured, not asserted: this is on the request path
// for every chat call. Run with -bench to see ns/op against the body size.

// initConfigForBench is the benchmark counterpart of initConfigForTests; the
// decode path touches the config singleton for thinking-format settings.
func initConfigForBench(b *testing.B) {
	b.Helper()
	if err := config.Init(b.TempDir() + "/config.json"); err != nil {
		b.Fatalf("config.Init: %v", err)
	}
}

func BenchmarkDecodePlusCapture50KB(b *testing.B) {
	initConfigForBench(b)
	filler := strings.Repeat("repository context line. ", 50*1024/26)
	body := []byte(`{"model":"m","messages":[{"role":"system","content":` +
		mustQuoteBench(filler) +
		`},{"role":"user","content":"hi"}],"max_tokens":64,"temperature":0,"stop":["x"],"seed":1}`)
	if !json.Valid(body) {
		b.Fatalf("invalid bench body")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var req OpenAIRequest
		if err := json.Unmarshal(body, &req); err != nil {
			b.Fatal(err)
		}
		req.captureExtraParams(body)
	}
}

func BenchmarkDecodeOnly50KB(b *testing.B) {
	initConfigForBench(b)
	filler := strings.Repeat("repository context line. ", 50*1024/26)
	body := []byte(`{"model":"m","messages":[{"role":"system","content":` +
		mustQuoteBench(filler) +
		`},{"role":"user","content":"hi"}],"max_tokens":64,"temperature":0,"stop":["x"],"seed":1}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var req OpenAIRequest
		if err := json.Unmarshal(body, &req); err != nil {
			b.Fatal(err)
		}
	}
}

func mustQuoteBench(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
