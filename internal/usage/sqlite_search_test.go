package usage

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestEventSearchPreservesLegacyLikeAndMetadataSemantics(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.db.Exec(
		`INSERT INTO dim_model(id,name) VALUES(1,'plain'),(2,'ModelNeedle');
		INSERT INTO dim_credential(id,name) VALUES(1,'CredentialNeedle');
		INSERT INTO dim_source(id,name) VALUES(1,'SourceNeedle');
		INSERT INTO dim_auth_index(id,name) VALUES(1,'AuthNeedle');
		INSERT INTO dim_api_key(id,name) VALUES(1,'ClientNeedle');
		INSERT INTO dim_provider(id,name) VALUES(1,'codex'),(2,'claude')`,
	); err != nil {
		t.Fatal(err)
	}
	metadata := []any{nil, "", "{broken", `null`, `[]`, `true`, `123`, `{}`, `{"reasoning_effort":"high"}`}
	for _, field := range []string{
		"service_tier", "request_service_tier", "response_service_tier", "request_speed", "response_speed",
	} {
		for _, value := range []string{
			`"priority"`, `"default"`, `"standard"`, `"ultrafast"`, `"request-only"`, `"response-unrecognized"`,
			`"multibyte-雪"`, `"MiXeD%_\\name"`, `" \tFAST\n"`, `"f38b20780f00b4d1"`,
			`1`, `1.0`, `1.5`, `1e2`, `1e999`, `true`, `false`, `null`,
			`{"name": "MiXeD", "value":1.0}`, `["priority", 1.0, true, null]`,
		} {
			metadata = append(metadata, fmt.Sprintf(`{"%s":%s,"reasoning_effort":"high"}`, field, value))
		}
	}
	metadata = append(
		metadata,
		`{"service_tier":"pr`+string(rune(92))+`u0069ority"}`,
		`{"request_serv`+string(rune(92))+`u0069ce_tier":"priority"}`,
		`{"request_service_tier":"priority","response_service_tier":"default"}`,
		`{"executor_type":"ClaudeExecutor","response_speed":"fast"}`,
		`{"executor_type":"ClaudeExecutor","request_speed":"fast"}`,
		`{"response_service_tier":"priority"}`,
	)
	for i, value := range metadata {
		var credential, source, auth, key any
		model, provider := 1, 1
		switch i % 7 {
		case 0:
			model = 2
		case 1:
			credential = 1
		case 2:
			source = 1
		case 3:
			auth = 1
		case 4:
			key = 1
		case 5:
			provider = 2
		}
		if _, err := store.db.Exec(
			`INSERT INTO events(id,fingerprint,ts_ns,model_id,credential_id,
			source_id,auth_index_id,api_key_id,provider_id,failed,metadata)
			VALUES(?,CAST(printf('%020d',?) AS BLOB),?,?,?,?,?,?,?,0,?)`,
			i+1, i+1, i+1, model, credential, source, auth, key, provider, value,
		); err != nil {
			t.Fatal(err)
		}
	}

	legacy := []string{
		"EXISTS (SELECT 1 FROM dim_model m WHERE m.id=events.model_id AND m.name LIKE ?)",
		"EXISTS (SELECT 1 FROM dim_credential c WHERE c.id=events.credential_id AND c.name LIKE ?)",
		"EXISTS (SELECT 1 FROM dim_source s WHERE s.id=events.source_id AND s.name LIKE ?)",
		"EXISTS (SELECT 1 FROM dim_auth_index a WHERE a.id=events.auth_index_id AND a.name LIKE ?)",
		"EXISTS (SELECT 1 FROM dim_api_key k WHERE k.id=events.api_key_id AND k.name LIKE ?)",
	}
	for _, field := range []string{
		"service_tier", "request_service_tier", "response_service_tier", "request_speed", "response_speed",
	} {
		legacy = append(
			legacy,
			"(CASE WHEN json_valid(events.metadata) THEN json_extract(events.metadata,'$."+field+"') END) LIKE ?",
		)
	}
	legacy = append(legacy, "("+observedRequestSpeedSQL()+") LIKE ?")
	readIDs := func(query string, args []any) []int64 {
		t.Helper()
		rows, err := store.db.Query(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	for _, search := range []string{
		"ModelNeedle", "credentialneedle", "SOURCENEEDLE", "AuthNeedle", "ClientNeedle", "Needle",
		"priority", "default", "standard", "FAST", "ultrafast", "flex", "batch", "unknown",
		"request-only", "response-unrecognized", "MiXeD", "雪", "f38b20780f00b4d1", "absent",
		"%", "_", "pr%ity", "pr_ority", "sta%rd", "st_nd_rd", "_n", "\\", `"`, "[", "{",
		"1", "1.0", "1.5", "100", "true", "false", "null", "Inf", "1e", " \tFAST\n", " \t ",
	} {
		t.Run(
			search, func(t *testing.T) {
				prepared, err := prepareEventsPageQuery(
					t.Context(), store.db, 0, 1000,
					EventFilters{Search: search}, "timestamp", false,
				)
				if err != nil {
					t.Fatal(err)
				}
				legacySQL := "SELECT id FROM events WHERE ts_ns>=? AND ts_ns<=?"
				args := []any{int64(0), int64(1000)}
				if query := strings.TrimSpace(search); query != "" {
					legacySQL += " AND (" + strings.Join(legacy, " OR ") + ")"
					for range legacy {
						args = append(args, "%"+query+"%")
					}
				}
				want := readIDs(legacySQL+" ORDER BY id", args)
				got := readIDs(
					"SELECT id FROM "+prepared.fromSQL+" "+prepared.whereSQL+" ORDER BY id", prepared.whereArgs,
				)
				if prepared.total != len(want) || !slices.Equal(got, want) {
					t.Fatalf("search=%q total=%d ids=%v want=%v", search, prepared.total, got, want)
				}
			},
		)
	}
	for _, compact := range []bool{false, true} {
		query := store.queryEventDetails
		if compact {
			query = store.queryCompactEventDetails
		}
		if _, _, err := query(
			t.Context(), time.Unix(0, 0), time.Unix(0, 1000),
			EventFilters{Search: "unknown"}, 1, MaxEventsPageSize, "timestamp", false,
		); err == nil || !strings.Contains(err.Error(), "metadata") {
			t.Fatalf("compact=%t selected malformed metadata error=%v", compact, err)
		}
	}
}

func TestEventAuthIndexSearchKeepsLargeMetadataOutOfRepeatedPredicates(t *testing.T) {
	store := openTestStore(t)
	const auth = "f38b20780f00b4d1"
	if _, err := store.db.Exec(
		`INSERT INTO dim_model(name) VALUES('history');
		INSERT INTO dim_auth_index(name) VALUES('f38b20780f00b4d1'),('other-auth')`,
	); err != nil {
		t.Fatal(err)
	}
	const events = 8192
	base := time.Date(2026, 10, 20, 8, 53, 0, 0, time.UTC)
	if _, err := store.db.Exec(
		`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<?)
		INSERT INTO events(fingerprint,ts_ns,model_id,auth_index_id,failed,total_tokens,metadata)
		SELECT CAST(printf('%020d',n) AS BLOB),?+n*1000000000,1,CASE WHEN n>? THEN 1 ELSE 2 END,0,1,
		json_object('reasoning_effort','high','response_service_tier','default','payload',?) FROM seq`,
		events, base.UnixNano(), events-6, strings.Repeat("x", 8192),
	); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	prepared, err := prepareEventsPageQuery(
		ctx, store.db, base.UnixNano(), base.Add(30*24*time.Hour).UnixNano(),
		EventFilters{Search: auth}, "timestamp", true,
	)
	if err != nil || prepared.total != 6 {
		t.Fatalf("auth-index search count=%d error=%v", prepared.total, err)
	}
	rows, err := store.db.Query(
		"EXPLAIN QUERY PLAN SELECT COUNT(*) FROM "+prepared.fromSQL+" "+prepared.whereSQL,
		prepared.whereArgs...,
	)
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
	if strings.Count(plan.String(), "LIST SUBQUERY") != 5 ||
		strings.Contains(plan.String(), "CORRELATED SCALAR SUBQUERY") ||
		strings.Contains(plan.String(), "SEARCH dim_provider") ||
		strings.Contains(prepared.fromSQL+prepared.whereSQL, "json_") {
		t.Fatalf("identifier search reintroduced per-event dimension or impossible speed work:\n%s", plan.String())
	}
	for _, compact := range []bool{false, true} {
		query := store.queryEventDetails
		if compact {
			query = store.queryCompactEventDetails
		}
		for page := 1; page <= 4; page++ {
			matched, total, err := query(
				ctx, base, base.Add(30*24*time.Hour), EventFilters{Search: auth},
				page, 2, "timestamp", true,
			)
			wantRows := max(0, min(2, 6-(page-1)*2))
			if err != nil || total != 6 || len(matched) != wantRows {
				t.Fatalf("compact=%t page=%d rows=%d total=%d error=%v", compact, page, len(matched), total, err)
			}
			for i, event := range matched {
				wantTimestamp := base.Add(time.Duration(events-(page-1)*2-i) * time.Second)
				if event.AuthIndex != auth || event.ReasoningEffort != "high" || !event.Timestamp.Equal(wantTimestamp) {
					t.Fatalf("compact=%t page=%d event=%+v", compact, page, event)
				}
			}
		}
	}
}
