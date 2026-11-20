package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

type postgresReadinessRows struct {
	values            []driver.Value
	read              bool
	nextErr, closeErr error
}

func (r *postgresReadinessRows) Columns() []string {
	return make([]string, len(r.values))
}

func (r *postgresReadinessRows) Close() error { return r.closeErr }

func (r *postgresReadinessRows) Next(dest []driver.Value) error {
	if r.read {
		if r.nextErr != nil {
			return r.nextErr
		}
		return io.EOF
	}
	r.read = true
	copy(dest, r.values)
	return nil
}

// noinspection GoBoolExpressions
func TestPostgresStoreCheckUpdateReadinessIsBoundedAndReadOnly(t *testing.T) {
	for _, mode := range []string{
		"ready", "missing_column", "wrong_type", "scan_error", "rows_error", "close_error", "cancelled", "deadline",
	} {
		t.Run(
			mode, func(t *testing.T) {
				store, state := newPostgresStoreForTest(t)
				store.cfg.Schema = "synthetic-schema"
				store.cfg.ConfigTable, store.cfg.AuthTable, store.cfg.CooldownTable = "synthetic-config",
					"synthetic-auth", "synthetic-cool"+
						"down"
				state.payload = "synthetic-private-content"
				var queries []string
				state.readinessQuery = func(ctx context.Context, query string) (driver.Rows, error) {
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > 5*time.Second {
						t.Fatal("readiness query has no bounded deadline")
					}
					queries = append(queries, query)
					for _, operation := range []string{
						"CREATE", "INSERT", "ALTER", "UPDATE", "DELETE", "SELECT *", state.payload,
					} {
						if strings.Contains(query, operation) {
							t.Fatalf("readiness query contains %q: %s", operation, query)
						}
					}
					if !strings.HasPrefix(query, "SELECT pg_typeof((SELECT ") || !strings.HasSuffix(
						query, " LIMIT 1",
					) ||
						!strings.Contains(query, `pg_typeof((SELECT "content" FROM `) {
						t.Fatalf("query must return column types with bounded scalar subqueries: %s", query)
					}
					if mode == "missing_column" {
						return nil, errors.New("synthetic missing column")
					}
					if mode == "deadline" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					values := []driver.Value{
						"text",
						"text",
						"timestamp with time zone",
						"timestamp with time zone",
					}
					if strings.Contains(query, store.fullTableName(store.cfg.AuthTable)) {
						values[1] = "jsonb"
					} else if strings.Contains(query, store.fullTableName(store.cfg.CooldownTable)) {
						values = []driver.Value{
							"text", "text", "jsonb", "boolean", "timestamp with time zone", "timestamp with time zone",
						}
					} else if !strings.Contains(query, store.fullTableName(store.cfg.ConfigTable)) {
						t.Fatalf("readiness queried an unconfigured table: %s", query)
					}
					if strings.Count(query, "pg_typeof(") != len(values) || strings.Count(
						query, "LIMIT 1",
					) != len(values)+1 {
						t.Fatalf("required column or scalar row limit missing: %s", query)
					}
					rows := &postgresReadinessRows{values: values}
					switch mode {
					case "wrong_type":
						values[1] = "integer"
					case "scan_error":
						values[1] = nil
					case "rows_error":
						rows.nextErr = errors.New("synthetic row iteration failure")
					case "close_error":
						rows.closeErr = errors.New("synthetic row close failure")
					}
					return rows, nil
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if mode == "cancelled" {
					cancel()
				}
				if mode == "deadline" {
					var end context.CancelFunc
					ctx, end = context.WithTimeout(ctx, time.Millisecond)
					defer end()
				}
				err := store.CheckUpdateReadiness(ctx)
				if (err == nil) != (mode == "ready") {
					t.Fatalf("readiness result=%v", err)
				}
				if mode == "ready" && len(queries) != 3 {
					t.Fatalf("table checks=%d, want 3", len(queries))
				}
				if mode == "cancelled" && !errors.Is(err, context.Canceled) || mode == "deadline" && !errors.Is(
					err, context.DeadlineExceeded,
				) {
					t.Fatalf("context failure lost: %v", err)
				}
				if state.execCalls != 0 || state.beginCalls != 0 ||
					state.payload != "synthetic-private-content" {
					t.Fatalf(
						"readiness mutated the database: exec=%d begin=%d",
						state.execCalls,
						state.beginCalls,
					)
				}
				files, err := os.ReadDir(store.authDir)
				if err != nil || len(files) != 0 {
					t.Fatalf("readiness wrote auth files: count=%d err=%v", len(files), err)
				}
			},
		)
	}
	var absent *PostgresStore
	if err := absent.CheckUpdateReadiness(t.Context()); err == nil {
		t.Fatal("uninitialized store was accepted")
	}
	if PostgresDataFormatVersion != 1 {
		t.Fatal("existing PostgreSQL format declaration changed")
	}
}
