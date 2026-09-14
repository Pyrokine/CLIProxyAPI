package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

func TestWriteUsageImportResultIncludesWarningWhenTotalsFail(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	writeUsageImportResult(
		c,
		usage.ImportResult{Added: 2, Skipped: 1, Format: "usageBackupV3"},
		0,
		0,
		errors.New("query failed"),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, recorder.Body.String())
	}
	if response["warning"] != "usage imported, but totals could not be queried" {
		t.Fatalf("warning response = %#v", response)
	}
	if _, ok := response["total_requests"]; ok {
		t.Fatalf("warning response unexpectedly included totals: %#v", response)
	}
}

func TestWriteUsageImportResultIncludesPostImportWarning(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	writeUsageImportResult(
		c,
		usage.ImportResult{
			Added:   1,
			Format:  "usageBackupV3",
			Warning: "usage imported, but post-import maintenance did not finish",
		},
		3,
		1,
		nil,
	)
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, recorder.Body.String())
	}
	if response["warning"] != "usage imported, but post-import maintenance did not finish" ||
		response["total_requests"] != float64(3) || response["failed_requests"] != float64(1) {
		t.Fatalf("post-import warning response = %#v", response)
	}
}

func TestImportUsageStatisticsHonorsCanceledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/usage/import", strings.NewReader("[]")).WithContext(ctx)
	h := &Handler{usagePersister: &usage.Persister{}}
	h.ImportUsageStatistics(c)
	if recorder.Body.Len() != 0 {
		t.Fatalf("canceled import wrote response body: %s", recorder.Body.String())
	}
}

func TestWriteUsageImportResultHonorsRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/usage/import", nil).WithContext(ctx)
	writeUsageImportResult(
		c,
		usage.ImportResult{Added: 1, Format: "usageBackupV3"},
		0,
		0,
		context.Canceled,
	)
	if recorder.Body.Len() != 0 {
		t.Fatalf("canceled import result wrote response body: %s", recorder.Body.String())
	}
}

func TestUsageExportImportEncryptedRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	source := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	source.Start(ctx)
	if !source.IsReady() {
		t.Fatal("source usage persister did not start")
	}
	defer source.Stop()

	const rawAPIKey = "sk-handler-backup-roundtrip-secret"
	legacy := `[{` +
		`"timestamp":"2026-07-29T10:11:12.123456789Z",` +
		`"model":"gpt-5.4",` +
		`"source":"codex-user.json",` +
		`"auth_index":"codex-user@example.com",` +
		`"api_key":"` + rawAPIKey + `",` +
		`"provider":"codex",` +
		`"latency_ms":321,` +
		`"request_id":"handler-backup-request",` +
		`"time_to_first_byte_ms":123,` +
		`"total_duration_ms":456,` +
		`"completed":true,` +
		`"reasoning_effort":"high",` +
		`"service_tier":"priority",` +
		`"request_service_tier":"priority",` +
		`"response_service_tier":"default",` +
		`"executor_type":"openai-response",` +
		`"thinking":{"intensity":"high","mode":"enabled","level":"deep","budget":4096},` +
		`"response_headers":{"Content-Type":["application/json"],"X-Upstream-Token":["secret-header"]},` +
		`"tokens":{"input_tokens":101,"output_tokens":202,"reasoning_tokens":33,"cached_tokens":44,"total_tokens":380},` +
		`"failed":false` +
		`}]`
	if _, err := source.ImportContext(ctx, []byte(legacy)); err != nil {
		t.Fatalf("source ImportContext: %v", err)
	}

	const backupKey = "portable-handler-backup-key"
	sourceHandler := NewHandler(
		&config.Config{},
		"",
		nil,
		withEnvSecret(""),
		withUsagePersister(source),
	)
	exportRecorder := httptest.NewRecorder()
	exportContext, _ := gin.CreateTestContext(exportRecorder)
	exportContext.Request = httptest.NewRequest(http.MethodGet, "/usage/export", nil)
	exportContext.Request.Header.Set("Authorization", "Bearer "+backupKey)
	sourceHandler.ExportUsageStatistics(exportContext)
	if exportRecorder.Code != http.StatusOK {
		t.Fatalf("ExportUsageStatistics status = %d body=%s", exportRecorder.Code, exportRecorder.Body.String())
	}
	if strings.Contains(exportRecorder.Body.String(), rawAPIKey) {
		t.Fatalf("ExportUsageStatistics exposed raw API key: %s", exportRecorder.Body.String())
	}

	target := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	target.Start(ctx)
	if !target.IsReady() {
		t.Fatal("target usage persister did not start")
	}
	defer target.Stop()
	targetHandler := NewHandler(
		&config.Config{},
		"",
		nil,
		withEnvSecret(""),
		withUsagePersister(target),
	)
	importRecorder := httptest.NewRecorder()
	importContext, _ := gin.CreateTestContext(importRecorder)
	importContext.Request = httptest.NewRequest(
		http.MethodPost,
		"/usage/import",
		strings.NewReader(exportRecorder.Body.String()),
	)
	importContext.Request.Header.Set(usageBackupKeyHeader, backupKey)
	targetHandler.ImportUsageStatistics(importContext)
	if importRecorder.Code != http.StatusOK {
		t.Fatalf("ImportUsageStatistics status = %d body=%s", importRecorder.Code, importRecorder.Body.String())
	}
	var importResponse struct {
		Added          int64  `json:"added"`
		Skipped        int64  `json:"skipped"`
		TotalRequests  int64  `json:"total_requests"`
		FailedRequests int64  `json:"failed_requests"`
		Warning        string `json:"warning"`
	}
	if err := json.Unmarshal(importRecorder.Body.Bytes(), &importResponse); err != nil {
		t.Fatalf("decode ImportUsageStatistics response: %v body=%s", err, importRecorder.Body.String())
	}
	if importResponse.Added != 1 || importResponse.Skipped != 0 ||
		importResponse.TotalRequests != 1 || importResponse.FailedRequests != 0 || importResponse.Warning != "" {
		t.Fatalf("ImportUsageStatistics response = %+v, want added/total 1 and no failures/warning", importResponse)
	}

	sourceEvents, err := source.QueryBackupEventsContext(ctx, time.Time{}, time.Time{}, usage.EventFilters{})
	if err != nil {
		t.Fatalf("source QueryBackupEventsContext: %v", err)
	}
	targetEvents, err := target.QueryBackupEventsContext(ctx, time.Time{}, time.Time{}, usage.EventFilters{})
	if err != nil {
		t.Fatalf("target QueryBackupEventsContext: %v", err)
	}
	if !reflect.DeepEqual(targetEvents, sourceEvents) {
		t.Fatalf("handler backup round trip changed events\nsource=%#v\ntarget=%#v", sourceEvents, targetEvents)
	}
}
