package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestUpdateAdaptiveRoutingNormalizesAndClonesPolicy(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	input := AdaptiveRoutingConfig{
		Enabled:      true,
		VirtualModel: " omni-auto ",
		Objective:    " QUALITY ",
		DefaultRoute: []string{" claude-opus-5 ", "claude-opus-5", "claude-sonnet-5"},
		Routes: map[string][]string{
			" Coding.Strong ": {" claude-opus-5 ", "claude-sonnet-5"},
		},
		Profiles: map[string]AdaptiveModelProfile{
			" claude-opus-5 ": {Quality: 95, LatencyMs: 4200},
		},
	}
	if err := UpdateAdaptiveRouting(input); err != nil {
		t.Fatalf("UpdateAdaptiveRouting: %v", err)
	}

	got := GetAdaptiveRouting()
	if got.VirtualModel != "omni-auto" || got.Objective != "quality" {
		t.Fatalf("normalized virtual model/objective = %q/%q", got.VirtualModel, got.Objective)
	}
	if want := []string{"claude-opus-5", "claude-sonnet-5"}; !reflect.DeepEqual(got.DefaultRoute, want) {
		t.Fatalf("default route = %v, want %v", got.DefaultRoute, want)
	}
	if want := []string{"claude-opus-5", "claude-sonnet-5"}; !reflect.DeepEqual(got.Routes["coding.strong"], want) {
		t.Fatalf("coding.strong route = %v, want %v", got.Routes["coding.strong"], want)
	}
	if _, ok := got.Profiles["claude-opus-5"]; !ok {
		t.Fatalf("normalized profile missing: %#v", got.Profiles)
	}

	// The returned policy must not alias global config storage.
	got.DefaultRoute[0] = "mutated"
	got.Routes["coding.strong"][0] = "mutated"
	delete(got.Profiles, "claude-opus-5")
	again := GetAdaptiveRouting()
	if again.DefaultRoute[0] != "claude-opus-5" || again.Routes["coding.strong"][0] != "claude-opus-5" {
		t.Fatalf("GetAdaptiveRouting returned aliased slices: %#v", again)
	}
	if _, ok := again.Profiles["claude-opus-5"]; !ok {
		t.Fatal("GetAdaptiveRouting returned aliased profiles map")
	}
}

func TestAdaptiveRoutingValidationRejectsUnreachableAndRecursivePolicy(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	for name, policy := range map[string]AdaptiveRoutingConfig{
		"unsupported route key": {
			Enabled: true, Routes: map[string][]string{"coding.extreme": {"claude-opus-5"}},
		},
		"recursive virtual model": {
			Enabled: true, VirtualModel: "router", DefaultRoute: []string{"router"},
		},
		"invalid objective": {
			Enabled: true, Objective: "cheapest", DefaultRoute: []string{"claude-opus-5"},
		},
		"header newline": {
			Enabled: true, DefaultRoute: []string{"claude-opus-5\r\nX-Bad: 1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := UpdateAdaptiveRouting(policy); err == nil {
				t.Fatal("invalid policy was accepted")
			}
		})
	}
}

func TestAdaptiveRoutingIsValidatedAndNormalizedOnLoad(t *testing.T) {
	dir := t.TempDir()
	validPath := filepath.Join(dir, "valid.json")
	valid := `{
		"password":"strong-password-for-test",
		"port":8080,
		"host":"127.0.0.1",
		"accounts":[],
		"adaptiveRouting":{
			"enabled":true,
			"virtualModel":" omni-auto ",
			"objective":" QUALITY ",
			"defaultRoute":[" claude-opus-5 ","claude-opus-5"],
			"routes":{" Coding.Strong ":[" claude-opus-5 "]}
		}
	}`
	if err := os.WriteFile(validPath, []byte(valid), 0o600); err != nil {
		t.Fatalf("write valid config: %v", err)
	}
	if err := Init(validPath); err != nil {
		t.Fatalf("Init valid adaptive config: %v", err)
	}
	got := GetAdaptiveRouting()
	if got.VirtualModel != "omni-auto" || got.Objective != "quality" || len(got.DefaultRoute) != 1 {
		t.Fatalf("loaded policy was not normalized: %#v", got)
	}
	if _, ok := got.Routes["coding.strong"]; !ok {
		t.Fatalf("normalized route missing: %#v", got.Routes)
	}

	invalidPath := filepath.Join(dir, "invalid.json")
	invalid := `{
		"password":"strong-password-for-test",
		"port":8080,
		"host":"127.0.0.1",
		"accounts":[],
		"adaptiveRouting":{"enabled":true,"routes":{"coding.extreme":["claude-opus-5"]}}
	}`
	if err := os.WriteFile(invalidPath, []byte(invalid), 0o600); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	if err := Init(invalidPath); err == nil || !strings.Contains(err.Error(), "invalid adaptiveRouting") {
		t.Fatalf("Init invalid policy error = %v, want adaptive validation failure", err)
	}
}

func TestMatchAdaptiveRoutingOnlyMatchesEnabledVirtualModel(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, ok := MatchAdaptiveRouting("omni-auto"); ok {
		t.Fatal("disabled policy matched")
	}
	if err := UpdateAdaptiveRouting(AdaptiveRoutingConfig{
		Enabled: true, VirtualModel: "router", DefaultRoute: []string{"claude-opus-5"},
	}); err != nil {
		t.Fatalf("UpdateAdaptiveRouting: %v", err)
	}
	if _, ok := MatchAdaptiveRouting("claude-opus-5"); ok {
		t.Fatal("explicit model matched adaptive policy")
	}
	matched, ok := MatchAdaptiveRouting("router")
	if !ok || matched.VirtualModel != "router" {
		t.Fatalf("virtual model did not match: ok=%v policy=%#v", ok, matched)
	}
}
