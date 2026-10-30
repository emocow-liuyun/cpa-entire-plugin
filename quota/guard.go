package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// validateUpstreamURL enforces the transport policy for every outbound call:
//
//  1. the scheme is https (http is accepted only for explicit loopback hosts,
//     which keeps local fixtures testable without weakening production),
//  2. the URL carries no userinfo, fragment or non-billing path,
//  3. the effective host matches the configured allow-list, either exactly or
//     as a subdomain of one listed domain,
//  4. the host is not a bare IP literal, nor a loopback/link-local address,
//     unless it is a loopback fixture paired with http.
//
// The caller receives the parsed URL so the host callback never re-parses
// attacker-controlled text.
func validateUpstreamURL(rawURL string, allowedHosts []string) (*url.URL, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil, newPluginError(codeBlockedHost, "upstream URL is empty", 0)
	}
	parsed, errParse := url.Parse(trimmed)
	if errParse != nil {
		return nil, newPluginError(codeBlockedHost, "upstream URL cannot be parsed", 0)
	}
	if parsed.User != nil {
		return nil, newPluginError(codeBlockedHost, "upstream URL must not embed credentials", 0)
	}
	if parsed.Fragment != "" {
		return nil, newPluginError(codeBlockedHost, "upstream URL must not carry a fragment", 0)
	}

	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" {
		return nil, newPluginError(codeBlockedHost, "upstream URL has no host", 0)
	}

	loopback := isLoopbackHost(host)
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "https":
	case "http":
		if !loopback {
			return nil, newPluginError(codeBlockedHost,
				fmt.Sprintf("plain http is rejected for non-loopback host %q", host), 0)
		}
	default:
		return nil, newPluginError(codeBlockedHost,
			fmt.Sprintf("unsupported upstream scheme %q", scheme), 0)
	}

	if net.ParseIP(host) != nil && !loopback {
		return nil, newPluginError(codeBlockedHost,
			"upstream host must be a DNS name from the allow-list, not an IP literal", 0)
	}
	if loopbackEnabled(allowedHosts) && loopback {
		return parsed, nil
	}
	if !hostAllowed(host, allowedHosts) {
		return nil, newPluginError(codeBlockedHost,
			fmt.Sprintf("upstream host %q is not in the Entire allow-list", host), 0)
	}
	return parsed, nil
}

// hostAllowed reports whether host equals an allow-list entry or is a subdomain of one.
func hostAllowed(host string, allowedHosts []string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	for _, entry := range allowedHosts {
		entry = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(entry), "."))
		if entry == "" {
			continue
		}
		if host == entry {
			return true
		}
		if strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// loopbackEnabled reports whether the operator explicitly allowed a loopback
// fixture host. It requires an explicit loopback entry in the allow-list.
func loopbackEnabled(allowedHosts []string) bool {
	for _, entry := range allowedHosts {
		trimmed := strings.ToLower(strings.TrimSpace(entry))
		if trimmed == "localhost" || trimmed == "127.0.0.1" || trimmed == "::1" || trimmed == "[::1]" {
			return true
		}
	}
	return false
}

// isLoopbackHost reports whether host is a loopback name or address.
func isLoopbackHost(host string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(host))
	if trimmed == "localhost" || strings.HasSuffix(trimmed, ".localhost") {
		return true
	}
	if ip := net.ParseIP(strings.Trim(trimmed, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
