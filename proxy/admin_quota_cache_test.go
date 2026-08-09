package proxy

import (
	"omniproxy/config"
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
