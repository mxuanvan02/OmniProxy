package pool

import (
	"errors"
	"fmt"
	"omniproxy/config"
	"testing"
	"time"
)

// The error strings below are verbatim from the live log window that motivated
// this change, not invented shapes. Classification is string-matching, so a test
// built on paraphrased errors proves nothing about production behaviour.
const (
	liveAuthErr      = `external call kiro.pix4k.com: HTTP 401: {"error":{"code":"invalid_api_key","message":"invalid API key","type":"authentication_error"}}`
	liveBalanceErr   = `external call NOFX: HTTP 403: {"code":"INSUFFICIENT_BALANCE","message":"Insufficient account balance"}`
	liveQuotaErr     = `external call 300M API GPT: HTTP 429: {"error":{"message":"API key token quota has been exhausted","type":"token_quota_exceeded"}}`
	liveTransientErr = `external call SOTAMODEL: HTTP 503: upstream temporarily unavailable`
)

func TestClassifyCooldownSeparatesCausesThatWaitingCannotFix(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want CooldownClass
	}{
		{"dead credential", errors.New(liveAuthErr), CooldownAuthFailed},
		{"empty wallet reported as 403", errors.New(liveBalanceErr), CooldownNoBalance},
		{"rate window", errors.New(liveQuotaErr), CooldownRateLimited},
		{"upstream blip", errors.New(liveTransientErr), CooldownTransient},
		{"nil", nil, CooldownUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyCooldown(tc.err); got != tc.want {
				t.Fatalf("class = %s, want %s", got, tc.want)
			}
		})
	}
}

// The bug this whole change exists for: a revoked key was recorded as an
// ordinary transient error, so it took three strikes to earn a one-minute rest
// and then walked straight back into rotation. Over a log window that produced
// 768 selections of one dead account, each one a real outbound request.
func TestDeadCredentialLeavesRotationOnTheFirstFailure(t *testing.T) {
	p := newModelPool(config.Account{ID: "dead"})

	p.RecordErrorClass("dead", errors.New(liveAuthErr), "claude-opus-5")

	if got := p.GetNextForModel("claude-opus-5"); got != nil {
		t.Fatal("a dead credential was still selectable after failing: every " +
			"return to the rotation costs another rejected upstream request")
	}
}

// An empty wallet must not be treated as a rate window. NOFX reports it as HTTP
// 403, which the auth check matches first, so ordering inside ClassifyCooldown
// is what keeps these apart.
func TestEmptyWalletIsNotMisreadAsDeadCredential(t *testing.T) {
	if got := ClassifyCooldown(errors.New(liveBalanceErr)); got != CooldownNoBalance {
		t.Fatalf("class = %s, want no_balance: a 403 carrying INSUFFICIENT_BALANCE "+
			"is a billing problem, and refreshing the token cannot fix it", got)
	}

	// Classification alone proves nothing: an earlier version of this test
	// asserted only the label and kept passing while the routing consequence
	// was disabled. Assert the behaviour the label is supposed to cause.
	p := newModelPool(config.Account{ID: "broke"})
	p.RecordErrorClass("broke", errors.New(liveBalanceErr), "claude-opus-5")

	if got := p.GetNextForModel("claude-opus-5"); got != nil {
		t.Fatal("an account with no balance was still selectable after failing; " +
			"every reselection spends a request that cannot succeed until someone tops up")
	}

	p.mu.RLock()
	until := p.modelLocks["broke"]["claude-opus-5"]
	p.mu.RUnlock()
	if remaining := time.Until(until); remaining < 20*time.Minute {
		t.Fatalf("no-balance cooldown is %s; too short to be worth more than a "+
			"rate-limit wait when only a human refill can fix it", remaining)
	}
}

// A rate limit reported as HTTP 403 (several gateways do this) must still be
// classified as rate-limited, or a paying account gets parked as if its key were
// revoked.
func TestRateLimitDressedAsForbiddenStaysRateLimited(t *testing.T) {
	err := errors.New(`HTTP 403: {"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`)
	if got := ClassifyCooldown(err); got != CooldownRateLimited {
		t.Fatalf("class = %s, want rate_limited", got)
	}
}

// Transient errors keep the historical three-strike behaviour. Dropping an
// account on one 503 would turn a momentary upstream blip into lost capacity.
func TestTransientErrorStillTakesThreeStrikes(t *testing.T) {
	p := newModelPool(config.Account{ID: "flaky"})

	for i := 1; i <= 2; i++ {
		p.RecordErrorClass("flaky", errors.New(liveTransientErr), "claude-opus-5")
		if got := p.GetNextForModel("claude-opus-5"); got == nil {
			t.Fatalf("account dropped after %d transient error(s); a 503 blip must not cost capacity", i)
		}
	}

	p.RecordErrorClass("flaky", errors.New(liveTransientErr), "claude-opus-5")
	if got := p.GetNextForModel("claude-opus-5"); got != nil {
		t.Fatal("three consecutive transient errors did not cool the model down")
	}
}

// Every cooldown must be finite. A class that never expires converts a
// provider-side incident into capacity that only a restart recovers, and the
// operator gets no signal that it happened.
func TestNoCooldownClassIsPermanent(t *testing.T) {
	for _, class := range []CooldownClass{
		CooldownTransient, CooldownRateLimited, CooldownAuthFailed,
		CooldownNoBalance, CooldownUnknown,
	} {
		d := class.duration()
		if d <= 0 {
			t.Fatalf("class %s has non-positive duration %s", class, d)
		}
		if d > 2*time.Hour {
			t.Fatalf("class %s parks an account for %s; an upstream bug would "+
				"then look like permanent capacity loss", class, d)
		}
	}
}

// A dead credential must stay out long enough to stop the storm. One minute was
// the old value and it demonstrably did not.
func TestAuthCooldownIsLongEnoughToStopTheRetryStorm(t *testing.T) {
	p := newModelPool(config.Account{ID: "dead"})
	p.RecordErrorClass("dead", errors.New(liveAuthErr), "claude-opus-5")

	p.mu.RLock()
	until := p.modelLocks["dead"]["claude-opus-5"]
	p.mu.RUnlock()

	remaining := time.Until(until)
	if remaining <= time.Minute {
		t.Fatalf("auth cooldown is %s — no better than the one-minute rest that "+
			"allowed 768 retries of one dead account", remaining)
	}
}

// The classified path must not have broken per-model isolation: a dead
// credential for one model is still a dead credential for that account, but the
// legacy boolean API and the classified one must agree on scoping rules.
func TestClassifiedErrorLocksOnlyTheFailingModel(t *testing.T) {
	p := newModelPool(config.Account{ID: "a"})
	p.RecordErrorClass("a", errors.New(liveQuotaErr), "claude-opus-5")

	if got := p.GetNextForModel("claude-sonnet-5"); got == nil || got.ID != "a" {
		t.Fatalf("a per-model failure took a sibling model out of rotation: %#v", got)
	}
}

// RecordError is still called from 15 places; its two-tier behaviour must be
// preserved exactly, or this refactor silently changes unrelated paths.
func TestLegacyRecordErrorBehaviourUnchanged(t *testing.T) {
	quota := newModelPool(config.Account{ID: "q"})
	quota.RecordError("q", true, "m")
	if got := quota.GetNextForModel("m"); got != nil {
		t.Fatal("legacy quota error no longer cools down immediately")
	}
	quota.mu.RLock()
	until := quota.modelLocks["q"]["m"]
	quota.mu.RUnlock()
	if remaining := time.Until(until); remaining < 59*time.Minute || remaining > time.Hour {
		t.Fatalf("legacy quota cooldown = %s, want about 1h", remaining)
	}

	plain := newModelPool(config.Account{ID: "p"})
	for i := 1; i <= 2; i++ {
		plain.RecordError("p", false, "m")
		if got := plain.GetNextForModel("m"); got == nil {
			t.Fatalf("legacy non-quota error cooled down after %d strike(s), want 3", i)
		}
	}
	plain.RecordError("p", false, "m")
	if got := plain.GetNextForModel("m"); got != nil {
		t.Fatal("legacy non-quota error no longer cools down on the third strike")
	}
}

// Success has to clear a classified cooldown too, otherwise an account that
// recovers stays parked for the full window.
func TestRecordSuccessClearsClassifiedCooldown(t *testing.T) {
	p := newModelPool(config.Account{ID: "a"})
	p.RecordErrorClass("a", errors.New(liveAuthErr), "claude-opus-5")
	if got := p.GetNextForModel("claude-opus-5"); got != nil {
		t.Fatal("precondition: the model should be locked")
	}

	p.RecordSuccess("a", "claude-opus-5")

	if got := p.GetNextForModel("claude-opus-5"); got == nil || got.ID != "a" {
		t.Fatalf("a recovered account is still parked: %#v", got)
	}
}

// Wrapped errors are the norm on the failover path — the chat handler wraps the
// upstream error with the account name before it reaches the pool.
func TestClassificationSurvivesErrorWrapping(t *testing.T) {
	wrapped := fmt.Errorf("dispatch chat: %w", errors.New(liveAuthErr))
	if got := ClassifyCooldown(wrapped); got != CooldownAuthFailed {
		t.Fatalf("class = %s for a wrapped auth error, want auth_failed", got)
	}
}
