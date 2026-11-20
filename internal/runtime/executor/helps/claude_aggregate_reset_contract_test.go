package helps

import (
	"net/http"
	"testing"
	"time"
)

func TestClaudeAggregateResetPreservesOriginalWindowInputs(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    time.Duration
	}{
		{
			"independent-oi-with-allowed-aggregate", http.Header{
				"Anthropic-Ratelimit-Unified-Status":       {"allowed"},
				"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
			}, 20 * time.Minute,
		},
		{
			"independent-oi-with-unknown-aggregate", http.Header{
				"Anthropic-Ratelimit-Unified-Status":       {"unknown"},
				"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
			}, 20 * time.Minute,
		},
		{
			"both-shared-allowed", http.Header{
				"Anthropic-Ratelimit-Unified-5h-Status": {"allowed"},
				"Anthropic-Ratelimit-Unified-7d-Status": {"allowed_warning"},
			}, 0,
		},
		{"five-hour-allowed", http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"allowed"}}, 0},
		{"seven-day-allowed", http.Header{"Anthropic-Ratelimit-Unified-7d-Status": {"allowed_warning"}}, 0},
		{"legacy-missing-status", http.Header{}, 20 * time.Minute},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				tc.headers.Set("Anthropic-Ratelimit-Unified-Reset", now.Add(20*time.Minute).Format(time.RFC3339))
				for _, status := range [][]int{nil, {http.StatusTooManyRequests}} {
					got := parseClaudeRateLimitResetWithFuzz(tc.headers, now, 0, 0, status...)
					if tc.want == 0 {
						if got != nil {
							t.Fatalf("allowed shared window accepted aggregate reset: %v", *got)
						}
						continue
					}
					assertClaudeResetDuration(t, got, tc.want, time.Millisecond)
				}
			},
		)
	}
}
