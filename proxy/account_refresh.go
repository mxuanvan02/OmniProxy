package proxy

import (
	"fmt"
	"omniproxy/auth"
	"omniproxy/config"
	"omniproxy/logger"
	"strings"
	"sync"
	"time"
)

// accountRefreshResult carries the outcome of a full single-account refresh so
// both the per-account endpoint and the bulk "Refresh All" endpoint can report
// the same information.
type accountRefreshResult struct {
	Message   string
	BanStatus string
	Info      *config.AccountInfo
}

// refreshAccountFull performs the complete refresh for ONE account.
//
// This is the shared implementation behind the per-account refresh button
// (POST /accounts/{id}/refresh) and the header "Refresh All" button
// (POST /accounts/refresh-all). Previously the bulk path used the background
// scheduler's refreshAllAccounts(), which is deliberately conservative: it
// skips external and service accounts entirely, refreshes the token only when
// it is close to expiry, never refreshes model catalogs or external credits,
// and never clears a stale ban. That made "Refresh All" materially weaker than
// pressing refresh on each card, which is surprising for an operator.
//
// Behaviour per account kind:
//   - service (search/image): reload the pool so metadata is re-read
//   - external OpenAI-compatible: refresh model catalog + credit balance
//   - Codex: refresh token, re-extract the JWT profile, model catalog, usage
//   - Kiro native: clear a stale ban, refresh the token, re-read usage limits,
//     and re-mark BANNED when upstream still rejects the account
func (h *Handler) refreshAccountFull(account *config.Account) (accountRefreshResult, error) {
	if account == nil {
		return accountRefreshResult{}, fmt.Errorf("account is nil")
	}
	id := account.ID

	if isServiceAccount(account) {
		h.pool.Reload()
		return accountRefreshResult{Message: "Service account metadata refreshed"}, nil
	}

	// External OpenAI-compatible providers have no Kiro usage/subscription to
	// refresh. Refresh their model list and credit balance (via /api/me) so
	// the admin UI shows real data.
	if isExternalAccount(account) {
		modelsErr := h.fetchAndCacheAccountModels(account)
		creditsErr := h.refreshExternalCredits(account)
		// 404 from /api/me means the provider doesn't expose a credits API —
		// not a failure.
		if creditsErr == ErrExternalCreditsNotSupported {
			creditsErr = nil
		}
		if modelsErr != nil && creditsErr != nil {
			return accountRefreshResult{}, fmt.Errorf("external provider refresh failed: %w", modelsErr)
		}
		msg := "External provider refreshed"
		if modelsErr != nil {
			msg = "Models refresh failed: " + modelsErr.Error()
		} else if creditsErr != nil {
			msg = "Models refreshed; credits unavailable: " + creditsErr.Error()
		}
		return accountRefreshResult{Message: msg}, nil
	}

	// Force an OAuth refresh-token exchange and persist the new credentials.
	refreshTokenIfNeeded := func() error {
		if account.RefreshToken == "" {
			return nil
		}
		newAccessToken, newRefreshToken, newExpiresAt, profileArn, _, _, err := auth.RefreshAccountToken(account)
		if err != nil {
			return err
		}
		account.AccessToken = newAccessToken
		if newRefreshToken != "" {
			account.RefreshToken = newRefreshToken
		}
		account.ExpiresAt = newExpiresAt
		config.UpdateAccountToken(id, newAccessToken, newRefreshToken, newExpiresAt)
		h.pool.UpdateToken(id, newAccessToken, newRefreshToken, newExpiresAt)
		if profileArn != "" {
			account.ProfileArn = profileArn
			config.UpdateAccountProfileArn(id, profileArn)
		}
		return nil
	}

	// Codex (ChatGPT subscription) accounts have no Kiro usage API to call.
	// Refresh their JWT-extracted profile (email, name, plan_type), seed the
	// fixed Codex subscription model list into the cache, and fetch live usage
	// data so the admin UI shows real-time token usage.
	//
	// NOTE: Do NOT clear ban here. A banned account should only be unbanned by
	// a successful test request, which proves it can actually serve traffic.
	if isCodexAccount(account) {
		var codexRefreshErr error
		if account.RefreshToken != "" {
			codexRefreshErr = refreshCodexAccountToken(account)
		}
		refreshCodexAccountID(account)
		modelsErr := h.fetchAndCacheAccountModels(account)
		var usageErr error
		if codexRefreshErr == nil {
			usageErr = fetchCodexUsage(account)
		} else {
			// Never probe usage with an access token whose refresh just failed.
			usageErr = fmt.Errorf("token refresh failed: %w", codexRefreshErr)
		}
		// Classify ban vs dead session. Ban vocabulary must win, otherwise a
		// terminated ChatGPT account is presented as a recoverable logout.
		markCodexAuthFailure(account, codexRefreshErr)
		markCodexAuthFailure(account, usageErr)
		msg := "Codex account refreshed"
		if modelsErr != nil && usageErr != nil {
			msg = "Codex profile refreshed; models + usage unavailable: " + usageErr.Error()
		} else if modelsErr != nil {
			msg = "Codex profile refreshed; models unavailable: " + modelsErr.Error()
		} else if usageErr != nil {
			msg = "Codex profile refreshed; usage unavailable: " + usageErr.Error()
		} else {
			msg = "Codex account refreshed (profile + models + usage)"
		}
		return accountRefreshResult{Message: msg}, nil
	}

	// Kiro accounts: clear ban before retrying usage. RefreshAccountInfo below
	// re-marks BANNED if the account is truly still suspended. This is safe
	// because Kiro's usage API returns a definitive ban/suspend flag.
	if account.BanStatus != "" && account.BanStatus != "ACTIVE" {
		logger.Infof("[RefreshAccount] Force-unban %s (was %s), retrying", account.Email, account.BanStatus)
		account.BanStatus = "ACTIVE"
		account.BanReason = ""
		account.BanTime = 0
		account.Enabled = true
		if err := config.UpdateAccount(account.ID, *account); err != nil {
			logger.Errorf("[RefreshAccount] Failed to persist unban for %s: %v", account.Email, err)
		}
	} else if !account.Enabled {
		// Inconsistent state: BanStatus=ACTIVE but Enabled=false. Re-enable.
		account.Enabled = true
		if err := config.UpdateAccount(account.ID, *account); err != nil {
			logger.Errorf("[RefreshAccount] Failed to re-enable %s: %v", account.Email, err)
		}
	}

	// Token expiring soon — refresh before calling the usage API.
	if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
		if err := refreshTokenIfNeeded(); err != nil {
			return accountRefreshResult{}, fmt.Errorf("token refresh failed: %w", err)
		}
	}

	markBanned := func(reason string, errMsg string) accountRefreshResult {
		account.BanStatus = "BANNED"
		account.BanReason = truncateErrBody([]byte(errMsg))
		account.BanTime = time.Now().Unix()
		account.Enabled = false
		if updateErr := config.UpdateAccount(account.ID, *account); updateErr != nil {
			logger.Errorf("[RefreshAccount] Failed to persist BANNED status for %s: %v", account.Email, updateErr)
		} else {
			logger.Warnf("[RefreshAccount] Marked %s as BANNED (%s): %s", account.Email, reason, errMsg)
		}
		return accountRefreshResult{
			Message:   "Account banned: " + truncateErrBody([]byte(errMsg)),
			BanStatus: "BANNED",
		}
	}

	info, err := RefreshAccountInfo(account)
	if err != nil {
		errMsg := err.Error()
		errLower := strings.ToLower(errMsg)

		// "temporarily is suspended" / "account suspended" all mean the account
		// is banned by AWS. No token retry needed — the account is rejected.
		if isSuspensionErrorMessage(errLower) {
			return markBanned("suspended", errMsg), nil
		}

		// 401/403 may just be a stale token — refresh once and retry.
		if strings.Contains(errMsg, "403") || strings.Contains(errMsg, "401") ||
			strings.Contains(errMsg, "invalid") || strings.Contains(errMsg, "expired") {
			if refreshErr := refreshTokenIfNeeded(); refreshErr == nil {
				info, err = RefreshAccountInfo(account)
				if err != nil && isSuspensionErrorMessage(strings.ToLower(err.Error())) {
					return markBanned("suspended after retry", err.Error()), nil
				}
			}
		}

		// Persistent 403 after a token refresh means the account itself is
		// rejected by AWS, not that auth was merely stale.
		if err != nil && isAuthErrorMessage(err.Error()) && !isExternalAccount(account) && !isCodexAccount(account) {
			account.BanStatus = "BANNED"
			account.BanReason = "Persistent 403 after token refresh: " + truncateErrBody([]byte(err.Error()))
			account.BanTime = time.Now().Unix()
			account.Enabled = false
			if updateErr := config.UpdateAccount(account.ID, *account); updateErr != nil {
				logger.Errorf("[RefreshAccount] Failed to persist BANNED status for %s: %v", account.Email, updateErr)
			} else {
				logger.Warnf("[RefreshAccount] Marked %s as BANNED (persistent 403 after token refresh)", account.Email)
			}
			return accountRefreshResult{
				Message:   "Account banned: persistent 403 after token refresh",
				BanStatus: "BANNED",
			}, nil
		}

		if err != nil {
			return accountRefreshResult{}, err
		}
	}

	if info == nil {
		return accountRefreshResult{Message: "Account refreshed"}, nil
	}
	if err := config.UpdateAccountInfo(id, *info); err != nil {
		return accountRefreshResult{}, err
	}
	return accountRefreshResult{Info: info}, nil
}

// accountRefreshOutcome records what happened to a single account during a bulk
// refresh so the caller can build an accurate summary instead of inferring one
// from post-hoc ban flags.
type accountRefreshOutcome struct {
	AccountID string
	Label     string
	Banned    bool
	Reauth    bool
	Skipped   bool
	Err       error
}

// bulkRefreshConcurrency bounds parallel upstream calls during "Refresh All".
// Each account refresh performs several network round-trips (token exchange,
// model catalog, usage/credits), so a full serial pass over a large pool is
// slow, while unbounded parallelism would hammer shared upstream endpoints.
const bulkRefreshConcurrency = 5

// refreshAllAccountsFull runs refreshAccountFull for every account, so the
// "Refresh All" button performs exactly the same work as pressing refresh on
// each individual account card.
func (h *Handler) refreshAllAccountsFull() []accountRefreshOutcome {
	accounts := config.GetAccounts()
	outcomes := make([]accountRefreshOutcome, len(accounts))

	sem := make(chan struct{}, bulkRefreshConcurrency)
	var wg sync.WaitGroup

	for i := range accounts {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			account := &accounts[idx]
			label := account.Nickname
			if label == "" {
				label = account.Email
			}
			if label == "" {
				label = account.ID
			}
			out := accountRefreshOutcome{AccountID: account.ID, Label: label}

			// An account with no credential cannot be refreshed; it has never
			// completed authentication. Service accounts carry their own
			// provider credentials and are still refreshable.
			if account.AccessToken == "" && !isServiceAccount(account) {
				out.Skipped = true
				outcomes[idx] = out
				return
			}

			if _, err := h.refreshAccountFull(account); err != nil {
				out.Err = err
				logger.Warnf("[RefreshAll] %s: %v", label, err)
			}
			// Re-read the persisted status: refreshAccountFull may have marked
			// the account BANNED or REAUTH_REQUIRED as part of the refresh.
			for _, latest := range config.GetAccounts() {
				if latest.ID != account.ID {
					continue
				}
				switch latest.BanStatus {
				case codexReauthRequiredStatus:
					out.Reauth = true
				case "BANNED", "SUSPENDED", "DISABLED":
					out.Banned = true
				}
				break
			}
			outcomes[idx] = out
		}(i)
	}

	wg.Wait()
	h.pool.Reload()
	return outcomes
}
