package usage

import (
	"context"
	"os"
	"testing"
	"time"
)

func BenchmarkSummaryUnfilteredStages(b *testing.B) {
	path := os.Getenv("CPA_USAGE_BENCHMARK_DB")
	if path == "" {
		b.Skip("set CPA_USAGE_BENCHMARK_DB to the retained synthetic fixture")
	}
	store, err := openStore(path)
	if err != nil {
		b.Fatal(err)
	}
	defer func(closeResource func() error) {
		if err := closeResource(); err != nil {
			b.Errorf("close benchmark stage store: %v", err)
		}
	}(store.Close)
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	to := from + 730*nsPerDay - 1
	table, err := summaryRangeSource(nsPerDay, nsPerDay, from, to, false)
	if err != nil {
		b.Fatal(err)
	}
	rows, err := store.db.Query(
		"EXPLAIN QUERY PLAN SELECT m.name,SUM(b.requests) FROM "+
			table+
			(" b JOIN dim_model m ON "+
				"m.id=b.model_id WHERE "+
				"b.bucket_ts_ns>=? AND "+
				"b.bucket_ts_ns<=? GROUP BY m.id,"+
				"m.name"),
		from,
		to,
	)
	if err != nil {
		b.Fatal(err)
	}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			b.Fatal(err)
		}
		b.Log(detail)
	}
	if err := rows.Err(); err != nil {
		b.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		b.Fatal(err)
	}
	for _, stage := range []string{"totals", "model", "credential", "api_key", "time_series"} {
		b.Run(
			stage, func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					result := emptyQueryResult()
					switch stage {
					case "totals":
						err = store.queryTotals(
							ctx, store.db, table, from, to, resolvedFilter{}, resolvedCredentialFilter{},
							resolvedFilter{}, &result,
						)
					case "model":
						err = store.queryByModel(
							ctx, store.db, table, from, to, resolvedFilter{}, resolvedCredentialFilter{},
							resolvedFilter{}, &result,
						)
					case "credential":
						err = store.queryByCredential(
							ctx, store.db, table, from, to, resolvedFilter{}, resolvedCredentialFilter{},
							resolvedFilter{}, &result,
						)
					case "api_key":
						err = store.queryByAPIKey(
							ctx, store.db, table, from, to, resolvedFilter{}, resolvedCredentialFilter{},
							resolvedFilter{}, &result,
						)
					case "time_series":
						err = store.queryTimeSeries(
							ctx, store.db, table, from, to, resolvedFilter{}, resolvedCredentialFilter{},
							resolvedFilter{}, "bucket_ts_ns", &result,
						)
					}
					cancel()
					if err != nil {
						b.Fatal(err)
					}
					if stage == "totals" && result.Totals.Requests != 730*8192*200 {
						b.Fatalf("requests=%d", result.Totals.Requests)
					}
				}
			},
		)
	}
}
