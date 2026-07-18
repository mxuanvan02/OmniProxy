package auth

import (
	"crypto/sha256"
	"fmt"
	"omniproxy/config"
	"sync"
	"time"
)

// refreshQueue serializes Kiro token refreshes to concurrency=1 across all
// accounts. kiro.dev's social refresh endpoint can invalidate sibling 
// accounts when two refreshes happen concurrently, so all kiro refreshes
// must run sequentially.
//
// Non-Kiro providers (if any are added later) pass through immediately.

var (
	kiroRefreshTail  sync.Mutex
	kiroRefreshInflight *kiroRefreshEntry
)

type kiroRefreshEntry struct {
	done  chan struct{}
	next  *kiroRefreshEntry
}

// SerialRefreshKiro runs fn serialized against every other kiro refresh.
// Different providers (if added) run concurrently; kiro refreshes are serialized.
func SerialRefreshKiro(fn func() (string, string, int64, string, string, string, error)) (string, string, int64, string, string, string, error) {
	kiroRefreshTail.Lock()

	// Chain behind any in-flight refresh.
	var prev *kiroRefreshEntry
	if kiroRefreshInflight != nil {
		prev = kiroRefreshInflight
	}

	myEntry := &kiroRefreshEntry{done: make(chan struct{})}
	kiroRefreshInflight = myEntry
	kiroRefreshTail.Unlock()

	// Wait for predecessor to complete.
	if prev != nil {
		<-prev.done
		// Small settle gap after predecessor completes 
		// DEFAULT_REFRESH_SPACING_MS = 2000ms. Gives kiro.dev time to settle
		// a rotation before the next account refreshes.
		time.Sleep(2 * time.Second)
	}

	defer func() {
		close(myEntry.done)
		kiroRefreshTail.Lock()
		kiroRefreshInflight = nil
		kiroRefreshTail.Unlock()
	}()

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
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	ProfileArn   string
	newClientID  string
	newClientSecret string
	storedAt     time.Time
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
	return SerialRefreshKiro(func() (string, string, int64, string, string, string, error) {
		return RefreshToken(account)
	})
}
