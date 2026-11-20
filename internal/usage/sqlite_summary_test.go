package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestSummaryUsesExactPersistentBoundariesAndAuthIndexWithoutEvents(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	times := []time.Time{
		base.Add(5 * time.Minute), base.Add(15 * time.Minute), base.Add(15*time.Minute + time.Nanosecond),
		base.Add(25 * time.Minute), base.Add(25*time.Minute + time.Nanosecond), base.Add(time.Hour),
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, ts := range times {
		detail := FlatDetail{
			Timestamp: ts, Model: "exact-model", Source: "source", AuthIndex: "auth-a",
			Tokens: tokenStats{InputTokens: int64(i + 1), TotalTokens: int64(i + 1)},
		}
		if _, err := persistEvent(ctx, tx, store, &detail, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	from, to := base.Add(15*time.Minute), base.Add(25*time.Minute)
	result, err := store.queryWithAuthIndexGranularity(ctx, from, to, EventFilters{}, "hourly", "10m", true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Totals.Requests != 3 || result.Totals.Tokens.TotalTokens != 9 ||
		result.ByAuthIndex["auth-a"].Requests != 3 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if _, err := store.db.ExecContext(ctx, "DROP TABLE events"); err != nil {
		t.Fatal(err)
	}
	persistent, err := store.queryWithAuthIndexGranularity(ctx, from, to, EventFilters{}, "hourly", "10m", true)
	if err != nil {
		t.Fatal(err)
	}
	if persistent.Totals.Requests != 3 || persistent.Totals.Tokens.TotalTokens != 9 {
		t.Fatalf("summary read events or changed results: %+v %v", persistent.Totals, err)
	}
}

func TestSummaryGrainsBackfillPreservesLegacyAggregatesAndUnknownPrecision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if _, err := importBytes(
		ctx, store, nil,
		[]byte(`[{"timestamp":"2026-11-01T10:15:00Z","model":"legacy","auth_index":`+
			`"auth-a","tokens":{"total_tokens":`+
			`1}}]`),
	); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"insert", "delete", "update"} {
		if _, err := store.db.ExecContext(ctx, "DROP TRIGGER summary_bucket_event_"+event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(
		ctx, "DROP TABLE summary_bucket; DELETE FROM meta WHERE key='summary_grains_v1'",
	); err != nil {
		t.Fatal(err)
	}
	oldHour := base.Add(-2 * time.Hour).UnixNano()
	if _, err := store.db.ExecContext(
		ctx,
		"INSERT INTO hour_bucket(bucket_ts_ns,model_id,requests,success,total_tokens) SELECT ?,id,100,100,"+
			"100 FROM dim_model WHERE name='legacy'",
		oldHour,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func(closeResource func() error) {
		if err := closeResource(); err != nil {
			t.Errorf("close migrated summary store: %v", err)
		}
	}(migrated.Close)
	var marker string
	if err := migrated.db.QueryRowContext(
		ctx, "SELECT value FROM meta WHERE key=?", summaryGrainsMigrationKey,
	).Scan(&marker); err != nil || marker != "complete" {
		t.Fatalf("marker=%q error=%v", marker, err)
	}
	current, err := migrated.querySummary(
		ctx, base, base.Add(time.Hour-time.Nanosecond), EventFilters{}, "hourly", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if current.Totals.Requests != 1 {
		t.Fatalf("backfilled=%+v error=%v", current.Totals, err)
	}
	historical, err := migrated.querySummary(
		ctx,
		time.Unix(0, oldHour),
		time.Unix(0, oldHour).Add(time.Hour-time.Nanosecond),
		EventFilters{},
		"hourly",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if historical.Totals.Requests != 100 {
		t.Fatalf("historical=%+v error=%v", historical.Totals, err)
	}
	if _, err := migrated.querySummary(
		ctx, time.Unix(0, oldHour).Add(time.Minute), time.Unix(0, oldHour).Add(2*time.Minute), EventFilters{}, "hourly",
		false,
	); !errors.Is(err, ErrSummaryPrecisionUnavailable) {
		t.Fatalf("missing historical precision error=%v", err)
	}
	var retained int
	if err := migrated.db.QueryRowContext(
		ctx, "SELECT SUM(requests) FROM hour_bucket WHERE bucket_ts_ns=?", oldHour,
	).Scan(&retained); err != nil || retained != 100 {
		t.Fatalf("retained=%d error=%v", retained, err)
	}
}

func TestSummaryGrainsMigrationFailureKeepsPendingAndRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"insert", "delete", "update"} {
		if _, err := store.db.Exec("DROP TRIGGER summary_bucket_event_" + event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(
		"DROP TABLE summary_bucket; DELETE FROM meta WHERE key='summary_grains_" +
			"v1'; CREATE TABLE summary_bucket(id INTEGER PRIMARY KEY)",
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if failed, err := openStore(path); err == nil {
		_ = failed.Close()
		t.Fatal("broken migration was accepted")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var marker string
	if err := db.QueryRow(
		"SELECT value FROM meta WHERE key=?", summaryGrainsMigrationKey,
	).Scan(&marker); err != nil || marker != "pending" {
		t.Fatalf("marker=%q error=%v", marker, err)
	}
	if _, err := db.Exec("DROP TABLE summary_bucket"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	retried, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func(closeResource func() error) {
		if err := closeResource(); err != nil {
			t.Errorf("close retried summary store: %v", err)
		}
	}(retried.Close)
	if err := retried.db.QueryRow(
		"SELECT value FROM meta WHERE key=?", summaryGrainsMigrationKey,
	).Scan(&marker); err != nil || marker != "complete" {
		t.Fatalf("retry marker=%q error=%v", marker, err)
	}
}

func TestSummaryGrainsBackfillPreservesMetricsDimensionsAndBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	times := []int64{
		-nsPerDay - 1, -nsPerHour, -nsPerTenMinute - 1, -1, 0, 1,
		nsPerTenMinute - 1, nsPerTenMinute, nsPerTenMinute + 1,
		nsPerHour - 1, nsPerHour, nsPerHour + 1, nsPerDay - 1, nsPerDay, nsPerDay + 1,
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for i, timestamp := range times {
		for group := range 3 {
			detail := FlatDetail{
				Timestamp: time.Unix(0, timestamp), Model: fmt.Sprintf("legacy-%d", i%2),
				RequestID: fmt.Sprintf("event-%d-%d", i, group), Failed: (i+group)%2 != 0,
				Tokens: tokenStats{
					InputTokens: 11, OutputTokens: 7, ReasoningTokens: 2,
					CachedTokens: 4, CacheReadTokens: 3, CacheWriteTokens: 1, TotalTokens: 18,
				},
				LatencyMs: int64(i), latencyRecorded: i%2 == 0, costTrusted: true, costMicro: int64(i + group),
				CostStatus: []CostStatus{costStatusComplete, costStatusPartial, CostStatusUnpriced}[group],
			}
			if group > 0 {
				detail.Source, detail.AuthIndex, detail.APIKey = "shared", fmt.Sprintf("auth-%d", group), "client"
				detail.Provider = fmt.Sprintf("provider-%d", i%2)
			}
			if inserted, err := persistEvent(ctx, tx, store, &detail, nil); err != nil || !inserted {
				t.Fatalf("seed event %d/%d: inserted=%t error=%v", i, group, inserted, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := store.rollupDirtyDays(ctx); err != nil {
		t.Fatal(err)
	}

	readTable := func(table string) [][]any {
		t.Helper()
		rows, err := store.db.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var result [][]any
		for rows.Next() {
			values, targets := make([]any, len(columns)), make([]any, len(columns))
			for i := range targets {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				t.Fatal(err)
			}
			result = append(result, values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := make(map[string][][]any)
	for _, table := range []string{"events", "hour_bucket", "day_bucket", "summary_projection"} {
		before[table] = readTable(table)
	}
	ranges := [][2]int64{
		{-nsPerDay - 1, nsPerDay + 1}, {-nsPerHour, nsPerHour - 1},
		{-nsPerTenMinute + 1, nsPerTenMinute - 1}, {0, nsPerTenMinute},
		{1, nsPerHour + 1}, {nsPerDay - 1, nsPerDay + 1},
	}
	readSummary := func(bounds [2]int64) QueryResult {
		t.Helper()
		grain := "10m"
		if bounds[1]-bounds[0] > int64(maxAuthIndexTenMinuteRange) {
			grain = "hourly"
		}
		result, err := store.queryWithAuthIndexGranularity(
			ctx, time.Unix(0, bounds[0]), time.Unix(0, bounds[1]),
			EventFilters{}, "hourly", grain, true,
		)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	var expected []QueryResult
	for _, bounds := range ranges {
		result := readSummary(bounds)
		var requests, tokens, cost int64
		if err := store.db.QueryRow(
			`SELECT COUNT(*),COALESCE(SUM(total_tokens),0),COALESCE(SUM(cost_micro),0)
			FROM events WHERE ts_ns>=? AND ts_ns<=?`, bounds[0], bounds[1],
		).Scan(&requests, &tokens, &cost); err != nil {
			t.Fatal(err)
		}
		if result.Totals.Requests != requests || result.Totals.Tokens.TotalTokens != tokens ||
			result.Totals.Cost != float64(cost)/costMicroPerUSD {
			t.Fatalf("range=%v summary=%+v raw=%d/%d/%d", bounds, result.Totals, requests, tokens, cost)
		}
		expected = append(expected, result)
	}
	for _, event := range []string{"insert", "delete", "update"} {
		if _, err := store.db.Exec("DROP TRIGGER summary_bucket_event_" + event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec("DROP TABLE summary_bucket; DELETE FROM meta WHERE key='summary_grains_v1'"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	for restart := range 2 {
		store, err = openStore(path)
		if err != nil {
			t.Fatal(err)
		}
		for table, rows := range before {
			if got := readTable(table); !reflect.DeepEqual(got, rows) {
				t.Fatalf("restart %d changed retained %s", restart, table)
			}
		}
		for i, bounds := range ranges {
			if result := readSummary(bounds); !reflect.DeepEqual(result, expected[i]) {
				t.Fatalf("restart=%d range=%v summary changed: got=%+v want=%+v", restart, bounds, result, expected[i])
			}
		}
		// Compare every persisted metric and nullable dimension with the original grouped event calculation.
		keys := "g.grain_ns," + summaryGrainFloor("e.ts_ns", "g.grain_ns") +
			",e.model_id,e.credential_id,e.api_key_id,e.provider_id,e.auth_index_id"
		metrics := summaryEventMetrics("e.")
		for i := range metrics {
			metrics[i] = "SUM(" + metrics[i] + ")"
		}
		grouped := "SELECT " + keys + "," + strings.Join(metrics, ",") +
			" FROM events e CROSS JOIN " + summaryGrainsSQL + " g GROUP BY " + keys
		actual := "SELECT grain_ns,bucket_ts_ns,model_id,credential_id,api_key_id,provider_id,auth_index_id," +
			strings.Join(summaryMetricColumns, ",") + " FROM summary_bucket"
		var differences int
		if err := store.db.QueryRow(
			"WITH expected AS (" + grouped + ") SELECT " +
				"(SELECT COUNT(*) FROM (" + actual + " EXCEPT SELECT * FROM expected)) + " +
				"(SELECT COUNT(*) FROM (SELECT * FROM expected EXCEPT " + actual + "))",
		).Scan(&differences); err != nil || differences != 0 {
			t.Fatalf("restart=%d grouped metric/dimension differences=%d error=%v", restart, differences, err)
		}
		if restart == 1 {
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			detail := FlatDetail{
				Timestamp: time.Unix(0, 1), Model: "legacy-0", RequestID: "after-restart",
				Tokens:      tokenStats{InputTokens: 1, TotalTokens: 1},
				costTrusted: true, costMicro: 17, CostStatus: costStatusComplete,
			}
			if inserted, err := persistEvent(ctx, tx, store, &detail, nil); err != nil || !inserted {
				t.Fatalf("write after migration: inserted=%t error=%v", inserted, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			result := readSummary(ranges[0])
			prior := expected[0].Totals
			if result.Totals.Requests != prior.Requests+1 || result.Totals.Tokens.TotalTokens != prior.Tokens.TotalTokens+1 ||
				math.Round(result.Totals.Cost*costMicroPerUSD) != math.Round(prior.Cost*costMicroPerUSD)+17 {
				t.Fatalf("write after migration summary=%+v prior=%+v", result.Totals, prior)
			}
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSummaryGrainsBackfillOverflowRollsBackAndRetries(t *testing.T) {
	for _, metric := range []string{
		"input_tokens", "output_tokens", "reasoning_tokens", "cached_tokens", "cache_read_tokens",
		"cache_write_tokens", "total_tokens", "cost_micro", "latency_ms_total",
	} {
		t.Run(
			metric, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "events.db")
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				if _, err := db.Exec(schemaSQL + "; INSERT INTO dim_model(name) VALUES('overflow')"); err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				if err := ensureSummaryGrains(ctx, db); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(
					`INSERT INTO events(id,fingerprint,ts_ns,model_id,failed)
				VALUES(-7,zeroblob(20),-7200000000000,1,0)`,
				); err != nil {
					t.Fatal(err)
				}
				for _, event := range []string{"insert", "delete", "update"} {
					if _, err := db.Exec("DROP TRIGGER summary_bucket_event_" + event); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := db.Exec(
					"UPDATE meta SET value='pending' WHERE key=?", summaryGrainsMigrationKey,
				); err != nil {
					t.Fatal(err)
				}
				column, value := metric, "?"
				if metric == "latency_ms_total" {
					column, value = "metadata", "json_object('latency_ms',?)"
				}
				for i, value64 := range []int64{math.MaxInt64, 1} {
					if _, err := db.Exec(
						"INSERT INTO events(id,fingerprint,ts_ns,model_id,failed,"+column+") "+
							"VALUES(?,randomblob(20),?,1,0,"+value+")", (i+1)*9, i+1, value64,
					); err != nil {
						t.Fatal(err)
					}
				}
				if err := ensureSummaryGrains(ctx, db); err == nil || !strings.Contains(
					err.Error(), "backfill exact summary",
				) {
					t.Fatalf("overflow migration error=%v", err)
				}
				var marker string
				if err := db.QueryRow(
					"SELECT value FROM meta WHERE key=?", summaryGrainsMigrationKey,
				).Scan(&marker); err != nil || marker != "pending" {
					t.Fatalf("failed migration marker=%q error=%v", marker, err)
				}
				var buckets, requests, events int
				if err := db.QueryRow(
					"SELECT COUNT(*),SUM(requests) FROM summary_bucket WHERE bucket_ts_ns=-7200000000000",
				).Scan(&buckets, &requests); err != nil || buckets != 3 || requests != 3 {
					t.Fatalf("sentinel buckets=%d requests=%d error=%v", buckets, requests, err)
				}
				if err := db.QueryRow(
					"SELECT COUNT(*) FROM summary_bucket WHERE bucket_ts_ns<>-7200000000000",
				).Scan(&buckets); err != nil || buckets != 0 {
					t.Fatalf("partial migration buckets=%d error=%v", buckets, err)
				}
				if err := db.QueryRow("SELECT COUNT(*) FROM events").Scan(&events); err != nil || events != 3 {
					t.Fatalf("retained events=%d error=%v", events, err)
				}
				if _, err := db.Exec(
					"UPDATE events SET "+column+"="+value+" WHERE id=9", int64(math.MaxInt64-1),
				); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				retried, err := openStore(path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = retried.Close() }()
				for _, grain := range []int64{nsPerTenMinute, nsPerHour} {
					var amount int64
					if err := retried.db.QueryRow(
						"SELECT "+metric+" FROM summary_bucket WHERE grain_ns=? AND bucket_ts_ns=0", grain,
					).Scan(&amount); err != nil || amount != math.MaxInt64 {
						t.Fatalf("retry grain=%d amount=%d error=%v", grain, amount, err)
					}
				}
				if err := retried.db.QueryRow(
					"SELECT value FROM meta WHERE key=?", summaryGrainsMigrationKey,
				).Scan(&marker); err != nil || marker != "complete" {
					t.Fatalf("retry migration marker=%q error=%v", marker, err)
				}
			},
		)
	}
}

func TestSummaryAcceptsLargeInputWithBoundedOutput(t *testing.T) {
	store := openTestStore(t)
	dropSummaryGrainsForLegacyFixture(t, store.db)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, table := range []string{"dim_model", "dim_credential", "dim_api_key"} {
		count := 24
		if table == "dim_api_key" {
			count = 16
		}
		if _, err := store.db.ExecContext(
			ctx,
			"WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq "+
				"WHERE n<?) INSERT INTO "+
				table+
				"(name) SELECT 'dimension-'||n FROM seq",
			count,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(
		ctx, `WITH RECURSIVE days(n) AS (SELECT 0 UNION ALL SELECT n+1 FROM days WHE`+
			"RE n<10)\n INSERT INTO day_bucket(bucket_ts_ns,model_id,credential_id,"+
			"api_key_id,requests,success,total_tokens)\n SELECT ?+days.n*8640000000"+
			`0000,m.id,c.id,k.id,1,1,1 FROM days CROSS JOIN dim_model m CROSS JOIN `+
			`dim_credential c CROSS JOIN dim_api_key k`,
		base.UnixNano(),
	); err != nil {
		t.Fatal(err)
	}
	if err := ensureSummaryGrains(ctx, store.db); err != nil {
		t.Fatal(err)
	}
	if err := ensureSummaryProjections(ctx, store.db); err != nil {
		t.Fatal(err)
	}
	var buckets int
	if err := store.db.QueryRowContext(
		ctx, "SELECT COUNT(*) FROM day_bucket",
	).Scan(&buckets); err != nil || buckets != 101376 {
		t.Fatalf("historical buckets=%d error=%v", buckets, err)
	}
	result, err := store.querySummary(
		ctx, base, base.Add(11*24*time.Hour-time.Nanosecond), EventFilters{}, "daily", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Totals.Requests != 101376 || len(result.ByModel) != 24 ||
		len(result.ByCredential) != 24 ||
		len(result.ByAPIKey) != 16 {
		t.Fatalf("large summary=%+v error=%v", result.Totals, err)
	}
}

func TestImportRollsBackExactSummaryWithWholePayload(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.db.Exec(
		`CREATE TRIGGER reject_exact_summary BEFORE INSERT ON summary_bucket WH` +
			`EN (SELECT name FROM dim_model WHERE id=NEW.model_id)='bad' BEGIN SELE` +
			`CT RAISE(ABORT,'injected exact summary failure'); END`,
	); err != nil {
		t.Fatal(err)
	}
	_, err := importBytes(
		context.Background(), store, nil,
		[]byte((`[{"timestamp":"2026-11-01T10:15:00Z","model":"good","tokens":{"total_tokens":1}},{"timestamp"` +
			`:"2026-11-01T10:16:00Z","model":"bad","tokens":{"total_tokens":1}}]`)),
	)
	if err == nil {
		t.Fatal("summary failure accepted")
	}
	for _, table := range []string{"events", "hour_bucket", "day_bucket", "summary_bucket", "dim_model"} {
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows=%d error=%v", table, count, err)
		}
	}
}

func TestSummaryGrainsFollowRepricingAndExactTrim(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if _, err := importBytes(
		ctx, store, nil,
		[]byte(`[{"timestamp":"2026-11-01T10:15:00Z","model":"priced","auth_index":"au`+
			`th-a","tokens":{"total_tokens":1}},{"timestamp":"2026-11-01T10:25:00Z"`+
			`,"model":"priced","auth_index":"auth-a","tokens":{"total_tokens":2}}]`),
	); err != nil {
		t.Fatal(err)
	}
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
	result, err := store.queryWithAuthIndexGranularity(
		ctx, base.Add(14*time.Minute), base.Add(26*time.Minute), EventFilters{}, "hourly", "10m", true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Totals.Cost != 0.000014 || result.ByAuthIndex["auth-a"].Cost != 0.000014 {
		t.Fatalf("repriced=%+v error=%v", result.Totals, err)
	}
	persister := &Persister{store: store, retention: config.UsageRetention{Days: 1}}
	if err := persister.trimBefore(base.Add(20 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	result, err = store.queryWithAuthIndexGranularity(
		ctx, base, base.Add(time.Hour), EventFilters{}, "hourly", "10m", true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Totals.Requests != 1 || result.Totals.Tokens.TotalTokens != 2 ||
		result.Totals.Cost != 0.000007 {
		t.Fatalf("trimmed=%+v error=%v", result.Totals, err)
	}
}

func TestSummarySupportsThousandsOfCredentialsWithoutGroupedSeries(t *testing.T) {
	store := openTestStore(t)
	dropSummaryGrainsForLegacyFixture(t, store.db)
	ctx := context.Background()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.db.ExecContext(
		ctx,
		"INSERT INTO dim_model(name) VALUES('model'); WITH RECURSIVE seq(n) AS(SELECT 1 UNION ALL SELECT n+1 "+
			"FROM seq WHERE n<2048) INSERT INTO dim_credential(name) SELECT 'credential-'||n FROM seq",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(
		ctx,
		`WITH RECURSIVE days(n) AS(SELECT 0 UNION ALL SELECT n+1 FROM days WHER`+
			`E n<364) INSERT INTO day_bucket(bucket_ts_ns,model_id,credential_id,re`+
			`quests,success,total_tokens) SELECT ?+days.n*86400000000000,1,c.id,1,1`+
			`,1 FROM days CROSS JOIN dim_credential c`,
		base.UnixNano(),
	); err != nil {
		t.Fatal(err)
	}
	if err := ensureSummaryGrains(ctx, store.db); err != nil {
		t.Fatal(err)
	}
	if err := ensureSummaryProjections(ctx, store.db); err != nil {
		t.Fatal(err)
	}
	var buckets int
	if err := store.db.QueryRowContext(
		ctx, "SELECT COUNT(*) FROM day_bucket",
	).Scan(&buckets); err != nil || buckets != 747520 {
		t.Fatalf("historical buckets=%d error=%v", buckets, err)
	}
	result, err := store.querySummary(
		ctx, base, base.Add(365*24*time.Hour-time.Nanosecond), EventFilters{}, "daily", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Totals.Requests != 747520 || len(result.ByCredential) != 2048 {
		t.Fatalf("requests=%d credentials=%d error=%v", result.Totals.Requests, len(result.ByCredential), err)
	}
}

func TestSummaryQueryPlanKeepsSelectiveDimensionIndexes(t *testing.T) {
	store := openTestStore(t)
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	to := from + 365*nsPerDay
	source, err := summaryRangeSource(nsPerDay, nsPerDay, from, to, false)
	if err != nil {
		t.Fatal(err)
	}
	metrics := make([]string, 0, len(summaryMetricColumns))
	for _, metric := range summaryMetricColumns {
		metrics = append(metrics, "SUM(b."+metric+")")
	}
	query := "EXPLAIN QUERY PLAN SELECT COALESCE(k.name,'(none)')," + strings.Join(
		metrics, ",",
	) + " FROM " + source + (" b LEFT JOIN dim_api_key k ON k.id=b.api_key_id WHERE b.bucket_ts_ns>=" +
		"? AND b.bucket_ts_ns<=? AND b.model_id=? AND b.credential_id=? AND b.a" +
		"pi_key_id=? AND b.provider_id=? GROUP BY COALESCE(k.name,'(none)')")
	rows, err := store.db.Query(query, from, to, 8, 8, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer func(closeResource func() error) {
		if err := closeResource(); err != nil {
			t.Errorf("close query plan rows: %v", err)
		}
	}(rows.Close)
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Log(detail)
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"hour_bucket", "day_bucket"} {
		if !strings.Contains(plan.String(), "idx_"+table+"_dimensions_time") {
			t.Fatalf("selective query lost %s dimension index:\n%s", table, plan.String())
		}
	}
}

func BenchmarkSummaryUnfilteredLarge(b *testing.B) {
	path := os.Getenv("CPA_USAGE_BENCHMARK_DB")
	if path == "" {
		b.Skip("set CPA_USAGE_BENCHMARK_DB to a retained 730-day, 8192-bucket/day synthetic fixture")
	}
	store, err := openStore(path)
	if err != nil {
		b.Fatal(err)
	}
	defer func(closeResource func() error) {
		if err := closeResource(); err != nil {
			b.Errorf("close benchmark summary store: %v", err)
		}
	}(store.Close)
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, days := range []int{365, 730} {
		b.Run(
			fmt.Sprintf("%dd", days), func(b *testing.B) {
				to := from.AddDate(0, 0, days).Add(-time.Nanosecond)
				var rows int64
				if err := store.db.QueryRow(
					"SELECT COUNT(*) FROM day_bucket WHERE bucket_ts_ns>=? AND bucket_ts_ns<=?", from.UnixNano(),
					to.UnixNano(),
				).Scan(&rows); err != nil || rows != int64(days*8192) {
					b.Fatalf("fixture rows=%d want=%d error=%v", rows, days*8192, err)
				}
				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					result, err := store.querySummary(ctx, from, to, EventFilters{}, "daily", false)
					cancel()
					if err != nil || result.Totals.Requests != rows*200 || len(result.ByModel) != 16 ||
						len(result.ByCredential) != 2048 ||
						len(result.ByAPIKey) != 4 ||
						len(result.TimeSeries) != days {
						b.Fatalf(
							"unfiltered %dd requests=%d models=%d credentials=%d keys=%d points=%d error=%v",
							days,
							result.Totals.Requests,
							len(result.ByModel),
							len(result.ByCredential),
							len(result.ByAPIKey),
							len(result.TimeSeries),
							err,
						)
					}
				}
			},
		)
	}
}

func TestSummaryRetainsDayOnlyHistoryWithoutInventingHourlyPrecision(t *testing.T) {
	store := openTestStore(t)
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx := context.Background()
	if _, err := store.db.Exec(
		"INSERT INTO dim_model(name) VALUES('historical'); INSERT INTO day_buck" +
			"et(bucket_ts_ns,model_id,requests,success) VALUES(1735689600000000000," +
			"1,10,10)",
	); err != nil {
		t.Fatal(err)
	}
	result, err := store.querySummary(
		ctx,
		base,
		base.Add(24*time.Hour-time.Nanosecond),
		EventFilters{},
		"daily",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Totals.Requests != 10 {
		t.Fatalf("daily=%+v error=%v", result.Totals, err)
	}
	for _, grain := range []string{"hourly", "daily"} {
		if _, err := store.querySummary(
			ctx, base.Add(time.Hour), base.Add(2*time.Hour), EventFilters{}, grain, false,
		); !errors.Is(err, ErrSummaryPrecisionUnavailable) {
			t.Fatalf("grain=%s precision error=%v", grain, err)
		}
	}
}
