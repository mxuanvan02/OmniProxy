package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"omniproxy/config"
	"omniproxy/logger"
	"strconv"
	"strings"
	"time"
)

// ErrExternalAdminNotConfigured means the account has no admin-API credentials
// (ExtAdminToken + ExtAdminUserID), so there is nothing to call with. The
// caller must skip the fetch entirely and keep any previous snapshot — an
// unconfigured account is not a failure, and emitting a request for it would
// only earn a 401 on every refresh cycle.
var ErrExternalAdminNotConfigured = fmt.Errorf("external admin API not configured for this account")

// externalSubscriptionPath is the USER-facing route. The plural/admin-looking
// names (/api/user/subscription, /api/user/subscriptions, ...) all answer HTTP
// 200 with success:false "insufficient privileges" in this fork, so they are
// traps: they look like the right route and fail quietly.
const externalSubscriptionPath = "/api/subscription/self"

// subscriptionEnvelope is the wire shape: data.all_subscriptions[] each wrap
// the object in a "subscription" key. Both nesting levels are real and both
// must be decoded, or every row comes back zero.
type subscriptionEnvelope struct {
	Data struct {
		AllSubscriptions []struct {
			Subscription struct {
				ID            int64   `json:"id"`
				PlanID        int     `json:"plan_id"`
				Status        string  `json:"status"`
				Source        string  `json:"source"`
				StartTime     int64   `json:"start_time"`
				EndTime       int64   `json:"end_time"`
				LastResetTime int64   `json:"last_reset_time"`
				NextResetTime int64   `json:"next_reset_time"`
				UsedPercent   float64 `json:"used_percent"`
				Unlimited     bool    `json:"unlimited"`
			} `json:"subscription"`
		} `json:"all_subscriptions"`
	} `json:"data"`
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// fetchExternalSubscriptions reads the account's plan windows from a new-api
// style gateway's admin API. Returns ErrExternalAdminNotConfigured when the
// account carries no admin credentials, and a descriptive error on any other
// failure so the caller can keep the last good snapshot instead of blanking it.
//
// Auth uses the console "System Access Token" (ExtAdminToken) sent RAW — not
// "Bearer "-prefixed — plus the numeric ExtAdminUserID in the New-Api-User
// header. The inference key (AccessToken, sk-…) is rejected by these routes with
// "invalid access token", and a system token whose New-Api-User does not match
// the logged-in user is rejected with "does not match logged in user".
func fetchExternalSubscriptions(account *config.Account) ([]config.ExternalSubscription, error) {
	if account == nil {
		return nil, ErrExternalAdminNotConfigured
	}
	adminToken := strings.TrimSpace(account.ExtAdminToken)
	if adminToken == "" || account.ExtAdminUserID == 0 {
		return nil, ErrExternalAdminNotConfigured
	}

	root := providerRootURL(strings.TrimRight(strings.TrimSpace(account.BaseURL), "/"))
	if root == "" {
		return nil, fmt.Errorf("external subscriptions: no baseUrl")
	}
	url := root + externalSubscriptionPath

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", adminToken) // raw system token, verified live
	req.Header.Set("New-Api-User", strconv.Itoa(account.ExtAdminUserID))
	req.Header.Set("User-Agent", externalOpenAIUserAgent)

	resp, err := doExternalOpenAIRequest(GetRestClientForProxy(ResolveAccountProxyURL(account)), req, account)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateErrBody(b))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(strings.ToLower(ct), "json") {
		// A SPA served for an unknown path decodes to nothing useful; treat as
		// an error, not an empty success, so the snapshot is not wiped.
		return nil, fmt.Errorf("non-JSON response (Content-Type %q)", ct)
	}

	var env subscriptionEnvelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode subscriptions: %w", err)
	}
	// 200 + success:false covers both an auth mismatch and an admin-only route.
	// Neither is data; surface it so the caller keeps the previous snapshot.
	if !env.Success {
		msg := env.Message
		if msg == "" {
			msg = "success:false"
		}
		return nil, fmt.Errorf("admin API refused: %s", msg)
	}

	out := make([]config.ExternalSubscription, 0, len(env.Data.AllSubscriptions))
	for _, item := range env.Data.AllSubscriptions {
		s := item.Subscription
		// The gateway returns every subscription ever held. Only a live one with
		// a pending reset is a usable window: expired rows carry next_reset_time
		// == 0 and a stale used_percent (one expired row read 96%).
		if !strings.EqualFold(s.Status, "active") || s.NextResetTime <= 0 {
			continue
		}
		sub := config.ExternalSubscription{
			ID:            s.ID,
			PlanID:        s.PlanID,
			Status:        s.Status,
			Source:        s.Source,
			UsedPercent:   s.UsedPercent,
			Unlimited:     s.Unlimited,
			StartTime:     s.StartTime,
			EndTime:       s.EndTime,
			LastResetTime: s.LastResetTime,
			NextResetTime: s.NextResetTime,
		}
		// Derive the window length from the reset pair rather than assuming 5h:
		// the same account runs a 18000s and a 604800s window at once. Zero when
		// last_reset is missing, which tells the renderer to omit the duration
		// label instead of printing a wrong one.
		if s.LastResetTime > 0 && s.NextResetTime > s.LastResetTime {
			sub.WindowMinutes = int((s.NextResetTime - s.LastResetTime) / 60)
		}
		out = append(out, sub)
	}
	return out, nil
}

// refreshExternalSubscriptions fetches the plan windows and persists the
// snapshot onto the account for the admin UI. Non-fatal: returns the fetcher's
// error so the caller decides whether to surface it, and never clears the
// previous snapshot on failure (the caller only writes on success).
func (h *Handler) refreshExternalSubscriptions(account *config.Account) error {
	subs, err := fetchExternalSubscriptions(account)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	if err := config.UpdateAccountExternalSubscriptions(account.ID, subs, now); err != nil {
		return fmt.Errorf("persist subscriptions: %w", err)
	}
	account.ExtSubscriptions = subs
	account.ExtSubsCheckedAt = now
	logger.Infof("[ExternalSubs] %s: %d active plan window(s) refreshed", account.Email, len(subs))
	return nil
}
