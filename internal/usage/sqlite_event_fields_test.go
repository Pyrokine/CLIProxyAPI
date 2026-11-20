package usage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func removeEventFields(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, event := range []string{"insert", "delete", "metadata_update", "core_update", "cost_update"} {
		if _, err := db.Exec("DROP TRIGGER IF EXISTS event_fields_" + event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("DROP TABLE event_fields; DELETE FROM meta WHERE key IN ('event_fields_version','event_fields_cursor','event_fields_task_v1')"); err != nil {
		t.Fatal(err)
	}
}

func eventStorageDigests(t *testing.T, db *sql.DB) map[string][32]byte {
	t.Helper()
	result := make(map[string][32]byte)
	for _, table := range []string{
		"events", "hour_bucket", "day_bucket", "summary_bucket", "summary_projection",
		"dim_model", "dim_source", "dim_auth_index", "dim_api_key", "dim_credential", "dim_provider",
		"request_log_artifact",
	} {
		rows, err := db.Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var encoded []string
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			encoded = append(encoded, string(data))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		slices.Sort(encoded)
		result[table] = sha256.Sum256([]byte(strings.Join(encoded, "\n")))
	}
	return result
}

func seedEventFieldsHistory(t *testing.T, store *sqliteStore, count int) time.Time {
	t.Helper()
	removeEventFields(t, store.db)
	base := time.Date(2026, 10, 20, 8, 53, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range count {
		detail := FlatDetail{
			Timestamp: base.Add(time.Duration(i) * time.Second), Model: "history-model", Source: "history-source",
			AuthIndex: "history-auth", APIKey: "history-client", Provider: "codex", Failed: i%3 == 0,
			Tokens: tokenStats{
				InputTokens: 17, OutputTokens: 13, ReasoningTokens: 3, CachedTokens: 5,
				CacheReadTokens: 2, CacheWriteTokens: 3, TotalTokens: 30,
			},
			RequestID: fmt.Sprintf("history-request-%d", i), ReasoningEffort: "high", ResponseServiceTier: "default",
			LatencyMs: int64(i + 1), latencyRecorded: true,
			costTrusted: true, costMicro: int64(i+1) * 10, CostStatus: costStatusComplete,
		}
		if i == 0 {
			detail = speedPricingDetail()
			detail.Timestamp, detail.AuthIndex, detail.Source = base, "history-auth", "history-source"
			detail.RequestID, detail.ReasoningEffort = "history-request-0", "high"
			_, detail.PricingReceipt, err = buildPricingReceipt(
				&detail, detailedPricingSnapshot(speedPricingPrice(), "request-time"), time.Now(),
			)
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := persistEventWithPricing(t.Context(), tx, store, &detail, pricingSnapshot{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestEventFieldsUpgradePreservesCanonicalHistoryAndResumes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	const events = eventFieldsPageSize*2 + 17
	base := seedEventFieldsHistory(t, store, events)
	before := eventStorageDigests(t, store.db)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM event_fields").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("startup performed historical backfill rows=%d error=%v", rows, err)
	}
	checkPage := func() {
		t.Helper()
		for _, compact := range []bool{false, true} {
			query := store.queryEventDetails
			if compact {
				query = store.queryCompactEventDetails
			}
			details, total, err := query(
				t.Context(), base, base.Add(30*24*time.Hour), EventFilters{Search: "history-auth"}, 1, 50, "timestamp",
				true,
			)
			if err != nil || total != events || len(details) != 50 {
				t.Fatalf("compact=%t total=%d rows=%d error=%v", compact, total, len(details), err)
			}
			for _, detail := range details {
				if detail.ReasoningEffort != "high" || detail.AuthIndex != "history-auth" {
					t.Fatalf("detail=%+v", detail)
				}
			}
		}
	}
	checkPage()
	if _, err := store.db.Exec(
		`CREATE TRIGGER reject_event_backfill_update BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT,'history update'); END;
CREATE TRIGGER reject_event_backfill_delete BEFORE DELETE ON events BEGIN SELECT RAISE(ABORT,'history delete'); END;`,
	); err != nil {
		t.Fatal(err)
	}
	if done, err := store.backfillEventFieldsPage(t.Context()); err != nil || done {
		t.Fatalf("first page done=%t error=%v", done, err)
	}
	var cursor string
	if err := store.db.QueryRow(
		"SELECT value FROM meta WHERE key=?", eventFieldsCursorKey,
	).Scan(&cursor); err != nil || cursor != "128" {
		t.Fatalf("cursor=%q error=%v", cursor, err)
	}
	checkPage()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.backfillEventFieldsPage(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled page error=%v", err)
	}
	if _, err := store.db.Exec("CREATE TRIGGER reject_event_fields_page BEFORE INSERT ON event_fields WHEN NEW.id>128 BEGIN SELECT RAISE(ABORT,'injected upgrade failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.backfillEventFieldsPage(t.Context()); err == nil {
		t.Fatal("upgrade accepted injected failure")
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM event_fields").Scan(&rows); err != nil || rows != 128 {
		t.Fatalf("rolled-back rows=%d error=%v", rows, err)
	}
	if err := store.db.QueryRow(
		"SELECT value FROM meta WHERE key=?", eventFieldsCursorKey,
	).Scan(&cursor); err != nil || cursor != "128" {
		t.Fatalf("failed page cursor=%q error=%v", cursor, err)
	}
	if _, err := store.db.Exec("DROP TRIGGER reject_event_fields_page"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, err := store.backfillEventFieldsPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	checkPage()
	if got := eventStorageDigests(t, store.db); !reflect.DeepEqual(got, before) {
		t.Fatalf("canonical data changed: got=%v want=%v", got, before)
	}
	if err := store.db.QueryRow(
		"SELECT value FROM meta WHERE key=?", eventFieldsCursorKey,
	).Scan(&cursor); err != nil || cursor != "complete" {
		t.Fatalf("complete cursor=%q error=%v", cursor, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	checkPage()
	var schema, layout string
	if err := store.db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT value FROM meta WHERE key=?", eventFieldsVersionKey).Scan(&layout); err != nil {
		t.Fatal(err)
	}
	if schema != "9" || layout != eventFieldsVersion || !strings.Contains(buildinfo.DataFormats, "usage=1:9:9") {
		t.Fatalf("canonical schema=%q layout=%q declaration=%q", schema, layout, buildinfo.DataFormats)
	}
}

func TestEventFieldsPricingMetadataPreservesLegacyDecoding(t *testing.T) {
	store := openTestStore(t)
	price := pricingSnapshot{priceFn: func(string) (float64, float64, float64, bool) { return 1, 2, 0.5, true }}
	cases := []sql.NullString{
		{}, {String: "", Valid: true}, {String: " ", Valid: true}, {String: "{broken", Valid: true},
	}
	for _, raw := range []string{
		`null`, `123`, `[]`, `true`, `"text"`, `{}`, `{"payload":"large","response_service_tier":"priority"}`,
		`{"request_speed":"fast","response_service_tier":"priority"}`, `{"Response_Service_Tier":"priority"}`,
		`{"response_service_tier":"priority","RESPONSE_SERVICE_TIER":"default"}`,
		`{"response_service_tier":"default","response_service_tier":"priority"}`,
		`{"response_speed":1}`, `{"response_speed":true}`, `{"response_speed":{}}`, `{"accounting_version":1.0}`,
		`{"accounting_version":1e2}`,
		`{"accounting_version":true}`, `{"token_breakdown":true}`, `{"accounting_version":2,"token_breakdown":{}}`,
		`{"executor_type":"ClaudeExecutor","response_speed":"ultrafast"}`,
	} {
		cases = append(cases, sql.NullString{String: raw, Valid: true})
	}
	for _, raw := range cases {
		t.Run(
			raw.String, func(t *testing.T) {
				var value any
				if raw.Valid {
					value = raw.String
				}
				var projected sql.NullString
				if err := store.db.QueryRow(
					"SELECT "+eventPricingMetadataSQL("events.")+" FROM (SELECT ? AS metadata) events", value,
				).Scan(&projected); err != nil {
					t.Fatal(err)
				}
				tokens := tokenStats{InputTokens: 10, TotalTokens: 10}
				want, wantErr := computeCurrentCatalogEventCost(tokens, nil, price, "model", "codex", time.Now(), raw)
				got, gotErr := computeCurrentCatalogEventCost(
					tokens, nil, price, "model", "codex", time.Now(), projected,
				)
				if (wantErr == nil) != (gotErr == nil) || got != want {
					t.Fatalf(
						"raw=%q projected=%q cost=%+v/%v want=%+v/%v", raw.String, projected.String, got, gotErr, want,
						wantErr,
					)
				}
			},
		)
	}
}

func TestEventFieldsUpdatesAndFailuresRemainAtomic(t *testing.T) {
	store := openTestStore(t)
	ctx := t.Context()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	detail := speedPricingDetail()
	detail.ReasoningEffort = "high"
	detail.RequestID = "atomic-event"
	_, detail.PricingReceipt, err = buildPricingReceipt(
		&detail, detailedPricingSnapshot(speedPricingPrice(), "original"), time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := persistEventWithPricing(
		ctx, tx, store, &detail, detailedPricingSnapshot(speedPricingPrice(), "original"),
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	before := eventStorageDigests(t, store.db)
	if _, err := store.db.Exec("CREATE TRIGGER fail_field_cost BEFORE UPDATE OF cost_micro ON event_fields BEGIN SELECT RAISE(ABORT,'field cost failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE events SET cost_micro=cost_micro+1"); err == nil {
		t.Fatal("event update bypassed projection failure")
	}
	if got := eventStorageDigests(t, store.db); !reflect.DeepEqual(got, before) {
		t.Fatal("failed projection changed canonical data")
	}
	if _, err := store.db.Exec("DROP TRIGGER fail_field_cost; CREATE TRIGGER reject_field_metadata_reprice BEFORE UPDATE OF metadata_state,observed_speed,pricing_metadata,request_speed,response_speed,ts_ns,model_id,api_key_id,source_id,auth_index_id,credential_id,provider_id,failed,total_tokens ON event_fields BEGIN SELECT RAISE(ABORT,'reparsed metadata'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE events SET cost_micro=cost_micro+1"); err != nil {
		t.Fatal(err)
	}
	var actual, projected int64
	if err := store.db.QueryRow("SELECT events.cost_micro,event_fields.cost_micro FROM events JOIN event_fields USING(id)").Scan(
		&actual, &projected,
	); err != nil || actual != projected {
		t.Fatalf("cost=%d projected=%d error=%v", actual, projected, err)
	}
	if _, err := store.db.Exec("DROP TRIGGER reject_field_metadata_reprice; UPDATE events SET metadata=json_set(metadata,'$.response_service_tier','priority')"); err != nil {
		t.Fatal(err)
	}
	var speed, effort string
	if err := store.db.QueryRow("SELECT observed_speed,reasoning_effort FROM event_fields").Scan(
		&speed, &effort,
	); err != nil || speed != "fast" || effort != "high" {
		t.Fatalf("speed=%q effort=%q error=%v", speed, effort, err)
	}
	if _, err := store.db.Exec("UPDATE events SET pricing_receipt=NULL"); err == nil {
		t.Fatal("projection upgrade weakened receipt immutability")
	}
	if _, err := store.db.Exec("DELETE FROM events"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM event_fields").Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted projection rows=%d error=%v", count, err)
	}
}

func TestEventFieldsUpgradeRepricingAndLiveWriter(t *testing.T) {
	store := openTestStore(t)
	const history = eventFieldsPageSize*2 + 17
	base := seedEventFieldsHistory(t, store, history)
	if err := ensureEventFields(t.Context(), store.db); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	stopUpgrade := store.startEventFieldsUpgrade(ctx)
	defer func() { cancel(); stopUpgrade() }()
	price := func(string) (float64, float64, float64, bool) { return 2, 4, 1, true }
	writer := newWriter(store, price)
	writer.start(ctx)
	defer writer.Stop()
	liveDone := make(chan struct{})
	go func() {
		defer close(liveDone)
		for i := range 200 {
			if !writer.submit(
				FlatDetail{
					Timestamp: base.Add(time.Hour + time.Duration(i)*time.Second), Model: "live-model",
					AuthIndex: "live-auth", ReasoningEffort: "high", RequestID: fmt.Sprintf("live-%d", i),
					Tokens: tokenStats{InputTokens: 10, TotalTokens: 10},
				},
			) {
				return
			}
		}
		writer.Stop()
	}()
	persister := &Persister{store: store, pricesFn: price, maintenanceCtx: ctx}
	repriceDone := make(chan error, 1)
	go func() { _, err := persister.recalculateCostsIncrementalContext(ctx); repriceDone <- err }()
	for {
		details, total, err := store.queryCompactEventDetails(
			ctx, base, base.Add(30*24*time.Hour), EventFilters{Search: "history-auth"}, 1, 50, "timestamp", true,
		)
		if err != nil || total != history || len(details) != 50 {
			t.Fatalf("online count=%d rows=%d error=%v", total, len(details), err)
		}
		progress, progressErr := persister.StorageUpgradeStatusContext(ctx)
		if progressErr != nil || progress.ProcessedEvents == nil || progress.TotalEvents == nil ||
			*progress.TotalEvents < history || *progress.TotalEvents > history+200 ||
			*progress.ProcessedEvents > *progress.TotalEvents {
			t.Fatalf("concurrent progress=%+v error=%v", progress, progressErr)
		}
		select {
		case err := <-repriceDone:
			if err != nil {
				t.Fatal(err)
			}
			goto repriced
		default:
		}
	}
repriced:
	select {
	case <-liveDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stopUpgrade()
	var count, mismatched int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&count); err != nil || count != history+200 {
		t.Fatalf("events=%d error=%v", count, err)
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM events e LEFT JOIN event_fields f ON f.id=e.id WHERE f.id IS NULL OR f.ts_ns<>e.ts_ns OR f.total_tokens<>e.total_tokens OR f.cost_micro<>e.cost_micro OR f.cost_status<>e.cost_status").Scan(&mismatched); err != nil || mismatched != 0 {
		t.Fatalf("mismatched=%d error=%v", mismatched, err)
	}
}

func TestEventFieldsSchemaReadableByExistingSQLiteTools(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python sqlite3 administration consumer is unavailable")
	}
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedEventFieldsHistory(t, store, 4)
	if err := ensureEventFields(t.Context(), store.db); err != nil {
		t.Fatal(err)
	}
	for {
		done, err := store.backfillEventFieldsPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	const script = `import json, sqlite3, sys
with sqlite3.connect('file:' + sys.argv[1] + '?mode=ro', uri=True) as db:
    db.execute('PRAGMA query_only=1')
    assert db.execute("SELECT value FROM meta WHERE key='schema_version'").fetchone() == ('9',)
    assert db.execute("SELECT value FROM meta WHERE key='event_fields_version'").fetchone() == ('1',)
    assert db.execute("SELECT value FROM meta WHERE key='event_fields_cursor'").fetchone() == ('complete',)
    assert db.execute('SELECT COUNT(*) FROM events').fetchone() == (4,)
    assert db.execute('SELECT COUNT(*) FROM event_fields').fetchone() == (4,)
    assert db.execute('SELECT COUNT(*) FROM events e JOIN event_fields f ON f.id=e.id WHERE e.ts_ns<>f.ts_ns OR e.cost_micro<>f.cost_micro').fetchone() == (0,)
    assert db.execute("SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'event_fields_%'").fetchone() == (5,)
print(json.dumps({'sqlite_version': sqlite3.sqlite_version, 'schema_readable': True, 'canonical_rows': 4}))
`
	output, err := exec.CommandContext(t.Context(), python, "-I", "-c", script, path).CombinedOutput()
	if err != nil {
		t.Fatalf("existing SQLite administration consumer: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}

func assertStorageUpgradeCounts(
	t *testing.T,
	store *sqliteStore,
	wantStatus string,
	processed, total int64,
) StorageUpgradeStatus {
	t.Helper()
	status, err := store.queryStorageUpgradeStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != wantStatus || status.Running != (wantStatus == "running") ||
		status.ProcessedEvents == nil || *status.ProcessedEvents != processed ||
		status.TotalEvents == nil || *status.TotalEvents != total || status.Percent == nil {
		t.Fatalf("status=%+v want=%s %d/%d", status, wantStatus, processed, total)
	}
	wantPercent := float64(0)
	if total > 0 {
		wantPercent = float64(processed) * 100 / float64(total)
	} else if wantStatus == "complete" {
		wantPercent = 100
	}
	if *status.Percent != wantPercent {
		t.Fatalf("percent=%v want=%v", *status.Percent, wantPercent)
	}
	return status
}

func TestStorageUpgradeStatusCountsGapsDeletesAndOnlineWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	base := seedEventFieldsHistory(t, store, 273)
	if _, err := store.db.Exec("UPDATE events SET id=-id; UPDATE events SET id=-id*10"); err != nil {
		t.Fatal(err)
	}
	if err := ensureEventFields(t.Context(), store.db); err != nil {
		t.Fatal(err)
	}
	initial := assertStorageUpgradeCounts(t, store, "idle", 0, 273)
	encoded, err := json.Marshal(initial)
	if err != nil || !strings.Contains(string(encoded), `"processed_events":0`) || initial.StartedAt != "" {
		t.Fatalf("initial JSON=%s error=%v", encoded, err)
	}
	if done, err := store.backfillEventFieldsPage(t.Context()); err != nil || done {
		t.Fatalf("first page done=%t error=%v", done, err)
	}
	status := assertStorageUpgradeCounts(t, store, "running", 128, 273)
	if status.StartedAt == "" || status.UpdatedAt == "" || status.CompletedAt != "" {
		t.Fatalf("running times=%+v", status)
	}
	if _, err := store.db.Exec("DELETE FROM events WHERE id IN (10,2730)"); err != nil {
		t.Fatal(err)
	}
	assertStorageUpgradeCounts(t, store, "running", 127, 271)
	tx, err := store.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	detail := FlatDetail{
		Timestamp: base.Add(time.Hour), Model: "live", RequestID: "live-status",
		Tokens: tokenStats{InputTokens: 1, TotalTokens: 1},
	}
	if _, err := persistEvent(t.Context(), tx, store, &detail, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertStorageUpgradeCounts(t, store, "running", 128, 272)
	for {
		done, err := store.backfillEventFieldsPage(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	completed := assertStorageUpgradeCounts(t, store, "complete", 272, 272)
	started, errStart := time.Parse(time.RFC3339Nano, completed.StartedAt)
	finished, errEnd := time.Parse(time.RFC3339Nano, completed.CompletedAt)
	if errStart != nil || errEnd != nil || finished.Before(started) || completed.UpdatedAt != completed.CompletedAt || completed.StartedAt != status.StartedAt {
		t.Fatalf("completed times=%+v parse=%v/%v", completed, errStart, errEnd)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	stop := store.startEventFieldsUpgrade(t.Context())
	stop()
	reopened := assertStorageUpgradeCounts(t, store, "complete", 272, 272)
	if reopened.StartedAt != completed.StartedAt || reopened.CompletedAt != completed.CompletedAt || reopened.UpdatedAt != completed.UpdatedAt {
		t.Fatalf("reopen changed recorded times: %+v -> %+v", completed, reopened)
	}
}

func TestStorageUpgradeStatusPreservesUnknownLegacyTimes(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("complete=%t", complete), func(t *testing.T) {
				store := openTestStore(t)
				seedEventFieldsHistory(t, store, 1)
				if err := ensureEventFields(t.Context(), store.db); err != nil {
					t.Fatal(err)
				}
				if complete {
					for {
						done, err := store.backfillEventFieldsPage(t.Context())
						if err != nil {
							t.Fatal(err)
						}
						if done {
							break
						}
					}
				}
				if _, err := store.db.Exec("DELETE FROM meta WHERE key=?", eventFieldsTaskKey); err != nil {
					t.Fatal(err)
				}
				stop := store.startEventFieldsUpgrade(t.Context())
				stop()
				status := assertStorageUpgradeCounts(t, store, "complete", 1, 1)
				if status.StartedAt != "" || complete && (status.CompletedAt != "" || status.UpdatedAt != "") || !complete && status.CompletedAt == "" {
					t.Fatalf("fabricated or missing observed times: %+v", status)
				}
			},
		)
	}
}

func TestStorageUpgradeStatusPublishesFailureCancellationAndResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	seedEventFieldsHistory(t, store, 273)
	if err := ensureEventFields(t.Context(), store.db); err != nil {
		t.Fatal(err)
	}
	if _, err := store.backfillEventFieldsPage(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("CREATE TRIGGER fail_upgrade BEFORE INSERT ON event_fields WHEN NEW.id>128 BEGIN SELECT RAISE(ABORT,'injected page failure'); END"); err != nil {
		t.Fatal(err)
	}
	stop := store.startEventFieldsUpgrade(t.Context())
	stop()
	failed := assertStorageUpgradeCounts(t, store, "error", 128, 273)
	if !strings.Contains(failed.Error, "injected page failure") || failed.UpdatedAt == "" || failed.CompletedAt != "" {
		t.Fatalf("failed status=%+v", failed)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted := assertStorageUpgradeCounts(t, store, "error", 128, 273)
	if persisted.Error != failed.Error || persisted.StartedAt != failed.StartedAt {
		t.Fatalf("failure was not retained: %+v -> %+v", failed, persisted)
	}
	if _, err := store.db.Exec("DROP TRIGGER fail_upgrade"); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	stop = store.startEventFieldsUpgrade(canceled)
	stop()
	interrupted := assertStorageUpgradeCounts(t, store, "error", 128, 273)
	if !strings.Contains(
		interrupted.Error, context.Canceled.Error(),
	) || interrupted.StartedAt != failed.StartedAt || interrupted.CompletedAt != "" {
		t.Fatalf("interrupted status=%+v", interrupted)
	}
	stop = store.startEventFieldsUpgrade(t.Context())
	stop()
	recovered := assertStorageUpgradeCounts(t, store, "complete", 273, 273)
	if recovered.Error != "" || recovered.StartedAt != failed.StartedAt || recovered.CompletedAt == "" {
		t.Fatalf("recovered status=%+v", recovered)
	}
}

func TestStorageUpgradeStatusRetainsFailureWhenMetaCannotBeWritten(t *testing.T) {
	store := openTestStore(t)
	seedEventFieldsHistory(t, store, 273)
	if err := ensureEventFields(t.Context(), store.db); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		`CREATE TRIGGER fail_upgrade BEFORE INSERT ON event_fields BEGIN SELECT RAISE(ABORT,'injected capacity failure'); END;
	CREATE TRIGGER fail_status BEFORE INSERT ON meta WHEN NEW.key='event_fields_task_v1' AND json_extract(NEW.value,'$.status')='error' BEGIN SELECT RAISE(ABORT,'status write unavailable'); END;`,
	); err != nil {
		t.Fatal(err)
	}
	stop := store.startEventFieldsUpgrade(t.Context())
	stop()
	status := assertStorageUpgradeCounts(t, store, "error", 0, 273)
	if !strings.Contains(status.Error, "injected capacity failure") || status.UpdatedAt == "" {
		t.Fatalf("failure disappeared after meta write failure: %+v", status)
	}
	if _, err := store.db.Exec("DROP TRIGGER fail_upgrade; DROP TRIGGER fail_status"); err != nil {
		t.Fatal(err)
	}
	stop = store.startEventFieldsUpgrade(t.Context())
	stop()
	assertStorageUpgradeCounts(t, store, "complete", 273, 273)
}

func TestStorageUpgradeStatusReportsSQLiteFullAndResumes(t *testing.T) {
	store := openTestStore(t)
	seedEventFieldsHistory(t, store, 273)
	if err := ensureEventFields(t.Context(), store.db); err != nil {
		t.Fatal(err)
	}
	var pageCount, pageSize int64
	if err := store.db.QueryRow("PRAGMA page_count").Scan(&pageCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := store.setMaxSizeBytes(t.Context(), pageCount*pageSize); err != nil {
		t.Fatal(err)
	}
	stop := store.startEventFieldsUpgrade(t.Context())
	stop()
	status := assertStorageUpgradeCounts(t, store, "error", 0, 273)
	if !strings.Contains(status.Error, "database or disk is full") || status.CompletedAt != "" {
		t.Fatalf("SQLite FULL status=%+v", status)
	}
	t.Log(status.Error)
	if err := store.setMaxSizeBytes(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	stop = store.startEventFieldsUpgrade(t.Context())
	stop()
	assertStorageUpgradeCounts(t, store, "complete", 273, 273)
}

func TestStorageUpgradeStatusShowsActiveResumeBeforeFirstCommit(t *testing.T) {
	store := openTestStore(t)
	seedEventFieldsHistory(t, store, 1)
	if err := ensureEventFields(t.Context(), store.db); err != nil {
		t.Fatal(err)
	}
	store.recordEventFieldsUpgradeError(errors.New("previous upgrade failure"))
	assertStorageUpgradeCounts(t, store, "error", 0, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	store.eventFieldsCapacityCheck = func() error { close(entered); <-release; return nil }
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stop := store.startEventFieldsUpgrade(ctx)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		stop()
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	active := assertStorageUpgradeCounts(t, store, "running", 0, 1)
	if active.Error != "" || active.StartedAt != "" || active.CompletedAt != "" {
		t.Fatalf("active worker exposed old failure or fabricated times: %+v", active)
	}
	close(release)
	stop()
	assertStorageUpgradeCounts(t, store, "complete", 1, 1)
	if store.eventFieldsRunning.Load() {
		t.Fatal("completed worker retained its active flag")
	}
}

func TestStorageUpgradeCompletesEmptyTailAtCapacityReserve(t *testing.T) {
	store := openTestStore(t)
	seedEventFieldsHistory(t, store, 1)
	if err := ensureEventFields(t.Context(), store.db); err != nil {
		t.Fatal(err)
	}
	if _, err := store.backfillEventFieldsPage(t.Context()); err != nil {
		t.Fatal(err)
	}
	store.eventFieldsCapacityCheck = func() error {
		t.Error("empty completion page requested data capacity")
		return errors.New("reserve capacity reached")
	}
	if done, err := store.backfillEventFieldsPage(t.Context()); err != nil || !done {
		t.Fatalf("empty completion page done=%t error=%v", done, err)
	}
	status := assertStorageUpgradeCounts(t, store, "complete", 1, 1)
	if status.CompletedAt == "" {
		t.Fatalf("completion time missing: %+v", status)
	}
}

func TestStorageUpgradeCapacityReserveScalesAndCaps(t *testing.T) {
	for _, limitMB := range []int{1, 64, 10240, 16384} {
		t.Run(
			fmt.Sprintf("limit=%dMiB", limitMB), func(t *testing.T) {
				p := NewPersister(t.TempDir(), config.UsageRetention{MaxDBSizeMB: limitMB})
				f, err := os.Create(filepath.Join(p.baseDir, dbFileName))
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				maximum := int64(limitMB) << 20
				reserve := min(maximum/100, int64(64<<20))
				if err := f.Truncate(maximum - reserve - 1); err != nil {
					t.Fatal(err)
				}
				if err := p.checkEventFieldsUpgradeCapacity(); err != nil {
					t.Fatalf("below reserve boundary: %v", err)
				}
				if err := f.Truncate(maximum - reserve); err != nil {
					t.Fatal(err)
				}
				if err := p.checkEventFieldsUpgradeCapacity(); err == nil || !strings.Contains(
					err.Error(), fmt.Sprintf("reserve %d bytes", reserve),
				) {
					t.Fatalf("at reserve boundary: %v", err)
				}
			},
		)
	}
}

func TestStorageUpgradeCapacityMeasurementFailureRetainsCommittedPage(t *testing.T) {
	store := openTestStore(t)
	seedEventFieldsHistory(t, store, 273)
	if err := ensureEventFields(t.Context(), store.db); err != nil {
		t.Fatal(err)
	}
	if _, err := store.backfillEventFieldsPage(t.Context()); err != nil {
		t.Fatal(err)
	}
	original := statUsageFile
	statUsageFile = func(string) (os.FileInfo, error) { return nil, os.ErrPermission }
	defer func() { statUsageFile = original }()
	p := NewPersister(t.TempDir(), config.UsageRetention{MaxDBSizeMB: 64})
	store.eventFieldsCapacityCheck = p.checkEventFieldsUpgradeCapacity
	stop := store.startEventFieldsUpgrade(t.Context())
	stop()
	status := assertStorageUpgradeCounts(t, store, "error", 128, 273)
	if !strings.Contains(status.Error, "measure storage upgrade capacity") || !strings.Contains(
		status.Error, os.ErrPermission.Error(),
	) {
		t.Fatalf("capacity measurement error=%+v", status)
	}
}

func TestStorageUpgradeYieldsCapacityToLiveWriterAndResumes(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := seedEventFieldsHistory(t, store, 273)
	if err := ensureEventFields(t.Context(), store.db); err != nil {
		t.Fatal(err)
	}
	if _, err := store.backfillEventFieldsPage(t.Context()); err != nil {
		t.Fatal(err)
	}
	const limit = int64(64 << 20)
	const reserve = limit / 100
	p := NewPersister(dir, config.UsageRetention{Days: -1, MaxDBSizeMB: 64})
	p.store = store
	store.eventFieldsCapacityCheck = p.checkEventFieldsUpgradeCapacity
	if _, err := store.db.Exec("CREATE TABLE capacity_padding(payload BLOB)"); err != nil {
		t.Fatal(err)
	}
	var pageCount, pageSize int64
	if err := store.db.QueryRow("PRAGMA page_count").Scan(&pageCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		"INSERT INTO capacity_padding(payload) VALUES(zeroblob(?))", limit-reserve*3/4-pageCount*pageSize,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := store.setMaxSizeBytes(t.Context(), limit); err != nil {
		t.Fatal(err)
	}
	sizeBefore, err := p.DBSizeWithError()
	if err != nil || sizeBefore < limit-reserve || sizeBefore >= limit {
		t.Fatalf("capacity fixture size=%d limit=%d reserve=%d error=%v", sizeBefore, limit, reserve, err)
	}
	stop := store.startEventFieldsUpgrade(t.Context())
	stop()
	paused := assertStorageUpgradeCounts(t, store, "error", 128, 273)
	if !strings.Contains(paused.Error, "reserve capacity for live usage") {
		t.Fatalf("paused=%+v", paused)
	}
	writer := newWriter(store, nil)
	writer.setCapacityCheck(p.refreshDBSizeCap)
	writer.start(t.Context())
	defer writer.Stop()
	p.writer = writer
	if !p.Record(
		FlatDetail{
			Timestamp: base.Add(time.Hour), Model: "live-capacity", RequestID: "live-capacity",
			Tokens: tokenStats{InputTokens: 1, TotalTokens: 1},
		},
	) {
		t.Fatal("online event refused while background upgrade yielded")
	}
	writer.Stop()
	assertStorageUpgradeCounts(t, store, "error", 129, 274)
	if p.refreshDBSizeCap() {
		t.Fatal("background upgrade exhausted the live usage reserve")
	}
	var cursor string
	if err := store.db.QueryRow(
		"SELECT value FROM meta WHERE key=?", eventFieldsCursorKey,
	).Scan(&cursor); err != nil || cursor != "128" {
		t.Fatalf("paused cursor=%q error=%v", cursor, err)
	}
	p.SetRetention(config.UsageRetention{Days: -1, MaxDBSizeMB: 128})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := NewPersister(dir, config.UsageRetention{Days: -1, MaxDBSizeMB: 128})
	reopened.Start(t.Context())
	defer reopened.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := reopened.StorageUpgradeStatusContext(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "complete" {
			if status.ProcessedEvents == nil || *status.ProcessedEvents != 274 || status.TotalEvents == nil || *status.TotalEvents != 274 || status.StartedAt != paused.StartedAt || status.CompletedAt == "" || status.Error != "" {
				t.Fatalf("resumed=%+v", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resume did not complete: %+v", status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Logf(
		"near-capacity size=%d reserve=%d cursor=%s; online event saved and restart completed", sizeBefore, reserve,
		cursor,
	)
}

func TestStorageUpgradeStatusReadOnlySnapshotUsesCountIndexes(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	assertStorageUpgradeCounts(t, store, "complete", 0, 0)
	rows, err := store.db.Query("EXPLAIN QUERY PLAN "+storageUpgradeStatusSQL, eventFieldsCursorKey, eventFieldsTaskKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"events", "event_fields"} {
		if !strings.Contains(plan.String(), "SCAN "+table+" USING COVERING INDEX idx_"+table+"_") {
			t.Fatalf("count did not use a lightweight index for %s:\n%s", table, plan.String())
		}
	}
	t.Log(plan.String())
}

func TestEventFieldsPendingUpgradeKeepsCanonicalPageIndex(t *testing.T) {
	store := openTestStore(t)
	const events = 8192
	const matched = 512
	base := time.Date(2026, 10, 20, 8, 53, 0, 0, time.UTC)
	if _, err := store.db.Exec(
		`INSERT INTO dim_model(name) VALUES('pending-history');
INSERT INTO dim_auth_index(name) VALUES('pending-auth'),('other-auth');
WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<?)
INSERT INTO events(fingerprint,ts_ns,model_id,auth_index_id,failed,total_tokens,metadata)
SELECT CAST(printf('%020d',n) AS BLOB),?+n*1000000000,1,CASE WHEN n>? THEN 1 ELSE 2 END,0,1,
json_object('reasoning_effort','high','response_service_tier','default','payload',?) FROM seq`,
		events, base.UnixNano(), events-matched, strings.Repeat("x", 8192),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		"DELETE FROM event_fields WHERE id>128; UPDATE meta SET value='128' WHERE key=?", eventFieldsCursorKey,
	); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	prepared, err := prepareEventsPageQuery(
		ctx, store.db, base.UnixNano(), base.Add(30*24*time.Hour).UnixNano(), EventFilters{Search: "pending-auth"},
		"timestamp", true,
	)
	if err != nil || prepared.total != matched {
		t.Fatalf("partial count=%d error=%v", prepared.total, err)
	}
	if strings.Contains(prepared.fromSQL, "UNION") || strings.Contains(prepared.fromSQL, "FROM event_fields") {
		t.Fatalf("partial upgrade used an unflattenable combined source: %s", prepared.fromSQL)
	}
	pageProjection := strings.SplitN(prepared.pageSQL(), " FROM ", 2)[0]
	if strings.Contains(pageProjection, "events.metadata,") || strings.HasSuffix(
		pageProjection, "events.metadata",
	) || strings.Contains(pageProjection, "events.*") {
		t.Fatalf("partial page copied raw metadata: %s", pageProjection)
	}
	args := append(append([]any{}, prepared.whereArgs...), 50, 0)
	rows, err := store.db.Query("EXPLAIN QUERY PLAN "+prepared.pageSQL()+"SELECT ts_ns FROM event_page", args...)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if !strings.Contains(plan.String(), "USING INDEX idx_events_ts_ns") {
		t.Fatalf("partial page lost canonical time index:\n%s", plan.String())
	}
	for _, compact := range []bool{false, true} {
		query := store.queryEventDetails
		if compact {
			query = store.queryCompactEventDetails
		}
		details, total, err := query(
			ctx, base, base.Add(30*24*time.Hour), EventFilters{Search: "pending-auth"}, 1, 50, "timestamp", true,
		)
		if err != nil || total != matched || len(details) != 50 {
			t.Fatalf("partial compact=%t total=%d rows=%d error=%v", compact, total, len(details), err)
		}
		for i, detail := range details {
			if detail.ReasoningEffort != "high" || detail.AuthIndex != "pending-auth" || !detail.Timestamp.Equal(base.Add(time.Duration(events-i)*time.Second)) {
				t.Fatalf("partial compact=%t detail=%+v", compact, detail)
			}
		}
	}
	var cursor string
	if err := store.db.QueryRow(
		"SELECT value FROM meta WHERE key=?", eventFieldsCursorKey,
	).Scan(&cursor); err != nil || cursor != "128" {
		t.Fatalf("read changed upgrade cursor=%q error=%v", cursor, err)
	}
}
