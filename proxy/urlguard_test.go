package proxy

import (
	"net"
	"net/http"
	"strings"
	"testing"
)

// All cases use literal IPs or invalid schemes so no DNS lookup is performed.
func TestValidateOutboundURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		blocked bool
	}{
		{"loopback v4", "http://127.0.0.1/latest", true},
		{"loopback v4 alt", "https://127.1.2.3:8080/x", true},
		{"loopback v6", "http://[::1]:9000/", true},
		{"private 10/8", "http://10.0.0.1/admin", true},
		{"private 172.16/12", "https://172.16.0.1/", true},
		{"private 192.168/16", "http://192.168.1.1/router", true},
		{"link-local metadata", "http://169.254.169.254/latest/meta-data/", true},
		{"link-local v6", "http://[fe80::1]/", true},
		{"cgnat 100.64/10", "http://100.64.0.1/", true},
		{"unspecified v4", "http://0.0.0.0:80/", true},
		{"unspecified v6", "http://[::]/", true},
		{"multicast", "http://224.0.0.1/", true},
		{"v4-mapped loopback", "http://[::ffff:127.0.0.1]/", true},
		{"v4-mapped private", "http://[::ffff:10.0.0.1]/", true},
		{"scheme file", "file:///etc/passwd", true},
		{"scheme gopher", "gopher://127.0.0.1:70/", true},
		{"scheme ftp", "ftp://example.com/x", true},
		{"scheme empty", "", true},
		{"scheme missing", "example.com/path", true},
		{"host missing", "http:///path", true},
		{"unparseable", "http://[::1", true},
		{"public v4", "https://93.184.216.34/", false},
		{"public v4 with port", "http://8.8.8.8:8080/search", false},
		{"public v6", "https://[2606:2800:220:1:248:1893:25c8:1946]/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOutboundURL(tc.raw)
			if tc.blocked {
				if err == nil {
					t.Fatalf("validateOutboundURL(%q) = nil, want error", tc.raw)
				}
				if !isBlockedURLError(err) {
					t.Fatalf("error %T is not *blockedURLError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateOutboundURL(%q) = %v, want nil", tc.raw, err)
			}
		})
	}
}

// Rejection messages must not echo the resolved internal address back to the
// client; those go to the log only.
func TestValidateOutboundURLDoesNotLeakResolvedAddress(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1/", "http://169.254.169.254/", "http://10.1.2.3/"} {
		err := validateOutboundURL(raw)
		if err == nil {
			t.Fatalf("validateOutboundURL(%q) = nil, want error", raw)
		}
		for _, leak := range []string{"127.0.0.1", "169.254.169.254", "10.1.2.3"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("error %q leaks resolved address %q", err.Error(), leak)
			}
		}
	}
}

func TestAllowPrivateOutboundToggle(t *testing.T) {
	original := allowPrivateOutbound
	allowPrivateOutbound = true
	t.Cleanup(func() { allowPrivateOutbound = original })

	for _, raw := range []string{"http://127.0.0.1:11434/api", "http://192.168.1.50:4000/v1"} {
		if err := validateOutboundURL(raw); err != nil {
			t.Fatalf("with allowPrivateOutbound, validateOutboundURL(%q) = %v, want nil", raw, err)
		}
	}
	// Metadata and unspecified addresses stay blocked even when private is allowed.
	for _, raw := range []string{"http://169.254.169.254/", "http://0.0.0.0/"} {
		if err := validateOutboundURL(raw); err == nil {
			t.Fatalf("with allowPrivateOutbound, validateOutboundURL(%q) = nil, want error", raw)
		}
	}
}

func TestIsBlockedIPNil(t *testing.T) {
	if !isBlockedIP(nil) {
		t.Fatal("isBlockedIP(nil) = false, want true")
	}
	if isBlockedIP(net.ParseIP("1.1.1.1")) {
		t.Fatal("isBlockedIP(1.1.1.1) = true, want false")
	}
}

// A blocked client URL must surface as 400, not 500/502.
func TestServiceErrorStatusBlockedURL(t *testing.T) {
	err := validateOutboundURL("http://169.254.169.254/")
	if err == nil {
		t.Fatal("expected blocked url error")
	}
	if got := serviceErrorStatus(err); got != http.StatusBadRequest {
		t.Fatalf("serviceErrorStatus = %d, want %d", got, http.StatusBadRequest)
	}
}

func containsSubstring(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
