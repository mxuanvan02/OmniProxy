// Package proxy: handler_antigravity_api.go
//
// Admin endpoints for adding Google Antigravity (Cloud Code Assist) accounts:
// an interactive OAuth login and an import of credentials an installed
// Antigravity / Gemini CLI already wrote on this machine.
//
// The import path exists because a Google account that is already authorised
// locally does not need a second grant, and because re-granting is what
// invalidates the IDE's own session.
package proxy

import (
	"encoding/json"
	"net/http"
	"omniproxy/auth"
	"omniproxy/config"
	"strings"
	"time"
)

const antigravityProviderLabel = "Google Antigravity"

// apiAntigravityLoginStart begins a PKCE login and returns the authorize URL.
// The browser is opened by the caller (the admin UI) rather than here: unlike
// the Codex flow this login needs no isolated profile, so there is no reason to
// launch a browser process on the server's behalf.
func (h *Handler) apiAntigravityLoginStart(w http.ResponseWriter, r *http.Request) {
	session, err := auth.StartAntigravityLogin()
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"authUrl":   session.AuthURL,
		"expiresIn": int(time.Until(session.ExpiresAt).Seconds()),
		"provider":  "antigravity",
	})
}

// apiAntigravityLoginPoll waits for the OAuth callback and, on success, creates
// or updates the account.
func (h *Handler) apiAntigravityLoginPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Nickname string `json:"nickname"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	tokens, err := auth.PollAntigravityLogin()
	if err != nil {
		if err.Error() == "authorization_pending" {
			json.NewEncoder(w).Encode(map[string]interface{}{"pending": true, "error": err.Error()})
			return
		}
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	account := antigravityAccountFromTokens(tokens.AccessToken, tokens.RefreshToken, tokens.ExpiresAt,
		tokens.Email, tokens.Name, tokens.Subject, req.Nickname, "")
	saved, updated, err := upsertAntigravityAccount(account, strings.TrimSpace(req.Nickname) != "")
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()

	// Resolve the account's own cloudaicompanion project now rather than on the
	// first chat request, so a project that cannot be provisioned surfaces while
	// the operator is still looking at the add-account screen.
	projectID, projectErr := ensureAntigravityProject(&saved)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"updated": updated,
		"account": antigravityAccountJSON(&saved, projectID, projectErr),
	})
}

// apiAntigravityLoginCancel tears down an abandoned login session.
func (h *Handler) apiAntigravityLoginCancel(w http.ResponseWriter, r *http.Request) {
	auth.CancelAntigravityLogin()
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// apiImportAntigravityCreds adds an account from credentials an installed
// Antigravity or Gemini CLI wrote locally, or from tokens supplied explicitly.
func (h *Handler) apiImportAntigravityCreds(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		ProjectID    string `json:"projectId"`
		Nickname     string `json:"nickname"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	accessToken := strings.TrimSpace(req.AccessToken)
	refreshToken := strings.TrimSpace(req.RefreshToken)
	expiresAt := req.ExpiresAt
	projectID := strings.TrimSpace(req.ProjectID)
	email, name, subject := "", "", ""

	if refreshToken == "" {
		local, err := auth.ReadLocalAntigravityCreds()
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		accessToken, refreshToken, expiresAt = local.AccessToken, local.RefreshToken, local.ExpiresAt
		email, name, subject = local.Email, local.Name, local.Subject
		if projectID == "" {
			projectID = local.ProjectID
		}
	}

	account := antigravityAccountFromTokens(accessToken, refreshToken, expiresAt, email, name, subject, req.Nickname, projectID)

	// An imported access token is often already expired, and the identity claims
	// may be missing when the local file carried no id_token. One refresh both
	// proves the refresh token works and fills in the profile.
	if err := refreshAntigravityAccountToken(&account); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refresh token rejected by Google: " + err.Error()})
		return
	}
	if account.Email == "" {
		if info := auth.FetchGoogleUserInfo(account.AccessToken); info.Email != "" {
			account.Email = info.Email
			account.GoogleSubject = info.Subject
			if account.Nickname == "" {
				account.Nickname = info.Name
			}
		}
	}
	if account.Email == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "could not determine the Google account for these credentials"})
		return
	}
	if account.Nickname == "" {
		account.Nickname = antigravityProviderLabel + " (" + account.Email + ")"
	}

	saved, updated, err := upsertAntigravityAccount(account, strings.TrimSpace(req.Nickname) != "")
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	resolvedProject, projectErr := ensureAntigravityProject(&saved)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"updated": updated,
		"account": antigravityAccountJSON(&saved, resolvedProject, projectErr),
	})
}

// antigravityAccountFromTokens builds the account record for a login or import.
func antigravityAccountFromTokens(accessToken, refreshToken string, expiresAt int64,
	email, name, subject, nickname, projectID string) config.Account {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		nickname = strings.TrimSpace(name)
	}
	if nickname == "" && email != "" {
		nickname = antigravityProviderLabel + " (" + email + ")"
	}
	account := config.Account{
		ID:              auth.GenerateAccountID(),
		Email:           strings.TrimSpace(email),
		Nickname:        nickname,
		AuthMethod:      antigravityAuthMethod,
		Provider:        antigravityProviderLabel,
		AccessToken:     strings.TrimSpace(accessToken),
		RefreshToken:    strings.TrimSpace(refreshToken),
		ExpiresAt:       expiresAt,
		GoogleSubject:   strings.TrimSpace(subject),
		GoogleProjectID: strings.TrimSpace(projectID),
		Region:          "external",
		Enabled:         true,
		BanStatus:       "ACTIVE",
		MachineId:       config.GenerateMachineId(),
		Capabilities:    []string{"chat"},
	}
	if account.ExpiresAt == 0 {
		account.ExpiresAt = time.Now().Unix() + 3600
	}
	return account
}

// upsertAntigravityAccount keeps one OmniProxy account per Google account. Two
// records sharing one refresh token would each refresh independently and can
// leave the other holding a credential Google has already replaced.
func upsertAntigravityAccount(candidate config.Account, nicknameExplicit bool) (config.Account, bool, error) {
	for _, existing := range config.GetAccounts() {
		if !isAntigravityAccount(&existing) || !sameGoogleIdentity(existing, candidate) {
			continue
		}
		existing.AccessToken = candidate.AccessToken
		existing.RefreshToken = candidate.RefreshToken
		existing.ExpiresAt = candidate.ExpiresAt
		existing.TokenRefreshedAt = time.Now().Unix()
		if candidate.Email != "" {
			existing.Email = candidate.Email
		}
		if candidate.GoogleSubject != "" {
			existing.GoogleSubject = candidate.GoogleSubject
		}
		if candidate.GoogleProjectID != "" {
			existing.GoogleProjectID = candidate.GoogleProjectID
		}
		if nicknameExplicit && candidate.Nickname != "" {
			existing.Nickname = candidate.Nickname
		}
		existing.Provider = antigravityProviderLabel
		existing.Enabled = true
		// A re-login is the operator's answer to a previous failure, so clear the
		// terminal state and let the next real request decide.
		existing.BanStatus = "ACTIVE"
		existing.BanReason = ""
		existing.BanTime = 0
		if err := config.UpdateAccount(existing.ID, existing); err != nil {
			return config.Account{}, false, err
		}
		return existing, true, nil
	}
	if err := config.AddAccount(candidate); err != nil {
		return config.Account{}, false, err
	}
	return candidate, false, nil
}

// sameGoogleIdentity matches on the Google subject when both sides have one.
// The subject is stable; an email can be changed on the Google account and
// would then create a duplicate record for the same credentials.
func sameGoogleIdentity(existing, candidate config.Account) bool {
	if existing.GoogleSubject != "" && candidate.GoogleSubject != "" {
		return existing.GoogleSubject == candidate.GoogleSubject
	}
	return existing.Email != "" && strings.EqualFold(existing.Email, candidate.Email)
}

// antigravityAccountJSON reports the account plus the outcome of project
// discovery. A project failure is surfaced rather than swallowed: without a
// project the account cannot serve a request, and the reason is what tells the
// operator whether to retry or use a different account.
func antigravityAccountJSON(account *config.Account, projectID string, projectErr error) map[string]interface{} {
	out := map[string]interface{}{
		"id":        account.ID,
		"email":     account.Email,
		"nickname":  account.Nickname,
		"projectId": projectID,
		"tier":      account.AntigravityTier,
		"expiresAt": account.ExpiresAt,
	}
	if projectErr != nil {
		out["projectError"] = projectErr.Error()
	}
	return out
}

// apiAntigravityLocalCreds reports whether importable credentials exist on this
// machine, so the UI can offer the import without the operator hunting for the
// file. Tokens are never returned — only the identity and path.
func (h *Handler) apiAntigravityLocalCreds(w http.ResponseWriter, r *http.Request) {
	local, err := auth.ReadLocalAntigravityCreds()
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"found": false, "error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"found":     true,
		"path":      local.Path,
		"email":     local.Email,
		"name":      local.Name,
		"projectId": local.ProjectID,
		"expiresAt": local.ExpiresAt,
	})
}

// apiAntigravityRefreshProject re-runs project discovery for one account. Tier
// and project assignment are server-side state, so an operator needs a way to
// re-resolve them without deleting and re-adding the account.
func (h *Handler) apiAntigravityRefreshProject(w http.ResponseWriter, r *http.Request, id string) {
	account := h.pool.GetByID(id)
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "account not found"})
		return
	}
	if !isAntigravityAccount(account) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "not an Antigravity account"})
		return
	}
	// Force a fresh lookup instead of reusing the cached project.
	account.AntigravityProjectCheckedAt = 0
	projectID, err := ensureAntigravityProject(account)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"projectId": projectID,
		"tier":      account.AntigravityTier,
	})
}
