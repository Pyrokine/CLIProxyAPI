package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

func TestGetUsageSummaryRejectsOversizedRange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(
		http.MethodGet,
		"/v0/management/usage/summary?from=2020-01-01T00:00:00Z&to=2026-01-01T00:00:00Z&granularity=daily",
		nil,
	)

	handler := &Handler{usagePersister: persister}
	handler.GetUsageSummary(ginContext)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestGetUsageSummaryRejectsInvalidGranularity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{}
	for _, path := range []string{
		"/v0/management/usage/summary?granularity=weekly",
		"/v0/management/usage/summary?auth_index_groups=true&auth_index_granularity=5m",
	} {
		recorder := httptest.NewRecorder()
		ginContext, _ := gin.CreateTestContext(recorder)
		ginContext.Request = httptest.NewRequest(http.MethodGet, path, nil)
		handler.GetUsageSummary(ginContext)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf(
				"GetUsageSummary(%q) status = %d, want %d body=%s", path, recorder.Code, http.StatusBadRequest,
				recorder.Body.String(),
			)
		}
	}
}

func TestGetUsageSummaryRejectsOversizedTenMinuteAuthIndexRange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(
		http.MethodGet,
		"/v0/management/usage/summary?from=2026-09-01T00:00:00Z&to=2026-09-01T05:00:00Z&"+
			"groups=none&auth_index_groups=true&auth_index_granularity=10m",
		nil,
	)

	handler := &Handler{usagePersister: persister}
	handler.GetUsageSummary(ginContext)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestGetUsageSummaryRejectsOutOfRangeTimestamp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(
		http.MethodGet,
		"/v0/management/usage/summary?from=0001-01-01T00:00:00Z&to=2026-01-01T00:00:00Z",
		nil,
	)

	handler := &Handler{usagePersister: persister}
	handler.GetUsageSummary(ginContext)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestGetUsageEventsRejectsOutOfRangeTimestamp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(
		http.MethodGet,
		"/v0/management/usage/events?from=2026-01-01T00:00:00Z&to=9999-12-31T23:59:59Z",
		nil,
	)

	handler := &Handler{usagePersister: persister}
	handler.GetUsageEvents(ginContext)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestGetUsageEventsRejectsDeepPage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(
		http.MethodGet,
		"/v0/management/usage/events?from=2026-08-03T00:00:00Z&to=2026-08-03T01:00:00Z&page=202&page_size=500",
		nil,
	)

	handler := &Handler{usagePersister: persister}
	handler.GetUsageEvents(ginContext)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}
