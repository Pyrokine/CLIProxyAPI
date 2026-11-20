package helps

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestAuditClaudeResetRetainsMissingStatusAndAggregateInputs(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	reset := func(seconds int64) string {
		return strconv.FormatInt(now.Add(time.Duration(seconds)*time.Second).Unix(), 10)
	}
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    time.Duration
	}{
		{"missing-5h-status", http.Header{"Anthropic-Ratelimit-Unified-5h-Reset": {reset(600)}}, 600 * time.Second},
		{"missing-7d-status", http.Header{"Anthropic-Ratelimit-Unified-7d-Reset": {reset(900)}}, 900 * time.Second},
		{
			"aggregate-without-status", http.Header{"Anthropic-Ratelimit-Unified-Reset": {reset(1200)}},
			1200 * time.Second,
		},
		{
			"independent-7d-oi-without-status", http.Header{"Anthropic-Ratelimit-Unified-7d_oi-Reset": {reset(1800)}},
			1800 * time.Second,
		},
		{
			"independent-oi-rejection-with-later-aggregate", http.Header{
				"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
				"Anthropic-Ratelimit-Unified-7d_oi-Reset":  {reset(600)},
				"Anthropic-Ratelimit-Unified-Reset":        {reset(1200)},
			}, 1200 * time.Second,
		},
		{
			"aggregate-rejection-with-missing-window-status", http.Header{
				"Anthropic-Ratelimit-Unified-Status":   {"rejected"},
				"Anthropic-Ratelimit-Unified-5h-Reset": {reset(1800)},
				"Anthropic-Ratelimit-Unified-Reset":    {reset(600)},
			}, 1800 * time.Second,
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				assertClaudeResetDuration(
					t, parseClaudeRateLimitResetWithFuzz(tc.headers, now, 0, 0), tc.want, time.Millisecond,
				)
			},
		)
	}
}
