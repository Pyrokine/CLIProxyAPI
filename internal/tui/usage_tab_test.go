package tui

import (
	"strings"
	"testing"
)

func TestDashboardRenderShowsTruncationWarning(t *testing.T) {
	model := dashboardModel{client: &Client{baseURL: "http://localhost"}}
	output := model.renderDashboard(
		map[string]any{},
		map[string]any{
			"warning": "usage details were truncated",
			"usage":   map[string]any{},
		},
		nil,
		nil,
	)
	if !strings.Contains(output, "usage details were truncated") {
		t.Fatalf("rendered dashboard missing warning: %q", output)
	}
}

func TestUsageTabRenderContentShowsTruncationWarning(t *testing.T) {
	model := usageTabModel{
		usage: map[string]any{
			"warning": "usage details were truncated",
			"usage":   map[string]any{},
		},
	}
	output := model.renderContent()
	if !strings.Contains(output, "usage details were truncated") {
		t.Fatalf("rendered usage content missing warning: %q", output)
	}
}

func TestUsageTabRenderTokenBreakdownIncludesTimeAndLatency(t *testing.T) {
	previousLocale := currentLocale
	currentLocale = "en"
	defer func() { currentLocale = previousLocale }()

	model := usageTabModel{}
	output := model.renderTokenBreakdown(
		map[string]any{
			"details": []any{
				map[string]any{
					"timestamp":             "2026-06-16T08:30:00Z",
					"latency_ms":            float64(120),
					"time_to_first_byte_ms": float64(45),
					"total_duration_ms":     float64(250),
					"tokens": map[string]any{
						"input_tokens":     float64(100),
						"output_tokens":    float64(25),
						"cached_tokens":    float64(10),
						"reasoning_tokens": float64(5),
					},
				},
			},
		},
	)

	for _, want := range []string{
		"Input:100", "Output:25", "Cached:10", "Reasoning:5", "Time:2026-06-16", "Latency:120ms", "TTFB:45ms",
		"Duration:250ms",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("rendered usage detail missing %q: %q", want, output)
		}
	}
}
