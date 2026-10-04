package proxy

import (
	"regexp"
	"strconv"
)

// Extracting per-attempt facts for the usage record without threading new
// parameters through every upstream adapter.
//
// Eight adapters (chat, responses, anthropic, codex, antigravity, agentrouter,
// gommo, kiro) each hold the *http.Response, so passing the status out would
// mean changing every one of them plus dispatchChat's signature. All of them
// already format their failures the same way — fmt.Errorf("HTTP %d from %s: %s")
// — so the status is recoverable from the error string, and the success path is
// by definition a 200 because every adapter returns early on any non-200.

// httpStatusInError matches the status the external adapters embed in their
// error strings: "HTTP 524 from VSLLM: <!DOCTYPE html>". The leading word
// boundary matters — an error body can contain its own numbers (a Ray ID, a
// token count, a JSON "code":502 field) and those are not the response status.
//
// Anchored on the literal "HTTP " prefix for the same reason hasStatusToken in
// pool/ is anchored on non-digit boundaries: bare three-digit scanning picks up
// digits from the Cloudflare HTML page these errors carry.
var httpStatusInError = regexp.MustCompile(`\bHTTP (\d{3})\b`)

// httpStatusFromError recovers the upstream status from an adapter error string.
// Returns 0 when the error carries no status, which is the honest answer for a
// transport failure that never got a response (connection reset, DNS, timeout):
// there was no HTTP status, and the UI must render "—" rather than invent one.
func httpStatusFromError(errMsg string) int {
	if errMsg == "" {
		return 0
	}
	m := httpStatusInError.FindStringSubmatch(errMsg)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	// A three-digit match cannot be out of HTTP range, but the guard keeps the
	// function total: a future "HTTP 1.1" style string in some adapter's message
	// would otherwise parse "1.1" as 1 via a looser pattern.
	if n < 100 || n > 599 {
		return 0
	}
	return n
}

// successStatus is recorded when a turn completed. Every adapter returns early on
// a non-200 upstream response, so reaching the success recording path means the
// upstream answered 200 — including the case where the SSE stream then got cut,
// which is why Status=="error" and HTTPStatus==200 can legitimately coexist.
const successStatus = 200
