package proxy

import (
	"encoding/json"
	"omniproxy/config"
	"testing"
)

// TestGetStatsApiKeyNames verifies the stats payload carries the id→label map
// the UI renders instead of raw key IDs: a named key shows its name, an
// unnamed key falls back to its MASKED value (never the raw key), and the map
// reaches the wire under the exact field name the frontend reads.
func TestGetStatsApiKeyNames(t *testing.T) {
	dir := t.TempDir()
	if err := config.Init(dir + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	named, err := config.AddApiKey(config.ApiKeyEntry{
		Name: "hermes-mac", Key: "sk-named-1234567890abcd", Enabled: true,
	})
	if err != nil {
		t.Fatalf("AddApiKey(named): %v", err)
	}
	unnamed, err := config.AddApiKey(config.ApiKeyEntry{
		Key: "sk-unnamed-9876543210wxyz", Enabled: true,
	})
	if err != nil {
		t.Fatalf("AddApiKey(unnamed): %v", err)
	}

	tracker := &UsageTracker{
		ringCap:    8,
		ring:       make([]RequestRecord, 8),
		activeReqs: make(map[string]ActiveRequest),
		dailyData:  make(map[string]*PeriodSummary),
	}
	stats := tracker.GetStats("24h")

	if stats.ApiKeyNames == nil {
		t.Fatal("ApiKeyNames map is nil")
	}
	if got := stats.ApiKeyNames[named.ID]; got != "hermes-mac" {
		t.Errorf("named key label = %q, want %q", got, "hermes-mac")
	}
	wantMasked := config.MaskApiKey("sk-unnamed-9876543210wxyz")
	if got := stats.ApiKeyNames[unnamed.ID]; got != wantMasked {
		t.Errorf("unnamed key label = %q, want masked %q", got, wantMasked)
	}
	// The raw secret must never appear as a label — the map is rendered
	// straight into the dashboard.
	for id, label := range stats.ApiKeyNames {
		if label == "sk-named-1234567890abcd" || label == "sk-unnamed-9876543210wxyz" {
			t.Errorf("raw key value leaked into ApiKeyNames for id %s", id)
		}
	}

	raw, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal stats: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	if _, ok := wire["apiKeyNames"]; !ok {
		t.Error(`stats JSON has no "apiKeyNames" field`)
	}
	if _, ok := wire["accountNames"]; !ok {
		t.Error(`stats JSON lost the pre-existing "accountNames" field`)
	}
}

// TestApiKeyIdFallback covers the last-resort label for a key entry with
// neither a name nor a key value (only possible via a hand-edited config).
func TestApiKeyIdFallback(t *testing.T) {
	if got := apiKeyIdFallback("0123456789abcdef"); got != "01234567" {
		t.Errorf("apiKeyIdFallback(long) = %q, want 01234567", got)
	}
	if got := apiKeyIdFallback("abc"); got != "abc" {
		t.Errorf("apiKeyIdFallback(short) = %q, want abc", got)
	}
}
