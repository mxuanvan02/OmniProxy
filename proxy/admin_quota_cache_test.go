package proxy

import (
	"omniproxy/config"
	"strings"
	"testing"
)

func TestBuildAccountQuotasUsesReportedCodexPrimaryWindow(t *testing.T) {
	tests := []struct {
		name          string
		windowMinutes int
		wantPrimary   string
	}{
		{name: "weekly", windowMinutes: 7 * 24 * 60, wantPrimary: "Primary"},
		{name: "five hours", windowMinutes: 5 * 60, wantPrimary: "Primary (5h)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := buildAccountQuotas(config.Account{
				AuthMethod:                "codex",
				CodexPrimaryUsedPercent:   25,
				CodexPrimaryWindowMinutes: tt.windowMinutes,
				CodexPrimaryResetAt:       1_700_000_000,
				CodexSecondaryUsedPercent: 40,
				CodexSecondaryResetAt:     1_700_100_000,
			}, 0)
			if len(rows) < 2 {
				t.Fatalf("expected primary and secondary quota rows, got %+v", rows)
			}
			if got := rows[0].Name; got != tt.wantPrimary {
				t.Fatalf("primary row name = %q, want %q", got, tt.wantPrimary)
			}
			if got := rows[1].Name; got != "Secondary (weekly)" {
				t.Fatalf("secondary row name = %q, want weekly quota row", got)
			}
		})
	}
}

func TestBuildAccountQuotasUsesPersistedWindowTokens(t *testing.T) {
	account := config.Account{
		AuthMethod:                   "codex",
		TotalTokens:                  90_000,
		CodexTokensSincePrimaryReset: 0,
		CodexPrimaryUsedPercent:      70,
		CodexPrimaryWindowMinutes:    7 * 24 * 60,
		CodexPrimaryResetAt:          1_700_000_000,
	}
	rows := buildAccountQuotas(account, 12_345)

	for _, row := range rows {
		if row.Name != "Tokens This Reset" {
			continue
		}
		if row.Used != 12_345 {
			t.Fatalf("window token row = %.0f, want 12345", row.Used)
		}
		return
	}
	t.Fatalf("expected Tokens This Reset row, got %+v", rows)
}

func TestBuildAccountQuotasOmitsWindowTokensWithoutUpstreamBoundary(t *testing.T) {
	rows := buildAccountQuotas(config.Account{
		AuthMethod:                   "codex",
		CodexTokensSincePrimaryReset: 99_999,
	}, 12_345)

	for _, row := range rows {
		if row.Name == "Tokens This Reset" {
			t.Fatalf("unexpected window token row without upstream reset boundary: %+v", row)
		}
	}
}

// liveSubs is the persisted snapshot shape for the account observed running two
// parallel 5-hour plans plus one 7-day plan.
func liveSubs() []config.ExternalSubscription {
	return []config.ExternalSubscription{
		{ID: 196543, PlanID: 5, Status: "active", UsedPercent: 100,
			LastResetTime: 1790586797, NextResetTime: 1791191597, WindowMinutes: 10080},
		{ID: 196758, PlanID: 14, Status: "active", UsedPercent: 90,
			LastResetTime: 1790611262, NextResetTime: 1790629262, WindowMinutes: 300},
		{ID: 186673, PlanID: 13, Status: "active", UsedPercent: 3,
			LastResetTime: 1790613834, NextResetTime: 1790631834, WindowMinutes: 300},
	}
}

func externalAccountWithSubs(subs []config.ExternalSubscription) config.Account {
	return config.Account{
		AuthMethod:       externalAuthMethod,
		AccessToken:      "sk-inference",
		ExtAdminToken:    "admin-token",
		ExtAdminUserID:   12639,
		ExtSubscriptions: subs,
		ExtSubsCheckedAt: 1790614000,
		TotalTokens:      1000,
		RequestCount:     10,
	}
}

// The 5-hour plan is the binding constraint on near-term capacity, so it must
// come first even though the gateway returned the 7-day plan first. One row per
// subscription — collapsing them into a single number would hide that a 90%-used
// 5h plan sits next to a 3%-used one.
func TestBuildAccountQuotasRendersOneRowPerSubscriptionShortestWindowFirst(t *testing.T) {
	rows := buildAccountQuotas(externalAccountWithSubs(liveSubs()), 0)

	var subs []quotaRow
	for _, r := range rows {
		if strings.HasPrefix(r.Name, "Sub #") {
			subs = append(subs, r)
		}
	}
	if len(subs) != 3 {
		t.Fatalf("subscription rows = %d, want 3 (one per active plan): %+v", len(subs), rows)
	}

	wantOrder := []string{"Sub #196758 (5h)", "Sub #186673 (5h)", "Sub #196543 (7d)"}
	for i, want := range wantOrder {
		if subs[i].Name != want {
			t.Errorf("row[%d].Name = %q, want %q (shortest window first, then most-used)", i, subs[i].Name, want)
		}
	}

	// The 5h row that is 90% used must carry the real numbers, not a derived
	// guess: upstream already reports the percentage.
	full := subs[0]
	if full.Used != 90 || full.Total != 100 || full.Remaining != 10 {
		t.Errorf("90%% row = used %.0f total %.0f remaining %d, want 90/100/10", full.Used, full.Total, full.Remaining)
	}
	if !full.Recurring {
		t.Error("a plan window resets periodically, so Recurring must be true")
	}
	if full.Unit != "%" {
		t.Errorf("Unit = %q, want %%", full.Unit)
	}
	if full.ResetAt == nil || *full.ResetAt != 1790629262 {
		t.Errorf("ResetAt = %v, want 1790629262 (next_reset_time)", full.ResetAt)
	}
}

// A 100%-used plan must render as 0% remaining rather than a negative bar.
func TestBuildAccountQuotasClampsExhaustedSubscription(t *testing.T) {
	rows := buildAccountQuotas(externalAccountWithSubs([]config.ExternalSubscription{
		{ID: 1, Status: "active", UsedPercent: 100, WindowMinutes: 300,
			LastResetTime: 100, NextResetTime: 100 + 18000},
	}), 0)

	for _, r := range rows {
		if r.Name != "Sub #1 (5h)" {
			continue
		}
		if r.Remaining != 0 {
			t.Fatalf("Remaining = %d, want 0 for an exhausted plan", r.Remaining)
		}
		return
	}
	t.Fatalf("no subscription row rendered: %+v", rows)
}

// Without evidence of the window length the label must not claim a duration.
// Printing "(5h)" on a plan whose window is unknown would be a wrong statement
// about the account, and the fetcher deliberately leaves WindowMinutes 0 when
// last_reset_time is absent.
func TestBuildAccountQuotasOmitsDurationLabelWithoutWindowEvidence(t *testing.T) {
	rows := buildAccountQuotas(externalAccountWithSubs([]config.ExternalSubscription{
		{ID: 42, Status: "active", UsedPercent: 25, NextResetTime: 1790629262, WindowMinutes: 0},
	}), 0)

	for _, r := range rows {
		if strings.HasPrefix(r.Name, "Sub #42") {
			if r.Name != "Sub #42" {
				t.Fatalf("Name = %q, want bare \"Sub #42\" with no duration claim", r.Name)
			}
			return
		}
	}
	t.Fatalf("no row for sub #42: %+v", rows)
}

// An account with no admin credentials configured has no snapshot and must
// render nothing, so the Quota page stays identical to today for every other
// external provider.
func TestBuildAccountQuotasNoSubscriptionRowsWhenUnconfigured(t *testing.T) {
	rows := buildAccountQuotas(config.Account{
		AuthMethod:  externalAuthMethod,
		AccessToken: "sk-only",
		TotalTokens: 1000,
	}, 0)

	for _, r := range rows {
		if strings.HasPrefix(r.Name, "Sub #") {
			t.Fatalf("unexpected subscription row for an account with no admin token: %+v", r)
		}
	}
}

// Window labels must scale, because one account legitimately runs both 5-hour
// and 7-day plans and a hardcoded "5h" would mislabel the weekly one.
func TestSubscriptionWindowLabel(t *testing.T) {
	for _, tc := range []struct {
		minutes int
		want    string
	}{
		{300, "5h"},
		{60, "1h"},
		{10080, "7d"},
		{1440, "1d"},
		{45, "45m"},
		{0, ""},
	} {
		if got := subscriptionWindowLabel(tc.minutes); got != tc.want {
			t.Errorf("subscriptionWindowLabel(%d) = %q, want %q", tc.minutes, got, tc.want)
		}
	}
}
