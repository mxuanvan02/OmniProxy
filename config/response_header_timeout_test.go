package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A gateway that buffers the whole completion before answering (apikey.click,
// measured 2026-09-10: headers at 63-66s for a reasoning model) must not be cut
// off by the response-header deadline. The wait is therefore configurable and
// defaults well above single-model inference latency.

func TestGetResponseHeaderTimeoutDefault(t *testing.T) {
	prev, had := os.LookupEnv("RESPONSE_HEADER_TIMEOUT_SECONDS")
	os.Unsetenv("RESPONSE_HEADER_TIMEOUT_SECONDS")
	defer func() {
		if had {
			os.Setenv("RESPONSE_HEADER_TIMEOUT_SECONDS", prev)
		}
	}()

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"accounts":[]}`), 0600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}
	if err := Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if got := GetResponseHeaderTimeout(); got != time.Duration(defaultResponseHeaderTimeoutSecs)*time.Second {
		t.Fatalf("default response header timeout = %s, want %ds", got, defaultResponseHeaderTimeoutSecs)
	}
	if defaultResponseHeaderTimeoutSecs < 120 {
		t.Fatalf("default response header timeout = %ds, too tight for buffering gateways", defaultResponseHeaderTimeoutSecs)
	}

	persisted, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	var got struct {
		ResponseHeaderTimeoutSeconds int `json:"responseHeaderTimeoutSeconds"`
	}
	if err := json.Unmarshal(persisted, &got); err != nil {
		t.Fatalf("decode migrated config: %v", err)
	}
	if got.ResponseHeaderTimeoutSeconds != defaultResponseHeaderTimeoutSecs {
		t.Fatalf("persisted response header timeout = %d, want %d", got.ResponseHeaderTimeoutSeconds, defaultResponseHeaderTimeoutSecs)
	}
}

func TestGetResponseHeaderTimeoutEnvOverride(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"accounts":[],"responseHeaderTimeoutSeconds":300}`), 0600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}
	if err := Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}

	prev, had := os.LookupEnv("RESPONSE_HEADER_TIMEOUT_SECONDS")
	os.Setenv("RESPONSE_HEADER_TIMEOUT_SECONDS", "90")
	defer func() {
		if had {
			os.Setenv("RESPONSE_HEADER_TIMEOUT_SECONDS", prev)
		} else {
			os.Unsetenv("RESPONSE_HEADER_TIMEOUT_SECONDS")
		}
	}()

	if got := GetResponseHeaderTimeout(); got != 90*time.Second {
		t.Fatalf("env override = %s, want 90s", got)
	}
}

func TestExplicitResponseHeaderTimeoutIsPreserved(t *testing.T) {
	prev, had := os.LookupEnv("RESPONSE_HEADER_TIMEOUT_SECONDS")
	os.Unsetenv("RESPONSE_HEADER_TIMEOUT_SECONDS")
	defer func() {
		if had {
			os.Setenv("RESPONSE_HEADER_TIMEOUT_SECONDS", prev)
		}
	}()

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"accounts":[],"responseHeaderTimeoutSeconds":600}`), 0600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}
	if err := Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if got := GetResponseHeaderTimeout(); got != 600*time.Second {
		t.Fatalf("explicit response header timeout = %s, want 600s", got)
	}
}

func TestDisabledResponseHeaderTimeoutIsPreserved(t *testing.T) {
	prev, had := os.LookupEnv("RESPONSE_HEADER_TIMEOUT_SECONDS")
	os.Unsetenv("RESPONSE_HEADER_TIMEOUT_SECONDS")
	defer func() {
		if had {
			os.Setenv("RESPONSE_HEADER_TIMEOUT_SECONDS", prev)
		}
	}()

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"accounts":[],"responseHeaderTimeoutSeconds":0}`), 0600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}
	if err := Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	// 0 disables the transport-level deadline; the overall client timeout and
	// the stream idle reader remain in force.
	if got := GetResponseHeaderTimeout(); got != 0 {
		t.Fatalf("explicit disabled response header timeout = %s, want 0", got)
	}
}
