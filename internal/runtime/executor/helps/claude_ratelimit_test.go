package helps

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestClaudeHeadersIndicateUnifiedRateLimitRejection(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   bool
	}{
		{name: "nil", header: nil, want: false},
		{name: "allowed", header: http.Header{"Anthropic-Ratelimit-Unified-Status": {"allowed"}}, want: false},
		{name: "allowed warning", header: http.Header{
			"Anthropic-Ratelimit-Unified-Status":    {"allowed_warning"},
			"Anthropic-Ratelimit-Unified-5h-Status": {"allowed_warning"},
			"Anthropic-Ratelimit-Unified-7d-Status": {"allowed_warning"},
		}, want: false},
		{name: "explicit 5h rejection", header: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
		}, want: true},
		{name: "explicit 7d rejection case insensitive", header: http.Header{
			"anthropic-ratelimit-unified-7D-status": {" ReJeCtEd "},
		}, want: true},
		{name: "aggregate rejection with missing shared statuses", header: http.Header{
			"Anthropic-Ratelimit-Unified-Status": {"rejected"},
		}, want: true},
		{name: "aggregate rejection with malformed shared statuses", header: http.Header{
			"Anthropic-Ratelimit-Unified-Status":    {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Status": {"unknown"},
			"Anthropic-Ratelimit-Unified-7d-Status": {"invalid"},
		}, want: true},
		{name: "Fable-only rejection", header: http.Header{
			"Anthropic-Ratelimit-Unified-Status":       {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Status":    {"allowed"},
			"Anthropic-Ratelimit-Unified-7d-Status":    {"allowed"},
			"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
		}, want: false},
		{name: "Fable-only rejection with allowed warning", header: http.Header{
			"Anthropic-Ratelimit-Unified-Status":       {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Status":    {"allowed_warning"},
			"Anthropic-Ratelimit-Unified-7d-Status":    {"allowed"},
			"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
		}, want: false},
		{name: "overage-only spend cap with both shared windows allowed", header: http.Header{
			"Anthropic-Ratelimit-Unified-Status":                  {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Status":               {"allowed"},
			"Anthropic-Ratelimit-Unified-7d-Status":               {"allowed_warning"},
			"Anthropic-Ratelimit-Unified-Overage-Status":          {"rejected"},
			"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": {"org_spend_cap_reached"},
		}, want: false},
		{name: "overage claim with omitted healthy 5h", header: http.Header{
			"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
			"Anthropic-Ratelimit-Unified-7d-Status":            {"allowed"},
			"Anthropic-Ratelimit-Unified-5h-Utilization":       {"0.00"},
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day_overage_included"},
		}, want: false},
		{name: "overage claim with omitted healthy 7d", header: http.Header{
			"Anthropic-Ratelimit-Unified-Status":                  {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Status":               {"allowed"},
			"Anthropic-Ratelimit-Unified-7d-Utilization":          {"0.75"},
			"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": {"org_spend_cap_reached"},
		}, want: false},
		{name: "overage claim without shared status is model scoped", header: http.Header{
			"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"overage"},
		}, want: false},
		{name: "overage claim with exhausted shared utilization is credential scoped", header: http.Header{
			"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"overage"},
			"Anthropic-Ratelimit-Unified-5h-Utilization":       {"1.0"},
		}, want: true},
		{name: "missing healthy utilization cannot prove overage-only", header: http.Header{
			"Anthropic-Ratelimit-Unified-Status":                  {"rejected"},
			"Anthropic-Ratelimit-Unified-7d-Status":               {"allowed"},
			"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": {"org_spend_cap_reached"},
		}, want: true},
		{name: "nonfinite utilization cannot prove overage-only", header: http.Header{
			"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
			"Anthropic-Ratelimit-Unified-7d-Status":            {"allowed"},
			"Anthropic-Ratelimit-Unified-5h-Utilization":       {"NaN"},
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day_overage_included"},
		}, want: true},
		{name: "shared rejection takes priority over overage", header: http.Header{
			"Anthropic-Ratelimit-Unified-Status":         {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Status":      {"rejected"},
			"Anthropic-Ratelimit-Unified-7d-Status":      {"allowed"},
			"Anthropic-Ratelimit-Unified-Overage-Status": {"rejected"},
		}, want: true},
		{name: "combined shared and Fable rejection", header: http.Header{
			"Anthropic-Ratelimit-Unified-Status":       {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Status":    {"rejected"},
			"Anthropic-Ratelimit-Unified-7d-Status":    {"allowed"},
			"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
		}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClaudeHeadersIndicateUnifiedRateLimitRejection(test.header); got != test.want {
				t.Fatalf("ClaudeHeadersIndicateUnifiedRateLimitRejection() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestParseClaudeRateLimitResetRetryAfterFormats(t *testing.T) {
	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{name: "integer seconds", raw: "60", want: time.Minute},
		{name: "floating seconds", raw: "1.5", want: 1500 * time.Millisecond},
		{name: "HTTP date", raw: now.Add(2 * time.Minute).Format(http.TimeFormat), want: 2 * time.Minute},
		{name: "RFC3339", raw: now.Add(3 * time.Minute).Format(time.RFC3339), want: 3 * time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{"retry-after": {test.raw}}
			got := parseClaudeRateLimitResetWithFuzz(header, now, 0, 0)
			assertClaudeResetDuration(t, got, test.want, time.Millisecond)
		})
	}
}

func TestParseClaudeRateLimitResetWindowFormats(t *testing.T) {
	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(5 * time.Hour)
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{name: "Unix seconds", raw: strconv.FormatInt(deadline.Unix(), 10), want: 5 * time.Hour},
		{name: "floating Unix seconds", raw: fmt.Sprintf("%d.5", deadline.Unix()), want: 5*time.Hour + 500*time.Millisecond},
		{name: "HTTP date", raw: deadline.Format(http.TimeFormat), want: 5 * time.Hour},
		{name: "RFC3339", raw: deadline.Format(time.RFC3339), want: 5 * time.Hour},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{
				"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
				"Anthropic-Ratelimit-Unified-5h-Reset":  {test.raw},
			}
			got := parseClaudeRateLimitResetWithFuzz(header, now, 0, 0)
			assertClaudeResetDuration(t, got, test.want, time.Millisecond)
		})
	}
}

func TestParseClaudeRateLimitResetWindowSelection(t *testing.T) {
	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)
	t.Run("latest rejected window wins", func(t *testing.T) {
		header := http.Header{
			"Retry-After":                              {"60"},
			"Anthropic-Ratelimit-Unified-Status":       {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Status":    {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Reset":     {strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10)},
			"Anthropic-Ratelimit-Unified-7d-Status":    {"rejected"},
			"Anthropic-Ratelimit-Unified-7d-Reset":     {strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10)},
			"Anthropic-Ratelimit-Unified-Reset":        {strconv.FormatInt(now.Add(6*24*time.Hour).Unix(), 10)},
			"Anthropic-Ratelimit-Unified-7d_oi-Status": {"allowed"},
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":  {strconv.FormatInt(now.Add(8*24*time.Hour).Unix(), 10)},
		}
		got := parseClaudeRateLimitResetWithFuzz(header, now, 0, 0)
		assertClaudeResetDuration(t, got, 7*24*time.Hour, time.Millisecond)
	})

	t.Run("combined shared and Fable rejection uses longer Fable reset", func(t *testing.T) {
		header := http.Header{
			"Anthropic-Ratelimit-Unified-Status":       {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Status":    {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Reset":     {strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10)},
			"Anthropic-Ratelimit-Unified-7d-Status":    {"allowed"},
			"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":  {strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10)},
		}
		got := parseClaudeRateLimitResetWithFuzz(header, now, 0, 0)
		assertClaudeResetDuration(t, got, 7*24*time.Hour, time.Millisecond)
	})

	t.Run("Fable-only ignores Retry-After and week-long resets", func(t *testing.T) {
		header := http.Header{
			"Retry-After":                              {"60"},
			"Anthropic-Ratelimit-Unified-Status":       {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Status":    {"allowed"},
			"Anthropic-Ratelimit-Unified-7d-Status":    {"allowed_warning"},
			"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":  {strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10)},
			"Anthropic-Ratelimit-Unified-Reset":        {strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10)},
		}
		if got := parseClaudeRateLimitResetWithFuzz(header, now, 0, 0); got != nil {
			t.Fatalf("Fable-only reset = %v, want nil", *got)
		}
	})

	t.Run("overage-only ignores Retry-After and shared reset", func(t *testing.T) {
		header := http.Header{
			"Retry-After":                                         {"60"},
			"Anthropic-Ratelimit-Unified-Status":                  {"rejected"},
			"Anthropic-Ratelimit-Unified-7d-Status":               {"allowed"},
			"Anthropic-Ratelimit-Unified-5h-Utilization":          {"0.00"},
			"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": {"org_spend_cap_reached"},
			"Anthropic-Ratelimit-Unified-7d_oi-Status":            {"rejected"},
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":             {strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10)},
			"Anthropic-Ratelimit-Unified-Reset":                   {strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10)},
		}
		if got := parseClaudeRateLimitResetWithFuzz(header, now, 0, 0); got != nil {
			t.Fatalf("overage-only reset = %v, want nil", *got)
		}
	})

	t.Run("Fable-only reset without Retry-After falls back", func(t *testing.T) {
		header := http.Header{
			"Anthropic-Ratelimit-Unified-Status":       {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Status":    {"allowed"},
			"Anthropic-Ratelimit-Unified-7d-Status":    {"allowed"},
			"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":  {strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10)},
			"Anthropic-Ratelimit-Unified-Reset":        {strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10)},
		}
		if got := parseClaudeRateLimitResetWithFuzz(header, now, 0, 0); got != nil {
			t.Fatalf("expected nil, got %v", *got)
		}
	})
}

func TestParseClaudeRateLimitResetRejectsInvalidCandidates(t *testing.T) {
	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)
	tests := []http.Header{
		{"Anthropic-Ratelimit-Unified-5h-Status": {"allowed"}, "Anthropic-Ratelimit-Unified-5h-Reset": {strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10)}},
		{"Anthropic-Ratelimit-Unified-5h-Status": {"allowed_warning"}, "Anthropic-Ratelimit-Unified-5h-Reset": {strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10)}},
		{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-5h-Reset": {strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)}},
		{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-5h-Reset": {"not-a-time"}},
		{"Retry-After": {"-1"}},
		{"Retry-After": {"invalid"}},
	}
	for index, header := range tests {
		if got := parseClaudeRateLimitResetWithFuzz(header, now, 0, 0); got != nil {
			t.Fatalf("case %d: expected nil, got %v", index, *got)
		}
	}
}

func TestClaudeOverageBillingResetIsNotCooldown(t *testing.T) {
	now := time.Date(2026, time.October, 7, 20, 46, 4, 0, time.UTC)
	billingReset := strconv.FormatInt(now.Add(25*24*time.Hour).Unix(), 10)
	fiveHourReset := strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10)
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    time.Duration
	}{
		{name: "overage without shared status", headers: http.Header{
			"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"overage"},
			"Anthropic-Ratelimit-Unified-Reset":                {billingReset},
			"Anthropic-Ratelimit-Unified-Overage-Reset":        {billingReset},
			"Retry-After": {"10800"},
		}},
		{name: "five hour rejection with monthly reset", headers: http.Header{
			"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Status":            {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Reset":             {fiveHourReset},
			"Anthropic-Ratelimit-Unified-Reset":                {billingReset},
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"},
		}, want: 5 * time.Hour},
		{name: "overage reset with short retry after", headers: http.Header{
			"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Status":            {"rejected"},
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"overage"},
			"Anthropic-Ratelimit-Unified-Reset":                {billingReset},
			"Anthropic-Ratelimit-Unified-Overage-Reset":        {billingReset},
			"Retry-After": {"60"},
		}, want: time.Minute},
		{name: "implausible retry after", headers: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
			"Retry-After":                           {"2034192"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseClaudeRateLimitResetWithFuzz(tc.headers, now, 0, 0)
			if tc.want == 0 {
				if got != nil {
					t.Fatalf("cooldown = %v, want nil", *got)
				}
				return
			}
			assertClaudeResetDuration(t, got, tc.want, time.Second)
		})
	}
}

func TestParseClaudeRateLimitResetFuzzBounds(t *testing.T) {
	now := time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC)
	header := http.Header{"Retry-After": {"100"}}
	for range 100 {
		got := ParseClaudeRateLimitReset(header, now)
		if got == nil {
			t.Fatal("expected duration")
		}
		fuzz := *got - 100*time.Second
		if fuzz < time.Second || fuzz > 30*time.Second {
			t.Fatalf("fuzz = %v, want [1s, 30s]", fuzz)
		}
	}
}

func assertClaudeResetDuration(t *testing.T, got *time.Duration, want, tolerance time.Duration) {
	t.Helper()
	if got == nil {
		t.Fatalf("duration = nil, want %v", want)
	}
	if delta := *got - want; delta < -tolerance || delta > tolerance {
		t.Fatalf("duration = %v, want %v (tolerance %v)", *got, want, tolerance)
	}
}
