package proxy

import (
	"context"
	"regexp"
	"strings"
	"time"

	"omniproxy/config"
)

// SVG test retry policy. This is deliberately NOT the pool's failover policy:
// proxy/account_failover.go decides whether to abandon an account for live
// traffic, where a 400 means "this request is malformed, stop" because moving to
// another account cannot fix it. Here the question is different — one pinned
// account is being measured, and evidence from api.apiforcode.com showed the
// same byte-identical think payload returning 400 on one attempt and 200 on the
// next, because the gateway round-robins across backends that disagree on the
// request shape. Without a retry that flakiness lands in the grid as a model
// failure and silently biases the comparison. So this classifier is scoped to
// the test path and never influences routing.

// svgTestMaxAttempts is one try plus two retries. Bounded so a dead gateway
// costs the operator seconds, not a hang.
const svgTestMaxAttempts = 3

// svgTestRetryBackoff is the wait before each retry, indexed by retry number.
// Short on purpose: the failures being retried are a gateway picking a bad
// backend, which clears on the next pick rather than after a cooldown window.
// The last entry is reused if more retries are ever configured.
var svgTestRetryBackoff = []time.Duration{2 * time.Second, 5 * time.Second}

// svgTestHTTPStatusPattern pulls the code out of the flattened error text the
// external builders produce ("HTTP 400 from <host>: <truncated body>"). The body
// is truncated upstream, so the status is the only reliable signal.
var svgTestHTTPStatusPattern = regexp.MustCompile(`\bHTTP (\d{3})\b`)

// svgTestDispatcher is dispatchChat's signature, parameterised so the retry loop
// can be exercised by a test without a live account or network.
type svgTestDispatcher func(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error

// dispatchSVGTestWithRetry runs one dispatch, repeating it while the failure
// looks transient and nothing has been emitted yet. The "nothing emitted" guard
// is the important half: once bytes have streamed into the callback, a retry
// would either duplicate the reply or discard a partial one, so the loop hands
// the error back instead. callback.OnReset (when set) clears the caller's
// accumulators between attempts, which is why a retried run cannot leak the
// previous attempt's text into the stored SVG. Returns the last error when every
// attempt failed.
func dispatchSVGTestWithRetry(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback, dispatch svgTestDispatcher, attempts int) error {
	if dispatch == nil {
		dispatch = dispatchChat
	}
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if !svgTestBackoff(ctx, attempt) {
				// The caller's context is shared across attempts, so once it is
				// done no retry can succeed. The upstream error is the more
				// useful record of why the run failed, so it is what we return.
				return lastErr
			}
			if callback != nil && callback.OnReset != nil {
				callback.OnReset()
			}
		}

		lastErr = dispatch(ctx, account, payload, callback)
		if lastErr == nil {
			return nil
		}
		if !svgTestRetryable(lastErr) {
			return lastErr
		}
		if callback != nil && callback.HasOutput != nil && callback.HasOutput() {
			return lastErr
		}
	}
	return lastErr
}

// svgTestBackoff waits before retry number `retry` (1-based), reporting false if
// the context gave up first. The ladder is clamped at both ends so an
// out-of-contract retry number degrades to the nearest rung instead of indexing
// out of range.
func svgTestBackoff(ctx context.Context, retry int) bool {
	idx := retry - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(svgTestRetryBackoff) {
		idx = len(svgTestRetryBackoff) - 1
	}
	timer := time.NewTimer(svgTestRetryBackoff[idx])
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// svgTestRetryable reports whether an error is worth another attempt. The rules
// are ordered: the definite dead ends are rejected first, because several of
// them mention words that would otherwise look transient.
func svgTestRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())

	// The shared context is already spent; another attempt fails identically.
	if strings.Contains(msg, "context canceled") || strings.Contains(msg, "context deadline exceeded") {
		return false
	}
	// Credentials and access are facts about the account, not the gateway's mood.
	// A 403 here was real: the account genuinely could not serve that model.
	if strings.Contains(msg, "quota") || strings.Contains(msg, "insufficient") || strings.Contains(msg, "billing") {
		return false
	}
	// A response-header timeout is the one transport failure that is deliberately
	// NOT retried, even though it carries no HTTP status and would otherwise fall
	// into the transient class below. It is expensive in a way the others are not:
	// the client already waited config.GetResponseHeaderTimeout (300s) and got
	// nothing, so two retries would turn one failed rung into a fifteen-minute
	// wait — breaking the "seconds, not a hang" bound svgTestMaxAttempts states.
	// The backoff ladder is calibrated for failures that clear on the next pick,
	// not for a gateway that stayed silent for five minutes.
	//
	// Observed: a low|medium|high sweep of api.hcnsec.cn failed all three rungs at
	// 300363ms, 300004ms and 300003ms, and a reasoning-off raw probe of the same
	// pair failed identically at 300325ms. Raw mode proves the model was not
	// out-thinking the clock, and four independent attempts landing within 360ms
	// of each other proves the gateway was wedged rather than unlucky.
	//
	// A slow model cannot produce this error. CallExternalOpenAI sets body["stream"]
	// unconditionally (external_openai.go:326), so a responsive gateway sends its
	// 200 plus SSE headers within seconds and the generation then runs against the
	// client's overall timeout, not the header one — which is why a stored 451s
	// qwen3.8-max run succeeded. Waiting the full 300s for headers therefore means
	// nothing at all arrived, so the failure is the gateway's, not the model's.
	if strings.Contains(msg, "timeout awaiting response headers") {
		return false
	}
	if status := svgTestHTTPStatusCode(err); status != 0 {
		switch status {
		case 408, 425, 429, 500, 502, 503, 504, 529:
			return true
		default:
			// 400 is the notable inclusion below; 401/403/404/413/422 are not.
			return status == 400
		}
	}

	// No status means the failure happened below HTTP: the stream was cut or the
	// connection never completed. Both are the transient class this exists for.
	transport := []string{
		"connection refused", "connection reset", "broken pipe", "unexpected eof",
		"eof", "i/o timeout", "tls handshake", "no such host", "server misbehaving",
		"dial tcp", "use of closed network connection", "stream ended before",
	}
	for _, t := range transport {
		if strings.Contains(msg, t) {
			return true
		}
	}
	return false
}

// svgTestHTTPStatusCode extracts the HTTP status from a flattened error, or 0
// when the message carries none.
func svgTestHTTPStatusCode(err error) int {
	m := svgTestHTTPStatusPattern.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	status := 0
	for _, r := range m[1] {
		status = status*10 + int(r-'0')
	}
	return status
}
