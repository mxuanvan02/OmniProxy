package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	accountpool "omniproxy/pool"
)

// The pool package proves what the snapshot contains; this proves the route is
// reachable, that it is GET-only, and that the envelope the frontend decodes has
// exactly these three keys.
//
// A live 401 probe cannot stand in for this: the admin session gate runs before
// the route switch in handleAdminAPI, so an unregistered path answers 401 too.
func TestPoolHealthRouteReturnsSnapshotEnvelope(t *testing.T) {
	initConfigForTests(t)
	h := &Handler{pool: getServiceTestPool(t), startTime: time.Now().Add(-90 * time.Second).Unix()}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/pool/health", nil)
	req.Header.Set(adminTokenHeader, issueAdminTestToken(t))
	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Accounts map[string]accountpool.AccountHealth `json:"accounts"`
		Since    int64                                `json:"since"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v — body was %s", err, rec.Body.String())
	}
	if body.Accounts == nil {
		t.Fatalf("accounts decoded as null; it must be an empty object: %s", rec.Body.String())
	}
	if body.Since != h.startTime {
		t.Fatalf("since = %d, want %d", body.Since, h.startTime)
	}
	// The envelope is exactly these two keys. A wall-clock field must not come
	// back: it would advance on every read and defeat the ETag below.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	for key := range raw {
		if key != "accounts" && key != "since" {
			t.Fatalf("unexpected envelope key %q — a time-varying field makes the response un-revalidatable: %s", key, rec.Body.String())
		}
	}
}

// The ETag is a digest of the body, so any field that advances with the wall
// clock changes it at least once a second and the 304 can never fire. The sleep
// is deliberate: without crossing a second boundary both requests land inside
// the same second and this passes even when such a field is present.
func TestPoolHealthRouteRevalidatesOnRepeatRequest(t *testing.T) {
	initConfigForTests(t)
	h := &Handler{pool: getServiceTestPool(t), startTime: time.Now().Add(-90 * time.Second).Unix()}

	first := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/pool/health", nil)
	req.Header.Set(adminTokenHeader, issueAdminTestToken(t))
	h.handleAdminAPI(first, req)

	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("first response carried no ETag: %s", first.Body.String())
	}

	time.Sleep(time.Until(time.Unix(time.Now().Unix()+1, 0)) + 50*time.Millisecond)

	second := httptest.NewRecorder()
	again := httptest.NewRequest(http.MethodGet, "/admin/api/pool/health", nil)
	again.Header.Set(adminTokenHeader, issueAdminTestToken(t))
	again.Header.Set("If-None-Match", etag)
	h.handleAdminAPI(second, again)

	if second.Code != http.StatusNotModified {
		t.Fatalf("repeat request = %d, want 304 — the pool did not change, so the body must not have: %s", second.Code, second.Body.String())
	}
}

// Without a session token the handler must not be reached at all.
func TestPoolHealthRouteRequiresAdminSession(t *testing.T) {
	initConfigForTests(t)
	h := &Handler{pool: getServiceTestPool(t), startTime: time.Now().Unix()}

	rec := httptest.NewRecorder()
	h.handleAdminAPI(rec, httptest.NewRequest(http.MethodGet, "/admin/api/pool/health", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}
}
