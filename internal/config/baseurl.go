package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// DefaultBaseURL is the production API origin.
const DefaultBaseURL = "https://api.apipod.ai"

// defaultConsoleURL is where users manage billing for the production API.
const defaultConsoleURL = "https://www.apipod.ai"

// NormalizeBaseURL validates a base URL and reduces it to scheme://host[:port].
//
// Credentials are keyed by the result, so a different host never sees a token
// issued for another. Plain http is accepted only for loopback hosts: anything
// else would put bearer tokens on the wire in clear text.
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultBaseURL, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid base URL %q: %w", raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if u.Host == "" || (scheme != "https" && scheme != "http") {
		return "", fmt.Errorf("invalid base URL %q: must be an absolute https:// URL", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("invalid base URL %q: must not contain credentials", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid base URL %q: must not contain a query or fragment", raw)
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		return "", fmt.Errorf("invalid base URL %q: must not contain a path", raw)
	}
	if scheme == "http" && !IsLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("invalid base URL %q: plain http is only allowed for 127.0.0.1, ::1 or localhost", raw)
	}
	return scheme + "://" + strings.ToLower(u.Host), nil
}

// IsLoopbackHost reports whether host is a loopback IP literal or "localhost".
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ConsoleURL returns the web console origin for a base URL. The production API
// lives on a separate host from the console; self-hosted and local setups
// serve both from one origin unless APIPOD_CONSOLE_URL says otherwise.
func ConsoleURL(baseURL, override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return NormalizeBaseURL(override)
	}
	if baseURL == DefaultBaseURL {
		return defaultConsoleURL, nil
	}
	return baseURL, nil
}
