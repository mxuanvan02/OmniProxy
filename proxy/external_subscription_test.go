package proxy

import (
	"net/http"
	"net/http/httptest"
	"omniproxy/config"
	"strings"
	"testing"
)

// The live VSLLM payload shape, trimmed to three subscriptions: two 5-hour
// windows running in parallel plus one 7-day window, with an expired row that
// must be filtered out. The numbers are the ones observed live (used_percent 3,
// 90 and 100), and the reset pairs differ by exactly 18000s / 604800s.
//
// Two shape quirks of this fork are encoded here on purpose, because both are
// silent-corruption traps:
//   - each list item wraps the object in a "subscription" key
//   - the gateway returns EVERY subscription ever held, including expired ones
//     whose next_reset_time is 0
const liveSubscriptionBody = `{
 "data": {
  "all_subscriptions": [
   {"subscription": {"id": 186673, "plan_id": 13, "status": "active", "source": "redemption",
     "start_time": 1790050742, "end_time": 1792642742,
     "last_reset_time": 1790613834, "next_reset_time": 1790631834,
     "upgrade_group": "default", "consume_priority": 0, "used_percent": 3, "unlimited": false}},
   {"subscription": {"id": 196758, "plan_id": 14, "status": "active", "source": "checkin",
     "start_time": 1790611262, "end_time": 1790697662,
     "last_reset_time": 1790611262, "next_reset_time": 1790629262,
     "upgrade_group": "default", "consume_priority": 0, "used_percent": 90, "unlimited": false}},
   {"subscription": {"id": 196543, "plan_id": 5, "status": "active", "source": "wallet",
     "start_time": 1790586797, "end_time": 1793178797,
     "last_reset_time": 1790586797, "next_reset_time": 1791191597,
     "upgrade_group": "default", "consume_priority": 0, "used_percent": 100, "unlimited": false}},
   {"subscription": {"id": 195670, "plan_id": 14, "status": "expired", "source": "checkin",
     "start_time": 1790527326, "end_time": 1790613726,
     "last_reset_time": 1790613700, "next_reset_time": 0,
     "upgrade_group": "default", "consume_priority": 0, "used_percent": 96, "unlimited": false}}
  ]
 },
 "message": "",
 "success": true
}`

// TestFetchExternalSubscriptionsDerivesWindowFromResetPair is the core case:
// active subscriptions come back with the window length DERIVED from the reset
// pair rather than assumed, and the expired row is dropped. Assuming "5h" would
// mislabel the 7-day plan; keeping expired rows would render a 96%-used bar for
// a plan that ended days ago.
func TestFetchExternalSubscriptionsDerivesWindowFromResetPair(t *testing.T) {
	initConfigForTests(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveSubscriptionBody))
	}))
	defer srv.Close()

	got, err := fetchExternalSubscriptions(&config.Account{
		BaseURL: srv.URL, AccessToken: "sk-inference-key",
		ExtAdminToken: "admin-token", ExtAdminUserID: 12639,
	})
	if err != nil {
		t.Fatalf("fetchExternalSubscriptions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("subscriptions = %d, want 3 (the expired row must be filtered): %+v", len(got), got)
	}

	want := []struct {
		id     int64
		window int
		used   float64
		next   int64
	}{
		{186673, 300, 3, 1790631834},
		{196758, 300, 90, 1790629262},
		{196543, 10080, 100, 1791191597},
	}
	for i, w := range want {
		g := got[i]
		if g.ID != w.id {
			t.Errorf("sub[%d].ID = %d, want %d", i, g.ID, w.id)
		}
		if g.WindowMinutes != w.window {
			t.Errorf("sub[%d] (%d).WindowMinutes = %d, want %d — the window must come from next_reset-last_reset, not a hardcoded 5h",
				i, w.id, g.WindowMinutes, w.window)
		}
		if g.UsedPercent != w.used {
			t.Errorf("sub[%d] (%d).UsedPercent = %v, want %v", i, w.id, g.UsedPercent, w.used)
		}
		if g.NextResetTime != w.next {
			t.Errorf("sub[%d] (%d).NextResetTime = %d, want %d", i, w.id, g.NextResetTime, w.next)
		}
		if g.Status != "active" {
			t.Errorf("sub[%d].Status = %q, want active", i, g.Status)
		}
	}
}

// The credential trap hit while probing this fork by hand: the admin routes
// reject the inference key with "invalid access token", and they reject a valid
// system token whose New-Api-User header does not match. A fetcher that reuses
// getProviderJSON unchanged would send the sk- key and always fail, so the
// headers are asserted explicitly.
func TestFetchExternalSubscriptionsSendsAdminTokenAndUserID(t *testing.T) {
	initConfigForTests(t)
	var gotAuth, gotUID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("New-Api-User")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"all_subscriptions":[]},"success":true}`))
	}))
	defer srv.Close()

	_, err := fetchExternalSubscriptions(&config.Account{
		BaseURL: srv.URL, AccessToken: "sk-inference-key",
		ExtAdminToken: "admin-token-xyz", ExtAdminUserID: 12639,
	})
	if err != nil {
		t.Fatalf("fetchExternalSubscriptions: %v", err)
	}
	if !strings.Contains(gotAuth, "admin-token-xyz") {
		t.Errorf("Authorization = %q, want the admin system token", gotAuth)
	}
	if strings.Contains(gotAuth, "sk-inference-key") {
		t.Error("Authorization carried the inference key; the admin route rejects it with 'invalid access token'")
	}
	if gotUID != "12639" {
		t.Errorf("New-Api-User = %q, want 12639 (new-api answers 'does not match logged in user' otherwise)", gotUID)
	}
}

// The admin routes answer HTTP 200 with success:false for both an auth problem
// and an admin-only route. A fetcher that checks only the status code would
// persist an empty/garbage snapshot over the previous good one.
func TestFetchExternalSubscriptionsRejectsSuccessFalse(t *testing.T) {
	initConfigForTests(t)
	for _, body := range []string{
		`{"message":"Unauthorized, New-Api-User does not match logged in user","success":false}`,
		`{"message":"Unauthorized, insufficient privileges","success":false}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		if _, err := fetchExternalSubscriptions(&config.Account{
			BaseURL: srv.URL, AccessToken: "k",
			ExtAdminToken: "t", ExtAdminUserID: 1,
		}); err == nil {
			t.Errorf("body %q: err = nil, want an error so the caller keeps the previous snapshot", body[:50])
		}
		srv.Close()
	}
}

// Without admin credentials there is nothing to call with, and no request must
// go out at all — otherwise every external account would hit an endpoint that
// 401s for it on every refresh cycle.
func TestFetchExternalSubscriptionsSkipsWithoutAdminCreds(t *testing.T) {
	initConfigForTests(t)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer srv.Close()

	_, err := fetchExternalSubscriptions(&config.Account{
		BaseURL: srv.URL, AccessToken: "sk-only",
	})
	if err != ErrExternalAdminNotConfigured {
		t.Fatalf("err = %v, want ErrExternalAdminNotConfigured", err)
	}
	if called {
		t.Error("a request was sent with no admin token configured; the fetch must be skipped")
	}
}

// A row missing last_reset_time cannot yield a window length. It must survive
// as a row (used% is still real and worth showing) with WindowMinutes 0, which
// is what tells the renderer to omit the "(5h)" label rather than print a wrong
// one.
func TestFetchExternalSubscriptionsKeepsRowWithoutWindowEvidence(t *testing.T) {
	initConfigForTests(t)
	body := `{"data":{"all_subscriptions":[{"subscription":{"id":7,"plan_id":9,"status":"active",
	  "used_percent": 42, "next_reset_time": 1790631834}}]},"success":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	got, err := fetchExternalSubscriptions(&config.Account{
		BaseURL: srv.URL, AccessToken: "k", ExtAdminToken: "t", ExtAdminUserID: 1,
	})
	if err != nil {
		t.Fatalf("fetchExternalSubscriptions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("subscriptions = %d, want 1", len(got))
	}
	if got[0].WindowMinutes != 0 {
		t.Errorf("WindowMinutes = %d, want 0 when last_reset_time is absent", got[0].WindowMinutes)
	}
	if got[0].UsedPercent != 42 {
		t.Errorf("UsedPercent = %v, want 42", got[0].UsedPercent)
	}
}

// The endpoint path must be the user-facing one. The plural/admin-looking names
// return 200 + success:false, so getting this wrong fails quietly.
func TestFetchExternalSubscriptionsHitsUserRoute(t *testing.T) {
	initConfigForTests(t)
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"all_subscriptions":[]},"success":true}`))
	}))
	defer srv.Close()

	// BaseURL carries a /v1 segment in some configs; the admin route is at the
	// root, so the version segment must be stripped.
	if _, err := fetchExternalSubscriptions(&config.Account{
		BaseURL: srv.URL + "/v1", AccessToken: "k",
		ExtAdminToken: "t", ExtAdminUserID: 1,
	}); err != nil {
		t.Fatalf("fetchExternalSubscriptions: %v", err)
	}
	if gotPath != "/api/subscription/self" {
		t.Errorf("path = %q, want /api/subscription/self", gotPath)
	}
}

// A non-JSON body (the SPA served for an unknown path) must be an error, not an
// empty success: empty success would wipe the cached snapshot.
func TestFetchExternalSubscriptionsRejectsHTML(t *testing.T) {
	initConfigForTests(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><body>app</body></html>`))
	}))
	defer srv.Close()

	if _, err := fetchExternalSubscriptions(&config.Account{
		BaseURL: srv.URL, AccessToken: "k", ExtAdminToken: "t", ExtAdminUserID: 1,
	}); err == nil {
		t.Fatal("HTML body accepted as a subscription snapshot; want an error")
	}
}
