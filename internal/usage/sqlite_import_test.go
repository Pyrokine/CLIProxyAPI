// Last compiled: 2026-05-07
// Author: pyro

package usage

import (
	"bytes"
	"context"
	"encoding/json"
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
	defer store.Close()

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
	defer store.Close()

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
