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
		Accounts      map[string]accountpool.AccountHealth `json:"accounts"`
		Since         int64                                `json:"since"`
		UptimeSeconds int64                                `json:"uptimeSeconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v — body was %s", err, rec.Body.String())
	}
	if body.Accounts == nil {
		t.Fatalf("accounts decoded as null; it must be an empty object: %s", rec.Body.String())
	}
	if body.UptimeSeconds < 89 || body.UptimeSeconds > 120 {
		t.Fatalf("uptimeSeconds = %d, want ~90 — the handler is not reading h.startTime", body.UptimeSeconds)
	}
	if body.Since != h.startTime {
		t.Fatalf("since = %d, want %d", body.Since, h.startTime)
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
