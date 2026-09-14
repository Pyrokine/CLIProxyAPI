package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

func TestGetUsageSummaryIncludesAuthIndexGroupsOnDemand(t *testing.T) {
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}
	defer persister.Stop()

	from := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	events := []usage.FlatDetail{
		{
			Timestamp: from.Add(10 * time.Minute),
			Model:     "model-a",
			AuthIndex: "auth-a",
			Provider:  "claude",
			Completed: true,
		},
		{
			Timestamp: from.Add(time.Hour + 10*time.Minute),
			Model:     "model-a",
			AuthIndex: "auth-a",
			Provider:  "claude",
			Completed: true,
			Failed:    true,
		},
		{
			Timestamp: from.Add(2*time.Hour + 10*time.Minute),
			Model:     "model-b",
			AuthIndex: "auth-b",
			Provider:  "openai-compatibility",
			Completed: true,
		},
		{
			Timestamp: from.Add(2*time.Hour + 20*time.Minute),
			Model:     "model-b",
			Provider:  "claude",
			Completed: true,
		},
	}
	for i := range events {
		persister.Record(events[i])
	}

	handler := NewHandler(
		&config.Config{},
		"",
		nil,
		withEnvSecret(""),
		withUsagePersister(persister),
	)
	basePath := fmt.Sprintf(
		"/usage/summary?from=%s&to=%s&granularity=hourly",
		from.Format(time.RFC3339),
		from.Add(2*time.Hour+59*time.Minute).Format(time.RFC3339),
	)
	requestSummary := func(path string) summaryResponse {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
		handler.GetUsageSummary(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GetUsageSummary status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var response summaryResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode GetUsageSummary response: %v", err)
		}
		return response
	}

	var regular summaryResponse
	deadline := time.Now().Add(5 * time.Second)
	for {
		regular = requestSummary(basePath)
		if regular.Totals.Requests == int64(len(events)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage records were not persisted before deadline: %+v", regular.Totals)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(regular.ByAuthIndex) != 0 || len(regular.TimeSeriesByAuthIndex) != 0 {
		t.Fatalf("regular summary returned auth-index groups: %+v", regular.ByAuthIndex)
	}

	grouped := requestSummary(basePath + "&auth_index_groups=true")
	statsA := grouped.ByAuthIndex["auth-a"]
	if statsA == nil || statsA.Requests != 2 || statsA.Success != 1 || statsA.Failure != 1 {
		t.Fatalf("auth-a aggregate = %+v", statsA)
	}
	statsB := grouped.ByAuthIndex["auth-b"]
	if statsB == nil || statsB.Requests != 1 || statsB.Success != 1 || statsB.Failure != 0 {
		t.Fatalf("auth-b aggregate = %+v", statsB)
	}
	if len(grouped.ByAuthIndex) != 2 {
		t.Fatalf("auth-index aggregate count = %d, want 2", len(grouped.ByAuthIndex))
	}
	seriesA := grouped.TimeSeriesByAuthIndex["auth-a"]
	if len(seriesA) != 3 || seriesA[0].Requests != 1 || seriesA[1].Requests != 1 || seriesA[2].Requests != 0 {
		t.Fatalf("auth-a time series = %+v", seriesA)
	}

	withoutStandardGroups := requestSummary(basePath + "&groups=none&auth_index_groups=true")
	if withoutStandardGroups.ByAuthIndex["auth-a"] == nil || withoutStandardGroups.ByAuthIndex["auth-b"] == nil {
		t.Fatalf("groups=none removed requested auth-index aggregates: %+v", withoutStandardGroups.ByAuthIndex)
	}
	if len(withoutStandardGroups.TimeSeriesByModel) != 0 ||
		len(withoutStandardGroups.TimeSeriesByCredential) != 0 ||
		len(withoutStandardGroups.TimeSeriesByAPIKey) != 0 {
		t.Fatalf("groups=none returned standard grouped series: %+v", withoutStandardGroups.TimeSeriesByModel)
	}
	if len(withoutStandardGroups.TimeSeriesByAuthIndex["auth-a"]) != 3 {
		t.Fatalf(
			"groups=none removed explicitly requested auth-index series: %+v",
			withoutStandardGroups.TimeSeriesByAuthIndex,
		)
	}

	tenMinutePath := fmt.Sprintf(
		"/usage/summary?from=%s&to=%s&granularity=hourly&groups=none&"+
			"auth_index_groups=true&auth_index_granularity=10m",
		from.Format(time.RFC3339),
		from.Add(3*time.Hour+59*time.Minute).Format(time.RFC3339),
	)
	tenMinute := requestSummary(tenMinutePath)
	seriesA = tenMinute.TimeSeriesByAuthIndex["auth-a"]
	if len(seriesA) != 24 {
		t.Fatalf("10-minute auth-a point count = %d, want 24: %+v", len(seriesA), seriesA)
	}
	if seriesA[1].Requests != 1 || seriesA[7].Requests != 1 ||
		seriesA[0].Requests != 0 || seriesA[23].Requests != 0 {
		t.Fatalf("10-minute auth-a series = %+v", seriesA)
	}
	seriesB := tenMinute.TimeSeriesByAuthIndex["auth-b"]
	if len(seriesB) != 24 || seriesB[13].Requests != 1 {
		t.Fatalf("10-minute auth-b series = %+v", seriesB)
	}
}
