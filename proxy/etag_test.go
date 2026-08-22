package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestETagFirstRequestEmitsTag covers the cold path: no validator from the
// client, so the full body must be written along with an ETag the client can
// echo back next time.
func TestETagFirstRequestEmitsTag(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/api/usage/stats", nil)

	withETag(rec, req, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":1}`))
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"total":1}` {
		t.Fatalf("body = %q, want the handler payload", got)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("ETag header missing; client has nothing to revalidate with")
	}
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Fatalf("ETag = %s, want a quoted entity-tag per RFC 9110", etag)
	}
	// Without no-cache the browser may serve from cache without revalidating,
	// which would defeat the whole mechanism.
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("Cache-Control = %q, want no-cache so If-None-Match is sent", cc)
	}
}

// TestETagUnchangedBodyReturns304 is the case that actually saves bandwidth:
// identical payload plus a matching validator must collapse to a bodyless 304.
func TestETagUnchangedBodyReturns304(t *testing.T) {
	payload := `{"accounts":[{"id":"a"},{"id":"b"}]}`
	handler := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}

	first := httptest.NewRecorder()
	withETag(first, httptest.NewRequest("GET", "/admin/api/accounts", nil), handler)
	etag := first.Header().Get("ETag")

	second := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/api/accounts", nil)
	req.Header.Set("If-None-Match", etag)
	withETag(second, req, handler)

	if second.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304 for an unchanged body", second.Code)
	}
	if body := second.Body.String(); body != "" {
		t.Fatalf("304 carried %d bytes; a 304 must have no body", len(body))
	}
	if cl := second.Header().Get("Content-Length"); cl != "" {
		t.Fatalf("Content-Length = %q on a 304; stale length contradicts the empty body", cl)
	}
}

// TestETagChangedBodyReturns200 guards the failure mode that would be worst in
// practice: a stale validator must never suppress fresh data.
func TestETagChangedBodyReturns200(t *testing.T) {
	first := httptest.NewRecorder()
	withETag(first, httptest.NewRequest("GET", "/admin/api/accounts", nil),
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"enabled":16}`))
		})
	staleTag := first.Header().Get("ETag")

	second := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/api/accounts", nil)
	req.Header.Set("If-None-Match", staleTag)
	withETag(second, req, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":17}`))
	})

	if second.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when the payload changed", second.Code)
	}
	if got := second.Body.String(); got != `{"enabled":17}` {
		t.Fatalf("body = %q, want the new payload", got)
	}
	if second.Header().Get("ETag") == staleTag {
		t.Fatal("ETag did not change even though the body did")
	}
}

// TestETagNonOKPassesThrough documents that error responses bypass the whole
// mechanism. Caching a 503 would be actively harmful: the client could hold a
// validator for a transient failure.
func TestETagNonOKPassesThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/api/usage/stats", nil)

	withETag(rec, req, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"pool empty"}`))
	})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the handler's 503 preserved", rec.Code)
	}
	if rec.Header().Get("ETag") != "" {
		t.Fatal("error response carried an ETag; clients could cache a transient failure")
	}
	if got := rec.Body.String(); got != `{"error":"pool empty"}` {
		t.Fatalf("body = %q, want the error payload intact", got)
	}
}

// TestETagMatchesWeakComparison pins the RFC 9110 13.1.2 semantics. Proxies and
// browsers may add the W/ prefix or send several candidates, and getting this
// wrong shows up as "304 never fires" rather than as an outright bug.
func TestETagMatchesWeakComparison(t *testing.T) {
	const current = `"abc123"`

	cases := []struct {
		name        string
		ifNoneMatch string
		want        bool
	}{
		{"empty header", "", false},
		{"exact match", `"abc123"`, true},
		{"wildcard", "*", true},
		{"weak prefix on client tag", `W/"abc123"`, true},
		{"lowercase weak prefix", `w/"abc123"`, true},
		{"one of several candidates", `"other", "abc123"`, true},
		{"whitespace padded list", `  "abc123"  `, true},
		{"different tag", `"zzz999"`, false},
		{"unquoted value", "abc123", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := etagMatches(tc.ifNoneMatch, current); got != tc.want {
				t.Fatalf("etagMatches(%q, %s) = %v, want %v",
					tc.ifNoneMatch, current, got, tc.want)
			}
		})
	}
}

// TestETagOversizedBodyDegradesGracefully asserts the safety valve: a body past
// the buffer cap must still reach the client in full, just without a validator.
// Silent truncation here would corrupt responses rather than merely slow them.
func TestETagOversizedBodyDegradesGracefully(t *testing.T) {
	chunk := strings.Repeat("x", 1<<20) // 1 MiB per write
	writes := (etagMaxBuffer / len(chunk)) + 2

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/api/usage/stats", nil)

	withETag(rec, req, func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < writes; i++ {
			if _, err := w.Write([]byte(chunk)); err != nil {
				t.Fatalf("write %d failed: %v", i, err)
			}
		}
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	wantLen := writes * len(chunk)
	if rec.Body.Len() != wantLen {
		t.Fatalf("body = %d bytes, want %d; oversized responses must not be truncated",
			rec.Body.Len(), wantLen)
	}
	if rec.Header().Get("ETag") != "" {
		t.Fatal("ETag emitted for a body we never fully hashed")
	}
}

// TestETagEmptyHandlerStillCompletes covers a handler that writes nothing at
// all: finish() must not leave the connection without a status line.
func TestETagEmptyHandlerStillCompletes(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/api/quota/overview", nil)

	withETag(rec, req, func(w http.ResponseWriter, r *http.Request) {})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with no body written", rec.Code)
	}
}

// TestETagStableAcrossIdenticalCalls is the invariant the dashboard relies on:
// same bytes must hash to the same tag, otherwise every poll looks like a change
// and 304 never fires.
func TestETagStableAcrossIdenticalCalls(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"quota":{"used":42}}`))
	}

	var tags []string
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		withETag(rec, httptest.NewRequest("GET", "/admin/api/quota/overview", nil), handler)
		tags = append(tags, rec.Header().Get("ETag"))
	}

	for i := 1; i < len(tags); i++ {
		if tags[i] != tags[0] {
			t.Fatalf("ETag drifted across identical bodies: %s vs %s", tags[0], tags[i])
		}
	}
}
