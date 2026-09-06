package proxy

import (
	"strings"
	"sync"
	"time"
)

// CatalogState explains why an account's model list looks the way it does.
//
// The count alone is ambiguous and was actively misleading: a service account
// that sells no chat models, a provider whose credential just died, and a
// provider whose catalog was never fetched all reported "0 models" — while an
// account seeded from a hardcoded list reported a number nobody had verified.
// Reading the dashboard, an operator could not tell a healthy account from a
// dead one.
type CatalogState string

const (
	// CatalogStateVerified means the provider answered a catalog request and
	// the count came from that response.
	CatalogStateVerified CatalogState = "verified"
	// CatalogStateStatic means the list was seeded from a built-in table
	// because the provider exposes no machine-readable catalog. The models may
	// well work, but nothing here confirms it.
	CatalogStateStatic CatalogState = "static"
	// CatalogStateNotApplicable means the provider legitimately has no chat
	// catalog to list (search/scrape APIs, media-only gateways).
	CatalogStateNotApplicable CatalogState = "not_applicable"
	// CatalogStateFailed means a fetch was attempted and rejected. The error
	// distinguishes a dead credential from an exhausted quota from a bot filter.
	CatalogStateFailed CatalogState = "failed"
	// CatalogStateEmpty means the provider answered with HTTP 200 and an empty
	// catalog. This is not the same as verified-with-zero, which is a
	// contradiction, nor the same as failed: the credential works.
	//
	// It matters more than it looks. SetModelList deliberately ignores an empty
	// list to avoid letting a transient blip filter an account out, so the pool
	// keeps no entry for it — and accountHasModel treats a missing entry as
	// cold start and returns true. The account therefore stays eligible for
	// *every* model while the dashboard would otherwise imply it serves none.
	CatalogStateEmpty CatalogState = "empty"
)

// CatalogStatus is the per-account provenance record behind the model count.
type CatalogStatus struct {
	State CatalogState `json:"state"`
	Count int          `json:"count"`
	// Source names where the list came from: an endpoint path for a live
	// fetch, or the table name for a static seed.
	Source string `json:"source,omitempty"`
	// Error carries the upstream rejection verbatim (truncated) so the
	// dashboard can show "invalid API key" instead of a bare zero.
	Error     string `json:"error,omitempty"`
	CheckedAt int64  `json:"checkedAt,omitempty"`
}

// catalogStatusErrLimit keeps a provider's HTML error page from being stored
// whole; the first line of a JSON error is what identifies the failure.
const catalogStatusErrLimit = 300

type catalogStatusStore struct {
	mu   sync.RWMutex
	byID map[string]CatalogStatus
}

func newCatalogStatusStore() *catalogStatusStore {
	return &catalogStatusStore{byID: make(map[string]CatalogStatus)}
}

// record stores the outcome of one catalog attempt. A failure never overwrites
// the count of a previously verified catalog: the models stay routable (see
// SetModelList, which deliberately keeps a stale catalog rather than emptying
// it), so the dashboard must keep reporting them while flagging the failure.
// A nil receiver is tolerated on purpose: a Handler built as a struct literal
// (tests, and any construction path that bypasses NewHandler) leaves the store
// unset, and provenance bookkeeping must never take down a live refresh.
func (s *catalogStatusStore) record(accountID string, status CatalogStatus, err error) {
	if s == nil || accountID == "" {
		return
	}
	status.CheckedAt = time.Now().Unix()
	if err != nil {
		status.State = CatalogStateFailed
		status.Error = truncateCatalogErr(err.Error())
	}
	// "Verified zero" is a contradiction: a fetch that succeeded and returned
	// nothing has not verified any model. Normalising here rather than at each
	// call site means no future branch can reintroduce the contradiction.
	if status.State == CatalogStateVerified && status.Count == 0 {
		status.State = CatalogStateEmpty
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if status.State == CatalogStateFailed {
		if prev, ok := s.byID[accountID]; ok && prev.Count > 0 {
			status.Count = prev.Count
			status.Source = prev.Source
		}
	}
	s.byID[accountID] = status
}

func (s *catalogStatusStore) get(accountID string) (CatalogStatus, bool) {
	if s == nil {
		return CatalogStatus{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.byID[accountID]
	return st, ok
}

// forget drops an account's record so a deleted account does not linger in the
// dashboard payload after its ID is reused.
func (s *catalogStatusStore) forget(accountID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.byID, accountID)
	s.mu.Unlock()
}

func truncateCatalogErr(msg string) string {
	msg = strings.TrimSpace(msg)
	if idx := strings.IndexAny(msg, "\r\n"); idx >= 0 {
		msg = strings.TrimSpace(msg[:idx])
	}
	if len(msg) > catalogStatusErrLimit {
		return msg[:catalogStatusErrLimit] + "…"
	}
	return msg
}

// catalogStatusFor resolves the record to publish for an account. An account
// that has never been probed reports an empty state rather than a fabricated
// one — "not yet checked" is a real answer and must not read as "verified 0".
func (h *Handler) catalogStatusFor(accountID string, cachedCount int) CatalogStatus {
	if h.catalogStatus == nil {
		return CatalogStatus{Count: cachedCount}
	}
	st, ok := h.catalogStatus.get(accountID)
	if !ok {
		return CatalogStatus{Count: cachedCount}
	}
	// The pool is authoritative for what is routable right now; the record
	// only explains where that list came from.
	if st.State != CatalogStateFailed || cachedCount > 0 {
		st.Count = cachedCount
	}
	// Overwriting the count can reintroduce the contradiction the recorder
	// normalised away: the pool drops an empty catalog rather than storing it
	// (SetModelList), so a verified account can read back as zero here.
	if st.State == CatalogStateVerified && st.Count == 0 {
		st.State = CatalogStateEmpty
	}
	return st
}
