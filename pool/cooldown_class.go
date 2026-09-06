package pool

import (
	"strings"
	"time"
)

// isNoBalanceError reports whether the upstream refused because the account has
// no money left, as distinct from having hit a rate window.
//
// The markers are the ones observed live rather than a guessed list: NOFX
// answers `{"code":"INSUFFICIENT_BALANCE","message":"Insufficient account
// balance"}` with HTTP 403, which IsAuthFailure would otherwise read as a dead
// credential. The credential is fine; the wallet is empty, and no amount of
// token refreshing changes that.
func isNoBalanceError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "insufficient_balance") ||
		strings.Contains(lower, "insufficient balance") ||
		strings.Contains(lower, "insufficient account balance") ||
		strings.Contains(lower, "insufficient credit") ||
		strings.Contains(lower, "insufficient funds") ||
		strings.Contains(lower, "balance is not enough") ||
		strings.Contains(lower, "not enough balance") ||
		strings.Contains(lower, "no balance")
}

// CooldownClass groups upstream failures by how long the account is actually
// unusable, which is not the same thing as how severe the error looks.
//
// Before this existed the pool had two tiers: a quota error cooled the model
// down for an hour, and *everything else* — including a permanently dead API
// key — waited for three consecutive failures and then cooled down for one
// minute. That is why a 401 "invalid API key" account was selected, dialled,
// and rejected 768 times in a single log window: every minute it silently
// returned to the rotation, and each return cost a real outbound HTTP request
// plus one failover slot on somebody's chat turn.
//
// The distinction that matters is whether waiting can fix the error:
//   - a rate limit resets on its own → wait, then retry
//   - a revoked key or an empty wallet does not → stay out until an operator acts
//   - a 5xx blip may already be gone → retry soon, but not instantly
type CooldownClass int

const (
	// CooldownTransient covers 5xx, overload and timeout: the upstream may
	// already be healthy again, so these keep the historical three-strike
	// behaviour rather than dropping an account for one blip.
	CooldownTransient CooldownClass = iota
	// CooldownRateLimited covers 429 and quota exhaustion. Retrying inside the
	// window cannot succeed, so the model is parked until the window rolls.
	CooldownRateLimited
	// CooldownAuthFailed covers 401/403 and invalid-credential replies. The
	// refresh paths have already run and failed by the time this is recorded;
	// re-dialling the same dead credential only burns requests.
	CooldownAuthFailed
	// CooldownNoBalance covers "insufficient balance" and credit exhaustion.
	// Unlike a rate limit this has no reset time — it needs a human to top up.
	CooldownNoBalance
	// CooldownUnknown is the fallback for errors we cannot classify. It keeps
	// the previous conservative behaviour: three strikes, then a short rest.
	CooldownUnknown
)

// Durations are deliberately finite. A permanent-looking failure can still be
// an upstream bug or a provider-side outage, and an account that never returns
// on its own turns a transient provider incident into silent capacity loss that
// only a restart clears.
const (
	cooldownRateLimited = time.Hour
	// Long enough to stop the retry storm (768 attempts became at most a
	// handful per hour), short enough that a provider-side auth outage heals
	// without operator intervention.
	cooldownAuthFailed = 15 * time.Minute
	// Balance is refilled by hand, so checking more often than this is waste;
	// still bounded so a mis-classified error cannot strand a good account.
	cooldownNoBalance = 30 * time.Minute
	// Applied only once the three-strike counter trips.
	cooldownShortRest = time.Minute
)

// immediate reports whether the class should cool down on the first failure
// instead of waiting for three consecutive ones. Counting to three is right for
// errors that might not repeat; for a dead credential it just guarantees two
// more wasted requests.
func (c CooldownClass) immediate() bool {
	switch c {
	case CooldownRateLimited, CooldownAuthFailed, CooldownNoBalance:
		return true
	default:
		return false
	}
}

// duration returns how long to park the account/model for this class.
func (c CooldownClass) duration() time.Duration {
	switch c {
	case CooldownRateLimited:
		return cooldownRateLimited
	case CooldownAuthFailed:
		return cooldownAuthFailed
	case CooldownNoBalance:
		return cooldownNoBalance
	default:
		return cooldownShortRest
	}
}

// String is for log lines: an operator reading "auth_failed 15m0s" can tell why
// an account vanished from the rotation without reading the code.
func (c CooldownClass) String() string {
	switch c {
	case CooldownTransient:
		return "transient"
	case CooldownRateLimited:
		return "rate_limited"
	case CooldownAuthFailed:
		return "auth_failed"
	case CooldownNoBalance:
		return "no_balance"
	default:
		return "unknown"
	}
}

// ClassifyCooldown maps an upstream error to its cooldown class.
//
// Order matters. Several gateways report throttling as HTTP 403 and billing
// failures as HTTP 402 or 403, so the specific markers have to be tested before
// the generic status-code checks — otherwise a rate limit is misread as a dead
// credential and a paying account is parked for 15 minutes instead of retried.
func ClassifyCooldown(err error) CooldownClass {
	if err == nil {
		return CooldownUnknown
	}
	switch {
	case IsRateLimitError(err), IsQuotaExhaustionError(err):
		return CooldownRateLimited
	case isNoBalanceError(err):
		return CooldownNoBalance
	case IsAuthFailure(err):
		return CooldownAuthFailed
	case IsTransientError(err):
		return CooldownTransient
	default:
		return CooldownUnknown
	}
}
