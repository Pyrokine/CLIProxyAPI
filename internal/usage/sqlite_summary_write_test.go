package usage

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSummaryGrainsRefreshInstalledTriggersAndKeepLargeHistoryWritable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(
		func() {
			if store != nil {
				_ = store.Close()
			}
		},
	)
	ctx := context.Background()
	writer := newWriter(store, nil)
	details := make([]FlatDetail, repriceEventPageSize)
	for i := range details {
		details[i] = FlatDetail{
			Timestamp: time.Unix(0, int64(i+1)), Model: "write-history", RequestID: fmt.Sprintf("seed-%d", i),
			Tokens: tokenStats{
				InputTokens: 100, OutputTokens: 20, ReasoningTokens: 3,
				CachedTokens: 4, CacheReadTokens: 3, CacheWriteTokens: 1, TotalTokens: 120,
			},
			costTrusted: true, costMicro: 7, CostStatus: costStatusComplete, LatencyMs: 3,
		}
		if i%2 != 0 {
			details[i].Source, details[i].Provider = "credential", "provider"
			details[i].APIKey, details[i].AuthIndex = "client", "auth"
		}
	}
	if err := writer.flush(details); err != nil {
		t.Fatal(err)
	}
	const history = 500000
	const historyStart = int64(4000000000000000000)
	if _, err := store.db.Exec(
		`WITH RECURSIVE seq(n) AS (
		SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<?
	) INSERT INTO summary_bucket(grain_ns,bucket_ts_ns,model_id,requests,success,total_tokens,cost_micro)
	SELECT 1,?+n,(SELECT id FROM dim_model WHERE name='write-history'),1,1,1,1 FROM seq`, history, historyStart,
	); err != nil {
		t.Fatal(err)
	}

	// This is the installed predicate from the old trigger, independent of the current generator.
	legacyWhere := "bucket_ts_ns=" + summaryGrainFloor("OLD.ts_ns", "grain_ns")
	for _, name := range []string{"model_id", "credential_id", "api_key_id", "provider_id", "auth_index_id"} {
		legacyWhere += " AND " + name + " IS OLD." + name
	}
	legacyUpdates := make([]string, len(summaryMetricColumns))
	for i, value := range summaryEventMetrics("OLD.") {
		legacyUpdates[i] = summaryMetricColumns[i] + "=" + summaryMetricColumns[i] + "-(" + value + ")"
	}
	legacyRemove := "UPDATE summary_bucket SET " + strings.Join(legacyUpdates, ",") + " WHERE " + legacyWhere +
		"; DELETE FROM summary_bucket WHERE requests=0 AND " + legacyWhere + ";"
	for _, trigger := range []struct{ name, event, body string }{
		{"insert", "INSERT", summaryInsertSQL("NEW.", summaryGrainsSQL+" g")},
		{"delete", "DELETE", legacyRemove},
		{"update", "UPDATE", legacyRemove + summaryInsertSQL("NEW.", summaryGrainsSQL+" g")},
	} {
		if _, err := store.db.Exec(
			"DROP TRIGGER summary_bucket_event_" + trigger.name +
				"; CREATE TRIGGER summary_bucket_event_" + trigger.name + " AFTER " + trigger.event +
				" ON events BEGIN " + trigger.body + " END",
		); err != nil {
			t.Fatal(err)
		}
	}
	var original string
	if err := store.db.QueryRow(
		"SELECT sql FROM sqlite_master WHERE name='summary_bucket_event_update'",
	).Scan(&original); err != nil {
		t.Fatal(err)
	}
	var marker string
	if err := store.db.QueryRow(
		"SELECT value FROM meta WHERE key=?", summaryGrainsMigrationKey,
	).Scan(&marker); err != nil || marker != "complete" {
		t.Fatalf("installed marker=%q error=%v", marker, err)
	}
	// Reopening a complete store must refresh only the trigger definitions.
	if _, err := store.db.Exec(
		`CREATE TRIGGER reject_summary_rebuild_insert BEFORE INSERT ON summary_bucket
		BEGIN SELECT RAISE(ABORT,'complete history was rebuilt'); END;
		CREATE TRIGGER reject_summary_rebuild_delete BEFORE DELETE ON summary_bucket
		BEGIN SELECT RAISE(ABORT,'complete history was rebuilt'); END`,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var installed string
	if err := store.db.QueryRow(
		"SELECT sql FROM sqlite_master WHERE name='summary_bucket_event_update'",
	).Scan(&installed); err != nil || installed == original {
		t.Fatalf("old installed update trigger was retained: error=%v", err)
	}
	if _, err := store.db.Exec(
		"DROP TRIGGER reject_summary_rebuild_insert; DROP TRIGGER reject_summary_rebuild_delete",
	); err != nil {
		t.Fatal(err)
	}

	replaceOld := strings.NewReplacer(
		"OLD.ts_ns", "1", "OLD.model_id", "1", "OLD.credential_id", "NULL", "OLD.api_key_id", "NULL",
		"OLD.provider_id", "NULL", "OLD.auth_index_id", "NULL", "OLD.failed", "0", "OLD.metadata", "'{}'",
		"OLD.cost_status", "'complete'", "OLD.input_tokens", "100", "OLD.output_tokens", "20",
		"OLD.reasoning_tokens", "3", "OLD.cached_tokens", "4", "OLD.cache_read_tokens", "3",
		"OLD.cache_write_tokens", "1", "OLD.total_tokens", "120", "OLD.cost_micro", "7",
	)
	for _, name := range []string{"delete", "update"} {
		var definition string
		if err := store.db.QueryRow(
			"SELECT sql FROM sqlite_master WHERE name=?", "summary_bucket_event_"+name,
		).Scan(&definition); err != nil {
			t.Fatal(err)
		}
		body := strings.TrimSuffix(strings.SplitN(definition, " BEGIN ", 2)[1], " END")
		searches := 0
		for _, statement := range strings.Split(replaceOld.Replace(body), ";") {
			statement = strings.TrimSpace(statement)
			if !strings.HasPrefix(statement, "UPDATE summary_bucket") &&
				!strings.HasPrefix(statement, "DELETE FROM summary_bucket") {
				continue
			}
			rows, err := store.db.Query("EXPLAIN QUERY PLAN " + statement)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var id, parent, unused int
				var plan string
				if err := rows.Scan(&id, &parent, &unused, &plan); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(plan, "SEARCH summary_bucket USING INDEX uq_summary_bucket_key") ||
					strings.Count(plan, "<expr>=?") != 4 {
					t.Fatalf("%s trigger lost the complete summary key: %s", name, plan)
				}
				searches++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			_ = rows.Close()
		}
		if searches != 6 {
			t.Fatalf("%s trigger index searches=%d, want 6", name, searches)
		}
	}

	assertExact := func() {
		t.Helper()
		keys := "g.grain_ns," + summaryGrainFloor("e.ts_ns", "g.grain_ns") +
			",e.model_id,e.credential_id,e.api_key_id,e.provider_id,e.auth_index_id"
		metrics := summaryEventMetrics("e.")
		for i := range metrics {
			metrics[i] = "SUM(" + metrics[i] + ")"
		}
		expected := "SELECT " + keys + "," + strings.Join(metrics, ",") +
			" FROM events e CROSS JOIN " + summaryGrainsSQL + " g GROUP BY " + keys
		actual := "SELECT grain_ns,bucket_ts_ns,model_id,credential_id,api_key_id,provider_id,auth_index_id," +
			strings.Join(summaryMetricColumns, ",") + " FROM summary_bucket WHERE bucket_ts_ns<4000000000000000000"
		var differences, retained, cost int64
		if err := store.db.QueryRow(
			"WITH expected AS (" + expected + ") SELECT " +
				"(SELECT COUNT(*) FROM (" + actual + " EXCEPT SELECT * FROM expected)) + " +
				"(SELECT COUNT(*) FROM (SELECT * FROM expected EXCEPT " + actual + "))",
		).Scan(&differences); err != nil || differences != 0 {
			t.Fatalf("summary metric/dimension differences=%d error=%v", differences, err)
		}
		if err := store.db.QueryRow(
			"SELECT COUNT(*),SUM(cost_micro) FROM summary_bucket WHERE bucket_ts_ns>=?", historyStart,
		).Scan(&retained, &cost); err != nil || retained != history || cost != history {
			t.Fatalf("retained history=%d cost=%d error=%v", retained, cost, err)
		}
	}
	assertExact()
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := store.db.BeginTx(bounded, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(
		bounded,
		"UPDATE events SET ts_ns=-600000000001,failed=1,input_tokens=101,total_tokens=121,cost_micro=9 WHERE id=1",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(bounded, "DELETE FROM events WHERE id=2"); err != nil {
		t.Fatal(err)
	}
	if err := rebuildUsageBucketsTx(bounded, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertExact()

	p := &Persister{store: store}
	pricing := pricingSnapshot{priceFn: func(string) (float64, float64, float64, bool) { return 2, 1, 0.5, true }}
	task, err := p.prepareCostRecalculationTask(ctx, store, pricing)
	if err != nil {
		t.Fatal(err)
	}
	repriceCtx, cancelReprice := context.WithTimeout(ctx, 5*time.Second)
	defer cancelReprice()
	done := make(chan error, 2)
	go func() {
		processed, err := p.repriceCostRecalculationPageWithRetry(repriceCtx, store, pricing, &task)
		if err == nil && !processed {
			err = fmt.Errorf("historical page was not processed")
		}
		done <- err
	}()
	go func() {
		liveWriter := newWriter(store, nil)
		liveWriter.start(ctx)
		defer liveWriter.Stop()
		live := FlatDetail{
			Timestamp: time.Unix(0, nsPerDay), Model: "write-history", RequestID: "live-during-repricing",
			Tokens: tokenStats{InputTokens: 317, OutputTokens: 5, TotalTokens: 322},
		}
		if !liveWriter.submit(live) {
			done <- fmt.Errorf("live record was not accepted")
			return
		}
		liveWriter.Stop()
		if stats := liveWriter.stats(); stats.EventCommitted != 1 || stats.EventRejected != 0 {
			done <- fmt.Errorf("live writer outcomes=%+v", stats)
			return
		}
		done <- nil
	}()
	for range 2 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	var live int
	if err := store.db.QueryRow(
		"SELECT COUNT(*) FROM events WHERE total_tokens=322",
	).Scan(&live); err != nil || live != 1 {
		t.Fatalf("live request persistence=%d error=%v", live, err)
	}
	if task.ProcessedEvents != repriceEventPageSize-1 || task.ChangedEvents != repriceEventPageSize-1 {
		t.Fatalf("historical page progress=%+v", task)
	}
	assertExact()

	if _, err := store.db.ExecContext(ctx, "UPDATE events SET cost_micro=9223372036854775807 WHERE id=3"); err == nil {
		t.Fatal("coarse-grain cost overflow was accepted")
	}
	assertExact()
}
