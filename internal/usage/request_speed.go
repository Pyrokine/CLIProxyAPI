package usage

import (
	"errors"
	"fmt"
	"strings"
)

var ErrInvalidRequestSpeedFilter = errors.New("usage: invalid request speed filter")

// ParseRequestSpeedFilter accepts one actual speed or an empty all-speeds filter.
func ParseRequestSpeedFilter(value string) (string, error) {
	speed := strings.ToLower(strings.TrimSpace(value))
	switch speed {
	case "", "standard", "fast", "ultrafast", "flex", "batch", "unknown":
		return speed, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidRequestSpeedFilter, value)
	}
}

// ObservedRequestSpeed derives the actual speed from upstream response evidence.
func ObservedRequestSpeed(detail *FlatDetail) (speed string, source string) {
	if detail == nil {
		return "unknown", "unknown"
	}
	raw, source := detail.ResponseServiceTier, "response_service_tier"
	if strings.EqualFold(strings.TrimSpace(detail.Provider), "claude") ||
		strings.EqualFold(strings.TrimSpace(detail.ExecutorType), "ClaudeExecutor") ||
		strings.TrimSpace(detail.RequestSpeed) != "" || strings.TrimSpace(detail.ResponseSpeed) != "" {
		raw, source = detail.ResponseSpeed, "response_speed"
	} else {
		switch tier := strings.TrimSpace(raw); {
		case strings.EqualFold(tier, "default"):
			raw = "standard"
		case strings.EqualFold(tier, "priority"):
			raw = "fast"
		}
	}
	speed, err := ParseRequestSpeedFilter(raw)
	if err != nil || speed == "" || speed == "unknown" {
		return "unknown", "unknown"
	}
	return speed, source
}

func observedRequestSpeedSQL() string {
	// SQLite trim needs the same whitespace characters as strings.TrimSpace.
	const whitespace = " \t\n\v\f\r\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
	normalize := func(expression string) string {
		return "lower(trim(COALESCE(" + expression + ", ''), '" + whitespace + "'))"
	}
	metadataValue := func(path string) string {
		return normalize("CASE WHEN json_valid(events.metadata) THEN json_extract(events.metadata, '$." + path + "') END")
	}
	tier := metadataValue("response_service_tier")
	observed := "CASE WHEN " + normalize("(SELECT name FROM dim_provider WHERE id = events.provider_id)") +
		" = 'claude' OR " + metadataValue("executor_type") + " = 'claudeexecutor' OR " +
		metadataValue("request_speed") + " <> '' OR " + metadataValue("response_speed") + " <> '' THEN " +
		metadataValue("response_speed") + " ELSE CASE " + tier +
		" WHEN 'default' THEN 'standard' WHEN 'priority' THEN 'fast' ELSE " + tier + " END END"
	return "CASE " + observed +
		" WHEN 'standard' THEN 'standard' WHEN 'fast' THEN 'fast' WHEN 'ultrafast' THEN 'ultrafast'" +
		" WHEN 'flex' THEN 'flex' WHEN 'batch' THEN 'batch' ELSE 'unknown' END"
}
