package usage

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestModernizedSQLBuildersPreserveExactStatements(t *testing.T) {
	for _, prefix := range []string{"", "OLD.", "NEW."} {
		for _, scope := range summaryProjectionScopes {
			for _, grain := range []int64{1, 600_000_000_000, 3_600_000_000_000} {
				t.Run(
					fmt.Sprintf("%s/%s/%d", prefix, scope, grain), func(t *testing.T) {
						clauses := []string{
							fmt.Sprintf(
								"grain_ns=%d AND dimension='%s' AND bucket_ts_ns=%s",
								grain, scope, summaryGrainFloor(prefix+"bucket_ts_ns", strconv.FormatInt(grain, 10)),
							),
						}
						for index, field := range []string{"model_id", "credential_id", "api_key_id", "provider_id"} {
							clauses = append(
								clauses, fmt.Sprintf("%s IS %s", field, projectionDimensions(prefix, scope)[index]),
							)
						}
						if got, want := projectionKey(prefix, scope, grain), strings.Join(
							clauses, " AND ",
						); got != want {
							t.Fatalf("projection statement changed:\n%s\nwant:\n%s", got, want)
						}
					},
				)
			}
		}
	}
	values := summaryEventMetrics("OLD.")
	updates := make([]string, 0, len(summaryMetricColumns))
	for index, field := range summaryMetricColumns {
		updates = append(updates, fmt.Sprintf("%s=%s-(%s)", field, field, values[index]))
	}
	var want strings.Builder
	for _, grain := range []int64{1, 600_000_000_000, 3_600_000_000_000} {
		clauses := []string{
			fmt.Sprintf("grain_ns=%d", grain),
			"bucket_ts_ns=" + summaryGrainFloor("OLD.ts_ns", strconv.FormatInt(grain, 10)),
			"model_id=OLD.model_id",
		}
		for _, field := range []string{"credential_id", "api_key_id", "provider_id", "auth_index_id"} {
			clauses = append(
				clauses,
				fmt.Sprintf("ifnull(%s,0)=ifnull(OLD.%s,0)", field, field),
				fmt.Sprintf("%s IS OLD.%s", field, field),
			)
		}
		where := strings.Join(clauses, " AND ")
		fmt.Fprintf(
			&want,
			"UPDATE summary_bucket SET %s WHERE %s; DELETE FROM summary_bucket WHERE requests=0 AND %s;",
			strings.Join(updates, ","), where, where,
		)
	}
	if got := summaryDeleteSQL(); got != want.String() {
		t.Fatalf("summary delete statement changed:\n%s\nwant:\n%s", got, want.String())
	}
}

func TestEventFieldsCostUpdateAvoidsIdentityIndexWrites(t *testing.T) {
	store := openTestStore(t)
	var triggerSQL string
	if err := store.db.QueryRow("SELECT sql FROM sqlite_master WHERE type='trigger' AND name='event_fields_cost_update'").Scan(&triggerSQL); err != nil {
		t.Fatal(err)
	}
	want := "UPDATE event_fields SET cost_micro=NEW.cost_micro,cost_status=NEW.cost_status,cost_reason=NEW.cost_reason WHERE id=NEW.id;"
	if !strings.Contains(triggerSQL, want) {
		t.Fatalf("cost trigger updates more than derived costs: %s", triggerSQL)
	}
	if err := store.db.QueryRow("SELECT sql FROM sqlite_master WHERE type='trigger' AND name='event_fields_core_update'").Scan(&triggerSQL); err != nil {
		t.Fatal(err)
	}
	if header := strings.SplitN(triggerSQL, " BEGIN ", 2)[0]; strings.Contains(
		header, "cost_micro",
	) || strings.Contains(header, "cost_status") || strings.Contains(header, "cost_reason") {
		t.Fatalf("cost-only update still invokes full core trigger: %s", header)
	}
	indexWrites := func(query string) int {
		t.Helper()
		rows, err := store.db.Query("EXPLAIN " + query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var address, p1, p2, p3, p5 int
			var opcode string
			var p4, comment sql.NullString
			if err := rows.Scan(&address, &opcode, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
				t.Fatal(err)
			}
			if opcode == "IdxDelete" || opcode == "IdxInsert" {
				count++
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return count
	}
	var all []string
	for _, column := range eventCoreColumns[1:] {
		all = append(all, column+"="+column)
	}
	before := indexWrites("UPDATE event_fields SET " + strings.Join(all, ",") + " WHERE id=1")
	after := indexWrites("UPDATE event_fields SET cost_micro=cost_micro+1,cost_status=cost_status,cost_reason=cost_reason WHERE id=1")
	if before == 0 || after != 0 {
		t.Fatalf("identity index write instructions before=%d after=%d", before, after)
	}
	t.Logf("identity index write instructions before=%d after=%d", before, after)
}
