package usage

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
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
		t.Run(
			test.name, func(t *testing.T) {
				if got := normaliseTokenStats(test.tokens); got != (tokenStats{}) {
					t.Fatalf("normaliseTokenStats(%+v) = %+v, want zero", test.tokens, got)
				}
			},
		)
	}

	explicit := tokenStats{InputTokens: math.MaxInt64, OutputTokens: 1, TotalTokens: 7}
	if got := normaliseTokenStats(explicit); got != explicit {
		t.Fatalf("explicit total changed: got %+v, want %+v", got, explicit)
	}
}

func TestRequestStatisticsAggregateSnapshotOmitsDetails(t *testing.T) {
	stats := newRequestStatistics()
	stats.record(
		context.Background(), coreusage.Record{
			APIKey: "client-key",
			Model:  "model",
			Detail: coreusage.Detail{
				TotalTokens: 3,
			},
		},
	)

	if _, err := stats.snapshotLimited(1, MaxLegacySnapshotDetailBytes); err != nil {
		t.Fatalf("SnapshotLimited() exact limit error = %v", err)
	}
	stats.mu.Lock()
	stats.apis["client-key"].Models["model"].Details[0].ResponseHeaders = http.Header{
		"X-Test": {strings.Repeat("x", 8<<10)},
	}
	stats.mu.Unlock()
	if _, err := stats.snapshotLimited(1, 4<<10); !errors.Is(err, ErrLegacySnapshotTooLarge) {
		t.Fatalf("SnapshotLimited() detail size error = %v, want %v", err, ErrLegacySnapshotTooLarge)
	}
	emptyDimensions := newRequestStatistics()
	emptyDimensions.apis[strings.Repeat("k", 4<<10)] = &apiStats{
		Models: map[string]*modelStats{},
	}
	if _, err := emptyDimensions.snapshotLimited(1, 2<<10); !errors.Is(err, ErrLegacySnapshotTooLarge) {
		t.Fatalf("SnapshotLimited() dimension size error = %v, want %v", err, ErrLegacySnapshotTooLarge)
	}
	stats.record(
		context.Background(), coreusage.Record{
			APIKey: "client-key",
			Model:  "model",
			Detail: coreusage.Detail{
				TotalTokens: 2,
			},
		},
	)
	if _, err := stats.snapshotLimited(1, MaxLegacySnapshotDetailBytes); !errors.Is(err, ErrLegacySnapshotTooLarge) {
		t.Fatalf("SnapshotLimited() error = %v, want %v", err, ErrLegacySnapshotTooLarge)
	}

	aggregate := stats.aggregateSnapshot()
	if aggregate.TotalRequests != 2 || aggregate.TotalTokens != 5 || len(aggregate.APIs) != 0 {
		t.Fatalf("aggregate snapshot = %+v, want global totals without API dimensions", aggregate)
	}
	if len(stats.snapshot().APIs["client-key"].Models["model"].Details) != 2 {
		t.Fatal("full snapshot lost request details")
	}
	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := stats.SnapshotLimitedContext(
		cancelledContext,
		2,
		MaxLegacySnapshotDetailBytes,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("SnapshotLimitedContext() error = %v, want %v", err, context.Canceled)
	}
	if _, err := stats.AggregateSnapshotContext(cancelledContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("AggregateSnapshotContext() error = %v, want %v", err, context.Canceled)
	}
}

func TestRequestStatisticsBoundsStoredDetailBytes(t *testing.T) {
	stats := newRequestStatistics()
	stats.detailsBytes = MaxLegacySnapshotDetailBytes - 1
	stats.record(
		context.Background(), coreusage.Record{
			APIKey: "client-key",
			Model:  "model",
			Detail: coreusage.Detail{TotalTokens: 3},
		},
	)

	if !stats.DetailsTruncated() {
		t.Fatal("DetailsTruncated() = false after detail byte budget was exhausted")
	}
	snapshot := stats.snapshot()
	if snapshot.TotalRequests != 1 || snapshot.TotalTokens != 3 {
		t.Fatalf("global totals = %d requests/%d tokens, want 1/3", snapshot.TotalRequests, snapshot.TotalTokens)
	}
	if details := snapshot.APIs["client-key"].Models["model"].Details; len(details) != 0 {
		t.Fatalf("stored details = %d, want 0", len(details))
	}
	if stats.detailsCount != 0 || stats.detailsBytes != MaxLegacySnapshotDetailBytes-1 {
		t.Fatalf("detail budget state = %d events/%d bytes", stats.detailsCount, stats.detailsBytes)
	}
}

func TestRequestStatisticsTrimReleasesDetailBudget(t *testing.T) {
	stats := newRequestStatistics()
	base := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	for _, timestamp := range []time.Time{base, base.Add(time.Hour)} {
		stats.record(
			context.Background(), coreusage.Record{
				APIKey:      "client-key",
				Model:       "model",
				RequestedAt: timestamp,
				Detail:      coreusage.Detail{TotalTokens: 1},
			},
		)
	}
	beforeBytes := stats.detailsBytes
	if beforeBytes <= 0 || stats.detailsCount != 2 {
		t.Fatalf("initial detail budget = %d events/%d bytes, want 2/positive", stats.detailsCount, beforeBytes)
	}

	archived := stats.trimDetails(base.Add(30 * time.Minute))
	if len(archived) != 1 {
		t.Fatalf("archived details = %d, want 1", len(archived))
	}
	if stats.detailsCount != 1 || stats.detailsBytes <= 0 || stats.detailsBytes >= beforeBytes {
		t.Fatalf(
			"remaining detail budget = %d events/%d bytes, before %d", stats.detailsCount, stats.detailsBytes,
			beforeBytes,
		)
	}
}

func TestLegacyAggregateBackendsUseUTC(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	timestamp := time.Date(2026, 8, 3, 0, 30, 0, 0, location)
	wantDay := "2026-08-02"
	wantHour := "16"

	stats := newRequestStatistics()
	stats.record(
		context.Background(), coreusage.Record{
			APIKey:      "client-key",
			Model:       "model",
			RequestedAt: timestamp,
		},
	)
	memorySnapshot := stats.aggregateSnapshot()
	memoryDetail := stats.snapshot().APIs["client-key"].Models["model"].Details[0]
	if memorySnapshot.RequestsByDay[wantDay] != 1 ||
		memorySnapshot.RequestsByHour[wantHour] != 1 ||
		memoryDetail.Timestamp.Location() != time.UTC {
		t.Fatalf("in-memory UTC snapshot = %+v detail=%s", memorySnapshot, memoryDetail.Timestamp)
	}

	store := openTestStore(t)
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	detail := FlatDetail{Timestamp: timestamp, Model: "model"}
	if _, err = persistEvent(context.Background(), tx, store, &detail, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	sqliteSnapshot, err := (&Persister{store: store}).QueryAggregateSnapshotContext(context.Background())
	if err != nil {
		t.Fatalf("QueryAggregateSnapshotContext: %v", err)
	}
	if sqliteSnapshot.RequestsByDay[wantDay] != 1 || sqliteSnapshot.RequestsByHour[wantHour] != 1 {
		t.Fatalf("SQLite UTC snapshot = %+v", sqliteSnapshot)
	}
}

func TestLoggerPluginRedactsAPIKeySourceForLegacyStatistics(t *testing.T) {
	raw := "abcd...wxyz"
	stats := newRequestStatistics()
	plugin := &loggerPlugin{stats: stats, enabled: func() bool { return true }}
	plugin.HandleUsage(
		context.Background(), coreusage.Record{
			APIKey:   "client-key",
			Model:    "model",
			Source:   raw,
			AuthType: "api_key",
		},
	)

	detail := stats.snapshot().APIs[coreusage.HashAPIKeyIdentity("client-key")].Models["model"].Details[0]
	display, kind, tagged := DecodeCredentialSource(detail.Source)
	if got, want := display, util.HideAPIKey(raw); got != want || kind != coreusage.CredentialSourceKindAPIKey || !tagged {
		t.Fatalf("legacy source = %q/%v/%v, want %q/true/true", got, kind, tagged, want)
	}
}

func TestLoggerPluginPreservesRedactedAPIKeySource(t *testing.T) {
	raw := "sk-provider-secret-wxyz"
	masked := util.HideAPIKey(raw)
	stats := newRequestStatistics()
	plugin := &loggerPlugin{stats: stats, enabled: func() bool { return true }}
	plugin.HandleUsage(
		context.Background(), coreusage.Record{
			APIKey:         "client-key",
			Model:          "model",
			Source:         masked,
			SourceRedacted: true,
			AuthType:       "apikey",
		},
	)

	detail := stats.snapshot().APIs[coreusage.HashAPIKeyIdentity("client-key")].Models["model"].Details[0]
	display, kind, tagged := DecodeCredentialSource(detail.Source)
	if display != masked || kind != coreusage.CredentialSourceKindAPIKey || !tagged {
		t.Fatalf("redacted source = %q/%q/%v, want %q/api_key/true", display, kind, tagged, masked)
	}
}

func TestLoggerPluginPreservesTypedIdentityWithAPIKeyAuth(t *testing.T) {
	stats := newRequestStatistics()
	plugin := &loggerPlugin{stats: stats, enabled: func() bool { return true }}
	plugin.HandleUsage(
		context.Background(), coreusage.Record{
			APIKey:     "client-key",
			Model:      "model",
			Source:     "project-123",
			SourceKind: coreusage.CredentialSourceKindIdentity,
			AuthType:   "api_key",
		},
	)

	detail := stats.snapshot().APIs[coreusage.HashAPIKeyIdentity("client-key")].Models["model"].Details[0]
	display, kind, tagged := DecodeCredentialSource(detail.Source)
	if display != "project-123" || kind != coreusage.CredentialSourceKindIdentity || !tagged {
		t.Fatalf("identity source = %q/%q/%v, want project-123/identity/true", display, kind, tagged)
	}
}

func TestLoggerPluginRedactsAPIKeySourceForSQLite(t *testing.T) {
	raw := "sk-provider-secret-wxyz"
	persister := NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}
	defer persister.Stop()

	plugin := &loggerPlugin{persister: persister, enabled: func() bool { return true }}
	requestedAt := time.Now().UTC().Add(-time.Minute)
	plugin.HandleUsage(
		context.Background(), coreusage.Record{
			APIKey:      "client-key",
			Model:       "model",
			Source:      raw,
			AuthType:    "apikey",
			RequestedAt: requestedAt,
		},
	)

	deadline := time.Now().Add(5 * time.Second)
	for {
		events, _, err := persister.QueryEvents(
			requestedAt.Add(-time.Minute),
			requestedAt.Add(time.Minute),
			EventFilters{},
			1,
			10,
			"timestamp",
			true,
		)
		if err != nil {
			t.Fatalf("QueryEvents: %v", err)
		}
		if len(events) == 1 {
			display, kind, tagged := DecodeCredentialSource(events[0].Source)
			if got, want := display, util.HideAPIKey(raw); got != want || kind != coreusage.CredentialSourceKindAPIKey || !tagged {
				t.Fatalf("SQLite source = %q/%v/%v, want %q/true/true", got, kind, tagged, want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("usage record was not persisted before deadline")
		}
		time.Sleep(25 * time.Millisecond)
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

	stats.record(
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

	snapshot := stats.snapshot()
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
		t.Fatalf(
			"day/hour tokens = %d/%d, want 5/5", snapshot.TokensByDay[dayKey],
			snapshot.TokensByHour[formatHour(hourKey)],
		)
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
						Details: []requestDetail{
							{
								Timestamp:           timestamp,
								RequestServiceTier:  "flex",
								ResponseServiceTier: "priority",
								Tokens:              tokenStats{TotalTokens: -1},
							},
						},
					},
				},
			},
		},
	}

	stats := newRequestStatistics()
	var imported FlatDetail
	result := stats.mergeSnapshot(snapshot, func(detail FlatDetail) { imported = detail })
	if result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("merge result = %+v", result)
	}
	if imported.Tokens != (tokenStats{}) {
		t.Fatalf("imported tokens = %+v, want zero", imported.Tokens)
	}
	got := stats.snapshot()
	if got.TotalRequests != 1 || got.TotalTokens != 0 {
		t.Fatalf("snapshot totals = requests %d tokens %d", got.TotalRequests, got.TotalTokens)
	}
	modelSnapshot := got.APIs["api-key"].Models["model"]
	if len(modelSnapshot.Details) != 1 || modelSnapshot.Details[0].Tokens != (tokenStats{}) {
		t.Fatalf("stored details = %+v", modelSnapshot.Details)
	}
}
