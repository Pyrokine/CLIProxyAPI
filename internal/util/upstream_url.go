package util

import (
	"net/url"
	"strings"
)

// SanitizeUpstreamURL retains the observed endpoint while removing URL credentials.
func SanitizeUpstreamURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "ws", "wss":
	default:
		return ""
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return ""
	}
	for key := range query {
		lower := strings.ToLower(key)
		if IsSensitiveFieldName(key) || lower == "code" || lower == "state" ||
			lower == "sig" || strings.Contains(lower, "signature") {
			query[key] = []string{"***"}
		}
	}
	parsed.User = nil
	parsed.Fragment = ""
	parsed.RawFragment = ""
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
