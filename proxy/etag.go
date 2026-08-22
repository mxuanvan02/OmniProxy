package proxy

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
)

// etagMaxBuffer caps how much of a response body we are willing to hold in
// memory in order to compute an ETag. The admin polling endpoints this wraps
// are on the order of 20-210 KiB, so 8 MiB is far above the working set while
// still bounding worst-case memory if a future handler starts streaming
// something large through the same route.
const etagMaxBuffer = 8 << 20

// etagWriter buffers a handler's response so a strong content hash can be
// computed after the fact, then either emits 200 with an ETag header or 304
// Not Modified when the client already holds that exact body.
//
// Motivation: the admin dashboard polls /usage/stats (~205 KiB), /accounts
// (~76 KiB) and /quota/overview (~23 KiB) every 5 seconds. The pool is static
// most of the time, so the overwhelming majority of those bytes are identical
// to what the browser already has. Revalidation turns each unchanged poll into
// a ~200 byte header exchange.
//
// This deliberately wraps at the dispatcher rather than inside each handler:
// the handlers build their payloads in several places and returning early from
// them would risk skipping side effects. Buffering the finished bytes is the
// only place where "did anything actually change" can be answered without
// duplicating handler logic.
type etagWriter struct {
	http.ResponseWriter
	req *http.Request

	buf    []byte
	status int

	wroteHeader bool
	// passthrough is set once we have committed to streaming bytes straight to
	// the client (non-200 status, or body larger than etagMaxBuffer). Once set,
	// no ETag is emitted and finish() becomes a no-op.
	passthrough bool
}

func newETagWriter(w http.ResponseWriter, r *http.Request) *etagWriter {
	return &etagWriter{ResponseWriter: w, req: r, status: http.StatusOK}
}

func (w *etagWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	// Only successful, complete bodies are worth hashing. Errors are small and
	// often carry per-request detail, so caching them buys nothing.
	if status != http.StatusOK {
		w.passthrough = true
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *etagWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.passthrough {
		return w.ResponseWriter.Write(p)
	}
	if len(w.buf)+len(p) > etagMaxBuffer {
		// Body outgrew the cap. Degrade to a plain 200 stream: flush what was
		// buffered so far, then hand the rest straight through. Correctness is
		// preserved; we simply forgo revalidation for this response.
		w.passthrough = true
		w.ResponseWriter.WriteHeader(http.StatusOK)
		if len(w.buf) > 0 {
			if _, err := w.ResponseWriter.Write(w.buf); err != nil {
				w.buf = nil
				return 0, err
			}
			w.buf = nil
		}
		return w.ResponseWriter.Write(p)
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// Flush is intentionally a no-op while buffering. These routes are not
// streaming endpoints, and honouring a mid-body flush would force us to commit
// a 200 before the hash is known.
func (w *etagWriter) Flush() {
	if w.passthrough {
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// finish must be called exactly once after the wrapped handler returns.
func (w *etagWriter) finish() {
	if w.passthrough {
		return
	}
	if !w.wroteHeader {
		// Handler produced nothing at all.
		w.ResponseWriter.WriteHeader(http.StatusOK)
		return
	}

	sum := sha256.Sum256(w.buf)
	// 128 bits of a SHA-256 digest is ample for change detection; the full
	// digest only lengthens every response header for no practical gain.
	etag := `"` + base64.RawURLEncoding.EncodeToString(sum[:16]) + `"`

	h := w.ResponseWriter.Header()
	h.Set("ETag", etag)
	// no-cache (not no-store) is the important part: it tells the browser to
	// keep the body but always revalidate, which is exactly what makes the 304
	// path fire. Without this the browser may not send If-None-Match at all.
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-cache, private")
	}

	if etagMatches(w.req.Header.Get("If-None-Match"), etag) {
		// A 304 carries no body, and a stale Content-Length would make the
		// response self-contradictory.
		h.Del("Content-Length")
		w.ResponseWriter.WriteHeader(http.StatusNotModified)
		return
	}

	w.ResponseWriter.WriteHeader(http.StatusOK)
	_, _ = w.ResponseWriter.Write(w.buf)
}

// etagMatches implements the If-None-Match comparison from RFC 9110 section
// 13.1.2, using weak comparison: "*" matches anything, otherwise any entry in
// the comma-separated list matches if it equals the current tag once the
// optional W/ prefix is stripped from both sides.
func etagMatches(ifNoneMatch, current string) bool {
	ifNoneMatch = strings.TrimSpace(ifNoneMatch)
	if ifNoneMatch == "" {
		return false
	}
	if ifNoneMatch == "*" {
		return true
	}
	want := stripWeakETag(current)
	if want == "" {
		return false
	}
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		if stripWeakETag(strings.TrimSpace(candidate)) == want {
			return true
		}
	}
	return false
}

func stripWeakETag(tag string) string {
	tag = strings.TrimSpace(tag)
	if strings.HasPrefix(tag, "W/") || strings.HasPrefix(tag, "w/") {
		tag = strings.TrimSpace(tag[2:])
	}
	return tag
}

// withETag runs fn with a buffering writer so the response can be revalidated.
func withETag(w http.ResponseWriter, r *http.Request, fn func(http.ResponseWriter, *http.Request)) {
	ew := newETagWriter(w, r)
	fn(ew, r)
	ew.finish()
}
