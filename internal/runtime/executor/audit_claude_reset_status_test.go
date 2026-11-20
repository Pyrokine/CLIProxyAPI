package executor

import (
	"net/http"
	"testing"
	"time"
)

func TestAuditClaudeMissingStatusResetInferenceRequires429(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
		headers := http.Header{"Anthropic-Ratelimit-Unified-5h-Reset": {time.Now().Add(time.Hour).Format(time.RFC3339)}}
		err := classifyClaudeUpstreamError(
			status, []byte(`{"error":{"message":"synthetic non-rate-limit error"}}`), false, headers,
		)
		retry := err.(interface{ RetryAfter() *time.Duration }).RetryAfter()
		if retry != nil {
			t.Fatalf("status=%d inferred a rate-limit cooldown from ordinary reset metadata: %s", status, *retry)
		}
		headers.Set("Retry-After", "60")
		err = classifyClaudeUpstreamError(status, nil, false, headers)
		retry = err.(interface{ RetryAfter() *time.Duration }).RetryAfter()
		if retry == nil || *retry < 61*time.Second || *retry > 90*time.Second {
			t.Fatalf("status=%d lost explicit Retry-After: %v", status, retry)
		}
	}
}
