package proxy

import (
	"superkiro/config"
	"superkiro/logger"
	"strings"
	"time"
)

// maxAccountRetryAttempts is no longer a fixed cap — the retry loops in handler.go
// now iterate until all accounts are exhausted (GetNextForModelExcluding returns nil).
// The constant is kept as a safety floor in case of refactoring gaps; actual iteration
// is unbounded, bounded only by the pool's available unique accounts.
const maxAccountRetryAttempts = 128

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "429") || strings.Contains(msg, "quota")
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
		strings.Contains(lower, "timeout exceeded") ||      // Go http.Client.Timeout
		strings.Contains(lower, "client.timeout") ||         // Go http.Client error prefix
		strings.Contains(lower, "context deadline exceeded") // Request context timeout
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

	updatedAccount := *account
	if !updatedAccount.Enabled && updatedAccount.BanStatus == banStatus && updatedAccount.BanReason == banReason {
		return
	}

	updatedAccount.Enabled = false
	updatedAccount.BanStatus = banStatus
	updatedAccount.BanReason = banReason
	updatedAccount.BanTime = time.Now().Unix()

	if err := config.UpdateAccount(account.ID, updatedAccount); err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	h.pool.Reload()
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
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
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
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
