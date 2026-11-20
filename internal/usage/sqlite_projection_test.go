package usage

import (
	"context"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSummaryProjectionsPreserveDirtyWindowsRepricingAndTrimming(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	persist := func(day int, model, provider string, tokens int64) {
		t.Helper()
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		detail := FlatDetail{
			Timestamp: base.Add(time.Duration(day)*24*time.Hour + 10*time.Hour), Model: model, Source: "shared",
			Provider: provider, APIKey: "key", Tokens: tokenStats{TotalTokens: tokens},
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			if errRollback := tx.Rollback(); errRollback != nil {
				t.Errorf("rollback failed event: %v", errRollback)
			}
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	persist(0, "a", "claude", 5)
	persist(35, "a", "claude", 9)
	persist(59, "b", "codex", 2)
	if err := store.rollupPendingDays(ctx); err != nil {
		t.Fatal(err)
	}
	compare := func(requests, tokens int64) {
		t.Helper()
		fast, err := store.querySummary(
			ctx, base, base.Add(60*24*time.Hour-time.Nanosecond), EventFilters{}, "daily", false,
		)
		if err != nil {
			t.Fatal(err)
		}
		original, err := store.querySummary(
			ctx, base, base.Add(60*24*time.Hour-time.Nanosecond), EventFilters{}, "daily", true,
		)
		if err != nil {
			t.Fatal(err)
		}
		if fast.Totals.Requests != requests || fast.Totals.Tokens.TotalTokens != tokens || !reflect.DeepEqual(
			fast.Totals, original.Totals,
		) || !reflect.DeepEqual(fast.ByModel, original.ByModel) || !reflect.DeepEqual(
			fast.ByCredential, original.ByCredential,
		) || !reflect.DeepEqual(fast.ByAPIKey, original.ByAPIKey) || !reflect.DeepEqual(
			fast.TimeSeries, original.TimeSeries,
		) {
			t.Fatalf("projection=%+v original=%+v", fast, original)
		}
	}
	compare(3, 16)
	persist(35, "b", "codex", 3)
	compare(4, 19)
	if err := store.rollupDirtyDays(ctx); err != nil {
		t.Fatal(err)
	}
	compare(4, 19)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE events SET cost_micro=7,cost_status='complete'"); err != nil {
		t.Fatal(err)
	}
	if err := rebuildUsageBucketsTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	compare(4, 19)
	result, err := store.querySummary(
		ctx, base, base.Add(60*24*time.Hour-time.Nanosecond), EventFilters{}, "daily", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Totals.Cost != 0.000028 {
		t.Fatalf("cost=%v error=%v", result.Totals.Cost, err)
	}
	persister := &Persister{store: store}
	if err := persister.trimBefore(base.Add(24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	compare(3, 14)
	if _, err := store.db.Exec("DROP TABLE events"); err != nil {
		t.Fatal(err)
	}
	compare(3, 14)
}

func TestSummaryProjectionFailureRollsBackWholeImport(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.db.Exec(
		`CREATE TRIGGER reject_projection BEFORE INSERT ON summary_projection WHEN ` +
			`NEW.dimension='model' AND (SELECT name FROM dim_model WHERE id=NEW.model_id)='bad' BEGIN SELECT ` +
			`RAISE(ABORT,'synthetic projection failure'); END`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := importBytes(
		context.Background(), store, nil,
		[]byte((`[{"timestamp":"2025-01-01T10:00:00Z","model":"good","tokens":{"total_tokens":1}},{"timestamp"` +
			`:"2025-01-01T11:00:00Z","model":"bad","tokens":{"total_tokens":2}}]`)),
	); err == nil {
		t.Fatal("projection failure accepted import")
	}
	for _, table := range []string{
		"events", "hour_bucket", "day_bucket", "summary_bucket", "summary_projection", "dim_model",
	} {
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows=%d error=%v", table, count, err)
		}
	}
}

func TestSummaryProjectionBackfillAndOverflowRetainLegalBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.db.Exec(
		"INSERT INTO dim_model(name) VALUES('huge'),('small'); INSERT INTO day_"+
			"bucket(bucket_ts_ns,model_id,requests,success,total_tokens) VALUES(?,1"+
			",1,1,?),(?,2,1,1,1)",
		base.UnixNano(), int64(math.MaxInt64), base.UnixNano(),
	); err != nil {
		t.Fatalf("legal original bucket rejected: %v", err)
	}
	if _, err := store.db.Exec("DELETE FROM meta WHERE key=?", summaryProjectionMigrationKey); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openStore(path)
	if err != nil {
		t.Fatalf("projection migration rejected legal source buckets: %v", err)
	}
	defer func(closeResource func() error) {
		if err := closeResource(); err != nil {
			t.Errorf("close projection store: %v", err)
		}
	}(store.Close)
	ctx := context.Background()
	to := base.Add(24*time.Hour - time.Nanosecond)
	if _, err := store.querySummary(ctx, base, to, EventFilters{}, "daily", false); err == nil {
		t.Fatal("overflowing query returned fake success")
	}
	result, err := store.querySummary(ctx, base, to, EventFilters{Model: "small"}, "daily", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Totals.Requests != 1 || result.Totals.Tokens.TotalTokens != 1 {
		t.Fatalf("small=%+v error=%v", result.Totals, err)
	}
	if _, err := store.db.Exec("DELETE FROM day_bucket WHERE model_id=1"); err != nil {
		t.Fatal(err)
	}
	result, err = store.querySummary(ctx, base, to, EventFilters{}, "daily", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Totals.Requests != 1 || result.Totals.Tokens.TotalTokens != 1 {
		t.Fatalf("recovered=%+v error=%v", result.Totals, err)
	}
}
