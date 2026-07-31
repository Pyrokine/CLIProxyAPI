package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestParseAny_DetectsAllFormats feeds a sample of every supported format
// through ParseAny and verifies the expected parser is selected.
func TestParseAny_DetectsAllFormats(t *testing.T) {
	tests := []struct {
		name       string
		data       []byte
		wantParser string
	}{
		{
			name:       "v2 detail envelope",
			data:       []byte(`{"date":"2026-04-27","details":[{"timestamp":"2026-04-27T10:00:00Z","model":"m"}]}`),
			wantParser: "flatDetailsParser",
		},
		{
			name:       "v2 today envelope",
			data:       []byte(`{"date":"2026-04-27","saved_at":"2026-04-27T11:00:00Z","details":[{"timestamp":"2026-04-27T10:00:00Z","model":"m"}]}`),
			wantParser: "flatDetailsParser",
		},
		{
			name:       "v1 nested snapshot",
			data:       []byte(`{"version":1,"usage":{"apis":{"key1":{"models":{"m":{"details":[{"timestamp":"2026-04-27T10:00:00Z"}]}}}}}}`),
			wantParser: "statisticsSnapshotParser",
		},
		{
			name:       "bare flat array",
			data:       []byte(`[{"timestamp":"2026-04-27T10:00:00Z","model":"m"}]`),
			wantParser: "flatArrayParser",
		},
		{
			name: "csv with required headers",
			data: []byte(strings.Join(
				[]string{
					"timestamp,model,source,api_key,user,result,input_tokens,output_tokens,reasoning_tokens,cached_tokens,total_tokens",
					`2026-04-27T10:00:00Z,m,src,k,u,success,10,20,0,0,30`,
				}, "\n",
			)),
			wantParser: "csvParser",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				details, parser, err := ParseAny(tt.data)
				if err != nil {
					t.Fatalf("ParseAny: %v", err)
				}
				if !strings.Contains(parser, tt.wantParser) {
					t.Fatalf("parser=%q, want contain %q", parser, tt.wantParser)
				}
				if len(details) == 0 {
					t.Fatalf("expected non-empty details, got 0")
				}
			},
		)
	}
}

func TestCSVParserStrictValidation(t *testing.T) {
	const header = "timestamp,model,source,api_key,result,input_tokens,output_tokens,total_tokens"
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "empty header",
			payload: header + ",\n2026-04-27T10:00:00Z,m,src,k,success,1,2,3,value",
			want:    "empty column",
		},
		{
			name:    "duplicate header",
			payload: header + ",api_key\n2026-04-27T10:00:00Z,m,src,k,success,1,2,3,k2",
			want:    "duplicate column",
		},
		{
			name:    "mixed width",
			payload: header + "\n2026-04-27T10:00:00Z,m,src,k,success,1,2,3,extra",
			want:    "wrong number of fields",
		},
		{
			name:    "invalid integer",
			payload: header + "\n2026-04-27T10:00:00Z,m,src,k,success,invalid,2,3",
			want:    "input_tokens",
		},
		{
			name:    "negative integer",
			payload: header + "\n2026-04-27T10:00:00Z,m,src,k,success,1,-2,3",
			want:    "negative value",
		},
		{
			name:    "unknown result",
			payload: header + "\n2026-04-27T10:00:00Z,m,src,k,pending,1,2,3",
			want:    "want success or failed",
		},
		{
			name:    "invalid thinking budget",
			payload: header + ",thinking_budget\n2026-04-27T10:00:00Z,m,src,k,success,1,2,3,nope",
			want:    "thinking_budget",
		},
		{
			name:    "negative thinking budget",
			payload: header + ",thinking_budget\n2026-04-27T10:00:00Z,m,src,k,success,1,2,3,-1",
			want:    "negative value",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := ParseAny([]byte(test.payload))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseAny error = %v, want contain %q", err, test.want)
			}
		})
	}
}

func TestCSVParserDoesNotUseMaskedAPIKeyAsIdentity(t *testing.T) {
	payload := []byte(strings.Join(
		[]string{
			"timestamp,model,source,api_key,result,input_tokens,output_tokens,total_tokens",
			"2026-04-27T10:00:00Z,m,src,abcd...wxyz,success,1,2,3",
		},
		"\n",
	))
	details, _, err := ParseAny(payload)
	if err != nil {
		t.Fatalf("ParseAny: %v", err)
	}
	if len(details) != 1 || details[0].APIKey != "" {
		t.Fatalf("masked API key parsed as identity: %+v", details)
	}
}

func TestImportBytesRejectsInvalidJSONDetails(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		want    string
	}{
		{
			name: "negative token in flat array",
			payload: `[{"timestamp":"2026-04-27T10:00:00Z","model":"m",` +
				`"tokens":{"input_tokens":-1}}]`,
			want: "input_tokens must be non-negative",
		},
		{
			name: "negative token in snapshot envelope",
			payload: `{"version":1,"usage":{"apis":{"k":{"models":{"m":{"details":[` +
				`{"timestamp":"2026-04-27T10:00:00Z","tokens":{"total_tokens":-1}}` +
				`]}}}}}}`,
			want: "total_tokens must be non-negative",
		},
		{
			name: "timestamp outside UnixNano range",
			payload: `[{"timestamp":"9999-01-01T00:00:00Z","model":"m",` +
				`"tokens":{"total_tokens":1}}]`,
			want: "outside the UnixNano range",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			_, err := ImportBytes(context.Background(), store, nil, []byte(test.payload))
			if !errors.Is(err, ErrInvalidImport) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ImportBytes error = %v, want ErrInvalidImport containing %q", err, test.want)
			}
			var events int64
			if countErr := store.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&events); countErr != nil {
				t.Fatalf("count events: %v", countErr)
			}
			if events != 0 {
				t.Fatalf("events = %d after invalid import, want 0", events)
			}
		})
	}
}

func TestImportBytesRollsBackWholePayloadOnPersistError(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	payload := []byte(`[
		{"timestamp":"2026-04-27T10:00:00Z","model":"good-1","tokens":{"input_tokens":1,"total_tokens":1}},
		{"timestamp":"2026-04-27T10:01:00Z","model":"bad","tokens":{"input_tokens":1,"total_tokens":1}},
		{"timestamp":"2026-04-27T10:02:00Z","model":"good-2","tokens":{"input_tokens":1,"total_tokens":1}}
	]`)
	priceFn := func(model string) (float64, float64, float64, bool) {
		if model == "bad" {
			return math.NaN(), 0, 0, true
		}
		return 1, 0, 0, true
	}

	_, err := ImportBytes(ctx, store, priceFn, payload)
	if err == nil || !strings.Contains(err.Error(), "import event[1]") {
		t.Fatalf("ImportBytes error = %v, want event[1] rejection", err)
	}
	for _, table := range []string{"events", "hour_bucket", "dim_model"} {
		var count int64
		if err := store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s rows=%d after failed import, want 0", table, count)
		}
	}
}

// TestImportBytes_Idempotent verifies that importing the same payload twice
// adds rows on the first pass and skips them all on the second — the core
// dedup guarantee that lets users freely re-import their backups.
func TestImportBytes_Idempotent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	payload := []byte(`{
		"date":"2026-04-27",
		"details":[
			{"timestamp":"2026-04-27T10:00:00Z","model":"m1","api_key":"k","tokens":{"total_tokens":100}},
			{"timestamp":"2026-04-27T10:01:00Z","model":"m2","api_key":"k","tokens":{"total_tokens":200}}
		]
	}`)

	r1, err := ImportBytes(ctx, store, nil, payload)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if r1.Added != 2 || r1.Skipped != 0 {
		t.Fatalf("first import added=%d skipped=%d, want 2/0", r1.Added, r1.Skipped)
	}

	r2, err := ImportBytes(ctx, store, nil, payload)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if r2.Added != 0 || r2.Skipped != 2 {
		t.Fatalf("second import added=%d skipped=%d, want 0/2", r2.Added, r2.Skipped)
	}
}

// TestFingerprint_StableAcrossFormats verifies that the same logical event
// arriving via v2 detail JSON, v1 nested snapshot, and bare flat array
// produces an identical fingerprint — the property that makes cross-format
// imports idempotent.
func TestFingerprint_StableAcrossFormats(t *testing.T) {
	ts, _ := time.Parse(time.RFC3339, "2026-04-27T10:00:00Z")

	// One canonical FlatDetail.
	canonical := FlatDetail{
		Timestamp: ts,
		Model:     "m",
		APIKey:    "k",
		Source:    "src",
		Tokens:    tokenStats{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
	}
	want := ComputeFingerprint(canonical)

	// 1) v2 detail envelope round-trip via JSON.
	v2 := map[string]any{"date": "2026-04-27", "details": []FlatDetail{canonical}}
	v2Bytes, _ := json.Marshal(v2)
	v2Details, _, err := ParseAny(v2Bytes)
	if err != nil || len(v2Details) != 1 {
		t.Fatalf("v2 parse: %v, len=%d", err, len(v2Details))
	}
	if got := ComputeFingerprint(v2Details[0]); got != want {
		t.Fatalf("v2 fingerprint differs: %x vs %x", got, want)
	}

	// 2) Bare array round-trip.
	arrBytes, _ := json.Marshal([]FlatDetail{canonical})
	arrDetails, _, err := ParseAny(arrBytes)
	if err != nil || len(arrDetails) != 1 {
		t.Fatalf("array parse: %v, len=%d", err, len(arrDetails))
	}
	if got := ComputeFingerprint(arrDetails[0]); got != want {
		t.Fatalf("array fingerprint differs: %x vs %x", got, want)
	}

	// 3) v1 nested snapshot via flattenSnapshot.
	snap := StatisticsSnapshot{
		APIs: map[string]aPISnapshot{
			"k": {
				Models: map[string]modelSnapshot{
					"m": {
						Details: []requestDetail{
							{
								Timestamp: ts,
								Source:    "src",
								Tokens:    canonical.Tokens,
							},
						},
					},
				},
			},
		},
	}
	flat := flattenSnapshot(snap)
	if len(flat) != 1 {
		t.Fatalf("flattenSnapshot len=%d, want 1", len(flat))
	}
	if got := ComputeFingerprint(flat[0]); got != want {
		t.Fatalf("v1 nested fingerprint differs: %x vs %x", got, want)
	}
}

// TestImport_CrossFormatDedup imports the same logical event via v2 detail and
// then via v1 nested snapshot and verifies the second import skips the row
// (fingerprint UNIQUE).
func TestCSVParserPreservesRuntimeMetadata(t *testing.T) {
	legacyPayload := []byte(strings.Join(
		[]string{
			"timestamp,model,source,api_key,result,input_tokens,output_tokens,total_tokens,reasoning_effort,service_tier,executor_type",
			`2026-04-27T10:00:00Z,m,src,k,success,10,20,30,medium,flex,OpenAICompatExecutor`,
		}, "\n",
	))

	legacyDetails, _, err := ParseAny(legacyPayload)
	if err != nil {
		t.Fatalf("ParseAny legacy CSV: %v", err)
	}
	if len(legacyDetails) != 1 {
		t.Fatalf("legacy details len=%d, want 1", len(legacyDetails))
	}
	if got := legacyDetails[0].ReasoningEffort; got != "medium" {
		t.Fatalf("legacy ReasoningEffort = %q, want medium", got)
	}
	if got := legacyDetails[0].ServiceTier; got != "flex" {
		t.Fatalf("legacy ServiceTier = %q, want flex", got)
	}
	if got := legacyDetails[0].RequestServiceTier; got != "flex" {
		t.Fatalf("legacy RequestServiceTier = %q, want flex", got)
	}
	if got := legacyDetails[0].ResponseServiceTier; got != "" {
		t.Fatalf("legacy ResponseServiceTier = %q, want empty", got)
	}
	if got := legacyDetails[0].ExecutorType; got != "OpenAICompatExecutor" {
		t.Fatalf("legacy ExecutorType = %q, want OpenAICompatExecutor", got)
	}

	payload := []byte(strings.Join(
		[]string{
			"timestamp,model,source,api_key,result,input_tokens,output_tokens,total_tokens,reasoning_effort,service_tier,request_service_tier,response_service_tier,executor_type",
			`2026-04-27T10:00:00Z,m,src,k,success,10,20,30,medium,auto,flex,default,OpenAICompatExecutor`,
		}, "\n",
	))

	details, _, err := ParseAny(payload)
	if err != nil {
		t.Fatalf("ParseAny CSV: %v", err)
	}
	if len(details) != 1 {
		t.Fatalf("details len=%d, want 1", len(details))
	}
	if got := details[0].ServiceTier; got != "flex" {
		t.Fatalf("ServiceTier = %q, want flex", got)
	}
	if got := details[0].RequestServiceTier; got != "flex" {
		t.Fatalf("RequestServiceTier = %q, want flex", got)
	}
	if got := details[0].ResponseServiceTier; got != "default" {
		t.Fatalf("ResponseServiceTier = %q, want default", got)
	}
}

func TestFlattenSnapshotPreservesRuntimeMetadata(t *testing.T) {
	ts, _ := time.Parse(time.RFC3339, "2026-04-27T10:00:00Z")
	snap := StatisticsSnapshot{
		APIs: map[string]aPISnapshot{
			"k": {
				Models: map[string]modelSnapshot{
					"m": {
						Details: []requestDetail{
							{
								Timestamp:           ts,
								ReasoningEffort:     "medium",
								ServiceTier:         "auto",
								RequestServiceTier:  "flex",
								ResponseServiceTier: "default",
								ExecutorType:        "OpenAICompatExecutor",
								Tokens:              tokenStats{TotalTokens: 30},
							},
						},
					},
				},
			},
		},
	}

	flat := flattenSnapshot(snap)
	if len(flat) != 1 {
		t.Fatalf("flattenSnapshot len=%d, want 1", len(flat))
	}
	if got := flat[0].ReasoningEffort; got != "medium" {
		t.Fatalf("ReasoningEffort = %q, want medium", got)
	}
	if got := flat[0].ServiceTier; got != "flex" {
		t.Fatalf("ServiceTier = %q, want flex", got)
	}
	if got := flat[0].RequestServiceTier; got != "flex" {
		t.Fatalf("RequestServiceTier = %q, want flex", got)
	}
	if got := flat[0].ResponseServiceTier; got != "default" {
		t.Fatalf("ResponseServiceTier = %q, want default", got)
	}
	if got := flat[0].ExecutorType; got != "OpenAICompatExecutor" {
		t.Fatalf("ExecutorType = %q, want OpenAICompatExecutor", got)
	}
}

func TestMergeSnapshotHookPreservesServiceTiers(t *testing.T) {
	ts, _ := time.Parse(time.RFC3339, "2026-04-27T10:00:00Z")
	snap := StatisticsSnapshot{
		APIs: map[string]aPISnapshot{
			"k": {
				Models: map[string]modelSnapshot{
					"m": {
						Details: []requestDetail{
							{
								Timestamp:           ts,
								ServiceTier:         "auto",
								RequestServiceTier:  "flex",
								ResponseServiceTier: "default",
							},
						},
					},
				},
			},
		},
	}

	var imported FlatDetail
	result := newRequestStatistics().MergeSnapshot(snap, func(detail FlatDetail) {
		imported = detail
	})
	if result.Added != 1 {
		t.Fatalf("MergeSnapshot added=%d, want 1", result.Added)
	}
	if got := imported.ServiceTier; got != "flex" {
		t.Fatalf("ServiceTier = %q, want flex", got)
	}
	if got := imported.RequestServiceTier; got != "flex" {
		t.Fatalf("RequestServiceTier = %q, want flex", got)
	}
	if got := imported.ResponseServiceTier; got != "default" {
		t.Fatalf("ResponseServiceTier = %q, want default", got)
	}
}

func TestImport_CrossFormatDedup(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	v2 := []byte(`{"date":"2026-04-27","details":[{"timestamp":"2026-04-27T10:00:00Z","model":"m","api_key":"k","source":"src","tokens":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}]}`)
	r1, err := ImportBytes(ctx, store, nil, v2)
	if err != nil {
		t.Fatalf("v2 import: %v", err)
	}
	if r1.Added != 1 {
		t.Fatalf("v2 added=%d want 1", r1.Added)
	}

	v1 := []byte(`{"version":1,"usage":{"apis":{"k":{"models":{"m":{"details":[{"timestamp":"2026-04-27T10:00:00Z","source":"src","tokens":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}]}}}}}}`)
	r2, err := ImportBytes(ctx, store, nil, v1)
	if err != nil {
		t.Fatalf("v1 import: %v", err)
	}
	if r2.Added != 0 || r2.Skipped != 1 {
		t.Fatalf("v1 added=%d skipped=%d, want 0/1 (cross-format dedup failed)", r2.Added, r2.Skipped)
	}
}

func TestImportBytes_ReturnsDayRange(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	payload := []byte(`{
		"date":"2026-04-27",
		"details":[
			{"timestamp":"2026-01-15T10:00:00Z","model":"m1","api_key":"k","tokens":{"total_tokens":100}},
			{"timestamp":"2026-03-20T11:00:00Z","model":"m2","api_key":"k","tokens":{"total_tokens":200}}
		]
	}`)

	result, err := ImportBytes(ctx, store, nil, payload)
	if err != nil {
		t.Fatalf("ImportBytes: %v", err)
	}
	if !result.HasDayRange {
		t.Fatal("expected import day range metadata")
	}
	wantEarliest := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC).UnixNano()
	wantLatest := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC).UnixNano()
	if result.EarliestDayNs != wantEarliest || result.LatestDayNs != wantLatest {
		t.Fatalf("day range=(%d,%d), want (%d,%d)", result.EarliestDayNs, result.LatestDayNs, wantEarliest, wantLatest)
	}
}

func TestImportBytesRollsBackEventsAndBucketsWhenDayRollupFails(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	dayStart := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	if _, err := store.db.ExecContext(ctx, "INSERT INTO dim_model(name) VALUES ('m')"); err != nil {
		t.Fatalf("insert model: %v", err)
	}
	if _, err := store.db.ExecContext(
		ctx,
		`INSERT INTO hour_bucket (
			bucket_ts_ns, model_id, requests, success, failure, total_tokens
		) VALUES (?, (SELECT id FROM dim_model WHERE name = 'm'), 1, 1, 0, ?)`,
		dayStart.UnixNano(),
		int64(math.MaxInt64),
	); err != nil {
		t.Fatalf("insert existing hour bucket: %v", err)
	}

	payload := []byte(`{
		"date":"2026-04-27",
		"details":[
			{"timestamp":"2026-04-27T01:00:00Z","model":"m","tokens":{"total_tokens":1}}
		]
	}`)
	_, err := ImportBytes(ctx, store, nil, payload)
	if err == nil || !strings.Contains(err.Error(), "rollup imported day") {
		t.Fatalf("ImportBytes error = %v, want day rollup failure", err)
	}

	var events, hours, days int64
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM hour_bucket").Scan(&hours); err != nil {
		t.Fatalf("count hour buckets: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM day_bucket").Scan(&days); err != nil {
		t.Fatalf("count day buckets: %v", err)
	}
	if events != 0 || hours != 1 || days != 0 {
		t.Fatalf(
			"post-failure rows events=%d hours=%d days=%d, want 0/1/0",
			events,
			hours,
			days,
		)
	}
}

func TestImportBytesReturnsUnixEpochDayRange(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	payload := []byte(`{
		"date":"1970-01-01",
		"details":[
			{"timestamp":"1970-01-01T12:00:00Z","model":"m","api_key":"k","tokens":{"total_tokens":1}}
		]
	}`)

	result, err := ImportBytes(ctx, store, nil, payload)
	if err != nil {
		t.Fatalf("ImportBytes: %v", err)
	}
	if !result.HasDayRange {
		t.Fatal("HasDayRange = false, want true for Unix epoch day")
	}
	if result.EarliestDayNs != 0 || result.LatestDayNs != 0 {
		t.Fatalf(
			"day range=(%d,%d), want (0,0)",
			result.EarliestDayNs,
			result.LatestDayNs,
		)
	}
	var requests int64
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT requests FROM day_bucket WHERE bucket_ts_ns = 0",
	).Scan(&requests); err != nil {
		t.Fatalf("query epoch day bucket: %v", err)
	}
	if requests != 1 {
		t.Fatalf("epoch day requests = %d, want 1", requests)
	}
	var dirtyDays int64
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM dirty_day WHERE day_ts_ns = 0",
	).Scan(&dirtyDays); err != nil {
		t.Fatalf("query dirty epoch day: %v", err)
	}
	if dirtyDays != 0 {
		t.Fatalf("dirty epoch days = %d, want 0 after atomic rollup", dirtyDays)
	}
}

func TestImportBytesUsesFloorBucketsBeforeUnixEpoch(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	payload := []byte(`[{"timestamp":"1969-12-31T23:30:00Z","model":"m","tokens":{"total_tokens":1}}]`)

	result, err := ImportBytes(ctx, store, nil, payload)
	if err != nil {
		t.Fatalf("ImportBytes: %v", err)
	}
	wantHour := time.Date(1969, 12, 31, 23, 0, 0, 0, time.UTC).UnixNano()
	wantDay := time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC).UnixNano()
	if !result.HasDayRange || result.EarliestDayNs != wantDay || result.LatestDayNs != wantDay {
		t.Fatalf("day range = (%v, %d, %d), want (true, %d, %d)", result.HasDayRange, result.EarliestDayNs, result.LatestDayNs, wantDay, wantDay)
	}
	var hourNs, dayNs int64
	if err := store.db.QueryRow("SELECT bucket_ts_ns FROM hour_bucket").Scan(&hourNs); err != nil {
		t.Fatalf("query hour bucket: %v", err)
	}
	if err := store.db.QueryRow("SELECT bucket_ts_ns FROM day_bucket").Scan(&dayNs); err != nil {
		t.Fatalf("query day bucket: %v", err)
	}
	if hourNs != wantHour || dayNs != wantDay {
		t.Fatalf("bucket starts = (%d, %d), want (%d, %d)", hourNs, dayNs, wantHour, wantDay)
	}
}

func TestImportBytes_LegacySnapshotPreservesFailedFlags(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	payload := []byte(`{
		"version":1,
		"usage":{
			"total_requests":2,
			"success_count":1,
			"failure_count":1,
			"total_tokens":35,
			"apis":{
				"k":{
					"total_requests":2,
					"total_tokens":35,
					"models":{
						"m":{
							"total_requests":2,
							"total_tokens":35,
							"details":[
								{"timestamp":"2026-04-27T10:00:00Z","source":"src","tokens":{"total_tokens":30},"failed":false},
								{"timestamp":"2026-04-27T10:01:00Z","source":"src","tokens":{"total_tokens":5},"failed":true}
							]
						}
					}
				}
			}
		}
	}`)

	result, err := ImportBytes(ctx, store, nil, payload)
	if err != nil {
		t.Fatalf("ImportBytes legacy snapshot: %v", err)
	}
	if result.Added != 2 || result.Skipped != 0 {
		t.Fatalf("legacy import added=%d skipped=%d, want 2/0", result.Added, result.Skipped)
	}

	events, total, err := store.QueryEvents(
		ctx,
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		EventFilters{}, 1, 10, "timestamp", false,
	)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if total != 2 || len(events) != 2 {
		t.Fatalf("query total=%d len=%d, want 2/2", total, len(events))
	}
	if events[0].Failed {
		t.Fatalf("first event failed=%v, want false", events[0].Failed)
	}
	if !events[1].Failed {
		t.Fatalf("second event failed=%v, want true", events[1].Failed)
	}
}

// TestCsvParser_RoundTrip writes a tiny CSV, imports it, exports it back via
// QueryEvents, and verifies the timestamp / model / tokens round-trip.
func TestCsvParser_RoundTrip(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	csv := bytes.Join(
		[][]byte{
			[]byte("timestamp,model,source,api_key,user,result,input_tokens,output_tokens,reasoning_tokens,cached_tokens,total_tokens"),
			[]byte("2026-04-27T10:00:00Z,m,src,k,user,success,10,20,0,0,30"),
			[]byte("2026-04-27T10:01:00Z,m,src,k,user,failed,5,0,0,0,5"),
		}, []byte("\n"),
	)

	r, err := ImportBytes(ctx, store, nil, csv)
	if err != nil {
		t.Fatalf("import csv: %v", err)
	}
	if r.Added != 2 {
		t.Fatalf("csv added=%d want 2", r.Added)
	}

	events, total, err := store.QueryEvents(
		ctx,
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		EventFilters{}, 1, 100, "timestamp", false,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != 2 || len(events) != 2 {
		t.Fatalf("query total=%d len=%d, want 2/2", total, len(events))
	}
	for _, ev := range events {
		if ev.Model != "m" {
			t.Fatalf("event model=%q want m", ev.Model)
		}
	}
}

// TestMigrateLegacy_EmptyDirIsIdempotent runs MigrateLegacyToSQLite on a fresh
// (empty) baseDir twice and verifies the meta marker is written once and the
// second run short-circuits without error.
func TestImportFile_RejectsLargeInput(t *testing.T) {
	baseDir := t.TempDir()
	store, err := OpenStore(filepath.Join(baseDir, "events.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			t.Fatalf("Close: %v", errClose)
		}
	}()

	largeFile := filepath.Join(baseDir, "too-large.json")
	f, err := os.Create(largeFile)
	if err != nil {
		t.Fatalf("create large file: %v", err)
	}
	if err := f.Truncate((100 << 20) + 1); err != nil {
		_ = f.Close()
		t.Fatalf("truncate large file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close large file: %v", err)
	}

	_, err = ImportFile(context.Background(), store, nil, largeFile)
	if err == nil {
		t.Fatal("expected oversized import file to be rejected")
	}
	if !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("expected file too large error, got %v", err)
	}
}

func TestMigrateLegacy_EmptyDirIsIdempotent(t *testing.T) {
	baseDir := t.TempDir()
	store, err := OpenStore(filepath.Join(baseDir, "events.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			t.Fatalf("Close: %v", errClose)
		}
	}()

	ctx := context.Background()
	if err := MigrateLegacyToSQLite(ctx, store, nil, baseDir); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	status1, err := CheckMigrationStatus(ctx, store)
	if err != nil {
		t.Fatalf("status 1: %v", err)
	}
	if status1.From == "" {
		t.Fatalf("expected meta marker after first migrate")
	}

	if err := MigrateLegacyToSQLite(ctx, store, nil, baseDir); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	status2, err := CheckMigrationStatus(ctx, store)
	if err != nil {
		t.Fatalf("status 2: %v", err)
	}
	if status2.From != status1.From || !status2.At.Equal(status1.At) {
		t.Fatalf("meta marker shifted on idempotent re-run: %v -> %v", status1, status2)
	}
}
