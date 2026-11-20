package usage

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLegacyMigrationRecoversHistoricalBucketsAfterReadFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory read permissions require an unprivileged test user")
	}
	ctx := context.Background()
	base := t.TempDir()
	month := filepath.Join(base, "2026-03")
	if err := os.Mkdir(month, 0700); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(month, "2026-03-01.json")
	oldData := []byte((`{"date":"2026-03-01","details":[{"timestamp":"2026-03-01T03:04:05Z","model":"` +
		`historical","provider":"claude","source":"retained.json","auth_index":"auth-historical","api_key":"` +
		`synthetic-client","tokens":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}]}`))
	newData := []byte((`{"date":"2026-11-11","details":[{"timestamp":"2026-11-11T08:05:00Z","model":"` +
		`historical","provider":"claude","source":"retained.json","auth_index":"auth-historical","api_key":"` +
		`synthetic-client","tokens":{"input_tokens":4,"output_tokens":3,"total_tokens":7},"failed":true}]}`))
	for path, data := range map[string][]byte{oldPath: oldData, filepath.Join(base, "today.json"): newData} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(month, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(month, 0700) })
	store := openTestStore(t)
	price := func(string) (float64, float64, float64, bool) { return 1, 2, 0, true }
	if err := migrateLegacyToSQLite(
		ctx, store, pricingSnapshot{priceFn: price}, base,
	); err == nil || !strings.Contains(err.Error(), month) {
		t.Fatalf("unreadable month migration error = %v", err)
	}
	if status, err := checkMigrationStatus(ctx, store); err != nil || status.From != "" {
		t.Fatalf("failed migration status = %+v, error = %v", status, err)
	}
	if err := os.Chmod(month, 0700); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyToSQLite(ctx, store, pricingSnapshot{priceFn: price}, base); err != nil {
		t.Fatal(err)
	}
	if status, err := checkMigrationStatus(ctx, store); err != nil || status.From == "" {
		t.Fatalf("recovered migration status = %+v, error = %v", status, err)
	}
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	events, total, err := store.queryEventDetails(ctx, from, to, EventFilters{}, 1, 10, "timestamp", false)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(events) != 2 {
		t.Fatalf("recovered events = %d/%d, error = %v", len(events), total, err)
	}
	if events[0].Timestamp.Format(time.RFC3339) != "2026-03-01T03:04:05Z" || events[0].Provider != "claude" ||
		events[0].AuthIndex != "auth-historical" || events[1].Timestamp.Format(time.RFC3339) != "2026-11-11T08:05:00Z" {
		t.Fatalf("historical range or identity changed: %+v", events)
	}
	summary, err := store.queryWithAuthIndex(ctx, from, to, EventFilters{}, "daily", true)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Totals.Requests != 2 || summary.Totals.Success != 1 || summary.Totals.Failure != 1 ||
		summary.Totals.Tokens.TotalTokens != 12 || math.Abs(summary.Totals.Cost-0.000018) > 1e-12 {
		t.Fatalf("historical summary = %+v, error = %v", summary.Totals, err)
	}
	for dimension, series := range map[string][]timePoint{
		"total":      summary.TimeSeries,
		"model":      summary.TimeSeriesByModel["historical"],
		"auth_index": summary.TimeSeriesByAuthIndex["auth-historical"],
	} {
		active := make(map[string]int64)
		for _, point := range series {
			if point.Requests > 0 {
				active[point.Time[:10]] += point.Requests
			}
		}
		if len(active) != 2 || active["2026-03-01"] != 1 || active["2026-11-11"] != 1 {
			t.Fatalf("%s historical series = %+v", dimension, active)
		}
	}
	if err := migrateLegacyToSQLite(ctx, store, pricingSnapshot{priceFn: price}, base); err != nil {
		t.Fatal(err)
	}
	if _, total, err := store.queryEventDetails(
		ctx, from, to, EventFilters{}, 1, 10, "timestamp", false,
	); err != nil || total != 2 {
		t.Fatalf("migration retry duplicated history: count=%d error=%v", total, err)
	}
	if retained, err := os.ReadFile(oldPath); err != nil || !bytes.Equal(retained, oldData) {
		t.Fatalf("original history changed: error=%v", err)
	}
	t.Log(
		"read failure left marker pending; retry restored 2026-03-01 through 2026-11-11, 2 requests/12 " +
			"tokens/18 micro-dollars and grouped buckets; original history retained",
	)
}
