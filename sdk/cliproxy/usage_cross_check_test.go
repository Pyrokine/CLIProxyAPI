package cliproxy

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	internalUsage "github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

// noinspection GoTypeAssertionOnErrors
func TestUsageOfflineCrossCheckConsumesPersistedEventsAndSummaries(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	root := t.TempDir()
	cfg := &config.Config{}
	persister := internalUsage.NewPersister(root, cfg.UsageRetention)
	persister.SetPriceFunc(func(string) (float64, float64, float64, bool) { return 2, 3, 0.5, true })
	persister.Start(t.Context())
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("SQLite did not become ready")
	}
	var event internalUsage.FlatDetail
	if err := json.Unmarshal(
		[]byte(`{
		"timestamp":"2026-08-30T12:34:56.123456789Z",
		"request_id":"fixture-request", "model":"fixture-model", "provider":"fixture-provider",
		"source":"fixture.json", "auth_index":"fixture-auth", "api_key":"fixture-client-identity",
		"completed":true, "generate":true,
		"tokens":{"input_tokens":100,"output_tokens":20,"cached_tokens":40,"total_tokens":120}
	}`), &event,
	); err != nil {
		t.Fatal(err)
	}
	if !persister.Record(event) {
		t.Fatal("usage event was rejected")
	}
	if err := persister.StopContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	prices := filepath.Join(root, "prices.json")
	if err := os.WriteFile(
		prices, []byte(`{"fixture-model":{"prompt":2,"completion":3,"cache":0.5}}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "cross_check.py"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(
		t.Context(), python, "-I", script, "--db", filepath.Join(root, "events.db"), "--prices", prices,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("offline cross-check failed: %v\n%s", err, output)
	}
	for _, expected := range []string{
		"fingerprint round-trip: 1 rows", "dimension conservation:",
		"exact summary: three grains, five dimensions, 19 metrics",
		"summary projections: three grains, four scopes, 19 metrics", "1 immutable receipts", "ALL CHECKS PASSED",
	} {
		if !strings.Contains(string(output), expected) {
			t.Fatalf("missing %q in offline cross-check output:\n%s", expected, output)
		}
	}
	for _, tc := range []struct {
		name, change, restore, failure string
	}{
		{
			name:    "hour model allocation",
			change:  "UPDATE hour_bucket SET model_id=(SELECT id FROM dim_model WHERE name='fixture-other-model')",
			restore: "UPDATE hour_bucket SET model_id=(SELECT id FROM dim_model WHERE name='fixture-model')",
			failure: "hour/model/credential/api_key/provider conservation",
		},
		{
			name:    "OAuth allocation",
			change:  "UPDATE hour_bucket SET api_key_id=NULL",
			restore: "UPDATE hour_bucket SET api_key_id=(SELECT api_key_id FROM events LIMIT 1)",
			failure: "hour/model/credential/api_key/provider conservation",
		},
		{
			name:   "exact auth index allocation",
			change: "UPDATE summary_bucket SET auth_index_id=NULL WHERE grain_ns=600000000000",
			restore: "UPDATE summary_bucket SET auth_index_id=(SELECT auth_index_id FROM " +
				"events LIMIT 1) WHERE " +
				"grain_ns=600000000000",
			failure: "summary_bucket grain=600000000000",
		},
		{
			name:    "exact cache metrics",
			change:  "UPDATE summary_bucket SET cache_read_tokens=cache_read_tokens+1 WHERE grain_ns=1",
			restore: "UPDATE summary_bucket SET cache_read_tokens=cache_read_tokens-1 WHERE grain_ns=1",
			failure: "cache_read_tokens:",
		},
		{
			name: "summary query projection cost",
			change: "UPDATE summary_projection SET cost_micro=cost_micro+1 WHERE " +
				"grain_ns=3600000000000 AND " +
				"dimension='all'",
			restore: "UPDATE summary_projection SET cost_micro=cost_micro-1 WHERE " +
				"grain_ns=3600000000000 AND " +
				"dimension='all'",
			failure: "summary_projection grain=3600000000000 dimension=all",
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				db, err := sql.Open("sqlite", filepath.Join(root, "events.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(
					func() {
						if err := db.Close(); err != nil {
							t.Error(err)
						}
					},
				)
				if _, err := db.Exec(
					"INSERT INTO dim_model(name) VALUES('fixture-other-model') ON " +
						"CONFLICT(name) DO " +
						"NOTHING",
				); err !=
					nil {
					t.Fatal(err)
				}
				changed, err := db.Exec(tc.change)
				if err != nil {
					t.Fatal(err)
				}
				if rows, errRows := changed.RowsAffected(); errRows != nil || rows == 0 {
					t.Fatalf("corruption fixture did not change any rows: rows=%d err=%v", rows, errRows)
				}
				t.Cleanup(
					func() {
						if _, err := db.Exec(tc.restore); err != nil {
							t.Error(err)
						}
					},
				)
				command := exec.CommandContext(
					t.Context(),
					python,
					"-I",
					script,
					"--db",
					filepath.Join(root, "events.db"),
					"--prices",
					prices,
				)
				output, err := command.CombinedOutput()
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 1 || !strings.Contains(string(output), tc.failure) {
					t.Fatalf("invalid allocation was not rejected: err=%v\n%s", err, output)
				}
			},
		)
	}
	t.Run(
		"legacy history remains explicitly unverified", func(t *testing.T) {
			legacyPath := filepath.Join(t.TempDir(), "legacy.db")
			db, err := sql.Open("sqlite", filepath.Join(root, "events.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("VACUUM INTO ?", legacyPath); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			legacy, err := sql.Open("sqlite", legacyPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(
				func() {
					if err := legacy.Close(); err != nil {
						t.Error(err)
					}
				},
			)
			if _, err := legacy.Exec(
				"DROP TRIGGER summary_bucket_event_insert; DROP TRIGGER summary_bucket_" +
					"event_delete; DROP TRIGGER summary_bucket_event_update; DROP TABLE sum" +
					"mary_bucket; DELETE FROM meta WHERE key='summary_grains_v1'",
			); err != nil {
				t.Fatal(err)
			}
			var latestHour int64
			if err := legacy.QueryRow("SELECT MAX(bucket_ts_ns) FROM hour_bucket").Scan(&latestHour); err != nil {
				t.Fatal(err)
			}
			fields := "model_id,credential_id,api_key_id,provider_id,requests,success,failure,input_tokens," +
				"output_tokens,reasoning_tokens,cached_tokens,total_tokens,cost_micro,cache_read_tokens," +
				"cache_write_tokens,complete_requests,partial_requests,unpriced_requests,complete_tokens," +
				"partial_tokens,unpriced_tokens,latency_ms_total,latency_samples"
			if _, err := legacy.Exec(
				"INSERT INTO hour_bucket(bucket_ts_ns,"+
					fields+
					") SELECT bucket_ts_ns-3600000000000,"+
					fields+
					" FROM hour_bucket WHERE bucket_ts_ns=?",
				latestHour,
			); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(
				t.Context(),
				python,
				"-I",
				script,
				"--db",
				legacyPath,
				"--prices",
				prices,
			)
			output, err := command.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 2 || !strings.Contains(
				string(output), "CHECKS INCOMPLETE",
			) || !strings.Contains(
				string(output), "history without complete retained details",
			) || !strings.Contains(string(output), "summary_bucket is absent") || strings.Contains(
				string(output), "FAIL:",
			) {
				t.Fatalf("legacy precision was misreported: err=%v\n%s", err, output)
			}
		},
	)
}
