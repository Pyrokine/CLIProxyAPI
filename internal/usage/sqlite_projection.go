package usage

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

const nsPerSummaryWindow = 30 * nsPerDay
const summaryProjectionMigrationKey = "summary_projection_v1"

var summaryProjectionScopes = []string{"all", "model", "credential", "api_key"}

func projectionDimensions(prefix, scope string) []string {
	values := []string{"NULL", "NULL", "NULL", "NULL"}
	switch scope {
	case "model":
		values[0] = prefix + "model_id"
	case "credential":
		values[1], values[3] = prefix+"credential_id", prefix+"provider_id"
	case "api_key":
		values[2] = prefix + "api_key_id"
	}
	return values
}

func projectionCondition(prefix, scope string, grain int64) string {
	condition := "1"
	if scope == "credential" {
		condition += " AND " + prefix + "credential_id IS NOT NULL"
	}
	if grain == nsPerSummaryWindow {
		condition += " AND " + prefix + "bucket_ts_ns >= " + strconv.FormatInt(math.MinInt64+grain-1, 10)
	}
	return condition
}

func projectionKey(prefix, scope string, grain int64) string {
	var where strings.Builder
	where.WriteString(
		fmt.Sprintf(
			"grain_ns=%d AND dimension='%s' AND bucket_ts_ns=%s", grain, scope,
			summaryGrainFloor(prefix+"bucket_ts_ns", strconv.FormatInt(grain, 10)),
		),
	)
	for i, field := range []string{"model_id", "credential_id", "api_key_id", "provider_id"} {
		where.WriteString(" AND " + field + " IS " + projectionDimensions(prefix, scope)[i])
	}
	return where.String()
}

func projectionUpdates(values []string, members string) string {
	updates := []string{"members=members" + members}
	invalid := []string{"invalid"}
	for i, metric := range summaryMetricColumns {
		next := "(" + metric + "+(" + values[i] + "))"
		valid := "typeof(" + next + ")='integer' AND " + next + ">=0"
		invalid = append(invalid, "NOT("+valid+")")
		updates = append(updates, metric+"=CASE WHEN invalid=0 AND "+valid+" THEN "+next+" ELSE "+metric+" END")
	}
	updates = append(updates, "invalid=("+strings.Join(invalid, " OR ")+")")
	return strings.Join(updates, ",")
}

func projectionInsertSQL(table, prefix, scope string, grain int64, aggregate bool) string {
	bucket := summaryGrainFloor(prefix+"bucket_ts_ns", strconv.FormatInt(grain, 10))
	dimensions := projectionDimensions(prefix, scope)
	values := append([]string{strconv.FormatInt(grain, 10), bucket, "'" + scope + "'"}, dimensions...)
	members := "1"
	if aggregate {
		members = "COUNT(*)"
	}
	values = append(values, members, "0")
	increments := make([]string, 0, len(summaryMetricColumns))
	for _, metric := range summaryMetricColumns {
		value := prefix + metric
		if aggregate {
			value = "SUM(" + value + ")"
		}
		values = append(values, value)
		increments = append(increments, "excluded."+metric)
	}
	query := "INSERT INTO summary_projection(grain_ns,bucket_ts_ns,dimension,model_i" +
		"d,credential_id,api_key_id,provider_id,members,invalid," + strings.Join(
		summaryMetricColumns, ",",
	) + ") SELECT " + strings.Join(values, ",")
	if aggregate {
		query += " FROM " + table + " b"
	}
	query += " WHERE " + projectionCondition(prefix, scope, grain)
	if aggregate {
		query += " GROUP BY " + bucket + "," + strings.Join(dimensions, ",")
		return query
	}
	return query + (" ON CONFLICT(grain_ns,dimension,bucket_ts_ns,ifnull(model_id,0),ifnull" +
		"(credential_id,0),ifnull(api_key_id,0),ifnull(provider_id,0)) DO UPDAT" +
		"E SET ") + projectionUpdates(
		increments, "+excluded.members",
	) + ";"
}

func projectionRemoveSQL(scope string, grain int64) string {
	values := make([]string, 0, len(summaryMetricColumns))
	for _, metric := range summaryMetricColumns {
		values = append(values, "-OLD."+metric)
	}
	where := projectionKey("OLD.", scope, grain)
	return "UPDATE summary_projection SET " + projectionUpdates(
		values, "-1",
	) + " WHERE " + where + "; DELETE FROM summary_projection WHERE members=0 AND " + where + ";"
}

// A source bucket remains legal when a wider projection overflows; invalid projections use the original read path.
func projectionWideBackfillSQL(table, scope string, grain int64) string {
	bucket := summaryGrainFloor("b.bucket_ts_ns", strconv.FormatInt(grain, 10))
	dimensions := projectionDimensions("b.", scope)
	selects := []string{
		strconv.FormatInt(
			grain,
			10,
		) + " AS grain_ns", bucket + " AS bucket_ts_ns", "'" + scope + "' AS dimension",
		"COUNT(*) AS members",
	}
	for i, field := range []string{"model_id", "credential_id", "api_key_id", "provider_id"} {
		selects = append(selects, dimensions[i]+" AS "+field)
	}
	for _, metric := range summaryMetricColumns {
		for limb := range 4 {
			selects = append(
				selects,
				fmt.Sprintf("SUM((b.%s >> %d) & 65535) AS %s_%d", metric, limb*16, metric, limb),
			)
		}
	}
	query := "SELECT " + strings.Join(selects, ",") + " FROM " + table + " b WHERE " + projectionCondition(
		"b.", scope, grain,
	) + " GROUP BY " + bucket + "," + strings.Join(dimensions, ",")
	// Sixteen-bit limbs and carries fit int64 within SQLite's maximum page count and page size.
	for limb := 1; limb < 4; limb++ {
		columns := []string{"*"}
		for _, metric := range summaryMetricColumns {
			previous := fmt.Sprintf("%s_%d", metric, limb-1)
			if limb > 1 {
				previous = fmt.Sprintf("%s_c%d", metric, limb-1)
			}
			columns = append(
				columns,
				fmt.Sprintf("%s_%d+(%s >> 16) AS %s_c%d", metric, limb, previous, metric, limb),
			)
		}
		query = "SELECT " + strings.Join(columns, ",") + " FROM (" + query + ")"
	}
	invalid := make([]string, 0, len(summaryMetricColumns))
	metrics := make([]string, 0, len(summaryMetricColumns))
	for _, metric := range summaryMetricColumns {
		invalid = append(invalid, metric+"_c3>=32768")
		metrics = append(
			metrics, fmt.Sprintf(
				"CASE WHEN %[1]s_c3>=32768 THEN 0 ELSE (%[1]s_c3 << 48) | ((%[1]s_c2 & "+
					"65535) << 32) | ((%[1]s_c1 & 65535) << 16) | (%[1]s_0 & 65535) END",
				metric,
			),
		)
	}
	return "INSERT INTO summary_projection(grain_ns,bucket_ts_ns,dimension,model_i" +
		"d,credential_id,api_key_id,provider_id,members,invalid," + strings.Join(
		summaryMetricColumns, ",",
	) +
		(") SELECT grain_ns,bucket_ts_ns,dimension,model_id,credential_id," +
			"api_key_id,provider_id,members," +
			"(") +
		strings.Join(
			invalid, " OR ",
		) + ")," + strings.Join(metrics, ",") + " FROM (" + query + ")"
}

func ensureSummaryProjections(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(
		ctx, "INSERT OR IGNORE INTO meta(key,value) VALUES(?, 'pending')", summaryProjectionMigrationKey,
	); err != nil {
		return err
	}
	var status string
	if err := db.QueryRowContext(
		ctx, "SELECT value FROM meta WHERE key=?", summaryProjectionMigrationKey,
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
		"dimension TEXT NOT NULL", "model_id INTEGER REFERENCES dim_model(id)",
		"credential_id INTEGER REFERENCES dim_credential(id)", "api_key_id INTEGER REFERENCES dim_api_key(id)",
		"provider_id INTEGER REFERENCES dim_provider(id)",
		"members INTEGER NOT NULL CHECK(typeof(members)='integer' AND members>=0)",
		"invalid INTEGER NOT NULL DEFAULT 0 CHECK(invalid IN(0,1))",
	}
	for _, metric := range summaryMetricColumns {
		columns = append(
			columns, metric+" INTEGER NOT NULL DEFAULT 0 CHECK(typeof("+metric+")='integer' AND "+metric+">=0)",
		)
	}
	if _, err := tx.ExecContext(
		ctx, "CREATE TABLE IF NOT EXISTS summary_projection("+strings.Join(columns, ",")+")",
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		"CREATE UNIQUE INDEX IF NOT EXISTS uq_summary_projection ON summary_projection(grain_ns,dimension,"+
			"bucket_ts_ns,ifnull(model_id,0),ifnull(credential_id,0),ifnull(api_key_id,0),ifnull(provider_id,"+
			"0))",
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		"CREATE INDEX IF NOT EXISTS idx_summary_projection_invalid ON summary_projection(bucket_ts_ns) WHERE invalid=1",
	); err != nil {
		return err
	}
	for _, table := range []string{"hour_bucket", "day_bucket"} {
		grains := []int64{nsPerHour}
		if table == "day_bucket" {
			grains = []int64{nsPerDay, nsPerSummaryWindow}
		}
		var insert, remove strings.Builder
		for _, grain := range grains {
			for _, scope := range summaryProjectionScopes {
				insert.WriteString(projectionInsertSQL(table, "NEW.", scope, grain, false))
				remove.WriteString(projectionRemoveSQL(scope, grain))
			}
		}
		for _, trigger := range []struct{ name, event, body string }{
			{"insert", "INSERT", insert.String()}, {"delete", "DELETE", remove.String()},
			{"update", "UPDATE", remove.String() + insert.String()},
		} {
			if _, err := tx.ExecContext(
				ctx,
				"CREATE TRIGGER IF NOT EXISTS summary_projection_"+
					table+
					"_"+
					trigger.name+
					" AFTER "+
					trigger.event+
					" ON "+
					table+
					" BEGIN "+
					trigger.body+
					" END",
			); err != nil {
				return err
			}
		}
		if status != "complete" {
			for _, grain := range grains {
				for _, scope := range summaryProjectionScopes {
					if _, err := tx.ExecContext(
						ctx,
						fmt.Sprintf(
							"DELETE FROM summary_projection WHERE grain_ns=%d AND "+
								"dimension='%s'", grain, scope,
						),
					); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, projectionInsertSQL(table, "b.", scope, grain, true)); err != nil {
						if !strings.Contains(err.Error(), "integer overflow") {
							return err
						}
						if _, err := tx.ExecContext(ctx, projectionWideBackfillSQL(table, scope, grain)); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	if _, err := tx.ExecContext(
		ctx, "UPDATE meta SET value='complete' WHERE key=?", summaryProjectionMigrationKey,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func summaryProjectionTable(grain int64, scope string) string {
	return fmt.Sprintf("(SELECT * FROM summary_projection WHERE grain_ns=%d AND dimension='%s')", grain, scope)
}

func summaryProjectionDirtyWindows(ctx context.Context, q dbQueryer, from, to int64) ([]int64, error) {
	fromWindow, err := floorUnixNano(from, nsPerSummaryWindow)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(
		ctx,
		"SELECT day_ts_ns FROM dirty_day WHERE day_ts_ns>=? AND day_ts_ns<=? ORDER BY day_ts_ns",
		fromWindow,
		to,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var windows []int64
	for rows.Next() {
		var day int64
		if err := rows.Scan(&day); err != nil {
			return nil, err
		}
		window, err := floorUnixNano(day, nsPerSummaryWindow)
		if err != nil {
			return nil, err
		}
		if len(windows) == 0 || windows[len(windows)-1] != window {
			windows = append(windows, window)
		}
	}
	return windows, rows.Err()
}

func canUseSummaryProjections(ctx context.Context, q dbQueryer, from, to int64) (bool, error) {
	fromDay, err := floorUnixNano(from, nsPerSummaryWindow)
	if err != nil {
		return false, nil
	}
	var invalid bool
	if err := q.QueryRowContext(
		ctx, "SELECT EXISTS(SELECT 1 FROM summary_projection WHERE invalid=1 AND bucket_ts_ns>=? AND bucket_ts_ns<=?)",
		fromDay, to,
	).Scan(&invalid); err != nil {
		return false, err
	}
	if invalid {
		log.Warn("usage: aggregate projection exceeds integer precision; querying original buckets")
	}
	return !invalid, nil
}
