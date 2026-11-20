package management

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
)

func TestMemoryUsageLoggerFeedsSummaryAndCompactEvents(t *testing.T) {
	stats := usage.NewRequestStatistics()
	logger := usage.NewLoggerPlugin(nil, stats, func() bool { return true })
	generate := false
	logger.HandleUsage(
		context.Background(), coreusage.Record{
			RequestedAt: time.Date(2026, 11, 1, 10, 15, 0, 0, time.UTC),
			Model:       "memory-model", Provider: "codex", Source: "identity@example.test", AuthIndex: "auth-memory",
			APIKey:    "synthetic-client-secret",
			RequestID: "memory-request", Latency: 250 * time.Millisecond, Generate: &generate,
			Detail: coreusage.Detail{InputTokens: 5, OutputTokens: 9, TotalTokens: 14}, Completed: true,
		},
	)
	h := &Handler{cfg: &config.Config{}, usageStats: stats}
	values := url.Values{
		"from": {"2026-11-01T10:10:00Z"}, "to": {"2026-11-01T10:20:00Z"}, "auth_index_groups": {"true"},
		"auth_index_granularity": {"10m"},
	}
	for _, view := range []string{"legacy", "summary", "compact", "full"} {
		t.Run(
			view, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				query := url.Values{}
				maps.Copy(query, values)
				path := "/usage"
				switch view {
				case "summary":
					path = "/usage/summary"
				case "compact", "full":
					path = "/usage/events"
					if view == "compact" {
						query.Set("view", "compact")
					}
				}
				c.Request = httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil)
				switch view {
				case "legacy":
					h.GetUsageStatistics(c)
				case "summary":
					h.GetUsageSummary(c)
				default:
					h.GetUsageEvents(c)
				}
				if recorder.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				if strings.Contains(recorder.Body.String(), "synthetic-client-secret") {
					t.Fatalf("client secret exposed: %s", recorder.Body.String())
				}
				var payload map[string]json.RawMessage
				if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				switch view {
				case "legacy":
					var legacy struct{ TotalRequests, TotalTokens int64 }
					var body struct {
						TotalRequests int64 `json:"total_requests"`
						TotalTokens   int64 `json:"total_tokens"`
					}
					if err := json.Unmarshal(payload["usage"], &body); err != nil {
						t.Fatal(err)
					}
					legacy.TotalRequests, legacy.TotalTokens = body.TotalRequests, body.TotalTokens
					if legacy.TotalRequests != 1 || legacy.TotalTokens != 14 {
						t.Fatalf("legacy=%s", recorder.Body.String())
					}
				case "summary":
					var summary summaryResponse
					if err := json.Unmarshal(recorder.Body.Bytes(), &summary); err != nil {
						t.Fatal(err)
					}
					if summary.Totals.Requests != 1 || summary.Totals.Tokens.Total != 14 ||
						summary.Totals.CostCoverage.Unpriced.Requests != 1 ||
						summary.ByProvider["codex"].Requests != 1 ||
						summary.ByAuthIndex["auth-memory"].Tokens.Total != 14 {
						t.Fatalf("summary=%s", recorder.Body.String())
					}
				default:
					var result struct {
						Total  int `json:"total"`
						Events []struct {
							Provider  string           `json:"provider"`
							RequestID string           `json:"request_id"`
							Tokens    map[string]int64 `json:"tokens"`
						} `json:"events"`
					}
					if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					key := "total_tokens"
					if result.Total != 1 || len(result.Events) != 1 || result.Events[0].Tokens[key] != 14 ||
						result.Events[0].Provider != "codex" ||
						result.Events[0].RequestID != "memory-request" {
						t.Fatalf("events=%s", recorder.Body.String())
					}
				}
			},
		)
	}
	values.Set("model", "missing")
	recorder := runConfigListRequest(h.GetUsageSummary, http.MethodGet, "/usage/summary?"+values.Encode(), "")
	var empty summaryResponse
	if err := json.Unmarshal(
		recorder.Body.Bytes(), &empty,
	); err != nil || recorder.Code != http.StatusOK || empty.Totals.Requests != 0 {
		t.Fatalf("zero filter status=%d body=%s error=%v", recorder.Code, recorder.Body.String(), err)
	}
}
