package management

import (
	"testing"
	"time"
)

func TestSummarizeInspectionResultsByProvider(t *testing.T) {
	older := time.Date(2026, 7, 7, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	results := []accountInspectionResult{
		{
			Provider:  " Claude ",
			Status:    accountHealthNormal,
			Reason:    "ok",
			CheckedAt: older,
		},
		{
			Provider:  "claude",
			Status:    accountHealthUnavailable,
			Reason:    "token_expired",
			CheckedAt: newer,
		},
		{
			Provider:  "claude",
			Status:    accountHealthDisabled,
			Reason:    "disabled",
			CheckedAt: older,
		},
		{
			Provider:  "codex",
			Status:    accountHealthUnavailable,
			Reason:    "refresh_failed",
			CheckedAt: older,
		},
		{
			Status:    accountHealthUnknown,
			Reason:    "pending",
			CheckedAt: older,
		},
	}

	byProvider := summarizeInspectionResultsByProvider(results)
	claude := byProvider["claude"]
	if claude.Provider != "claude" || claude.Total != 3 {
		t.Fatalf("claude summary identity = %+v", claude)
	}
	if claude.Normal != 1 || claude.Abnormal != 1 || claude.Disabled != 1 || claude.Unavailable != 1 {
		t.Fatalf("claude summary counters = %+v", claude)
	}
	if claude.TokenInvalid != 1 || claude.RefreshFailed != 0 {
		t.Fatalf("claude issue counters = %+v", claude)
	}
	if claude.LastCheckedAt == nil || !claude.LastCheckedAt.Equal(newer) {
		t.Fatalf("claude last checked = %v, want %v", claude.LastCheckedAt, newer)
	}
	if len(claude.TopReasons) == 0 || claude.TopReasons[0].Reason != "disabled" {
		t.Fatalf("claude top reasons = %+v", claude.TopReasons)
	}

	codex := byProvider["codex"]
	if codex.RefreshFailed != 1 || codex.Unavailable != 1 {
		t.Fatalf("codex summary counters = %+v", codex)
	}
	unknown := byProvider["unknown"]
	if unknown.Provider != "unknown" || unknown.Unknown != 1 || unknown.Abnormal != 1 {
		t.Fatalf("unknown provider summary = %+v", unknown)
	}
}
