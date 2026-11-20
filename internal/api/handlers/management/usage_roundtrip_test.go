package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

const roundtripManagementKey = "synthetic-usage-management-key"

func roundtripRouter(p *usage.Persister) *gin.Engine {
	cfg := &config.Config{}
	cfg.APIKeys = []string{"synthetic-client-key"}
	h := NewHandler(cfg, "", nil, withEnvSecret(roundtripManagementKey), withUsagePersister(p))
	r := gin.New()
	r.Use(h.MiddlewareWithConfig(nil))
	r.GET("/usage/events", h.GetUsageEvents)
	r.GET("/usage/export", h.ExportUsageStatistics)
	r.GET("/usage/db-size", h.GetUsageDBSize)
	r.GET("/usage/summary", h.GetUsageSummary)
	r.POST("/usage/import", h.ImportUsageStatistics)
	return r
}

func roundtripRequest(r http.Handler, method, path string, body []byte, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestUsageDisplayExportRoundTrip(t *testing.T) {
	frontend := os.Getenv("CPAM_TEST_ROOT")
	if frontend == "" {
		t.Skip("set CPAM_TEST_ROOT to exercise the real frontend exporter")
	}
	bun := os.Getenv("BUN_TEST_BINARY")
	if bun == "" {
		bun = "bun"
	}
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	dir := t.TempDir()
	source := usage.NewPersister(dir, config.UsageRetention{Days: -1})
	source.Start(ctx)
	for i, payload := range []string{
		`{"timestamp":"2026-11-09T01:02:03.123456789Z","model":"fixture","api_key":"synthetic-client-key","` +
			`auth_index":"auth-a","provider":"codex","request_id":"request-a","client_model":"client-alias",` +
			`"upstream_url":"https://upstream.example.test/v1/responses","upstream_model":"forwarded-model",` +
			`"response_model":"returned-model","response_provider":"reported-channel","response_provider_source":"body.provider",` +
			`"request_service_tier":"priority","response_service_tier":"default","tokens":{"input_tokens":` +
			`9007199254740993,"output_tokens":2,"total_tokens":9007199254740995},"failed":false}`,
		`{"timestamp":"2026-11-09T02:02:03Z","model":"fixture","api_key":"synth` +
			`etic-client-key","auth_index":"auth-b","provider":"claude","request_id` +
			`":"request-b","tokens":{"input_tokens":4,"output_tokens":2,"total_toke` +
			`ns":6},"request_speed":"fast","response_speed":"standard","failed":true}`,
		`{"timestamp":"2026-11-09T03:02:03Z","model":"other","api_key":"","auth` +
			`_index":"","provider":"gemini","tokens":{"input_tokens":3,"output_toke` +
			`ns":0,"total_tokens":3},"failed":false}`,
	} {
		var detail usage.FlatDetail
		if err := json.Unmarshal([]byte(payload), &detail); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			detail.Source = usage.TagIdentityCredentialSource("account@example.test")
		}
		if i == 1 {
			detail.Source = usage.TagAPIKeyCredentialSource("synthetic-provider-secret", false)
		}
		if !source.Record(detail) {
			t.Fatal("record refused")
		}
	}
	source.Stop()
	source = usage.NewPersister(dir, config.UsageRetention{Days: -1})
	source.Start(ctx)
	defer source.Stop()
	router := roundtripRouter(source)
	if denied := roundtripRequest(
		router, "GET", "/usage/events?export=true", nil, "",
	); denied.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated export = %d", denied.Code)
	}
	const query = "/usage/events?from=2026-11-09T00:00:00Z&to=2026-11-10T00:00:" +
		"00Z&page_size=500&sort=timestamp&order=asc"
	display := roundtripRequest(router, "GET", query, nil, roundtripManagementKey)
	if display.Code != 200 || strings.Contains(
		display.Body.String(), "import_data",
	) || strings.Contains(display.Body.String(), "raw_api_key") {
		t.Fatalf("display boundary: %d %s", display.Code, display.Body.String())
	}
	expected, err := source.QueryBackupEventsContext(ctx, time.Time{}, time.Time{}, usage.EventFilters{})
	if err != nil || len(expected) != 3 {
		t.Fatalf("source events: %v count=%d", err, len(expected))
	}
	exports := func(filter string) map[string]string {
		t.Helper()
		response := roundtripRequest(router, "GET", query+"&export=true"+filter, nil, roundtripManagementKey)
		if response.Code != 200 {
			t.Fatalf("events status %d: %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "synthetic-client-key") || strings.Contains(
			response.Body.String(), "synthetic-provider-secret",
		) {
			t.Fatal("export leaked raw secret")
		}
		command := exec.CommandContext(
			t.Context(), bun, "run", filepath.Join(frontend, "tests/remediationUsageRoundtrip.test.ts"),
			"--export-fixture",
		)
		command.Dir = frontend
		command.Stdin = bytes.NewReader(append(append([]byte("["), response.Body.Bytes()...), ']'))
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			t.Fatalf("frontend export: %v %s", err, stderr.String())
		}
		var result map[string]string
		if err = json.Unmarshal(output, &result); err != nil {
			t.Fatalf("decode frontend export: %v %s", err, output)
		}
		return result
	}
	all := exports("")
	subset := exports("&model=fixture")
	// A valid management session with a wrong portable key must not import display identities.
	request := httptest.NewRequest("POST", "/usage/import", strings.NewReader(all["json"]))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Authorization", "Bearer "+roundtripManagementKey)
	request.Header.Set(usageBackupKeyHeader, "wrong-synthetic-backup-key")
	rejected := httptest.NewRecorder()
	router.ServeHTTP(rejected, request)
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("wrong export key status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	if badView := roundtripRequest(
		router, "GET", query+"&view=compact&export=true", nil, roundtripManagementKey,
	); badView.Code != http.StatusBadRequest {
		t.Fatalf("compact export status=%d", badView.Code)
	}
	for _, format := range []string{"json", "csv"} {
		t.Run(
			format, func(t *testing.T) {
				target := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
				target.Start(ctx)
				defer target.Stop()
				targetRouter := roundtripRouter(target)
				importPayload := func(r *gin.Engine, payload string, added, skipped int64) {
					t.Helper()
					result := roundtripRequest(
						r,
						"POST",
						"/usage/import",
						[]byte(payload),
						roundtripManagementKey,
					)
					var counts struct{ Added, Skipped int64 }
					if err := json.Unmarshal(
						result.Body.Bytes(), &counts,
					); err != nil || result.Code != 200 || counts.Added != added || counts.Skipped != skipped {
						t.Fatalf(
							"import: status=%d counts=%+v want=%d/%d body=%s",
							result.Code,
							counts,
							added,
							skipped,
							result.Body.String(),
						)
					}
				}
				importPayload(router, all[format], 0, 3)
				importPayload(targetRouter, subset[format], 2, 0)
				importPayload(targetRouter, all[format], 1, 2)
				importPayload(targetRouter, all[format], 0, 3)
				actual, err := target.QueryBackupEventsContext(
					ctx,
					time.Time{},
					time.Time{},
					usage.EventFilters{},
				)
				if err != nil || !reflect.DeepEqual(actual, expected) {
					t.Fatalf("canonical events changed: %v\nwant=%#v\ngot=%#v", err, expected, actual)
				}
				summaryQuery := strings.Replace(query, "/usage/events", "/usage/summary", 1) + "&model=fixture"
				originalSummary := roundtripRequest(router, "GET", summaryQuery, nil, roundtripManagementKey)
				restoredSummary := roundtripRequest(
					targetRouter,
					"GET",
					summaryQuery,
					nil,
					roundtripManagementKey,
				)
				if !bytes.Equal(originalSummary.Body.Bytes(), restoredSummary.Body.Bytes()) {
					t.Fatalf(
						"filtered dimensions changed\n%s\n%s", originalSummary.Body.String(),
						restoredSummary.Body.String(),
					)
				}
			},
		)
	}
}

func TestUsageDBSizeStatFailureAndRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dir, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	router := roundtripRouter(usage.NewPersister(dir, config.UsageRetention{Days: -1}))
	failed := roundtripRequest(router, "GET", "/usage/db-size", nil, roundtripManagementKey)
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("stat failure status=%d body=%s", failed.Code, failed.Body.String())
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.db"), []byte("1234567"), 0600); err != nil {
		t.Fatal(err)
	}
	recovered := roundtripRequest(router, "GET", "/usage/db-size", nil, roundtripManagementKey)
	if recovered.Code != 200 || !strings.Contains(recovered.Body.String(), `"size_bytes":7`) {
		t.Fatalf("recovery: %d %s", recovered.Code, recovered.Body.String())
	}
	if frontend := os.Getenv("CPAM_TEST_ROOT"); frontend != "" {
		bun := os.Getenv("BUN_TEST_BINARY")
		if bun == "" {
			bun = "bun"
		}
		command := exec.CommandContext(
			t.Context(), bun, "run", filepath.Join(frontend, "tests/remediationUsageRoundtrip.test.ts"),
			"--size-fixture",
		)
		command.Dir = frontend
		command.Stdin = strings.NewReader(
			`[{"status":` + fmt.Sprint(
				failed.Code,
			) + `,"body":` + failed.Body.String() + `},{"status":` + fmt.Sprint(
				recovered.Code,
			) + `,"body":` + recovered.Body.String() + `}]`,
		)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("frontend size consumers: %v %s", err, output)
		}
		var states []struct {
			LoadError *string
			Size      *int64
			Runtime   struct {
				UsageDBSize struct {
					Status    string
					SizeBytes int64
				}
			}
		}
		if err := json.Unmarshal(output, &states); err != nil || len(states) != 2 {
			t.Fatalf("frontend size response: %v %s", err, output)
		}
		if states[0].LoadError == nil || states[0].Size != nil ||
			states[0].Runtime.UsageDBSize.Status != "error" ||
			states[1].LoadError != nil ||
			states[1].Size == nil ||
			*states[1].Size != 7 ||
			states[1].Runtime.UsageDBSize.Status != "ready" {
			t.Fatalf("frontend size states: %s", output)
		}
	}

}
