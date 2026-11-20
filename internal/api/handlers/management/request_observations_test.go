package management

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestRequestObservationsLoggerToCompactAPI(t *testing.T) {
	dir := t.TempDir()
	persister := usage.NewPersister(dir, config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("synthetic SQLite did not start")
	}
	logger := usage.NewLoggerPlugin(persister, nil, func() bool { return true })
	base := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	for index, record := range []coreusage.Record{
		{
			RequestID: "tier-downgrade", Provider: "codex", RequestServiceTier: "priority",
			ResponseServiceTier: "default", UpstreamModel: "forwarded-model", ResponseModel: "returned-model",
			ResponseProvider: "reported-channel", ResponseProviderSource: "body.response.provider",
		},
		{
			RequestID: "claude-fast", Provider: "claude", ExecutorType: "ClaudeExecutor", RequestServiceTier: "default",
			ResponseServiceTier: "standard", RequestSpeed: "fast", ResponseSpeed: "fast",
			UpstreamModel: "forwarded-model", ResponseModel: "returned-model", ResponseProvider: "reported-channel",
			ResponseProviderSource: "header.X-Upstream-Provider",
		},
		{RequestID: "legacy-unknown", Provider: "codex", RequestServiceTier: "priority"},
		{
			RequestID: "failed-attempt", Provider: "codex", RequestServiceTier: "priority",
			UpstreamModel: "forwarded-model", Failed: true,
		},
		{RequestID: "cache-read-missing", Provider: "codex", CacheWriteReported: new(false)},
		{RequestID: "cache-read-zero", Provider: "codex", CacheReadReported: true, CacheWriteReported: new(true)},
		{
			RequestID: "cache-read-hit", Provider: "codex", CacheReadReported: true, CacheWriteReported: new(false),
			Detail: coreusage.Detail{CacheReadTokens: 3, CacheCreationTokens: 1},
		},
	} {
		record.Model = "routed-model"
		record.ClientModel = "client-alias"
		if record.RequestID != "legacy-unknown" {
			record.UpstreamURL = "https://name:synthetic-url-secret@upstream.example.test/v1/responses?api_key=synthetic-query-secret&region=test"
			record.UpstreamNextHopStatus = "not_reported"
			record.ResponseURLStatus = "no_response"
			if !record.Failed {
				record.ResponseURL = "https://name:synthetic-url-secret@final.example.test/result?api_key=synthetic-query-secret&region=test"
				record.ResponseURLStatus, record.ResponseURLSource = "observed", "http_response_request_url"
			}
		}
		record.RequestedAt = base.Add(time.Duration(index) * time.Second)
		record.APIKey = "synthetic-client-key"
		record.Detail = coreusage.Detail{
			InputTokens: 5, OutputTokens: 1, TotalTokens: 6, CacheReadReported: record.CacheReadReported,
			CacheReadTokens: record.Detail.CacheReadTokens, CacheWriteReported: record.CacheWriteReported,
			CacheCreationTokens: record.Detail.CacheCreationTokens,
		}
		if record.RequestID == "tier-downgrade" {
			record.BillingMetadata = &coreusage.BillingMetadata{
				SchemaVersion: 1, ContextInputTokens: new(int64(5)), Region: "us", RegionSource: "usage.inference_geo",
				ChargesReported: true,
			}
		}
		record.Completed = !record.Failed
		logger.HandleUsage(context.Background(), record)
	}
	persister.Stop()
	persister = usage.NewPersister(dir, config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("synthetic SQLite did not reopen")
	}
	router := roundtripRouter(persister)
	query := "/usage/events?from=2026-11-18T00:00:00Z&to=2026-11-19T00:00:00Z&page_size=50&sort=timestamp&order=asc"
	compact := roundtripRequest(router, http.MethodGet, query+"&view=compact", nil, roundtripManagementKey)
	full := roundtripRequest(router, http.MethodGet, query, nil, roundtripManagementKey)
	if compact.Code != 200 || full.Code != 200 {
		t.Fatalf(
			"usage API: compact=%d %s full=%d %s", compact.Code, compact.Body.String(), full.Code, full.Body.String(),
		)
	}
	var result, fullResult struct {
		Events []map[string]any `json:"events"`
		Total  int              `json:"total"`
	}
	if err := json.Unmarshal(compact.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(full.Body.Bytes(), &fullResult); err != nil {
		t.Fatal(err)
	}
	if result.Total != 7 || len(result.Events) != 7 || len(fullResult.Events) != 7 {
		t.Fatalf("event counts: %+v", result)
	}
	for index, event := range result.Events {
		for _, key := range []string{
			"model", "provider", "client_model", "upstream_url", "upstream_model", "response_model",
			"response_provider", "response_provider_source", "response_url", "response_url_source",
			"response_url_status", "upstream_next_hop_status", "cache_read_reported", "cache_write_reported",
			"billing_metadata", "service_tier", "request_service_tier", "response_service_tier", "request_speed",
			"response_speed", "executor_type", "cost", "cost_status", "pricing_receipt",
		} {
			if !reflect.DeepEqual(event[key], fullResult.Events[index][key]) {
				t.Fatalf(
					"compact lost %s at %d: compact=%v full=%v", key, index, event[key], fullResult.Events[index][key],
				)
			}
		}
		if event["model"] != "routed-model" || event["client_model"] != "client-alias" {
			t.Fatalf("model identity changed: %+v", event)
		}
	}
	if result.Events[0]["request_service_tier"] != "priority" || result.Events[0]["response_service_tier"] != "default" || result.Events[1]["request_speed"] != "fast" || result.Events[1]["response_speed"] != "fast" {
		t.Fatalf("tier/speed results: %+v", result.Events)
	}
	for _, key := range []string{
		"response_service_tier", "response_speed", "upstream_url", "upstream_model", "response_model",
		"response_provider", "response_url", "response_url_source", "response_url_status", "upstream_next_hop_status",
		"cache_read_reported",
	} {
		if _, exists := result.Events[2][key]; exists {
			t.Fatalf("legacy event acquired %s", key)
		}
	}
	if result.Events[3]["failed"] != true || result.Events[3]["completed"] != false || result.Events[3]["upstream_model"] != "forwarded-model" {
		t.Fatalf("failure lost its outbound destination: %+v", result.Events[3])
	}
	billing, ok := result.Events[0]["billing_metadata"].(map[string]any)
	if !ok || billing["context_input_tokens"] != float64(5) || billing["region"] != "us" || billing["charges_reported"] != true {
		t.Fatalf("compact billing metadata lost: %+v", billing)
	}
	if result.Events[4]["cache_read_reported"] != nil || result.Events[5]["cache_read_reported"] != true || result.Events[6]["cache_read_reported"] != true {
		t.Fatalf("cache read missing/zero/positive collapsed: %+v", result.Events[4:])
	}
	if result.Events[2]["cache_write_reported"] != nil || result.Events[4]["cache_write_reported"] != false || result.Events[5]["cache_write_reported"] != true || result.Events[6]["cache_write_reported"] != false {
		t.Fatalf("cache write missing/unknown/zero/partial collapsed: %+v", result.Events)
	}
	zeroTokens := result.Events[5]["tokens"].(map[string]any)
	hitTokens := result.Events[6]["tokens"].(map[string]any)
	if zeroTokens["cache_read_tokens"] != float64(0) || hitTokens["cache_read_tokens"] != float64(3) {
		t.Fatalf("cache quantities changed: zero=%v hit=%v", zeroTokens, hitTokens)
	}
	for _, secret := range []string{"synthetic-url-secret", "synthetic-query-secret", "name:"} {
		if strings.Contains(compact.Body.String(), secret) || strings.Contains(full.Body.String(), secret) {
			t.Fatalf("API disclosed URL credential %q", secret)
		}
	}
	if output := os.Getenv("CPA_DETAILS_FIXTURE"); output != "" {
		if err := os.WriteFile(output, compact.Body.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
