package proxy

import "testing"

// The error strings below are verbatim from the live logs that motivated the
// usage-table columns, not invented shapes. The whole point of parsing them is
// that these are what the adapters actually produce.
func TestHTTPStatusFromError(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want int
	}{
		// Real shapes seen in the hitokiri/Mac logs.
		{"cloudflare 524 html body",
			`HTTP 524 from VSLLM: <!DOCTYPE html><html><head><title>524: A timeout occurred</title>`, 524},
		{"cloudflare 502",
			`HTTP 502 from VSLLM: <!DOCTYPE html>`, 502},
		{"520 unknown origin error",
			`HTTP 520 from VSLLM: <!DOCTYPE html>`, 520},
		{"402 quota", `HTTP 402 from AgentRouter: {"error":{"code":"quota"}}`, 402},
		{"401 auth", `HTTP 401 from VSLLM: {"error":"bad key"}`, 401},
		{"429 rate limit", `HTTP 429 from VSLLM: {"error":{"message":"请求过于频繁"}}`, 429},
		{"400 with json body", `HTTP 400 from SOTA MINH: {"error":{"code":"model_not_found"}}`, 400},
		// Transport failures carry no status at all. Inventing one would be
		// worse than leaving it unknown: 0 renders "—", a fake 500 blames the
		// upstream for a local network drop.
		{"connection reset has no status",
			`external call VSLLM: Post "https://vsllm.com/v1/chat/completions": read tcp [2402::1]:56422->[2606::1]:443: read: connection reset by peer`, 0},
		{"context canceled", `external call VSLLM: context canceled`, 0},
		{"idle timeout", `stream idle timeout: upstream produced no data within idle window`, 0},
		{"sse cut", `external SSE stream ended before a terminal finish_reason or [DONE]`, 0},
		{"empty", ``, 0},
		// Numbers that are NOT the status must not be picked up.
		{"digits in a ray id are not the status",
			`external call VSLLM: Post "https://x": CF-RAY a44a46c7e819c691-SIN`, 0},
		{"json code field is not the status",
			`upstream refused: {"code":502,"message":"bad gateway"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := httpStatusFromError(tc.msg); got != tc.want {
				t.Errorf("httpStatusFromError(%q) = %d, want %d", tc.msg, got, tc.want)
			}
		})
	}
}

// A wrapped error keeps the inner status: the adapters wrap with
// "external call %s: %w", so the status can be several prefixes deep.
func TestHTTPStatusFromErrorWrapped(t *testing.T) {
	msg := `external call VSLLM: HTTP 524 from VSLLM: <!DOCTYPE html>`
	if got := httpStatusFromError(msg); got != 524 {
		t.Errorf("wrapped error: got %d, want 524", got)
	}
}

// The first status in the message wins. A retry that failed twice can carry two,
// and the outermost one describes the attempt being recorded.
func TestHTTPStatusFromErrorTakesFirst(t *testing.T) {
	msg := `HTTP 502 from VSLLM: retrying gave HTTP 524`
	if got := httpStatusFromError(msg); got != 502 {
		t.Errorf("got %d, want the first status 502", got)
	}
}
