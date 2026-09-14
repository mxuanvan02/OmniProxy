package proxy

import (
	"errors"
	"testing"
)

func TestTerminalRequestErrorPolicy(t *testing.T) {
	for _, tc := range []struct {
		message  string
		terminal bool
	}{
		{"HTTP 400 invalid_request_error", true},
		{"HTTP 413 payload too large", true},
		{"HTTP 422 invalid input", true},
		{"context_length_exceeded", true},
		{"HTTP 401 unauthorized", false},
		{"HTTP 429 rate limit", false},
		{"HTTP 503 unavailable", false},
		{"connection refused", false},
	} {
		t.Run(tc.message, func(t *testing.T) {
			if got := isTerminalRequestError(errors.New(tc.message)); got != tc.terminal {
				t.Fatalf("terminal = %v, want %v", got, tc.terminal)
			}
		})
	}
}

func TestConcreteModelIdentity(t *testing.T) {
	for _, model := range []string{"gpt-4o", "gpt-4o-mini", "gpt-4-turbo", "claude-3-opus", "claude-sonnet-4-20250514"} {
		t.Run(model, func(t *testing.T) {
			got, thinking := ParseModelAndThinking(model+"-thinking", "-thinking")
			if got != model || !thinking {
				t.Fatalf("got (%q, %v), want (%q, true)", got, thinking, model)
			}
		})
	}
}
