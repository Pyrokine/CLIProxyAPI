package management

import (
	"context"
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
		WithEnvSecret(""),
		WithUsagePersister(source),
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
		WithEnvSecret(""),
		WithUsagePersister(target),
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
