package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func assertMeasuredLatency(t *testing.T, result QueryResult, total, samples int64) {
	t.Helper()
	if result.Totals.Latency.TotalMs != total || result.Totals.Latency.SampleCount != samples {
		t.Fatalf("total latency=%+v want total=%d samples=%d", result.Totals.Latency, total, samples)
	}
	for _, model := range result.ByModel {
		if model.Latency.TotalMs != total || model.Latency.SampleCount != samples {
			t.Fatalf("model latency=%+v", model.Latency)
		}
	}
	for _, key := range result.ByAPIKey {
		if key.Latency.TotalMs != total || key.Latency.SampleCount != samples {
			t.Fatalf("API key latency=%+v", key.Latency)
		}
	}
}

func TestMeasuredZeroLatencySurvivesPersistenceQueriesAndSerialization(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, extra := range []string{
		`,"latency_ms":0`, `,"latency_ms":12`, `,"completed":true`, `,"latency_ms":null`, `,"latency_ms":-1`,
	} {
		var detail FlatDetail
		raw := fmt.Sprintf(
			`{"timestamp":%q,"model":"latency-model","api_key":"synthetic-latency-k`+
				`ey","tokens":{"input_tokens":1,"total_tokens":1}%s}`,
			base.Add(time.Duration(i)*time.Minute).Format(time.RFC3339Nano), extra,
		)
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			t.Fatal(err)
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(detail)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(encoded, &value); err != nil {
			t.Fatal(err)
		}
		_, present := value["latency_ms"]
		if present != (i == 0 || i == 1 || i == 4) {
			t.Fatalf("latency presence changed in JSON row=%d body=%s", i, encoded)
		}
		backup, err := json.Marshal(usageBackupEventFromFlatDetail(detail, ""))
		if err != nil {
			t.Fatal(err)
		}
		var event usageBackupEvent
		if err := json.Unmarshal(backup, &event); err != nil {
			t.Fatal(err)
		}
		restored, err := event.flatDetail(detail.APIKey)
		if err != nil {
			t.Fatal(err)
		}
		expectedSamples := int64(0)
		if i < 2 {
			expectedSamples = 1
		}
		if _, actual := latencyBucketValues(restored.LatencyMs, restored.latencyRecorded); actual != expectedSamples {
			t.Fatalf("backup changed latency presence row=%d samples=%d", i, actual)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, from := range []time.Time{base.Add(-time.Hour), base.Add(-time.Nanosecond)} {
		result, err := store.querySummary(ctx, from, base.Add(time.Hour), EventFilters{}, "hourly", true)
		if err != nil {
			t.Fatal(err)
		}
		assertMeasuredLatency(t, result, 12, 2)
	}
	day := base.Truncate(24 * time.Hour)
	if err := store.rollupDay(ctx, day.UnixNano()); err != nil {
		t.Fatal(err)
	}
	var total, samples int64
	if err := store.db.QueryRowContext(
		ctx, "SELECT SUM(latency_ms_total),SUM(latency_samples) FROM day_bucket",
	).Scan(&total, &samples); err != nil || total != 12 || samples != 2 {
		t.Fatalf("day latency total=%d samples=%d err=%v", total, samples, err)
	}
	for _, compact := range []bool{false, true} {
		var events []FlatDetail
		if compact {
			events, _, err = store.queryCompactEventDetails(
				ctx, base.Add(-time.Hour), base.Add(time.Hour), EventFilters{}, 1, 20, "timestamp", false,
			)
		} else {
			events, _, err = store.queryEventDetails(
				ctx, base.Add(-time.Hour), base.Add(time.Hour), EventFilters{}, 1, 20, "timestamp", false,
			)
		}
		if err != nil || len(events) != 5 {
			t.Fatalf("compact=%t events=%d err=%v", compact, len(events), err)
		}
		if !events[0].latencyRecorded || events[0].LatencyMs != 0 || events[2].latencyRecorded ||
			events[3].latencyRecorded {
			t.Fatalf("compact=%t lost nullable latency presence", compact)
		}
	}
}

func TestMeasuredZeroLatencyImportsAndLiveSubMillisecondSamples(t *testing.T) {
	for _, payload := range []string{
		`[{"timestamp":"2026-11-06T09:00:00Z","model":"m","latency_ms":0},` +
			`{"timestamp":"2026-11-06T09:01:00Z","model":` +
			`"m"}]`,
		`{"version":1,"usage":{"apis":{"key":{"models":{"m":{"details":[{"times` +
			`tamp":"2026-11-06T09:00:00Z","latency_ms":0},{"timestamp":"2026-11-06T` +
			`09:01:00Z"}]}}}}}}`,
		"timestamp,model,source,api_key,result,input_tokens,output_tokens,total" +
			"_tokens,latency_ms\n2026-11-06T09:00:00Z,m,s,k,success,1,0,1,0\n2026-1" +
			"1-06T09:01:00Z,m,s,k,success,1,0,1,\n",
	} {
		details, _, err := parseAny([]byte(payload))
		if err != nil || len(details) != 2 {
			t.Fatalf("import details=%d err=%v", len(details), err)
		}
		if _, count := latencyBucketValues(details[0].LatencyMs, details[0].latencyRecorded); count != 1 {
			t.Fatalf("explicit imported zero was discarded: %s", payload)
		}
		if _, count := latencyBucketValues(details[1].LatencyMs, details[1].latencyRecorded); count != 0 {
			t.Fatalf("missing imported latency became measured: %s", payload)
		}
	}
	previous := statisticsEnabled.Load()
	statisticsEnabled.Store(true)
	defer statisticsEnabled.Store(previous)
	stats := NewRequestStatistics()
	base := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for i, duration := range []time.Duration{time.Microsecond, 0, 10 * time.Millisecond} {
		stats.record(
			context.Background(), coreusage.Record{
				RequestedAt: base.Add(time.Duration(i) * time.Minute), Model: "m", APIKey: "synthetic-live-key",
				Latency: duration, Completed: true,
			},
		)
	}
	result, err := stats.QuerySummary(
		context.Background(),
		base.Add(-time.Hour),
		base.Add(time.Hour),
		EventFilters{},
		"hourly",
		"",
		true,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertMeasuredLatency(t, result, 10, 2)
	encoded, err := json.Marshal(stats.snapshot())
	if err != nil || !strings.Contains(string(encoded), `"latency_ms":0`) {
		t.Fatalf("legacy snapshot lost measured zero: %s err=%v", encoded, err)
	}
}
