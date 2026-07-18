package proxy

import (
	"strings"
	"omniproxy/config"
	"testing"
)

func TestBuildStreamingHeaderValuesAlignsWithKiroIDEFormat(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := &config.Account{RefreshToken: "refresh-token-test"}
	values := buildStreamingHeaderValues(account, "q.us-east-1.amazonaws.com")

	const expectedMachineID = "d3d96a75e6746685699d9be56622a81c"
	if got := machineIdFromAccount(account); got != expectedMachineID {
		t.Fatalf("credential-derived machine id = %q, want %q", got, expectedMachineID)
	}
	if values.Host != "q.us-east-1.amazonaws.com" {
		t.Fatalf("expected host to be preserved, got %q", values.Host)
	}
	if !strings.Contains(values.UserAgent, "aws-sdk-js/1.0.34") {
		t.Fatalf("expected streaming sdk version in user agent, got %q", values.UserAgent)
	}
	if !strings.Contains(values.UserAgent, "api/codewhispererstreaming#1.0.34") {
		t.Fatalf("expected streaming API marker in user agent, got %q", values.UserAgent)
	}
	const expectedIdentity = "KiroIDE-0.11.107-" + expectedMachineID
	if !strings.Contains(values.UserAgent, expectedIdentity) {
		t.Fatalf("expected fixed Kiro identity %q in user agent, got %q", expectedIdentity, values.UserAgent)
	}
	if !strings.Contains(values.AmzUserAgent, "aws-sdk-js/1.0.34 "+expectedIdentity) {
		t.Fatalf("expected x-amz-user-agent to include fixed version and machine id, got %q", values.AmzUserAgent)
	}
}

func TestBuildRuntimeHeaderValuesUsesRuntimeAPIFormat(t *testing.T) {
	account := &config.Account{MachineId: "machine-456"}
	values := buildRuntimeHeaderValues(account, "codewhisperer.us-east-1.amazonaws.com")

	if !strings.Contains(values.UserAgent, "aws-sdk-js/1.0.0") {
		t.Fatalf("expected runtime sdk version in user agent, got %q", values.UserAgent)
	}
	if !strings.Contains(values.UserAgent, "api/codewhispererruntime#1.0.0") {
		t.Fatalf("expected runtime API marker in user agent, got %q", values.UserAgent)
	}
	if !strings.Contains(values.UserAgent, "m/N,E") {
		t.Fatalf("expected runtime mode marker in user agent, got %q", values.UserAgent)
	}
}
