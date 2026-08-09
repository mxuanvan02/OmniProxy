package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"omniproxy/config"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSerialRefreshKiroSerializesAllCallers(t *testing.T) {
	const callers = 8

	start := make(chan struct{})
	entered := make(chan struct{}, callers)
	release := make(chan struct{})
	var wg sync.WaitGroup

	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, _, _, _, _, err := SerialRefreshKiro(func() (string, string, int64, string, string, string, error) {
				entered <- struct{}{}
				<-release
				return "", "", 0, "", "", "", nil
			})
			if err != nil {
				t.Errorf("serialized refresh returned error: %v", err)
			}
		}()
	}

	close(start)
	for i := 0; i < callers; i++ {
		<-entered
		select {
		case <-entered:
			t.Fatalf("more than one refresh entered its upstream exchange at iteration %d", i)
		default:
		}
		release <- struct{}{}
	}
	wg.Wait()
}

func TestRefreshAccountTokenSharesRotatedCodexCredential(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse refresh form: %v", err)
		}
		if got := r.Form.Get("refresh_token"); !strings.HasPrefix(got, "refresh-concurrent-") {
			t.Fatalf("unexpected refresh token submitted: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"refresh-rotated","expires_in":3600}`,
			makeCodexJWT("acct_concurrent"))
	}))
	defer server.Close()

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	previous := SetGlobalAuthClientForTest(&http.Client{Transport: rewriteAuthRequestTransport{target: target}})
	defer SetGlobalAuthClientForTest(previous)

	const callers = 8
	oldRefresh := fmt.Sprintf("refresh-concurrent-%d", time.Now().UnixNano())
	results := make(chan string, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			account := &config.Account{ID: "codex-concurrent", AuthMethod: "codex", RefreshToken: oldRefresh}
			_, refresh, _, _, _, _, err := RefreshAccountToken(account)
			if err != nil {
				errs <- err
				return
			}
			results <- refresh
		}()
	}
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Fatalf("concurrent refresh: %v", err)
	}
	for refresh := range results {
		if refresh != "refresh-rotated" {
			t.Fatalf("got refresh token %q, want rotated token", refresh)
		}
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream refresh calls = %d, want 1", got)
	}
}
