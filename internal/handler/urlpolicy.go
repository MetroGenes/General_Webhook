package handler

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
)

// ValidateURLDestination validates a public destination. Private addresses
// require the explicit allow_private action option and a non-empty allowlist.
func ValidateURLDestination(raw string, allowlist []string) error {
	return ValidateURLDestinationWithPolicy(raw, allowlist, false)
}

// ValidateURLDestinationWithPolicy enforces a structural scheme/host/path
// allowlist. It intentionally rejects URL userinfo and string-prefix matching.
func ValidateURLDestinationWithPolicy(raw string, allowlist []string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid url")
	}
	if u.User != nil {
		return fmt.Errorf("url userinfo is not allowed")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("url scheme %q not allowed", scheme)
	}
	host := normalizeHostname(u.Hostname())
	if host == "" {
		return fmt.Errorf("invalid url host")
	}
	if isBlockedHostname(host) {
		return fmt.Errorf("destination host blocked: %s", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := validateDestinationIP(ip, allowPrivate && len(allowlist) > 0); err != nil {
			return err
		}
	}

	if len(allowlist) == 0 {
		if scheme != "https" {
			return fmt.Errorf("url scheme %q not allowed without allowlist (https only)", scheme)
		}
		return nil
	}

	for _, entry := range allowlist {
		if matchAllowlistEntry(u, strings.TrimSpace(entry)) {
			return nil
		}
	}
	return fmt.Errorf("destination not in url_allowlist")
}

func matchAllowlistEntry(u *url.URL, entry string) bool {
	if entry == "" {
		return false
	}
	if _, network, err := net.ParseCIDR(entry); err == nil {
		ip := net.ParseIP(u.Hostname())
		return ip != nil && network.Contains(ip)
	}

	if strings.Contains(entry, "://") {
		allowed, err := url.Parse(entry)
		if err != nil || allowed.Scheme == "" || allowed.Host == "" || allowed.User != nil || allowed.RawQuery != "" || allowed.Fragment != "" {
			return false
		}
		if !strings.EqualFold(u.Scheme, allowed.Scheme) || normalizeHostname(u.Hostname()) != normalizeHostname(allowed.Hostname()) {
			return false
		}
		if effectivePort(u) != effectivePort(allowed) {
			return false
		}
		return pathPrefixMatch(u.EscapedPath(), allowed.EscapedPath())
	}

	// Exact host or host:port. A host entry permits either http or https; the
	// caller has intentionally listed it.
	if strings.EqualFold(u.Host, entry) {
		return true
	}
	return !strings.Contains(entry, ":") && normalizeHostname(u.Hostname()) == normalizeHostname(entry) && u.Port() == ""
}

func pathPrefixMatch(got, prefix string) bool {
	if prefix == "" || prefix == "/" {
		return true
	}
	got = path.Clean("/" + strings.TrimPrefix(got, "/"))
	prefix = path.Clean("/" + strings.TrimPrefix(prefix, "/"))
	return got == prefix || strings.HasPrefix(got, strings.TrimSuffix(prefix, "/")+"/")
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

func normalizeHostname(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func isBlockedHostname(host string) bool {
	h := normalizeHostname(host)
	switch h {
	case "localhost", "metadata.google.internal", "metadata.azure.internal":
		return true
	}
	return strings.HasSuffix(h, ".localhost")
}

func validateDestinationIP(ip net.IP, allowPrivate bool) error {
	if ip == nil {
		return fmt.Errorf("invalid destination ip")
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("destination ip blocked: %s", ip.String())
	}
	if ip.IsPrivate() && !allowPrivate {
		return fmt.Errorf("private destination ip requires allow_private: %s", ip.String())
	}
	return nil
}
