package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// validateUpstreamURL enforces the transport policy for every outbound call:
//
//  1. scheme is https, or http only for an explicit loopback fixture host,
//  2. no userinfo and no fragment,
//  3. the host is not a bare IP literal unless it is that loopback fixture,
//  4. the effective host matches the configured allow-list exactly or as a subdomain.
//
// The parsed URL is returned so callers never re-parse attacker-influenced text.
func validateUpstreamURL(rawURL string, allowedHosts []string) (*url.URL, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil, newPluginError(codeHostCallFailed, "upstream URL is empty", 0)
	}
	parsed, errParse := url.Parse(trimmed)
	if errParse != nil {
		return nil, newPluginError(codeHostCallFailed, "upstream URL cannot be parsed", 0)
	}
	if parsed.User != nil {
		return nil, newPluginError(codeHostCallFailed, "upstream URL must not embed credentials", 0)
	}
	if parsed.Fragment != "" {
		return nil, newPluginError(codeHostCallFailed, "upstream URL must not carry a fragment", 0)
	}

	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" {
		return nil, newPluginError(codeHostCallFailed, "upstream URL has no host", 0)
	}
	loopback := isLoopbackHost(host)

	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		if !loopback {
			return nil, newPluginError(codeHostCallFailed,
				fmt.Sprintf("plain http is rejected for non-loopback host %q", host), 0)
		}
	default:
		return nil, newPluginError(codeHostCallFailed,
			fmt.Sprintf("unsupported upstream scheme %q", parsed.Scheme), 0)
	}

	if net.ParseIP(host) != nil && !loopback {
		return nil, newPluginError(codeHostCallFailed,
			"upstream host must be a DNS name from the allow-list, not an IP literal", 0)
	}
	if !hostAllowed(host, allowedHosts) {
		return nil, newPluginError(codeHostCallFailed,
			fmt.Sprintf("upstream host %q is not in the Entire allow-list", host), 0)
	}
	return parsed, nil
}

// isLoopbackHost reports whether host is a loopback literal or localhost.
func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// apiURL joins a base origin with the shared API version and a route path.
func apiURL(base, route string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, apiVersion) {
		return base + route
	}
	return base + apiVersion + route
}
