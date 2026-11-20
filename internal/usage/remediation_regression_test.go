package usage

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestImportNormalizesModelAcrossFormatsBeforeDedup(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for i, payload := range []string{
		`[{"timestamp":"2026-04-27T10:00:00Z","model":" UNKNOWN ","tokens":{"input_tokens":1,"total_tokens":1}}]`,
		`{"date":"2026-04-27","details":[{"timestamp":"2026-04-27T10:00:00Z","model":"unknown","tokens":{"` +
			`input_tokens":1,"total_tokens":1}}]}`,
	} {
		result, err := importBytes(ctx, store, nil, []byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		if result.Added != int64(1-i) || result.Skipped != int64(i) {
			t.Fatalf("pass=%d result=%+v", i, result)
		}
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM dim_model WHERE name = 'unknown'").Scan(&count); err != nil ||
		count != 1 {
		t.Fatalf("dimensions=%d err=%v", count, err)
	}
}

func TestImportHistoricalCSVWithoutAPIKeyAndLegacyCredential(t *testing.T) {
	for _, header := range []string{"source", "credential", "auth_id"} {
		store := openTestStore(t)
		payload := "timestamp,model," +
			header +
			(",result,input_tokens,output_tokens," +
				"total_tokens\n2026-04-27T10:00:00Z,unknown,sample.json," +
				"success,1,2," +
				"3\n")
		result, err := importBytes(context.Background(), store, nil, []byte(payload))
		if err != nil || result.Added != 1 {
			t.Fatalf("header=%s result=%+v err=%v", header, result, err)
		}
		details, _, err := parseAny([]byte(payload))
		if err != nil || len(details) != 1 || details[0].Source != "sample.json" || details[0].APIKey != "" {
			t.Fatalf("details=%+v err=%v", details, err)
		}
	}
}

func TestRetentionCutoffHandlesExtremeDaysAndPreservesEvents(t *testing.T) {
	store := openTestStore(t)
	now := time.Now().UTC()
	event := FlatDetail{
		Timestamp: now.Add(
			-32 * 24 * time.Hour,
		), Model: "model", Tokens: tokenStats{InputTokens: 1, TotalTokens: 1},
	}
	payload, err := json.Marshal([]FlatDetail{event})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importBytes(context.Background(), store, nil, payload); err != nil {
		t.Fatal(err)
	}
	p := &Persister{store: store, retention: config.UsageRetention{Days: 132000}}
	if preview := p.TrimPreview(); preview.FilesCount != 0 {
		t.Fatalf("preview=%+v", preview)
	}
	if err := p.Trim(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	for _, days := range []int{1, 100000, 110000} {
		cutoff := retentionCutoff(now, days)
		expected := now.AddDate(0, 0, -days)
		if !cutoff.Equal(expected) {
			t.Fatalf("days=%d cutoff=%v expected=%v", days, cutoff, expected)
		}
	}
	if got := retentionCutoff(now, math.MaxInt).UnixNano(); got != math.MinInt64 {
		t.Fatalf("extreme cutoff=%d", got)
	}
}

func TestTrimPropagatesSQLiteFailure(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.db.Exec("DROP TABLE events"); err != nil {
		t.Fatal(err)
	}
	p := &Persister{store: store, retention: config.UsageRetention{Days: 1}}
	if err := p.Trim(); err == nil {
		t.Fatal("expected SQLite failure")
	}
}

func TestLegacyMigrationReadFailureDoesNotPublishMarker(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory read permissions require an unprivileged test user")
	}
	base := t.TempDir()
	month := filepath.Join(base, "2026-04")
	if err := os.Mkdir(month, 0000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(month, 0700) }()
	store := openTestStore(t)
	if err := migrateLegacyToSQLite(context.Background(), store, pricingSnapshot{}, base); err == nil {
		t.Fatal("unreadable month accepted")
	}
	status, err := checkMigrationStatus(context.Background(), store)
	if err != nil || status.From != "" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if err := os.Chmod(month, 0700); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyToSQLite(context.Background(), store, pricingSnapshot{}, base); err != nil {
		t.Fatal(err)
	}
	status, err = checkMigrationStatus(context.Background(), store)
	if err != nil || status.From == "" {
		t.Fatalf("retry status=%+v err=%v", status, err)
	}
}
