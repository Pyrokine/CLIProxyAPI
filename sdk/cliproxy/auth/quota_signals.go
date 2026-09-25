package auth

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

const (
	maxQuotaSignalHeaders = 64
	maxQuotaSignalValue   = 512
)

type upstreamResponseHeadersKey struct{}

type upstreamResponseHeadersHolder struct {
	mu      sync.RWMutex
	headers http.Header
}

func newUpstreamAttemptContext(ctx context.Context, stream bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = internallogging.WithResponseHeadersHolder(ctx)
	ctx = coreusage.WithStream(ctx, stream)
	return context.WithValue(ctx, upstreamResponseHeadersKey{}, &upstreamResponseHeadersHolder{})
}

// RecordUpstreamResponseHeaders records response headers for the current
// upstream attempt. Runtime executors call this from their shared response
// logging path so auth state never reads headers from an earlier attempt.
func RecordUpstreamResponseHeaders(ctx context.Context, headers http.Header) {
	if ctx == nil {
		return
	}
	holder, ok := ctx.Value(upstreamResponseHeadersKey{}).(*upstreamResponseHeadersHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	if holder.headers == nil {
		holder.headers = make(http.Header, len(headers))
	}
	for name, values := range headers {
		canonicalName := http.CanonicalHeaderKey(strings.TrimSpace(name))
		if canonicalName == "" {
			continue
		}
		holder.headers[canonicalName] = append([]string(nil), values...)
	}
	holder.mu.Unlock()
}

func upstreamResponseHeaders(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	holder, ok := ctx.Value(upstreamResponseHeadersKey{}).(*upstreamResponseHeadersHolder)
	if !ok || holder == nil {
		return nil
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	return holder.headers.Clone()
}

// ProviderSupportsQuotaObservation reports whether provider emits passive
// credential-level quota watermarks understood by collectQuotaSignals.
func ProviderSupportsQuotaObservation(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "codex", "devin":
		return true
	default:
		return false
	}
}

// ObserveResponseHeadersForProvider replaces the passive quota snapshot with
// signals carried by the current upstream response. A response without quota
// signals leaves the previous snapshot unchanged.
func (q *QuotaState) ObserveResponseHeadersForProvider(
	provider string,
	headers http.Header,
	observedAt time.Time,
) bool {
	if q == nil {
		return false
	}
	if !ProviderSupportsQuotaObservation(provider) {
		return q.ClearObservationSignals()
	}
	next := collectQuotaSignals(provider, headers)
	if len(next) == 0 {
		return false
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	q.Signals = next
	q.ObservedAt = observedAt
	return true
}

// ClearObservationSignals clears passive observations without changing
// cooldown or scheduler state.
func (q *QuotaState) ClearObservationSignals() bool {
	if q == nil || (len(q.Signals) == 0 && q.ObservedAt.IsZero()) {
		return false
	}
	q.Signals = nil
	q.ObservedAt = time.Time{}
	return true
}

// applyCooldownFields updates scheduler state while preserving the latest
// passive observation snapshot.
func applyCooldownFields(dst *QuotaState, cooldown QuotaState) {
	if dst == nil {
		return
	}
	dst.Exceeded = cooldown.Exceeded
	dst.Reason = cooldown.Reason
	dst.Family = cooldown.Family
	dst.NextRecoverAt = cooldown.NextRecoverAt
	dst.BackoffLevel = cooldown.BackoffLevel
	dst.Generation = cooldown.Generation
	dst.ResetAuthority = cooldown.ResetAuthority
}

func collectQuotaSignals(provider string, headers http.Header) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	names := make([]string, 0, len(headers))
	values := make(map[string]string, len(headers))
	for key, headerValues := range headers {
		canonicalKey := http.CanonicalHeaderKey(strings.TrimSpace(key))
		if !isQuotaSignalHeaderForProvider(provider, canonicalKey) || len(headerValues) == 0 {
			continue
		}
		value := strings.TrimSpace(headerValues[len(headerValues)-1])
		if !validQuotaSignalValue(value) {
			continue
		}
		if _, exists := values[canonicalKey]; !exists {
			names = append(names, canonicalKey)
		}
		values[canonicalKey] = value
	}
	if len(names) == 0 {
		return nil
	}
	sort.Slice(names, func(i, j int) bool {
		leftRank := quotaSignalRetentionRank(names[i])
		rightRank := quotaSignalRetentionRank(names[j])
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return names[i] < names[j]
	})
	if len(names) > maxQuotaSignalHeaders {
		names = names[:maxQuotaSignalHeaders]
	}
	signals := make(map[string]string, len(names))
	for _, name := range names {
		signals[name] = values[name]
	}
	return signals
}

func validQuotaSignalValue(value string) bool {
	if value == "" || len(value) > maxQuotaSignalValue {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func quotaSignalRetentionRank(name string) int {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch {
	case lower == "retry-after", strings.HasPrefix(lower, "anthropic-ratelimit-unified-"):
		return 0
	case lower == "x-codex-plan-type", lower == "x-codex-active-limit", strings.HasPrefix(lower, "x-codex-credits-"):
		return 1
	case lower == "x-codex-allowed", lower == "x-codex-limit-reached",
		strings.HasPrefix(lower, "x-codex-primary-"), strings.HasPrefix(lower, "x-codex-secondary-"):
		return 2
	case strings.HasPrefix(lower, "x-codex-code-review-"):
		return 3
	case strings.HasPrefix(lower, "x-codex-additional-"):
		return 5
	case strings.HasPrefix(lower, "x-codex-"):
		return 4
	default:
		return 6
	}
}

func isQuotaSignalHeaderForProvider(provider, name string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "retry-after" {
		return provider == "claude" || provider == "codex"
	}
	if strings.HasPrefix(name, "anthropic-ratelimit-unified-") {
		return provider == "claude"
	}
	if strings.HasPrefix(name, "x-ratelimit-") {
		return provider == "codex"
	}
	if !strings.HasPrefix(name, "x-codex-") || provider != "codex" {
		return false
	}
	if name == "x-codex-active-limit" || name == "x-codex-plan-type" ||
		strings.HasPrefix(name, "x-codex-credits-") {
		return true
	}
	for _, marker := range []string{
		"-allowed",
		"-limit-reached",
		"-limit-name",
		"-used-percent",
		"-window-minutes",
		"-reset-after-seconds",
		"-reset-at",
		"-over-secondary-limit-percent",
	} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}
