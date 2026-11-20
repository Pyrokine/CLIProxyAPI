package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var ErrSummaryPrecisionUnavailable = errors.New("usage: detailed historical summary precision is unavailable")

var summaryMetricColumns = strings.Fields(
	"requests success failure input_tokens output_tokens reasoning_tokens c" +
		"ached_tokens cache_read_tokens cache_write_tokens total_tokens cost_mi" +
		"cro complete_requests partial_requests unpriced_requests complete_toke" +
		"ns partial_tokens unpriced_tokens latency_ms_total latency_samples",
)

const summaryGrainsSQL = "(SELECT 1 AS grain_ns UNION ALL SELECT 600000000000 UNION ALL SELECT 3600000000000)"
const summaryGrainsMigrationKey = "summary_grains_v1"

func summaryGrainFloor(column, grain string) string {
	return "(CASE WHEN " +
		column +
		" < 0 THEN ((" +
		column +
		" + 1) / " +
		grain +
		" - 1) * " +
		grain +
		" ELSE " +
		column +
		" / " +
		grain +
		" * " +
		grain +
		" END)"
}
func summaryEventMetrics(prefix string) []string {
	values := []string{"1", "1-" + prefix + "failed", prefix + "failed"}
	for _, name := range []string{
		"input_tokens", "output_tokens", "reasoning_tokens", "cached_tokens", "cache_read_tokens", "cache_write_tokens",
		"total_tokens", "cost_micro",
	} {
		values = append(values, prefix+name)
	}
	for _, kind := range []string{"complete", "partial", "unpriced"} {
		values = append(values, "CASE WHEN "+prefix+"cost_status='"+kind+"' THEN 1 ELSE 0 END")
	}
	for _, kind := range []string{"complete", "partial", "unpriced"} {
		values = append(
			values,
			"CASE WHEN "+prefix+"cost_status='"+kind+"' THEN "+prefix+"total_tokens ELSE 0 END",
		)
	}
	valid := "json_valid(" +
		prefix +
		"metadata) AND json_type(" +
		prefix +
		"metadata,'$.latency_ms')='integer' AND json_extract(" +
		prefix +
		"metadata,'$.latency_ms')>=0"
	values = append(
		values, "CASE WHEN "+valid+" THEN json_extract("+prefix+"metadata,'$.latency_ms') ELSE 0 END",
		"CASE WHEN "+valid+" THEN 1 ELSE 0 END",
	)
	return values
}
func summaryInsertSQL(prefix, from string) string {
	dimensions := []string{
		prefix + "model_id", prefix + "credential_id", prefix + "api_key_id", prefix + "provider_id",
		prefix + "auth_index_id",
	}
	bucket := summaryGrainFloor(prefix+"ts_ns", "g.grain_ns")
	values := append([]string{"g.grain_ns", bucket}, dimensions...)
	values = append(values, summaryEventMetrics(prefix)...)
	query := "INSERT INTO summary_bucket(grain_ns,bucket_ts_ns,model_id," +
		"credential_id,api_key_id,provider_id,auth_index_id," +
		strings.Join(
			summaryMetricColumns,
			",",
		) + ") SELECT " + strings.Join(
		values,
		",",
	) + " FROM " + from + " WHERE 1"
	updates := make([]string, 0, len(summaryMetricColumns))
	for _, name := range summaryMetricColumns {
		updates = append(updates, name+"=summary_bucket."+name+"+excluded."+name)
	}
	return query + (" ON CONFLICT(grain_ns,bucket_ts_ns,model_id,ifnull(credential_id,0),ifnull(api_key_id,0)," +
		"ifnull(provider_id,0),ifnull(auth_index_id,0)) DO UPDATE SET ") + strings.Join(
		updates, ",",
	) + ";"
}
func summaryDeleteSQL() string {
	values := summaryEventMetrics("OLD.")
	updates := make([]string, 0, len(summaryMetricColumns))
	for i, name := range summaryMetricColumns {
		updates = append(updates, name+"="+name+"-("+values[i]+")")
	}
	var body strings.Builder
	for _, grainNS := range []int64{1, nsPerTenMinute, nsPerHour} {
		grain := strconv.FormatInt(grainNS, 10)
		var where strings.Builder
		where.WriteString(
			"grain_ns=" + grain + " AND bucket_ts_ns=" + summaryGrainFloor("OLD.ts_ns", grain) +
				" AND model_id=OLD.model_id",
		)
		for _, name := range []string{"credential_id", "api_key_id", "provider_id", "auth_index_id"} {
			// Match the expression index while retaining the original NULL-versus-zero comparison.
			where.WriteString(" AND ifnull(" + name + ",0)=ifnull(OLD." + name + ",0) AND " + name + " IS OLD." + name)
		}
		body.WriteString(
			"UPDATE summary_bucket SET " + strings.Join(updates, ",") + " WHERE " + where.String() +
				"; DELETE FROM summary_bucket WHERE requests=0 AND " + where.String() + ";",
		)
	}
	return body.String()
}
func ensureSummaryGrains(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(
		ctx, "INSERT OR IGNORE INTO meta(key,value) VALUES(?, 'pending')", summaryGrainsMigrationKey,
	); err != nil {
		return err
	}
	var status string
	if err := db.QueryRowContext(
		ctx, "SELECT value FROM meta WHERE key=?", summaryGrainsMigrationKey,
	).Scan(&status); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	columns := []string{
		"grain_ns INTEGER NOT NULL", "bucket_ts_ns INTEGER NOT NULL CHECK(typeof(bucket_ts_ns)='integer')",
		"model_id INTEGER NOT NULL REFERENCES dim_model(id)", "credential_id INTEGER REFERENCES dim_credential(id)",
		"api_key_id INTEGER REFERENCES dim_api_key(id)", "provider_id INTEGER REFERENCES dim_provider(id)",
		"auth_index_id INTEGER REFERENCES dim_auth_index(id)",
	}
	for _, name := range summaryMetricColumns {
		columns = append(
			columns,
			name+" INTEGER NOT NULL DEFAULT 0 CHECK(typeof("+name+")='integer' AND "+name+">=0)",
		)
	}
	if _, err := tx.ExecContext(
		ctx, "CREATE TABLE IF NOT EXISTS summary_bucket(id INTEGER PRIMARY KEY,"+strings.Join(columns, ",")+")",
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		"CREATE UNIQUE INDEX IF NOT EXISTS uq_summary_bucket_key ON summary_buc"+
			"ket(grain_ns,bucket_ts_ns,model_id,ifnull(credential_id,0),ifnull(api_"+
			"key_id,0),ifnull(provider_id,0),ifnull(auth_index_id,0))",
	); err != nil {
		return err
	}
	for _, dimension := range []string{"", "model_id,", "credential_id,", "api_key_id,", "auth_index_id,"} {
		name := strings.TrimSuffix(dimension, ",")
		if name == "" {
			name = "time"
		}
		if _, err := tx.ExecContext(
			ctx,
			"CREATE INDEX IF NOT EXISTS idx_summary_bucket_"+
				name+
				" ON summary_bucket(grain_ns,"+
				dimension+
				"bucket_ts_ns)",
		); err != nil {
			return err
		}
	}
	for _, table := range []string{"hour_bucket", "day_bucket"} {
		if _, err := tx.ExecContext(
			ctx,
			"CREATE INDEX IF NOT EXISTS idx_"+
				table+
				"_dimensions_time ON "+
				table+
				"(credential_id,provider_id,model_id,api_key_id,bucket_ts_ns)",
		); err != nil {
			return err
		}
	}
	insert := summaryInsertSQL("NEW.", summaryGrainsSQL+" g")
	remove := summaryDeleteSQL()
	for _, trigger := range []struct{ name, event, body string }{
		{"insert", "INSERT", insert}, {"delete", "DELETE", remove}, {"update", "UPDATE", remove + insert},
	} {
		// Completed stores also need current trigger definitions without rebuilding their history.
		if _, err := tx.ExecContext(ctx, "DROP TRIGGER IF EXISTS summary_bucket_event_"+trigger.name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			"CREATE TRIGGER summary_bucket_event_"+
				trigger.name+
				" AFTER "+
				trigger.event+
				" ON events BEGIN "+
				trigger.body+
				" END",
		); err != nil {
			return err
		}
	}
	if status != "complete" {
		if _, err := tx.ExecContext(ctx, "DELETE FROM summary_bucket"); err != nil {
			return err
		}
		// Stream the trigger UPSERT so full-history sorting never copies retained event metadata.
		if _, err := tx.ExecContext(
			ctx, summaryInsertSQL("e.", "events e CROSS JOIN "+summaryGrainsSQL+" g"),
		); err != nil {
			return fmt.Errorf("backfill exact summary: %w", err)
		}
		var events int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&events); err != nil {
			return err
		}
		for _, grain := range []int64{1, nsPerTenMinute, nsPerHour} {
			var requests int64
			if err := tx.QueryRowContext(
				ctx, "SELECT COALESCE(SUM(requests),0) FROM summary_bucket WHERE grain_ns=?", grain,
			).Scan(&requests); err != nil {
				return err
			}
			if requests != events {
				return fmt.Errorf(
					"summary backfill grain %d has %d requests for %d events",
					grain,
					requests,
					events,
				)
			}
		}
		if _, err := tx.ExecContext(
			ctx, "UPDATE meta SET value='complete' WHERE key=?", summaryGrainsMigrationKey,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type summaryProjectionRange struct {
	dimension    string
	dirtyWindows []int64
}

// Full buckets retain historical aggregates; only incomplete boundaries use exact persisted rows.
func summaryRangeSource(
	grain, outputGrain, from, to int64,
	authIndex bool,
	projection ...summaryProjectionRange,
) (string, error) {
	if authIndex && grain == nsPerDay {
		return summaryRangeSource(nsPerHour, outputGrain, from, to, true)
	}
	columns := summaryBucketValueColumns
	if authIndex {
		columns += ",auth_index_id"
	}
	project := sqliteFloorBucketExpr("bucket_ts_ns", outputGrain) + " AS bucket_ts_ns," + columns
	if grain == 1 {
		return fmt.Sprintf(
			"(SELECT %s FROM summary_bucket WHERE grain_ns=1 AND bucket_ts_ns >= %d AND bucket_ts_ns <= %d)",
			project,
			from,
			to,
		), nil
	}
	lower, err := floorUnixNano(from, grain)
	if err != nil {
		return "", err
	}
	upper, err := floorUnixNano(to, grain)
	if err != nil {
		return "", err
	}
	first, last := lower, upper
	full := true
	if from != lower {
		if lower > math.MaxInt64-grain {
			full = false
		} else {
			first += grain
		}
	}
	if to-upper != grain-1 {
		if upper < math.MinInt64+grain {
			full = false
		} else {
			last -= grain
		}
	}
	smaller := int64(1)
	if grain == nsPerDay {
		smaller = nsPerHour
	} else if grain == nsPerSummaryWindow {
		smaller = nsPerDay
	} else if authIndex && grain == nsPerHour {
		smaller = nsPerTenMinute
	}
	if !full || first > last {
		return summaryRangeSource(smaller, outputGrain, from, to, authIndex, projection...)
	}
	table := "hour_bucket"
	if grain == nsPerDay {
		table = daySummaryBucketSource()
	}
	if len(projection) > 0 {
		scope := projection[0].dimension
		table = summaryProjectionTable(grain, scope)
		if grain == nsPerDay {
			table = daySummaryBucketSource(table, summaryProjectionTable(nsPerHour, scope))
		}
	}
	if authIndex {
		table = fmt.Sprintf("(SELECT * FROM summary_bucket WHERE grain_ns=%d)", grain)
	}
	parts := []string{
		fmt.Sprintf(
			"SELECT %s FROM %s WHERE bucket_ts_ns >= %d AND bucket_ts_ns <= %d", project, table, first, last,
		),
	}
	if grain == nsPerSummaryWindow && len(projection) > 0 {
		for _, window := range projection[0].dirtyWindows {
			if window < first || window > last {
				continue
			}
			parts[0] += fmt.Sprintf(" AND bucket_ts_ns<>%d", window)
			finer, err := summaryRangeSource(
				nsPerDay,
				outputGrain,
				window,
				window+grain-1,
				false,
				projection...,
			)
			if err != nil {
				return "", err
			}
			parts = append(parts, "SELECT * FROM "+finer)
		}
	}
	if from < first {
		left, err := summaryRangeSource(smaller, outputGrain, from, first-1, authIndex, projection...)
		if err != nil {
			return "", err
		}
		parts = append(parts, "SELECT * FROM "+left)
	}
	if last <= math.MaxInt64-grain && last+grain <= to {
		right, err := summaryRangeSource(smaller, outputGrain, last+grain, to, authIndex, projection...)
		if err != nil {
			return "", err
		}
		parts = append(parts, "SELECT * FROM "+right)
	}
	return "(" + strings.Join(parts, " UNION ALL ") + ")", nil
}

func ensureBoundarySummaryPrecision(
	ctx context.Context,
	q dbQueryer,
	from, to int64,
	model resolvedFilter,
	credential resolvedCredentialFilter,
	apiKey resolvedFilter,
	daily bool,
) error {
	dayFrom, err := floorUnixNano(from, nsPerDay)
	if err != nil {
		return err
	}
	dayTo, err := floorUnixNano(to, nsPerDay)
	if err != nil {
		return err
	}
	dayFilter, dayArgs := composeFilterSQL(model, credential, apiKey, "d.")
	dayBounds := "d.bucket_ts_ns>=? AND d.bucket_ts_ns<=?"
	if daily {
		dayBounds = "(d.bucket_ts_ns=? AND ?<>0 OR d.bucket_ts_ns=? AND ?<>86400000000000-1)"
		dayArgs = append([]any{dayFrom, from - dayFrom, dayTo, to - dayTo}, dayArgs...)
	} else {
		dayArgs = append([]any{dayFrom, dayTo}, dayArgs...)
	}
	var missingDays bool
	dayQuery := "SELECT EXISTS(SELECT 1 FROM day_bucket d WHERE " +
		dayBounds +
		dayFilter +
		(" AND d.requests>COALESCE((SELECT SUM(h.requests) FROM hour_bucket h WHERE h.bucket_ts_ns>=d." +
			"bucket_ts_ns AND h.bucket_ts_ns-d.bucket_ts_ns<86400000000000 AND h.model_id=d.model_id AND h." +
			"credential_id IS d.credential_id AND h.api_key_id IS d.api_key_id AND h.provider_id IS d." +
			"provider_id),0))")
	if err := q.QueryRowContext(ctx, dayQuery, dayArgs...).Scan(&missingDays); err != nil {
		return err
	}
	if missingDays {
		return ErrSummaryPrecisionUnavailable
	}
	filter, args := composeFilterSQL(model, credential, apiKey, "h.")
	hours := make([]int64, 0, 2)
	first, err := floorUnixNano(from, nsPerHour)
	if err != nil {
		return err
	}
	last, err := floorUnixNano(to, nsPerHour)
	if err != nil {
		return err
	}
	if first != from {
		hours = append(hours, first)
	}
	if to-last != nsPerHour-1 && (len(hours) == 0 || hours[0] != last) {
		hours = append(hours, last)
	}
	for _, hour := range hours {
		query := "SELECT EXISTS(SELECT 1 FROM hour_bucket h WHERE h.bucket_ts_ns=?" +
			filter +
			(" AND h.requests > COALESCE((SELECT SUM(f.requests) FROM summary_bucket" +
				" f WHERE f.grain_ns=3600000000000 AND f.bucket_ts_ns=h.bucket_ts_ns AN" +
				"D f.model_id=h.model_id AND f.credential_id IS h.credential_id AND f.a" +
				"pi_key_id IS h.api_key_id AND f.provider_id IS h.provider_id),0))")
		var missing bool
		if err := q.QueryRowContext(ctx, query, append([]any{hour}, args...)...).Scan(&missing); err != nil {
			return err
		}
		if missing {
			return ErrSummaryPrecisionUnavailable
		}
	}
	return nil
}
