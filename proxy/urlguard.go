package proxy

import (
	"net"
	"net/url"
	"omniproxy/logger"
	"strings"
)

// allowPrivateOutbound re-enables loopback/RFC1918/CGNAT outbound targets for
// self-hosted gateways on a trusted network. Link-local (cloud metadata),
// unspecified and multicast addresses stay blocked regardless.
var allowPrivateOutbound bool

var cgnatRange = func() *net.IPNet {
	_, network, _ := net.ParseCIDR("100.64.0.0/10")
	return network
}()

// blockedURLError carries a client-safe reason; resolved addresses are logged
// rather than returned so internal topology is not echoed back.
type blockedURLError struct{ reason string }

func (e *blockedURLError) Error() string { return e.reason }

func isBlockedURLError(err error) bool {
	_, ok := err.(*blockedURLError)
	return ok
}

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	if allowPrivateOutbound {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && cgnatRange.Contains(v4) {
		return true
	}
	return false
}

func validateOutboundURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return &blockedURLError{reason: "url is not a valid http(s) URL"}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return &blockedURLError{reason: "url scheme must be http or https"}
	}
	host := u.Hostname()
	if host == "" {
		return &blockedURLError{reason: "url is missing a host"}
	}
	if ip := net.ParseIP(host); ip != nil {
		return checkResolved(host, []net.IP{ip})
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		logger.Warnf("[SSRF] dns lookup failed for host=%s: %v", host, err)
		return &blockedURLError{reason: "url host could not be resolved"}
	}
	if len(addrs) == 0 {
		return &blockedURLError{reason: "url host could not be resolved"}
	}
	return checkResolved(host, addrs)
}

func checkResolved(host string, addrs []net.IP) error {
	for _, ip := range addrs {
		if isBlockedIP(ip) {
			logger.Warnf("[SSRF] blocked outbound url host=%s resolved=%s", host, ip)
			return &blockedURLError{reason: "url resolves to a disallowed network address"}
		}
	}
	return nil
}
