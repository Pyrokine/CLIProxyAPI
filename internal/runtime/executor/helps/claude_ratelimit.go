package helps

import (
	cryptorand "crypto/rand"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultClaudeRateLimitFuzzMinSeconds = 1
	defaultClaudeRateLimitFuzzMaxSeconds = 30
	maxClaudeRateLimitWindow             = 7*24*time.Hour + time.Hour
)

// ClaudeHeadersIndicateUnifiedRateLimitRejection reports whether Anthropic
// rejected a shared 5-hour, 7-day, or aggregate window rather than only overage.
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
	return !isClaudeOverageOnlyRejection(headers, unifiedStatus, status5h, status7d, status7dOI)
}

func isClaudeRateLimitWindowAllowed(status string) bool {
	return status == "allowed" || status == "allowed_warning"
}

func isClaudeOverageOnlyRejection(headers http.Header, unifiedStatus, status5h, status7d, status7dOI string) bool {
	if status5h == "rejected" || status7d == "rejected" {
		return false
	}
	overageStatus := claudeRateLimitHeaderStatus(headers, "Anthropic-Ratelimit-Unified-Overage-Status")
	disabledReason := strings.TrimSpace(getClaudeHeader(headers, "Anthropic-Ratelimit-Unified-Overage-Disabled-Reason"))
	claim := claudeRateLimitHeaderStatus(headers, "Anthropic-Ratelimit-Unified-Representative-Claim")
	if status7dOI != "rejected" && overageStatus != "rejected" && disabledReason == "" && !strings.Contains(claim, "overage") {
		return false
	}
	if isClaudeRateLimitWindowAllowed(status5h) && isClaudeRateLimitWindowAllowed(status7d) {
		return true
	}
	if (status5h == "" && isClaudeRateLimitWindowAllowed(status7d) &&
		isClaudeRateLimitUtilizationHealthy(getClaudeHeader(headers, "Anthropic-Ratelimit-Unified-5h-Utilization"))) ||
		(status7d == "" && isClaudeRateLimitWindowAllowed(status5h) &&
			isClaudeRateLimitUtilizationHealthy(getClaudeHeader(headers, "Anthropic-Ratelimit-Unified-7d-Utilization"))) {
		return true
	}
	if unifiedStatus == "rejected" && status5h == "" && status7d == "" && strings.Contains(claim, "overage") {
		util5h := strings.TrimSpace(getClaudeHeader(headers, "Anthropic-Ratelimit-Unified-5h-Utilization"))
		util7d := strings.TrimSpace(getClaudeHeader(headers, "Anthropic-Ratelimit-Unified-7d-Utilization"))
		return (util5h == "" || isClaudeRateLimitUtilizationHealthy(util5h)) &&
			(util7d == "" || isClaudeRateLimitUtilizationHealthy(util7d))
	}
	return false
}

func isClaudeRateLimitUtilizationHealthy(raw string) bool {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	return err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value < 1
}

// ParseClaudeRateLimitReset returns the latest applicable future reset plus a
// bounded grace period. It ignores reset headers for windows that are not
// rejected and avoids shared cooldowns for overage-only limits.
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
	overageOnly := isClaudeOverageOnlyRejection(headers, unifiedStatus, status5h, status7d, status7dOI)

	deadlines := make([]time.Time, 0, 5)
	if !overageOnly {
		if deadline, ok := parseClaudeRetryAfter(getClaudeHeader(headers, "Retry-After"), now); ok && deadline.After(now) {
			deadlines = append(deadlines, deadline)
		}
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
	if status7dOI == "rejected" && !overageOnly {
		if deadline, ok := parseClaudeReset(getClaudeHeader(headers, "Anthropic-Ratelimit-Unified-7d_oi-Reset")); ok && deadline.After(now) {
			deadlines = append(deadlines, deadline)
		}
	}
	sharedRejected := !overageOnly &&
		(unifiedStatus == "rejected" || status5h == "rejected" || status7d == "rejected")
	if sharedRejected {
		unifiedReset := getClaudeHeader(headers, "Anthropic-Ratelimit-Unified-Reset")
		deadline, valid := parseClaudeReset(unifiedReset)
		overageReset, validOverage := parseClaudeReset(getClaudeHeader(headers, "Anthropic-Ratelimit-Unified-Overage-Reset"))
		billingBoundary := strings.Contains(claudeRateLimitHeaderStatus(headers, "Anthropic-Ratelimit-Unified-Representative-Claim"), "overage") &&
			valid && validOverage && deadline.Equal(overageReset)
		if valid && deadline.After(now) && !billingBoundary {
			deadlines = append(deadlines, deadline)
		}
	}
	kept := deadlines[:0]
	for _, deadline := range deadlines {
		if delay := deadline.Sub(now); delay > 0 && delay <= maxClaudeRateLimitWindow {
			kept = append(kept, deadline)
		}
	}
	deadlines = kept
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
