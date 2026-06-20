package proxy

import "testing"

func TestNetworkErrorClassifier(t *testing.T) {
	tests := []struct {
		msg string
		exp bool
	}{
		{msg: "dial tcp 127.0.0.1:8080: connect: connection refused", exp: true},
		{msg: "dial tcp: lookup localhost: no such host", exp: true},
		{msg: "dial tcp 127.0.0.1:8080: i/o timeout", exp: true},
		{msg: "read: connection reset by peer", exp: true},
		{msg: "write: broken pipe", exp: true},
		{msg: "EOF", exp: true},
		{msg: "HTTP 503 from Kiro IDE: upstream unavailable", exp: false},
		{msg: "HTTP 401 from Kiro IDE: unauthorized", exp: false},
		{msg: "HTTP 429: quota exhausted", exp: false},
		{msg: "no available Kiro profile", exp: false},
	}

	for _, tc := range tests {
		got := isNetworkError(tc.msg)
		if got != tc.exp {
			t.Errorf("isNetworkError(%q) = %v, want %v", tc.msg, got, tc.exp)
		}
	}
}

func TestAccountFailureClassifiers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) bool
		msg  string
	}{
		{name: "quota", fn: isQuotaErrorMessage, msg: "HTTP 429: quota exhausted"},
		{name: "overage", fn: isOverageErrorMessage, msg: "HTTP 402 from Kiro IDE: OVERAGE limit exceeded"},
		{name: "suspension", fn: isSuspensionErrorMessage, msg: "Your User ID temporarily is suspended"},
		{name: "profile", fn: isProfileUnavailableErrorMessage, msg: "no available Kiro profile"},
		{name: "auth", fn: isAuthErrorMessage, msg: "Authentication failed - token invalid or expired"},
	}

	for _, tc := range tests {
		if !tc.fn(tc.msg) {
			t.Fatalf("%s classifier did not match %q", tc.name, tc.msg)
		}
	}
}
