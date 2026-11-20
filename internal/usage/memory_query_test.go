package usage

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestMemorySummaryAppliesAuthIndexRangeOnlyWhenRequested(t *testing.T) {
	stats := NewRequestStatistics()
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(30 * 24 * time.Hour)
	if _, err := stats.QuerySummary(
		context.Background(), from, to, EventFilters{}, "hourly", "10m", false, false,
	); err != nil {
		t.Fatalf("inactive auth-index granularity rejected summary: %v", err)
	}
	if _, err := stats.QuerySummary(
		context.Background(), from, to, EventFilters{}, "hourly", "10m", false, true,
	); !errors.Is(err, ErrSummaryQueryTooLarge) {
		t.Fatalf("active auth-index range error=%v", err)
	}
}

func TestMemoryQueryReportsDiscardedDetailsAndTokenOverflow(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 10, 15, 0, 0, time.UTC)
	t.Run(
		"discarded", func(t *testing.T) {
			stats := NewRequestStatistics()
			logger := NewLoggerPlugin(nil, stats, func() bool { return true })
			for i := int64(0); i <= MaxLegacySnapshotEvents; i++ {
				logger.HandleUsage(
					ctx, coreusage.Record{RequestedAt: at, Model: "m", Detail: coreusage.Detail{TotalTokens: 1}},
				)
			}
			if _, err := stats.QuerySummary(
				ctx, at.Add(-time.Minute), at.Add(time.Minute), EventFilters{}, "hourly", "", false, false,
			); !errors.Is(err, ErrSummaryPrecisionUnavailable) {
				t.Fatalf("summary error=%v", err)
			}
			if _, _, err := stats.QueryEventsContext(
				ctx, at.Add(-time.Minute), at.Add(time.Minute), EventFilters{}, 1, 50, "timestamp", false,
			); !errors.Is(err, ErrSummaryPrecisionUnavailable) {
				t.Fatalf("events error=%v", err)
			}
		},
	)
	t.Run(
		"overflow", func(t *testing.T) {
			stats := NewRequestStatistics()
			logger := NewLoggerPlugin(nil, stats, func() bool { return true })
			for _, tokens := range []int64{math.MaxInt64, 1} {
				logger.HandleUsage(
					ctx, coreusage.Record{RequestedAt: at, Model: "m", Detail: coreusage.Detail{TotalTokens: tokens}},
				)
			}
			if _, err := stats.QuerySummary(
				ctx, at.Add(-time.Minute), at.Add(time.Minute), EventFilters{}, "hourly", "", true, false,
			); err == nil {
				t.Fatal("overflow returned a successful summary")
			}
		},
	)
}

func TestMemoryCanonicalAPIKeyNoneFilter(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	literalIdentity := coreusage.HashAPIKeyIdentity("(none)")
	stats := NewRequestStatistics()
	for index, identity := range []string{"", literalIdentity} {
		stats.apis[identity] = &apiStats{
			Models: map[string]*modelStats{
				"none-model": {Details: []requestDetail{{Timestamp: at.Add(time.Duration(index) * time.Minute)}}},
			},
		}
	}
	for _, tc := range []struct {
		name    string
		filters EventFilters
		want    []string
	}{
		{"legacy union", EventFilters{APIKey: "(none)"}, []string{"", literalIdentity}},
		{"canonical empty", EventFilters{APIKey: "(none)", CanonicalAPIKeys: true}, []string{""}},
		{"canonical repeated empty", EventFilters{APIKeys: []string{"(none)"}, CanonicalAPIKeys: true}, []string{""}},
		{"canonical literal", EventFilters{APIKey: literalIdentity, CanonicalAPIKeys: true}, []string{literalIdentity}},
		{
			"canonical both", EventFilters{APIKeys: []string{"(none)", literalIdentity}, CanonicalAPIKeys: true},
			[]string{"", literalIdentity},
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				events, count, err := stats.QueryEventsContext(
					ctx, at, at.Add(time.Hour), tc.filters, 1, 50, "timestamp", false,
				)
				if err != nil || count != len(tc.want) || len(events) != len(tc.want) {
					t.Fatalf("events=%d/%d error=%v want %d", count, len(events), err, len(tc.want))
				}
				for index, event := range events {
					if event.APIKey != tc.want[index] {
						t.Fatalf("event identity=%q want %q", event.APIKey, tc.want[index])
					}
				}
				summary, err := stats.QuerySummary(ctx, at, at.Add(time.Hour), tc.filters, "hourly", "", true, false)
				if err != nil || summary.Totals.Requests != int64(len(tc.want)) || len(summary.ByAPIKey) != len(tc.want) ||
					len(summary.TimeSeriesByAPIKey) != len(tc.want) {
					t.Fatalf(
						"summary=%d groups=%d series=%d error=%v", summary.Totals.Requests, len(summary.ByAPIKey),
						len(summary.TimeSeriesByAPIKey), err,
					)
				}
			},
		)
	}
}
