package proxy

import (
	"errors"
	"strings"
	"testing"
)

// A bare model count cannot be read by an operator: a search API with no chat
// catalog, a provider whose key just died, and one that was never probed all
// report zero. These tests pin the distinction.
func TestCatalogStatusDistinguishesZeroCounts(t *testing.T) {
	s := newCatalogStatusStore()

	s.record("service", CatalogStatus{
		State:  CatalogStateNotApplicable,
		Source: "service provider (no chat catalog)",
	}, nil)
	s.record("dead-key", CatalogStatus{Source: "/v1/models"},
		errors.New(`HTTP 401: {"error":{"code":"invalid_api_key"}}`))

	svc, ok := s.get("service")
	if !ok {
		t.Fatal("service account has no record")
	}
	if svc.State != CatalogStateNotApplicable {
		t.Fatalf("service state = %q, want not_applicable", svc.State)
	}
	if svc.Error != "" {
		t.Fatalf("a provider with nothing to list is not an error: %q", svc.Error)
	}

	dead, ok := s.get("dead-key")
	if !ok {
		t.Fatal("failed account has no record")
	}
	if dead.State != CatalogStateFailed {
		t.Fatalf("dead credential state = %q, want failed", dead.State)
	}
	if !strings.Contains(dead.Error, "invalid_api_key") {
		t.Fatalf("failure must carry the upstream reason, got %q", dead.Error)
	}
	if dead.State == svc.State {
		t.Fatal("a dead credential and a catalog-less provider must not read alike")
	}
}

// A seeded list is not evidence. Static and verified carry the same shape, so
// only the state tells an operator whether anything confirmed the count.
func TestCatalogStatusSeparatesStaticFromVerified(t *testing.T) {
	s := newCatalogStatusStore()
	s.record("codex", CatalogStatus{
		State:  CatalogStateStatic,
		Count:  15,
		Source: "codex subscription table",
	}, nil)
	s.record("external", CatalogStatus{
		State:  CatalogStateVerified,
		Count:  4,
		Source: "/v1/models",
	}, nil)

	codex, _ := s.get("codex")
	ext, _ := s.get("external")
	if codex.State != CatalogStateStatic {
		t.Fatalf("codex state = %q, want static", codex.State)
	}
	if ext.State != CatalogStateVerified {
		t.Fatalf("external state = %q, want verified", ext.State)
	}
	if codex.Count == 0 || ext.Count == 0 {
		t.Fatal("both records should keep their counts")
	}
}

// SetModelList deliberately keeps a stale catalog rather than emptying it on a
// failed refresh, so the models stay routable. The status record must not
// contradict that by reporting zero.
func TestFailedRefreshKeepsPreviouslyVerifiedCount(t *testing.T) {
	s := newCatalogStatusStore()
	s.record("acct", CatalogStatus{
		State:  CatalogStateVerified,
		Count:  7,
		Source: "/v1/models",
	}, nil)
	s.record("acct", CatalogStatus{Source: "/v1/models"}, errors.New("HTTP 429: quota exhausted"))

	got, _ := s.get("acct")
	if got.State != CatalogStateFailed {
		t.Fatalf("state = %q, want failed", got.State)
	}
	if got.Count != 7 {
		t.Fatalf("count = %d, want 7 — the stale catalog is still routable", got.Count)
	}
	if !strings.Contains(got.Error, "quota exhausted") {
		t.Fatalf("error = %q, want the quota reason", got.Error)
	}
}

// An account nobody probed yet must not be published as an authoritative zero.
func TestCatalogStatusForUnprobedAccountHasNoState(t *testing.T) {
	h := &Handler{catalogStatus: newCatalogStatusStore()}
	got := h.catalogStatusFor("never-seen", 0)
	if got.State != "" {
		t.Fatalf("state = %q, want empty for an unprobed account", got.State)
	}
	if got.Count != 0 {
		t.Fatalf("count = %d, want 0", got.Count)
	}
}

// The pool decides what is routable right now; the record only explains where
// the list came from. A count that grew after the last probe must not be
// masked by the stored number.
func TestCatalogStatusForPrefersLivePoolCount(t *testing.T) {
	h := &Handler{catalogStatus: newCatalogStatusStore()}
	h.catalogStatus.record("acct", CatalogStatus{
		State:  CatalogStateVerified,
		Count:  2,
		Source: "/v1/models",
	}, nil)

	got := h.catalogStatusFor("acct", 5)
	if got.Count != 5 {
		t.Fatalf("count = %d, want the live pool count 5", got.Count)
	}
	if got.State != CatalogStateVerified {
		t.Fatalf("state = %q, want verified preserved", got.State)
	}
}

// A nil store must not panic: tests and any handler built without the
// constructor still read this path.
func TestCatalogStatusForNilStore(t *testing.T) {
	h := &Handler{}
	got := h.catalogStatusFor("acct", 3)
	if got.Count != 3 {
		t.Fatalf("count = %d, want 3", got.Count)
	}
	if got.State != "" {
		t.Fatalf("state = %q, want empty", got.State)
	}
}

func TestTruncateCatalogErrKeepsFirstLine(t *testing.T) {
	// Cloudflare answers with an HTML page; storing it whole would bury the
	// one line that identifies the failure.
	msg := truncateCatalogErr("HTTP 403: <!DOCTYPE html>\n<html>\n<head>lots more</head>")
	if strings.Contains(msg, "\n") {
		t.Fatalf("multi-line error was not collapsed: %q", msg)
	}
	if !strings.HasPrefix(msg, "HTTP 403:") {
		t.Fatalf("lost the status prefix: %q", msg)
	}

	long := truncateCatalogErr(strings.Repeat("x", catalogStatusErrLimit+50))
	if len([]rune(long)) > catalogStatusErrLimit+1 {
		t.Fatalf("error not truncated: %d runes", len([]rune(long)))
	}
}

func TestCatalogStatusForgetDropsRecord(t *testing.T) {
	s := newCatalogStatusStore()
	s.record("acct", CatalogStatus{State: CatalogStateVerified, Count: 1}, nil)
	s.forget("acct")
	if _, ok := s.get("acct"); ok {
		t.Fatal("record survived forget()")
	}
}

// Tests and any Handler built as a struct literal leave catalogStatus nil.
// record/get/forget must no-op rather than panic on a live refresh path.
func TestCatalogStatusNilReceiverIsNoop(t *testing.T) {
	var s *catalogStatusStore
	s.record("acct", CatalogStatus{State: CatalogStateVerified, Count: 1}, nil)
	if _, ok := s.get("acct"); ok {
		t.Fatal("nil store reported a record")
	}
	s.forget("acct")
}
