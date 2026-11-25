package management

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
)

func TestUsageEventExactIDAndActualSummaryGranularity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := time.Date(2026, 10, 15, 15, 14, 0, 0, time.UTC)
	const targetID = "Req-target"
	for _, useSQLite := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("sqlite=%v", useSQLite), func(t *testing.T) {
				h := &Handler{cfg: &config.Config{}, usageStats: usage.NewRequestStatistics()}
				const newer = 501
				if useSQLite {
					p := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
					p.Start(t.Context())
					t.Cleanup(p.Stop)
					h.usagePersister = p
					details := make([]usage.FlatDetail, newer+1)
					if err := json.Unmarshal(
						[]byte(`{"timestamp":"2026-10-15T15:14:00Z","model":"fixture","request_id":"Req-target","latency_ms":0,"failed":false}`),
						&details[0],
					); err != nil {
						t.Fatal(err)
					}
					for i := 1; i < len(details); i++ {
						details[i] = usage.FlatDetail{
							Timestamp: base.Add(time.Duration(i) * time.Millisecond),
							Model:     "fixture", RequestID: "req-target", Failed: true,
						}
					}
					payload, err := json.Marshal(details)
					if err != nil {
						t.Fatal(err)
					}
					if result, err := p.ImportContext(t.Context(), payload); err != nil || result.Added != newer+1 {
						t.Fatalf("seed fixture: %+v err=%v", result, err)
					}
				} else {
					collector := usage.NewLoggerPlugin(nil, h.usageStats, func() bool { return true })
					for i := range newer + 1 {
						id := "req-target"
						if i == 0 {
							id = targetID
						}
						collector.HandleUsage(
							t.Context(), coreusage.Record{
								RequestedAt: base.Add(time.Duration(i) * time.Millisecond),
								Model:       "fixture", RequestID: id, Failed: i != 0,
								Latency: 500 * time.Microsecond,
							},
						)
					}
				}
				shortFrom := base.Add(-time.Minute)
				query := url.Values{
					"from": {shortFrom.Format(time.RFC3339)}, "to": {base.Add(time.Minute).Format(time.RFC3339)},
					"page_size": {"500"}, "view": {"compact"},
				}
				readPage := func() compactEventsResponse {
					t.Helper()
					response := runConfigListRequest(
						h.GetUsageEvents, http.MethodGet, "/usage/events?"+query.Encode(), "",
					)
					var parsed compactEventsResponse
					if err := json.Unmarshal(
						response.Body.Bytes(), &parsed,
					); err != nil || response.Code != http.StatusOK {
						t.Fatalf("usage page: %d %s, err=%v", response.Code, response.Body.String(), err)
					}
					return parsed
				}
				page := readPage()
				if page.Total != newer+1 || len(page.Events) != 500 || page.AccessiblePages != 2 {
					t.Fatalf(
						"first page counts total=%d, events=%d, accessible=%d", page.Total, len(page.Events),
						page.AccessiblePages,
					)
				}
				for _, event := range page.Events {
					if event.RequestID == targetID || (useSQLite && event.LatencyMs != nil) {
						t.Fatalf("first page contains exact target or unknown latency became measured: %+v", event)
					}
				}
				query.Set("request_id", targetID)
				for _, view := range []string{"compact", ""} {
					query.Set("view", view)
					exact := readPage()
					if exact.Total != 1 || len(exact.Events) != 1 || exact.RequestIDFilter != targetID || exact.Events[0].RequestID != targetID {
						t.Fatalf("exact request lookup: %+v", exact)
					}
					if exact.Events[0].LatencyMs == nil || *exact.Events[0].LatencyMs != 0 {
						t.Fatal("measured zero latency not returned")
					}
				}
				for _, tc := range []struct {
					span time.Duration
					want string
				}{{time.Hour, "hourly"}, {113 * 24 * time.Hour, "daily"}} {
					summaryQuery := url.Values{
						"from": {shortFrom.Format(time.RFC3339)}, "to": {base.Add(tc.span).Format(time.RFC3339)},
						"granularity": {"hourly"}, "groups": {"none"},
					}
					response := runConfigListRequest(
						h.GetUsageSummary, http.MethodGet, "/usage/summary?"+summaryQuery.Encode(), "",
					)
					var summary summaryResponse
					if err := json.Unmarshal(
						response.Body.Bytes(), &summary,
					); err != nil || response.Code != http.StatusOK || summary.Period.Granularity != tc.want || summary.Totals.Requests != newer+1 {
						t.Fatalf(
							"summary grain for %s: status=%d period=%+v total=%d err=%v", tc.span, response.Code,
							summary.Period, summary.Totals.Requests, err,
						)
					}
					grain := time.Hour
					if tc.want == "daily" {
						grain = 24 * time.Hour
					}
					first := summary.TimeSeries[0]
					if first.Time != base.Truncate(grain).Format(time.RFC3339) || first.Requests != newer+1 {
						t.Fatalf("actual bucket: %+v, granularity=%s", first, tc.want)
					}
				}
			},
		)
	}
}

func TestUsageEventsReportReachablePagesSeparately(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	p := usage.NewPersister(dir, config.UsageRetention{Days: -1})
	p.Start(t.Context())
	t.Cleanup(p.Stop)
	base := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	db, err := sql.Open("sqlite", filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO dim_model(id,name) VALUES(1,'fixture')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(
		t.Context(), `WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<120000)
		INSERT INTO events(fingerprint,ts_ns,model_id,failed) SELECT CAST(printf('%020d',n) AS BLOB),?+n,1,0 FROM seq`,
		base.UnixNano(),
	); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{}, usagePersister: p}
	const query = "/usage/events?view=compact&from=2026-10-15T00:00:00Z&to=2026-10-16T00:00:00Z&page_size=50&page="
	response := runConfigListRequest(h.GetUsageEvents, http.MethodGet, query+"2001", "")
	var page compactEventsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || response.Code != http.StatusOK ||
		page.Total != 120000 || page.TotalPages != 2400 || page.AccessiblePages != 2001 || page.Page != 2001 || len(page.Events) != 50 {
		t.Fatalf(
			"last accessible page: status=%d pagination=%+v len(events)=%d err=%v", response.Code,
			page.eventsPagination, len(page.Events), err,
		)
	}
	if next := runConfigListRequest(
		h.GetUsageEvents, http.MethodGet, query+"2002", "",
	); next.Code != http.StatusBadRequest {
		t.Fatalf("next page: %d %s", next.Code, next.Body.String())
	}
}
