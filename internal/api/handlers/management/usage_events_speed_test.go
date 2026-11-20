package management

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
)

func assertUsageSpeedFilter(t *testing.T, payload []byte, expected string) {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	value, present := envelope["speed_filter"]
	if expected == "" {
		if present {
			t.Fatalf("inactive speed filter returned %s", value)
		}
		return
	}
	if !present || string(value) != strconv.Quote(expected) {
		t.Fatalf("speed_filter = %s, want %q", value, expected)
	}
}

func TestGetUsageEventsSpeedFilterAndExport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	seeds := []struct {
		detail usage.FlatDetail
		speed  string
	}{
		{usage.FlatDetail{ResponseServiceTier: "default"}, "standard"},
		{usage.FlatDetail{ResponseServiceTier: "priority"}, "fast"},
		{usage.FlatDetail{ResponseServiceTier: "ULTRAFAST"}, "ultrafast"},
		{usage.FlatDetail{ResponseServiceTier: "flex"}, "flex"},
		{usage.FlatDetail{ResponseServiceTier: "batch"}, "batch"},
		{usage.FlatDetail{ServiceTier: "priority"}, "unknown"},
		{usage.FlatDetail{Provider: "claude", ResponseServiceTier: "priority"}, "unknown"},
		{usage.FlatDetail{ExecutorType: "ClaudeExecutor", ResponseServiceTier: "priority"}, "unknown"},
		{usage.FlatDetail{RequestSpeed: "fast", ResponseServiceTier: "priority"}, "unknown"},
		{usage.FlatDetail{RequestServiceTier: "priority", ResponseServiceTier: "default"}, "standard"},
		{usage.FlatDetail{Provider: "claude", ResponseSpeed: " \tFAST\n", Failed: true}, "fast"},
		{usage.FlatDetail{ResponseServiceTier: "priority", Failed: true}, "fast"},
		{usage.FlatDetail{Provider: "claude", ResponseSpeed: "standard", ResponseServiceTier: "priority"}, "standard"},
		{usage.FlatDetail{RequestServiceTier: "priority", Failed: true}, "unknown"},
	}
	for i := range seeds {
		detail := &seeds[i].detail
		detail.Timestamp = base.Add(time.Duration(i) * time.Second)
		detail.RequestID = fmt.Sprintf("speed-event-%02d", i)
		detail.Model = "speed-model"
		detail.Source = "synthetic-speed-credential"
		detail.APIKey = "synthetic-speed-client"
		detail.Tokens.InputTokens = 1
		detail.Tokens.TotalTokens = 1
	}

	for _, backend := range []string{"sqlite", "memory"} {
		t.Run(
			backend, func(t *testing.T) {
				h := &Handler{cfg: &config.Config{APIKeys: []string{"synthetic-speed-client"}}}
				if backend == "sqlite" {
					persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
					persister.Start(t.Context())
					t.Cleanup(persister.Stop)
					if !persister.IsReady() {
						t.Fatal("temporary SQLite unavailable")
					}
					details := make([]usage.FlatDetail, len(seeds))
					for i := range seeds {
						details[i] = seeds[i].detail
					}
					payload, err := json.Marshal(details)
					if err != nil {
						t.Fatal(err)
					}
					if result, err := persister.ImportContext(
						t.Context(), payload,
					); err != nil || result.Added != int64(len(seeds)) {
						t.Fatalf("seed import = %+v, %v", result, err)
					}
					h.usagePersister = persister
				} else {
					stats := usage.NewRequestStatistics()
					logger := usage.NewLoggerPlugin(nil, stats, func() bool { return true })
					for _, seed := range seeds {
						detail := seed.detail
						logger.HandleUsage(
							t.Context(), coreusage.Record{
								RequestedAt: detail.Timestamp, RequestID: detail.RequestID, Model: detail.Model,
								Source: detail.Source, APIKey: detail.APIKey, Provider: detail.Provider,
								ExecutorType: detail.ExecutorType,
								ServiceTier:  detail.ServiceTier, RequestServiceTier: detail.RequestServiceTier,
								ResponseServiceTier: detail.ResponseServiceTier,
								RequestSpeed:        detail.RequestSpeed, ResponseSpeed: detail.ResponseSpeed,
								Failed: detail.Failed, Completed: detail.Completed,
								Detail: coreusage.Detail{InputTokens: 1, TotalTokens: 1},
							},
						)
					}
					h.usageStats = stats
				}
				baseQuery := url.Values{
					"from": {base.Add(-time.Second).Format(time.RFC3339Nano)},
					"to":   {base.Add(time.Hour).Format(time.RFC3339Nano)},
					"sort": {"timestamp"}, "order": {"asc"}, "page_size": {"2"},
				}
				request := func(path string, query url.Values, export bool) *httptest.ResponseRecorder {
					t.Helper()
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil)
					if export {
						c.Request.Header.Set(usageBackupKeyHeader, "synthetic-speed-backup-key")
					}
					if path == "/usage/export" {
						h.ExportUsageStatistics(c)
					} else {
						h.GetUsageEvents(c)
					}
					return recorder
				}

				queries := []struct {
					name    string
					filters url.Values
					want    []int
				}{
					{"standard", url.Values{"speed": {"standard"}}, []int{0, 9, 12}},
					{"fast", url.Values{"speed": {" \tFaSt\n"}}, []int{1, 10, 11}},
					{"ultrafast", url.Values{"speed": {"ultrafast"}}, []int{2}},
					{"flex", url.Values{"speed": {"flex"}}, []int{3}},
					{"batch", url.Values{"speed": {"batch"}}, []int{4}},
					{"unknown", url.Values{"speed": {"UNKNOWN"}}, []int{5, 6, 7, 8, 13}},
					{"fast_failure", url.Values{"speed": {"fast"}, "status": {"failure"}}, []int{10, 11}},
					{"fast_success", url.Values{"speed": {"fast"}, "status": {"success"}}, []int{1}},
					{
						"fast_intersection", url.Values{
							"speed": {"fast"}, "model": {"speed-model"}, "source": {"synthetic-speed-credential"},
							"api_key": {"synthetic-speed-client"},
						}, []int{1, 10, 11},
					},
					{"raw_requested_speed", url.Values{"speed": {"unknown"}, "search": {"fast"}}, []int{8}},
					{"raw_requested_tier", url.Values{"speed": {"standard"}, "search": {"priority"}}, []int{9, 12}},
					{"normalized_label", url.Values{"search": {"standard"}}, []int{0, 9, 12}},
					{"empty", url.Values{"speed": {""}}, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}},
					{"no_match", url.Values{"speed": {"fast"}, "model": {"absent"}}, nil},
				}
				for _, view := range []string{"", "compact"} {
					for _, query := range queries {
						t.Run(
							view+"/"+query.name, func(t *testing.T) {
								values := maps.Clone(baseQuery)
								maps.Copy(values, query.filters)
								values.Set("view", view)
								for page := 1; page <= (len(query.want)+1)/2+1; page++ {
									values.Set("page", strconv.Itoa(page))
									recorder := request("/usage/events", values, false)
									if recorder.Code != http.StatusOK {
										t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
									}
									assertUsageSpeedFilter(
										t, recorder.Body.Bytes(),
										strings.ToLower(strings.TrimSpace(query.filters.Get("speed"))),
									)
									var payload struct {
										Total      int `json:"total"`
										TotalPages int `json:"total_pages"`
										Events     []struct {
											RequestID string `json:"request_id"`
											Speed     string `json:"speed"`
											Failed    bool   `json:"failed"`
											Completed bool   `json:"completed"`
										} `json:"events"`
									}
									if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
										t.Fatal(err)
									}
									if payload.Total != len(query.want) || payload.TotalPages != (len(query.want)+1)/2 {
										t.Fatalf(
											"count=%d pages=%d want count=%d", payload.Total, payload.TotalPages,
											len(query.want),
										)
									}
									start := min((page-1)*2, len(query.want))
									indices := query.want[start:min(start+2, len(query.want))]
									if len(payload.Events) != len(indices) {
										t.Fatalf("page %d events=%d want %d", page, len(payload.Events), len(indices))
									}
									for i, index := range indices {
										got, want := payload.Events[i], seeds[index]
										if got.RequestID != want.detail.RequestID || got.Speed != want.speed || got.Failed != want.detail.Failed || got.Completed {
											t.Fatalf(
												"event=%+v want request=%s speed=%s failed=%t", got,
												want.detail.RequestID, want.speed, want.detail.Failed,
											)
										}
									}
								}
							},
						)
					}
					for _, invalid := range []string{"priority", "default", "turbo", "fast,flex", "' OR 1=1 --"} {
						values := maps.Clone(baseQuery)
						values.Set("speed", invalid)
						values.Set("view", view)
						values.Set("model", "absent")
						if response := request(
							"/usage/events", values, false,
						); response.Code != http.StatusBadRequest || !strings.Contains(
							response.Body.String(), "invalid speed",
						) {
							t.Fatalf(
								"invalid speed %q: status=%d body=%s", invalid, response.Code, response.Body.String(),
							)
						}
					}
					values := maps.Clone(baseQuery)
					values["speed"] = []string{"fast", "unknown"}
					values.Set("view", view)
					if response := request("/usage/events", values, false); response.Code != http.StatusBadRequest {
						t.Fatalf("repeated speed accepted: status=%d body=%s", response.Code, response.Body.String())
					}
				}

				values := maps.Clone(baseQuery)
				values.Set("speed", "fast")
				values.Set("export", "true")
				values.Set("page", "2")
				response := request("/usage/events", values, true)
				assertUsageSpeedFilter(t, response.Body.Bytes(), "fast")
				var exported struct {
					Total  int `json:"total"`
					Events []struct {
						RequestID  string `json:"request_id"`
						Speed      string `json:"speed"`
						ImportData string `json:"import_data"`
					} `json:"events"`
				}
				if err := json.Unmarshal(
					response.Body.Bytes(), &exported,
				); err != nil || response.Code != http.StatusOK {
					t.Fatalf("detail export status=%d body=%s error=%v", response.Code, response.Body.String(), err)
				}
				if exported.Total != 3 || len(exported.Events) != 1 || exported.Events[0].RequestID != seeds[11].detail.RequestID ||
					exported.Events[0].Speed != "fast" || !strings.HasPrefix(
					exported.Events[0].ImportData, "usage-event-v1:",
				) {
					t.Fatalf("detail export=%s", response.Body.String())
				}
				if backend == "sqlite" {
					for _, speed := range []string{"FAST", "unknown", ""} {
						query := url.Values{"speed": {speed}}
						response := request("/usage/export", query, true)
						if response.Code != http.StatusOK {
							t.Fatalf("backup status=%d body=%s", response.Code, response.Body.String())
						}
						assertUsageSpeedFilter(t, response.Body.Bytes(), strings.ToLower(speed))
						events, err := usage.ParseUsageBackup(response.Body.Bytes(), "synthetic-speed-backup-key")
						if err != nil {
							t.Fatal(err)
						}
						var got, want []string
						for _, seed := range seeds {
							if speed == "" || seed.speed == strings.ToLower(speed) {
								want = append(want, seed.detail.RequestID)
							}
						}
						for _, event := range events {
							got = append(got, event.RequestID)
							if actual, _ := usage.ObservedRequestSpeed(&event); speed != "" && actual != strings.ToLower(speed) {
								t.Fatalf("backup contains actual speed %s", actual)
							}
						}
						if !slices.Equal(got, want) {
							t.Fatalf("backup ids=%v want %v", got, want)
						}
					}
					query := url.Values{
						"speed": {"fast"}, "model": {"speed-model"}, "api_key": {"synthetic-speed-client"},
						"credential": {"claude:synthetic-speed-credential"},
						"from":       {base.Add(5 * time.Second).Format(time.RFC3339Nano)},
						"to":         {base.Add(time.Hour).Format(time.RFC3339Nano)},
					}
					response := request("/usage/export", query, true)
					if response.Code != http.StatusOK {
						t.Fatalf("filtered backup status=%d body=%s", response.Code, response.Body.String())
					}
					assertUsageSpeedFilter(t, response.Body.Bytes(), "fast")
					events, err := usage.ParseUsageBackup(response.Body.Bytes(), "synthetic-speed-backup-key")
					if err != nil || len(events) != 1 || events[0].RequestID != seeds[10].detail.RequestID {
						t.Fatalf("filtered backup events=%+v error=%v", events, err)
					}
					query.Set("model", "absent")
					response = request("/usage/export", query, true)
					if response.Code != http.StatusOK {
						t.Fatalf("empty backup status=%d body=%s", response.Code, response.Body.String())
					}
					assertUsageSpeedFilter(t, response.Body.Bytes(), "fast")
					events, err = usage.ParseUsageBackup(response.Body.Bytes(), "synthetic-speed-backup-key")
					if err != nil || len(events) != 0 {
						t.Fatalf("empty backup events=%+v error=%v", events, err)
					}
					for _, invalid := range []url.Values{{"speed": {"turbo"}}, {"speed": {"fast", "unknown"}}} {
						if response := request(
							"/usage/export", invalid, true,
						); response.Code != http.StatusBadRequest || !strings.Contains(
							response.Body.String(), "invalid speed",
						) {
							t.Fatalf("invalid backup status=%d body=%s", response.Code, response.Body.String())
						}
					}
				}
			},
		)
	}
}
