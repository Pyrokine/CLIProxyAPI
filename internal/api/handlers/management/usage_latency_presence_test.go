package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
)

func TestUsageSummaryMeasuredZeroLatencyUsesSampleCountAcrossDimensions(t *testing.T) {
	for _, name := range []string{"mixed", "zero", "unknown"} {
		t.Run(
			name, func(t *testing.T) {
				persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
				persister.Start(context.Background())
				defer persister.Stop()
				if !persister.IsReady() {
					t.Fatal("synthetic SQLite store unavailable")
				}
				h := &Handler{usagePersister: persister}
				latencies := []string{`,"latency_ms":0`, `,"latency_ms":10`, ""}
				if name == "zero" {
					latencies = []string{`,"latency_ms":0`, ""}
				} else if name == "unknown" {
					latencies = []string{"", `,"latency_ms":null`}
				}
				var events []string
				for i, latency := range latencies {
					events = append(
						events, fmt.Sprintf(
							`{"timestamp":"2026-11-06T09:0%d:00Z","model":"measured-latency","api_k`+
								`ey":"synthetic-client","tokens":{"input_tokens":1,"total_tokens":1}%s}`,
							i, latency,
						),
					)
				}
				c, response := newArtifactCacheTestContext(
					http.MethodPost, "/usage/import", "["+strings.Join(events, ",")+"]",
				)
				h.ImportUsageStatistics(c)
				if response.Code != 200 {
					t.Fatalf("import status=%d body=%s", response.Code, response.Body.String())
				}
				c, response = newArtifactCacheTestContext(
					http.MethodGet,
					"/usage/summary?from=2026-11-06T08:00:00Z&to=2026-11-06T10:00:00Z&include_groups=true", "",
				)
				h.GetUsageSummary(c)
				if response.Code != 200 {
					t.Fatalf("summary status=%d body=%s", response.Code, response.Body.String())
				}
				var body struct {
					Totals   map[string]any            `json:"totals"`
					ByModel  map[string]map[string]any `json:"by_model"`
					ByAPIKey map[string]map[string]any `json:"by_api_key"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if len(body.ByModel) != 1 || len(body.ByAPIKey) != 1 {
					t.Fatalf("summary dimensions missing: %s", response.Body.String())
				}
				rows := []map[string]any{body.Totals}
				for _, row := range body.ByModel {
					rows = append(rows, row)
				}
				for _, row := range body.ByAPIKey {
					rows = append(rows, row)
				}
				for _, row := range rows {
					if name == "unknown" {
						for _, key := range []string{"average_latency_ms", "total_latency_ms", "latency_sample_count"} {
							if _, exists := row[key]; exists {
								t.Fatalf("unknown latency became measured: %v", row)
							}
						}
						continue
					}
					average, total, count := float64(5), float64(10), float64(2)
					if name == "zero" {
						average, total, count = 0, 0, 1
					}
					if row["average_latency_ms"] != average || row["total_latency_ms"] != total ||
						row["latency_sample_count"] != count {
						t.Fatalf("latency summary=%v want average=%v total=%v count=%v", row, average, total, count)
					}
				}
			},
		)
	}
}
