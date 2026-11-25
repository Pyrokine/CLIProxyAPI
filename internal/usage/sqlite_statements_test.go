package usage

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestEventStatementsReuseAndReprepare(t *testing.T) {
	store := openTestStore(t)
	store.db.SetMaxOpenConns(1)
	var compilations [3]atomic.Int64
	conn, err := store.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = conn.Raw(
		func(raw any) error {
			return raw.(sqlite.AuthorizerRegisterer).RegisterAuthorizer(
				func(action sqlite.AuthorizerActionCode, table, _, _, trigger string) sqlite.AuthorizerReturnCode {
					if action == sqlite.AuthInsert && trigger == "" {
						switch table {
						case "events":
							compilations[0].Add(1)
						case "hour_bucket":
							compilations[1].Add(1)
						case "dirty_day":
							compilations[2].Add(1)
						}
					}
					return sqlite.AuthorizerOK
				},
			)
		},
	)
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	writer := newWriter(store, nil)
	base := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	flush := func(offset int) {
		t.Helper()
		if err := writer.flush(
			[]FlatDetail{
				{
					Timestamp: base.Add(time.Duration(offset) * time.Second), Model: "reused-sql",
					Tokens: tokenStats{InputTokens: 1, TotalTokens: 1},
				},
			},
		); err != nil {
			t.Fatal(err)
		}
	}
	// Installing an authorizer expires the existing statements, so warm them once.
	flush(0)
	var before [3]int64
	for i := range before {
		before[i] = compilations[i].Load()
		if before[i] != 1 {
			t.Fatalf("statement[%d] compiled %d times, want 1", i, before[i])
		}
	}
	for i := range 5 {
		flush(i + 1)
	}
	flush(0)
	for i := range before {
		if got := compilations[i].Load(); got != before[i] {
			t.Fatalf("statement[%d] compiled again across transactions: before=%d after=%d", i, before[i], got)
		}
	}
	if _, err := store.db.ExecContext(
		t.Context(), `CREATE TRIGGER reject_reused_sql BEFORE INSERT ON events
		BEGIN SELECT RAISE(ABORT, 'statement schema changed'); END`,
	); err != nil {
		t.Fatal(err)
	}
	batch := []FlatDetail{{Timestamp: base.Add(time.Hour), Model: "reused-sql", Tokens: tokenStats{TotalTokens: 1}}}
	if err := writer.flushTransactionWithPricing(t.Context(), batch, pricingSnapshot{}); err == nil {
		t.Fatal("prepared insert ignored the new rejection trigger")
	}
	if _, err := store.db.ExecContext(t.Context(), "DROP TRIGGER reject_reused_sql"); err != nil {
		t.Fatal(err)
	}
	flush(6)
	for i := range before {
		if got := compilations[i].Load(); got <= before[i] {
			t.Fatalf("statement[%d] did not recompile after schema change: before=%d after=%d", i, before[i], got)
		}
	}
	var events, requests int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT SUM(requests) FROM hour_bucket").Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if events != 7 || requests != 7 {
		t.Fatalf("events=%d requests=%d, want 7/7 after duplicate and rejected inserts", events, requests)
	}
	t.Logf(
		"SQLite insert compilation counts before repeated writes=%v; after schema update=%v", before,
		[3]int64{compilations[0].Load(), compilations[1].Load(), compilations[2].Load()},
	)
}

func TestEventStatementsTransactionAndLifecycle(t *testing.T) {
	for _, prepared := range []bool{true, false} {
		name := "prepared"
		if !prepared {
			name = "unprepared"
		}
		t.Run(
			name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "events.db")
				store, err := openStore(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				if !prepared {
					store = &sqliteStore{db: store.db}
				}
				detail := FlatDetail{
					Timestamp: time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC), Model: "transactional-sql",
					Tokens: tokenStats{InputTokens: 1, TotalTokens: 1},
				}
				tx, err := store.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if added, err := persistEvent(t.Context(), tx, store, &detail, nil); err != nil || !added {
					_ = tx.Rollback()
					t.Fatalf("persist before rollback: added=%v error=%v", added, err)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				for _, table := range []string{"events", "hour_bucket", "dirty_day", "event_fields", "dim_model"} {
					var count int
					if err := store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
						t.Fatalf("%s rows after rollback=%d error=%v", table, count, err)
					}
				}
				writer := newWriter(store, nil)
				if err := writer.flush([]FlatDetail{detail, detail}); err != nil {
					t.Fatal(err)
				}
				tx, err = store.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				canceled, cancel := context.WithCancel(t.Context())
				cancel()
				if _, err := persistEvent(canceled, tx, store, &detail, nil); !errors.Is(err, context.Canceled) {
					_ = tx.Rollback()
					t.Fatalf("canceled persist error=%v", err)
				}
				_ = tx.Rollback()
				store.db.SetMaxIdleConns(0)
				store.db.SetMaxIdleConns(1)
				detail.Timestamp = detail.Timestamp.Add(time.Second)
				tx, err = store.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if added, err := persistEvent(t.Context(), tx, store, &detail, nil); err != nil || !added {
					_ = tx.Rollback()
					t.Fatalf("persist after connection replacement: added=%v error=%v", added, err)
				}
				if err := store.Close(); err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatalf("commit of transaction active when DB closed: %v", err)
				}
				if err := writer.flush([]FlatDetail{detail}); err == nil {
					t.Fatal("writer accepted a transaction after store close")
				}
				reopened, err := openStore(path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = reopened.Close() }()
				var events, requests, fields int
				if err := reopened.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&events); err != nil {
					t.Fatal(err)
				}
				if err := reopened.db.QueryRow("SELECT SUM(requests) FROM hour_bucket").Scan(&requests); err != nil {
					t.Fatal(err)
				}
				if err := reopened.db.QueryRow("SELECT COUNT(*) FROM event_fields").Scan(&fields); err != nil {
					t.Fatal(err)
				}
				if events != 2 || requests != 2 || fields != 2 {
					t.Fatalf("reopened events=%d requests=%d fields=%d, want 2/2/2", events, requests, fields)
				}
			},
		)
	}
}

func BenchmarkWriterFixedSQL(b *testing.B) {
	store, err := openStore(filepath.Join(b.TempDir(), "events.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	writer := newWriter(
		store, func(string) (float64, float64, float64, bool) {
			return 1, 2, 0.5, true
		},
	)
	base := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	batch := make([]FlatDetail, writerBatchSize)
	for i := range batch {
		batch[i] = FlatDetail{
			Timestamp: base.Add(time.Duration(i)), Model: "fixed-sql-model",
			APIKey: "fixed-sql-key", Source: "fixed-sql-source", Provider: "fixed-sql-provider",
			Tokens: tokenStats{InputTokens: 317, OutputTokens: 5, TotalTokens: 322},
		}
	}
	if err := writer.flush(batch); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := range batch {
			batch[i].Timestamp = base.Add(time.Duration((int64(n)+1)*int64(len(batch)) + int64(i)))
		}
		if err := writer.flush(batch); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(len(batch)), "records/op")
	var events, requests int64
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM events").Scan(&events); err != nil {
		b.Fatal(err)
	}
	if err := store.db.QueryRowContext(
		context.Background(), "SELECT SUM(requests) FROM hour_bucket",
	).Scan(&requests); err != nil {
		b.Fatal(err)
	}
	want := (int64(b.N) + 1) * int64(len(batch))
	if events != want || requests != want {
		b.Fatalf("events=%d requests=%d, want %d", events, requests, want)
	}
}
