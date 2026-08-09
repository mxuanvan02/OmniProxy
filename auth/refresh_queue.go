package auth

import (
	"crypto/sha256"
	"fmt"
	"omniproxy/config"
	"sync"
	"time"
)

// refreshQueue serializes refresh-token exchanges to concurrency=1. This is
// required for rotating providers: submitting the same refresh token twice can
// invalidate the account's only usable credential.

var kiroRefreshMu sync.Mutex

// SerialRefreshKiro runs fn serialized against every other OAuth refresh.
// The historical name is retained for compatibility with existing call sites.
func SerialRefreshKiro(fn func() (string, string, int64, string, string, string, error)) (string, string, int64, string, string, string, error) {
	// Keep the lock for the whole upstream exchange. The old linked-list
	// queue cleared its tail when the first caller finished, even while later
	// callers were still queued; a new caller could then refresh in parallel
	// and consume a rotating refresh token twice.
	kiroRefreshMu.Lock()
	defer kiroRefreshMu.Unlock()
	return fn()
}

// ─── Token Rotation Map ──────────────────────────────
//
// When a rotating-token provider refreshes, the old refresh_token is consumed
// and a new one is issued. Any subsequent caller arriving with the OLD token
// would hit upstream and trigger 401 "Bad credentials".
//
// This in-memory map caches recent rotations so a stale caller can be redirected
// to the new tokens WITHOUT touching upstream.
//
// Key: sha256(oldRefreshToken) → Value: rotationResult + expiry
type rotationResult struct {
	AccessToken     string
	RefreshToken    string
	ExpiresAt       int64
	ProfileArn      string
	newClientID     string
	newClientSecret string
	storedAt        time.Time
}

const rotationMapTTL = 60 * time.Second

var (
	rotationMapMu sync.Mutex
	rotationMap   = make(map[string]*rotationResult)
)

func rotationKey(refreshToken string) string {
	h := sha256.Sum256([]byte(refreshToken))
	return fmt.Sprintf("%x", h)
}

// RecordRotation stores the old→new token mapping after a successful refresh.
func RecordRotation(oldRefreshToken, accessToken, newRefreshToken, profileArn string, expiresAt int64) {
	if oldRefreshToken == "" || newRefreshToken == "" || oldRefreshToken == newRefreshToken {
		return
	}
	rotationMapMu.Lock()
	defer rotationMapMu.Unlock()
	rotationMap[rotationKey(oldRefreshToken)] = &rotationResult{
		AccessToken:  accessToken,
		RefreshToken: newRefreshToken,
		ExpiresAt:    expiresAt,
		ProfileArn:   profileArn,
		storedAt:     time.Now(),
	}
	// Clean up stale entries
	for k, v := range rotationMap {
		if time.Since(v.storedAt) > rotationMapTTL {
			delete(rotationMap, k)
		}
	}
}

// CheckRotation checks if a refresh token was already rotated by a sibling.
// Returns the new tokens if found, nil otherwise.
func CheckRotation(oldRefreshToken string) *rotationResult {
	if oldRefreshToken == "" {
		return nil
	}
	rotationMapMu.Lock()
	defer rotationMapMu.Unlock()
	key := rotationKey(oldRefreshToken)
	entry, ok := rotationMap[key]
	if !ok || time.Since(entry.storedAt) > rotationMapTTL {
		if ok {
			delete(rotationMap, key)
		}
		return nil
	}
	return entry
}

// RefreshAccountToken refreshes the token for an account with serialization.
// It ensures only one kiro refresh runs at a time across all accounts.
// API-key accounts have no refresh token — they return immediately.
func RefreshAccountToken(account *config.Account) (string, string, int64, string, string, string, error) {
	if account != nil && account.AuthMethod == "api_key" {
		return account.AccessToken, "", account.ExpiresAt, account.ProfileArn, account.ClientID, account.ClientSecret, nil
	}
	if account != nil {
		if rot := CheckRotation(account.RefreshToken); rot != nil {
			return rot.AccessToken, rot.RefreshToken, rot.ExpiresAt, rot.ProfileArn, rot.newClientID, rot.newClientSecret, nil
		}
	}
	return SerialRefreshKiro(func() (string, string, int64, string, string, string, error) {
		// A caller can have joined the queue before the predecessor completed.
		// Re-check after waiting so a rotated refresh token is never submitted
		// to the upstream endpoint a second time.
		if account != nil {
			if rot := CheckRotation(account.RefreshToken); rot != nil {
				return rot.AccessToken, rot.RefreshToken, rot.ExpiresAt, rot.ProfileArn, rot.newClientID, rot.newClientSecret, nil
			}
		}

		oldRefreshToken := ""
		if account != nil {
			oldRefreshToken = account.RefreshToken
		}
		accessToken, refreshToken, expiresAt, profileArn, clientID, clientSecret, err := RefreshToken(account)
		if err != nil {
			return "", "", 0, "", "", "", err
		}
		// Some OAuth providers issue a new access token without rotating the
		// refresh token. Every caller can then persist the effective token safely.
		if refreshToken == "" {
			refreshToken = oldRefreshToken
		}
		// Publish the rotation before releasing the mutex. Otherwise a caller
		// arriving in the tiny gap after the upstream response could submit the
		// already-consumed token before it sees the rotation cache.
		RecordRotation(oldRefreshToken, accessToken, refreshToken, profileArn, expiresAt)
		return accessToken, refreshToken, expiresAt, profileArn, clientID, clientSecret, nil
	})
}
