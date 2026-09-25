package helps

import (
	cryptorand "crypto/rand"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultClaudeRateLimitFuzzMinSeconds = 1
	defaultClaudeRateLimitFuzzMaxSeconds = 30
)

// ClaudeHeadersIndicateUnifiedRateLimitRejection reports whether Anthropic
// rejected a shared 5-hour, 7-day, or aggregate unified rate-limit window.
func ClaudeHeadersIndicateUnifiedRateLimitRejection(headers http.Header) bool {
	if headers == nil {
		return false
	}
	unifiedStatus := claudeRateLimitHeaderStatus(headers, "Anthropic-Ratelimit-Unified-Status")
	status5h := claudeRateLimitHeaderStatus(headers, "Anthropic-Ratelimit-Unified-5h-Status")
	if status5h == "rejected" {
		return true
	}
	status7d := claudeRateLimitHeaderStatus(headers, "Anthropic-Ratelimit-Unified-7d-Status")
	if status7d == "rejected" {
		return true
	}
	if unifiedStatus != "rejected" {
		return false
	}
	status7dOI := claudeRateLimitHeaderStatus(headers, "Anthropic-Ratelimit-Unified-7d_oi-Status")
	return !isClaudeFableOnlyRejection(status5h, status7d, status7dOI)
}

func isClaudeRateLimitWindowAllowed(status string) bool {
	return status == "allowed" || status == "allowed_warning"
}

func isClaudeFableOnlyRejection(status5h, status7d, status7dOI string) bool {
	return isClaudeRateLimitWindowAllowed(status5h) &&
		isClaudeRateLimitWindowAllowed(status7d) &&
		status7dOI == "rejected"
}

// ParseClaudeRateLimitReset returns the latest applicable future reset plus a
// bounded grace period. It ignores reset headers for windows that are not
// rejected and avoids applying week-long shared cooldowns to Fable-only limits.
func ParseClaudeRateLimitReset(headers http.Header, now time.Time) *time.Duration {
	return parseClaudeRateLimitResetWithFuzz(
		headers,
		now,
		defaultClaudeRateLimitFuzzMinSeconds,
		defaultClaudeRateLimitFuzzMaxSeconds,
	)
}

func parseClaudeRateLimitResetWithFuzz(
	headers http.Header,
	now time.Time,
	minFuzzSeconds,
	maxFuzzSeconds int,
) *time.Duration {
	if headers == nil {
		return nil
	}

	unifiedStatus := claudeRateLimitHeaderStatus(headers, "Anthropic-Ratelimit-Unified-Status")
	status5h := claudeRateLimitHeaderStatus(headers, "Anthropic-Ratelimit-Unified-5h-Status")
	status7d := claudeRateLimitHeaderStatus(headers, "Anthropic-Ratelimit-Unified-7d-Status")
	status7dOI := claudeRateLimitHeaderStatus(headers, "Anthropic-Ratelimit-Unified-7d_oi-Status")
	fableOnly := isClaudeFableOnlyRejection(status5h, status7d, status7dOI)

	deadlines := make([]time.Time, 0, 5)
	if deadline, ok := parseClaudeRetryAfter(getClaudeHeader(headers, "Retry-After"), now); ok && deadline.After(now) {
		deadlines = append(deadlines, deadline)
	}
	if status5h == "rejected" {
		if deadline, ok := parseClaudeReset(getClaudeHeader(headers, "Anthropic-Ratelimit-Unified-5h-Reset")); ok && deadline.After(now) {
			deadlines = append(deadlines, deadline)
		}
	}
	if status7d == "rejected" {
		if deadline, ok := parseClaudeReset(getClaudeHeader(headers, "Anthropic-Ratelimit-Unified-7d-Reset")); ok && deadline.After(now) {
			deadlines = append(deadlines, deadline)
		}
	}
	if status7dOI == "rejected" && !fableOnly {
		if deadline, ok := parseClaudeReset(getClaudeHeader(headers, "Anthropic-Ratelimit-Unified-7d_oi-Reset")); ok && deadline.After(now) {
			deadlines = append(deadlines, deadline)
		}
	}
	sharedRejected := !fableOnly &&
		(unifiedStatus == "rejected" || status5h == "rejected" || status7d == "rejected")
	if sharedRejected {
		if deadline, ok := parseClaudeReset(getClaudeHeader(headers, "Anthropic-Ratelimit-Unified-Reset")); ok && deadline.After(now) {
			deadlines = append(deadlines, deadline)
		}
	}
	if len(deadlines) == 0 {
		return nil
	}

	latest := deadlines[0]
	for _, deadline := range deadlines[1:] {
		if deadline.After(latest) {
			latest = deadline
		}
	}
	delay := latest.Sub(now)
	if delay <= 0 {
		return nil
	}
	delay += randomClaudeRateLimitFuzz(minFuzzSeconds, maxFuzzSeconds)
	return &delay
}

func claudeRateLimitHeaderStatus(headers http.Header, name string) string {
	return strings.ToLower(strings.TrimSpace(getClaudeHeader(headers, name)))
}

func getClaudeHeader(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func parseClaudeReset(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil && seconds > 0 {
		wholeSeconds := int64(seconds)
		nanoseconds := int64((seconds - float64(wholeSeconds)) * float64(time.Second))
		return time.Unix(wholeSeconds, nanoseconds), true
	}
	if deadline, err := time.Parse(time.RFC3339, raw); err == nil {
		return deadline, true
	}
	if deadline, err := http.ParseTime(raw); err == nil {
		return deadline, true
	}
	return time.Time{}, false
}

func parseClaudeRetryAfter(raw string, now time.Time) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil && seconds > 0 {
		return now.Add(time.Duration(seconds * float64(time.Second))), true
	}
	if deadline, err := http.ParseTime(raw); err == nil {
		return deadline, true
	}
	if deadline, err := time.Parse(time.RFC3339, raw); err == nil {
		return deadline, true
	}
	return time.Time{}, false
}

func randomClaudeRateLimitFuzz(minSeconds, maxSeconds int) time.Duration {
	if minSeconds < 0 {
		minSeconds = 0
	}
	if maxSeconds <= minSeconds {
		return time.Duration(minSeconds) * time.Second
	}
	random, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(maxSeconds-minSeconds+1)))
	if err != nil {
		return time.Duration(minSeconds) * time.Second
	}
	return time.Duration(minSeconds+int(random.Int64())) * time.Second
}
