package usage

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

type requestSpeedCase struct {
	name   string
	detail FlatDetail
	speed  string
	source string
}

func requestSpeedCases() []requestSpeedCase {
	cases := []requestSpeedCase{
		{"legacy", FlatDetail{ServiceTier: "priority"}, "unknown", "unknown"},
		{"request_tier_only", FlatDetail{RequestServiceTier: "request-only-priority"}, "unknown", "unknown"},
		{
			"request_not_applied", FlatDetail{RequestServiceTier: "priority", ResponseServiceTier: "default"},
			"standard", "response_service_tier",
		},
		{"tier_priority", FlatDetail{ResponseServiceTier: " \tPrIoRiTy\r\n"}, "fast", "response_service_tier"},
		{"tier_unrecognized", FlatDetail{ResponseServiceTier: "response-unrecognized-tier"}, "unknown", "unknown"},
		{"claude_priority", FlatDetail{Provider: " \tClAuDe\n", ResponseServiceTier: "priority"}, "unknown", "unknown"},
		{
			"executor_priority", FlatDetail{ExecutorType: " cLaUdEeXeCuToR ", ResponseServiceTier: "priority"},
			"unknown", "unknown",
		},
		{
			"request_speed_only", FlatDetail{RequestSpeed: "request-only-fast", ResponseServiceTier: "priority"},
			"unknown", "unknown",
		},
		{"speed_priority", FlatDetail{ResponseSpeed: "priority", ResponseServiceTier: "default"}, "unknown", "unknown"},
		{"speed_default", FlatDetail{Provider: "claude", ResponseSpeed: "default"}, "unknown", "unknown"},
		{
			"speed_unrecognized", FlatDetail{ResponseSpeed: "response-unrecognized-speed", ResponseServiceTier: "fast"},
			"unknown", "unknown",
		},
		{
			"speed_overrides_tier",
			FlatDetail{RequestSpeed: "fast", ResponseSpeed: "standard", ResponseServiceTier: "priority"}, "standard",
			"response_speed",
		},
		{
			"failed_returned_speed", FlatDetail{Provider: "claude", ResponseSpeed: "fast", Failed: true}, "fast",
			"response_speed",
		},
		{
			"failed_returned_tier", FlatDetail{ResponseServiceTier: "priority", Failed: true}, "fast",
			"response_service_tier",
		},
		{"failed_without_response", FlatDetail{RequestServiceTier: "priority", Failed: true}, "unknown", "unknown"},
		{"unicode_whitespace", FlatDetail{ResponseSpeed: "\u3000\tFaSt\u00a0\n"}, "fast", "response_speed"},
		{
			"whitespace_speed_is_absent", FlatDetail{RequestSpeed: " \t\r\n", ResponseServiceTier: "default"},
			"standard", "response_service_tier",
		},
		{"unknown_response", FlatDetail{ResponseServiceTier: "UNKNOWN"}, "unknown", "unknown"},
		{"no_observations", FlatDetail{}, "unknown", "unknown"},
	}
	for _, speed := range []string{"standard", "fast", "ultrafast", "flex", "batch"} {
		cases = append(
			cases,
			requestSpeedCase{
				"tier_" + speed, FlatDetail{ResponseServiceTier: strings.ToUpper(speed)}, speed,
				"response_service_tier",
			},
			requestSpeedCase{
				"speed_" + speed, FlatDetail{Provider: "claude", ResponseSpeed: " \t" + strings.ToUpper(speed) + "\n"},
				speed, "response_speed",
			},
		)
	}
	return cases
}

func TestObservedRequestSpeed(t *testing.T) {
	if speed, source := ObservedRequestSpeed(nil); speed != "unknown" || source != "unknown" {
		t.Fatalf("nil observation = %q, %q", speed, source)
	}
	for _, tc := range requestSpeedCases() {
		t.Run(
			tc.name, func(t *testing.T) {
				if speed, source := ObservedRequestSpeed(&tc.detail); speed != tc.speed || source != tc.source {
					t.Fatalf("observation = %q, %q; want %q, %q", speed, source, tc.speed, tc.source)
				}
				payload, err := json.Marshal(tc.detail)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(payload, &fields); err != nil {
					t.Fatal(err)
				}
				if string(fields["speed"]) != `"`+tc.speed+`"` {
					t.Fatalf("JSON speed = %s, want %q", fields["speed"], tc.speed)
				}
				metadata, err := encodeEventMetadata(&tc.detail)
				if err != nil {
					t.Fatal(err)
				}
				if metadata != nil {
					var stored map[string]json.RawMessage
					if err := json.Unmarshal([]byte(metadata.(string)), &stored); err != nil {
						t.Fatal(err)
					}
					if _, exists := stored["speed"]; exists {
						t.Fatalf("derived speed persisted in metadata: %s", metadata)
					}
				}
			},
		)
	}
	var legacy FlatDetail
	if err := json.Unmarshal([]byte(`{"service_tier":"priority","speed":"fast"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if speed, _ := ObservedRequestSpeed(&legacy); speed != "unknown" {
		t.Fatalf("client-provided derived speed changed legacy observation: %q", speed)
	}
}

func TestObservedRequestSpeedRejectsNonASCIITierAlias(t *testing.T) {
	detail := FlatDetail{ResponseServiceTier: "PRİORİTY"}
	speed, source := ObservedRequestSpeed(&detail)
	if speed != "unknown" || source != "unknown" {
		t.Fatalf("non-ASCII tier = %q, %q; want unknown evidence", speed, source)
	}
	store := openTestStore(t)
	metadata, err := json.Marshal(map[string]string{"response_service_tier": detail.ResponseServiceTier})
	if err != nil {
		t.Fatal(err)
	}
	var sqlSpeed string
	if err := store.db.QueryRowContext(
		t.Context(), "SELECT "+observedRequestSpeedSQL()+
			" FROM (SELECT ? AS metadata, NULL AS provider_id) AS events", string(metadata),
	).Scan(&sqlSpeed); err != nil {
		t.Fatal(err)
	}
	if sqlSpeed != speed {
		t.Fatalf("SQL speed = %q, Go speed = %q", sqlSpeed, speed)
	}
}

func TestParseRequestSpeedFilter(t *testing.T) {
	for _, value := range []string{"", " \t\n", "standard", " FAST ", "ULTRAFAST", "flex", "batch", "UNKNOWN"} {
		got, err := ParseRequestSpeedFilter(value)
		if err != nil || got != strings.ToLower(strings.TrimSpace(value)) {
			t.Fatalf("parse %q = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"priority", "default", "turbo", "fast,flex", "fast unknown", "' OR 1=1 --"} {
		if _, err := ParseRequestSpeedFilter(value); !errors.Is(err, ErrInvalidRequestSpeedFilter) {
			t.Fatalf("parse %q error = %v", value, err)
		}
	}
}

func TestRequestSpeedQueriesSQLiteAndMemory(t *testing.T) {
	ctx := t.Context()
	store := openTestStore(t)
	stats := NewRequestStatistics()
	base := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	cases := requestSpeedCases()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := range cases {
		detail := &cases[i].detail
		detail.Timestamp = base.Add(time.Duration(i) * time.Second)
		detail.RequestID = cases[i].name
		detail.Model = "speed-model"
		detail.Source = "speed-credential"
		detail.APIKey = "synthetic-speed-client"
		detail.Tokens = tokenStats{InputTokens: 1, TotalTokens: 1}
		if _, err := persistEvent(ctx, tx, store, detail, nil); err != nil {
			t.Fatal(err)
		}
		imported := stats.mergeSnapshot(
			StatisticsSnapshot{
				APIs: map[string]aPISnapshot{
					detail.APIKey: {
						Models: map[string]modelSnapshot{
							detail.Model: {
								Details: []requestDetail{
									{
										Timestamp: detail.Timestamp, RequestID: detail.RequestID,
										Source: detail.Source, Provider: detail.Provider,
										ExecutorType: detail.ExecutorType,
										ServiceTier:  detail.ServiceTier, RequestServiceTier: detail.RequestServiceTier,
										ResponseServiceTier: detail.ResponseServiceTier,
										RequestSpeed:        detail.RequestSpeed, ResponseSpeed: detail.ResponseSpeed,
										Failed: detail.Failed, Completed: detail.Completed, Tokens: detail.Tokens,
									},
								},
							},
						},
					},
				},
			},
		)
		if imported.Added != 1 {
			t.Fatalf("memory seed %s = %+v", cases[i].name, imported)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range cases {
		var actual string
		if err := store.db.QueryRowContext(
			ctx, "SELECT "+observedRequestSpeedSQL()+
				" FROM events WHERE json_extract(metadata, '$.request_id') = ?", tc.name,
		).Scan(&actual); err != nil {
			t.Fatal(err)
		}
		if actual != tc.speed {
			t.Fatalf("SQL %s = %q, want %q", tc.name, actual, tc.speed)
		}
	}

	queries := []struct {
		name    string
		filters EventFilters
	}{
		{"all", EventFilters{}},
		{"standard", EventFilters{Speed: "standard"}},
		{"fast", EventFilters{Speed: " \tFAST\n"}},
		{"ultrafast", EventFilters{Speed: "ultrafast"}},
		{"flex", EventFilters{Speed: "flex"}},
		{"batch", EventFilters{Speed: "batch"}},
		{"unknown", EventFilters{Speed: "unknown"}},
		{"fast_failed", EventFilters{Speed: "fast", Status: "failure"}},
		{"fast_success", EventFilters{Speed: "fast", Status: "success"}},
		{
			"combined", EventFilters{
				Speed: "fast", Model: "speed-model", Source: "speed-credential", APIKey: "synthetic-speed-client",
			},
		},
		{"missing_model", EventFilters{Speed: "fast", Model: "absent"}},
	}
	for _, search := range []string{
		"priority", "default", "standard", "ULTRAFAST", "request-only", "response-unrecognized", "unknown", " \tFAST\n",
	} {
		queries = append(
			queries, struct {
				name    string
				filters EventFilters
			}{"search_" + strings.TrimSpace(search), EventFilters{Search: search}},
		)
	}
	queries = append(
		queries, struct {
			name    string
			filters EventFilters
		}{"request_only_unknown", EventFilters{Speed: "unknown", Search: "request-only"}},
	)

	for _, query := range queries {
		t.Run(
			query.name, func(t *testing.T) {
				var want []string
				for _, tc := range cases {
					filter := query.filters
					if speed := strings.ToLower(strings.TrimSpace(filter.Speed)); speed != "" && speed != tc.speed {
						continue
					}
					if filter.Model != "" && filter.Model != tc.detail.Model {
						continue
					}
					if filter.Status == "success" && tc.detail.Failed || filter.Status == "failure" && !tc.detail.Failed {
						continue
					}
					searchable := strings.Join(
						[]string{
							tc.detail.ServiceTier, tc.detail.RequestServiceTier,
							tc.detail.ResponseServiceTier, tc.detail.RequestSpeed, tc.detail.ResponseSpeed, tc.speed,
						}, "\n",
					)
					if search := strings.TrimSpace(filter.Search); search != "" &&
						!strings.Contains(strings.ToLower(searchable), strings.ToLower(search)) {
						continue
					}
					want = append(want, tc.name)
				}
				for name, queryEvents := range map[string]func(
					context.Context,
					time.Time,
					time.Time,
					EventFilters,
					int,
					int,
					string,
					bool,
				) ([]FlatDetail, int, error){
					"sqlite_full":    store.queryEventDetails,
					"sqlite_compact": store.queryCompactEventDetails,
					"memory":         stats.QueryEventsContext,
				} {
					t.Run(
						name, func(t *testing.T) {
							for page := 1; page <= (len(want)+1)/2+1; page++ {
								events, total, err := queryEvents(
									ctx, base.Add(-time.Second), base.Add(time.Hour), query.filters, page, 2,
									"timestamp", false,
								)
								if err != nil {
									t.Fatal(err)
								}
								if total != len(want) {
									t.Fatalf("total = %d, want %d", total, len(want))
								}
								var got []string
								for _, event := range events {
									got = append(got, event.RequestID)
								}
								start := min((page-1)*2, len(want))
								expected := want[start:min(start+2, len(want))]
								if !slices.Equal(got, expected) {
									t.Fatalf("page %d = %v, want %v", page, got, expected)
								}
							}
						},
					)
				}
			},
		)
	}

	for _, speed := range []string{"standard", "fast", "ultrafast", "flex", "batch", "unknown"} {
		events, err := store.queryBackupEventSet(ctx, time.Time{}, time.Time{}, EventFilters{Speed: speed})
		if err != nil {
			t.Fatal(err)
		}
		wantCount := 0
		for _, tc := range cases {
			if tc.speed == speed {
				wantCount++
			}
		}
		if len(events) != wantCount {
			t.Fatalf("backup %s count = %d, want %d", speed, len(events), wantCount)
		}
		for _, event := range events {
			if actual, _ := ObservedRequestSpeed(&event); actual != speed {
				t.Fatalf("backup %s contains %s", speed, event.RequestID)
			}
		}
	}

	for _, invalid := range []string{"turbo", "fast,flex", "priority", "' OR 1=1 --"} {
		filters := EventFilters{Speed: invalid, Model: "absent"}
		for name, query := range map[string]func(
			context.Context,
			time.Time,
			time.Time,
			EventFilters,
			int,
			int,
			string,
			bool,
		) ([]FlatDetail, int, error){
			"full": store.queryEventDetails, "compact": store.queryCompactEventDetails,
			"memory": stats.QueryEventsContext,
		} {
			if _, _, err := query(
				ctx, base.Add(-time.Second), base.Add(time.Hour), filters, 1, 2, "timestamp", false,
			); !errors.Is(err, ErrInvalidRequestSpeedFilter) {
				t.Fatalf("%s %q error = %v", name, invalid, err)
			}
		}
		if _, err := store.queryBackupEventSet(ctx, time.Time{}, time.Time{}, filters); !errors.Is(
			err, ErrInvalidRequestSpeedFilter,
		) {
			t.Fatalf("backup %q error = %v", invalid, err)
		}
	}
}

func TestRequestSpeedSQLLegacyMetadata(t *testing.T) {
	store := openTestStore(t)
	for _, metadata := range []any{
		nil, "", `{}`, `{"service_tier":"priority"}`, `{"request_service_tier":"priority"}`,
	} {
		var speed string
		query := "SELECT " + observedRequestSpeedSQL() + " FROM (SELECT ? AS metadata, NULL AS provider_id) AS events"
		if err := store.db.QueryRowContext(t.Context(), query, metadata).Scan(&speed); err != nil {
			t.Fatal(err)
		}
		if speed != "unknown" {
			t.Fatalf("legacy metadata %v = %q, want unknown", metadata, speed)
		}
	}
}

func TestRequestSpeedSQLWhitespaceMatchesGo(t *testing.T) {
	store := openTestStore(t)
	for _, r := range " \t\n\v\f\r\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000" {
		metadata, err := json.Marshal(map[string]string{"response_speed": string(r) + "FaSt" + string(r)})
		if err != nil {
			t.Fatal(err)
		}
		var speed string
		query := "SELECT " + observedRequestSpeedSQL() + " FROM (SELECT ? AS metadata, NULL AS provider_id) AS events"
		if err := store.db.QueryRowContext(t.Context(), query, string(metadata)).Scan(&speed); err != nil {
			t.Fatal(err)
		}
		if speed != "fast" {
			t.Fatalf("whitespace U+%04X = %q", r, speed)
		}
	}
}
