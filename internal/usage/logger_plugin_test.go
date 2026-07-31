package usage

import (
	"context"
	"math"
	"testing"
	"time"

	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestNormaliseTokenStatsRejectsNegativeAndOverflowingValues(t *testing.T) {
	tests := []struct {
		name   string
		tokens tokenStats
	}{
		{name: "negative input", tokens: tokenStats{InputTokens: -1}},
		{name: "negative total", tokens: tokenStats{TotalTokens: -1}},
		{name: "overflowing fallback total", tokens: tokenStats{InputTokens: math.MaxInt64, OutputTokens: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normaliseTokenStats(test.tokens); got != (tokenStats{}) {
				t.Fatalf("normaliseTokenStats(%+v) = %+v, want zero", test.tokens, got)
			}
		})
	}

	explicit := tokenStats{InputTokens: math.MaxInt64, OutputTokens: 1, TotalTokens: 7}
	if got := normaliseTokenStats(explicit); got != explicit {
		t.Fatalf("explicit total changed: got %+v, want %+v", got, explicit)
	}
}

func TestNormaliseDetailRejectsNegativeCacheBreakdown(t *testing.T) {
	if got := normaliseDetail(coreusage.Detail{InputTokens: 5, CacheReadTokens: -1}); got != (tokenStats{}) {
		t.Fatalf("normaliseDetail = %+v, want zero", got)
	}
	if got := normaliseDetail(coreusage.Detail{OutputTokens: 5, CacheCreationTokens: -1}); got != (tokenStats{}) {
		t.Fatalf("normaliseDetail = %+v, want zero", got)
	}
}

func TestRequestStatisticsAggregateOverflowKeepsRequestMetadata(t *testing.T) {
	timestamp := time.Date(2026, time.July, 14, 8, 0, 0, 0, time.UTC)
	dayKey := timestamp.Format("2006-01-02")
	hourKey := timestamp.Hour()
	model := &modelStats{TotalTokens: 5}
	api := &apiStats{TotalTokens: 5, Models: map[string]*modelStats{"model": model}}
	stats := newRequestStatistics()
	stats.totalTokens = math.MaxInt64
	stats.apis["api-key"] = api
	stats.tokensByDay[dayKey] = 5
	stats.tokensByHour[hourKey] = 5

	stats.Record(
		context.Background(),
		coreusage.Record{
			APIKey:              "api-key",
			Model:               "model",
			RequestedAt:         timestamp,
			RequestServiceTier:  "flex",
			ResponseServiceTier: "priority",
			Detail:              coreusage.Detail{TotalTokens: 1},
		},
	)

	snapshot := stats.Snapshot()
	if snapshot.TotalRequests != 1 || snapshot.SuccessCount != 1 {
		t.Fatalf("request counts = total %d success %d", snapshot.TotalRequests, snapshot.SuccessCount)
	}
	if snapshot.TotalTokens != math.MaxInt64 {
		t.Fatalf("global tokens = %d, want MaxInt64", snapshot.TotalTokens)
	}
	apiSnapshot := snapshot.APIs["api-key"]
	modelSnapshot := apiSnapshot.Models["model"]
	if apiSnapshot.TotalTokens != 5 || modelSnapshot.TotalTokens != 5 {
		t.Fatalf("api/model tokens = %d/%d, want 5/5", apiSnapshot.TotalTokens, modelSnapshot.TotalTokens)
	}
	if snapshot.TokensByDay[dayKey] != 5 || snapshot.TokensByHour[formatHour(hourKey)] != 5 {
		t.Fatalf("day/hour tokens = %d/%d, want 5/5", snapshot.TokensByDay[dayKey], snapshot.TokensByHour[formatHour(hourKey)])
	}
	if len(modelSnapshot.Details) != 1 {
		t.Fatalf("details = %d, want 1", len(modelSnapshot.Details))
	}
	detail := modelSnapshot.Details[0]
	if detail.RequestServiceTier != "flex" || detail.ResponseServiceTier != "priority" {
		t.Fatalf("service tiers = %q/%q", detail.RequestServiceTier, detail.ResponseServiceTier)
	}
}

func TestMergeSnapshotNormalisesInvalidTokenDetail(t *testing.T) {
	timestamp := time.Date(2026, time.July, 14, 9, 0, 0, 0, time.UTC)
	snapshot := StatisticsSnapshot{
		APIs: map[string]aPISnapshot{
			"api-key": {
				Models: map[string]modelSnapshot{
					"model": {
						Details: []requestDetail{{
							Timestamp:           timestamp,
							RequestServiceTier:  "flex",
							ResponseServiceTier: "priority",
							Tokens:              tokenStats{TotalTokens: -1},
						}},
					},
				},
			},
		},
	}

	stats := newRequestStatistics()
	var imported FlatDetail
	result := stats.MergeSnapshot(snapshot, func(detail FlatDetail) { imported = detail })
	if result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("merge result = %+v", result)
	}
	if imported.Tokens != (tokenStats{}) {
		t.Fatalf("imported tokens = %+v, want zero", imported.Tokens)
	}
	got := stats.Snapshot()
	if got.TotalRequests != 1 || got.TotalTokens != 0 {
		t.Fatalf("snapshot totals = requests %d tokens %d", got.TotalRequests, got.TotalTokens)
	}
	modelSnapshot := got.APIs["api-key"].Models["model"]
	if len(modelSnapshot.Details) != 1 || modelSnapshot.Details[0].Tokens != (tokenStats{}) {
		t.Fatalf("stored details = %+v", modelSnapshot.Details)
	}
}
