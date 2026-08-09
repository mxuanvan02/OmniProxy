package proxy

import (
	"omniproxy/config"
	"omniproxy/logger"
	"strings"
	"time"
)

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "429") ||
		strings.Contains(msg, "quota") ||
		strings.Contains(msg, "rate_limit") ||
		strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "credit limit") ||
		strings.Contains(msg, "usage_limit") ||
		strings.Contains(msg, "usage limit")
}

func isOverageErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "402") && strings.Contains(msg, "overage")
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

func isAuthErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	if isQuotaErrorMessage(msg) {
		return false
	}
	// Match standalone status codes (no adjacent digits) — catches "401" from
	// "refresh failed: 401 ..." and "HTTP 403 from ..." alike.
	if hasStatusToken(msg, "401") || hasStatusToken(msg, "403") {
		return true
	}
	return strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "bad credentials") ||
		strings.Contains(msg, "token invalid") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "access token expired") ||
		strings.Contains(msg, "refresh token expired")
}

// isNetworkError reports whether an error string indicates a transport-level
// network failure (connection refused, DNS failure, timeout, reset, etc.).
// These affect all accounts equally and should not trigger model-lock cooldowns
// that exhaust the pool — a brief per-account rotation is sufficient.
func isNetworkError(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "no such host") ||
		strings.Contains(lower, "i/o timeout") ||
		strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "broken pipe") ||
		strings.Contains(lower, "eof") ||
		strings.Contains(lower, "dial tcp") ||
		strings.Contains(lower, "dial udp") ||
		strings.Contains(lower, "timeout exceeded") || // Go http.Client.Timeout
		strings.Contains(lower, "client.timeout") || // Go http.Client error prefix
		strings.Contains(lower, "context deadline exceeded") || // Request context timeout
		strings.Contains(lower, "stream idle timeout") // idleTimeoutReader — upstream silent after 200
}

// hasStatusToken returns true when status appears in s with non-digit boundaries
// on both sides, so "401" matches "refresh failed: 401 ..." but not "request_401abc".
func hasStatusToken(s, status string) bool {
	for {
		idx := strings.Index(s, status)
		if idx < 0 {
			return false
		}
		leftOK := idx == 0 || !isDigit(s[idx-1])
		rightIdx := idx + len(status)
		rightOK := rightIdx >= len(s) || !isDigit(s[rightIdx])
		if leftOK && rightOK {
			return true
		}
		s = s[idx+len(status):]
	}
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}
	// Persist the ban status so the admin UI reflects the real state.
	// "temporarily is suspended" / "TEMPORARILY_SUSPENDED" are definitive AWS
	// ban signals — the account is rejected upstream, not a transient blip.
	account.BanStatus = banStatus
	account.BanReason = truncateErrBody([]byte(banReason))
	account.BanTime = time.Now().Unix()
	account.Enabled = false
	if err := config.UpdateAccountPreservingCredentials(account.ID, *account); err != nil {
		logger.Errorf("[AccountFailover] Failed to persist %s status for %s: %v", banStatus, account.Email, err)
	} else {
		logger.Warnf("[AccountFailover] Marked %s as %s: %s", account.Email, banStatus, banReason)
	}
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	// Overages is a Kiro/AWS subscription concept — not applicable to
	// external OpenAI-compatible providers or Codex (ChatGPT subscription)
	// accounts. FetchOverageStatus would hit q.external.amazonaws.com and
	// fail with a DNS error. Skip the refresh for non-Kiro accounts; the
	// overage-like error is logged upstream and treated as a soft cooldown.
	if isExternalAccount(account) || isCodexAccount(account) {
		logger.Warnf("[AccountFailover] Skipping overage refresh for non-Kiro account %s (overage-like error: soft cooldown only)", account.Email)
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error, model string) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()
	switch {
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.pool.RecordError(account.ID, false, model)
	case isQuotaErrorMessage(errMsg):
		h.pool.RecordError(account.ID, true, model)
	case isSuspensionErrorMessage(errMsg):
		// The "temporarily suspended" / "account suspended" patterns are Kiro/
		// AWS-specific upstream messages. External OpenAI-compatible providers
		// and Codex (ChatGPT subscription) accounts may return errors that
		// happen to contain "suspended" without meaning the account is banned
		// — never auto-disable non-Kiro accounts on this pattern; treat as a
		// soft cooldown so operators can investigate.
		if isExternalAccount(account) || isCodexAccount(account) {
			logger.Warnf("[AccountFailover] Non-Kiro account %s returned suspension-like error (not auto-banning): %v", account.Email, err)
			h.pool.RecordError(account.ID, false, model)
		} else {
			h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
		}
	case isProfileUnavailableErrorMessage(errMsg):
		// Profile ARN may be transiently unresolvable (upstream blip, stale token).
		// Treat as a soft failure: short cooldown so the next request rotates account,
		// but never auto-disable — operators can still investigate via warn logs.
		h.pool.RecordError(account.ID, false, model)
	case isAuthErrorMessage(errMsg):
		// Soft cooldown only — never disable. auth.RefreshToken already tried OIDC
		// re-registration + social fallback internally. If all paths fail, brief
		// cooldown prevents tight loops while next cycle retries.
		h.pool.RecordError(account.ID, false, model)
	case isNetworkError(errMsg):
		// Network errors (connection refused, DNS failure, timeout) affect all
		// accounts equally when the gateway is down. Do NOT model-lock — just
		// rotate to the next account. The brief cooldown from RecordError would
		// exhaust the pool unnecessarily.
		logger.Debugf("[AccountFailover] Network error for %s: %v — rotating without cooldown", account.Email, err)
	default:
		h.pool.RecordError(account.ID, false, model)
	}
}
