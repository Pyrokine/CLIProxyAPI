package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

func TestGetUsageEventsCompactProjectsAndBoundsResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("MANAGEMENT_PASSWORD", "")

	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	t.Cleanup(persister.Stop)
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}

	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	apiKey := "compact-secret-api-key"
	for index := range 50 {
		budget := 4096
		persister.Record(
			usage.FlatDetail{
				Timestamp:         base.Add(time.Duration(index) * time.Second),
				Model:             "compact-model",
				Source:            "compact-source",
				AuthIndex:         "compact-auth",
				APIKey:            apiKey,
				Provider:          "compact-provider",
				LatencyMs:         1200 + int64(index),
				RequestID:         fmt.Sprintf("compact-request-%02d", index),
				TimeToFirstByteMs: 200,
				TotalDurationMs:   1500,
				Completed:         true,
				ClientIP:          "192.0.2.1",
				XForwardedFor:     "198.51.100.1",
				UserAgent:         strings.Repeat("metadata-heavy-user-agent", 20),
				ReasoningEffort:   "high",
				Thinking: &usage.Thinking{
					Intensity: "high",
					Mode:      "enabled",
					Level:     "deep",
					Budget:    &budget,
				},
				ResponseHeaders: http.Header{
					"X-Large-Metadata": {strings.Repeat("x", 4<<10)},
				},
				Failed: index%7 == 0,
			},
		)
	}
	if !persister.SubmitRequestLogArtifact(
		logging.RequestLogArtifact{
			RequestID: "compact-request-00",
			State:     logging.RequestLogAvailable,
			Location:  logging.RequestLogLocationHome,
			HomeIP:    "203.0.113.1",
			Revision:  1,
			UpdatedAt: base.Add(time.Minute),
		},
	) {
		t.Fatal("request log artifact was not accepted")
	}

	handler := NewHandler(
		&config.Config{APIKeyAliases: map[string]string{apiKey: "compact-alias"}},
		"",
		nil,
		withEnvSecret(""),
		withUsagePersister(persister),
	)
	path := fmt.Sprintf(
		"/usage/events?view=compact&from=%s&to=%s&page=1&page_size=50&sort=timestamp&order=desc",
		base.Add(-time.Minute).Format(time.RFC3339),
		base.Add(time.Hour).Format(time.RFC3339),
	)
	type compactEnvelope struct {
		Events []map[string]json.RawMessage `json:"events"`
		Total  int                          `json:"total"`
	}
	request := func() (*httptest.ResponseRecorder, compactEnvelope) {
		t.Helper()
		recorder := httptest.NewRecorder()
		ginContext, _ := gin.CreateTestContext(recorder)
		ginContext.Request = httptest.NewRequest(http.MethodGet, path, nil)
		handler.GetUsageEvents(ginContext)
		if recorder.Code != http.StatusOK {
			t.Fatalf(
				"GetUsageEvents compact status = %d, want %d body=%s", recorder.Code, http.StatusOK,
				recorder.Body.String(),
			)
		}
		var response compactEnvelope
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode compact response: %v body=%s", err, recorder.Body.String())
		}
		return recorder, response
	}

	var recorder *httptest.ResponseRecorder
	var response compactEnvelope
	deadline := time.Now().Add(5 * time.Second)
	for {
		recorder, response = request()
		artifactReady := false
		for _, event := range response.Events {
			var requestID string
			var homeIP string
			_ = json.Unmarshal(event["request_id"], &requestID)
			_ = json.Unmarshal(event["request_log_home_ip"], &homeIP)
			if requestID == "compact-request-00" && homeIP == "203.0.113.1" {
				artifactReady = true
				break
			}
		}
		if response.Total == 50 && len(response.Events) == 50 && artifactReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf(
				"compact events were not persisted before deadline: total=%d events=%d", response.Total,
				len(response.Events),
			)
		}
		time.Sleep(25 * time.Millisecond)
	}

	if recorder.Body.Len() >= 60<<10 {
		t.Fatalf("compact response size = %d bytes, want less than %d", recorder.Body.Len(), 60<<10)
	}
	allowed := map[string]struct{}{
		"timestamp": {}, "model": {}, "source": {}, "auth_index": {}, "api_key": {},
		"api_key_alias": {}, "provider": {}, "latency_ms": {}, "request_id": {},
		"request_log_state": {}, "request_log_home_ip": {}, "time_to_first_byte_ms": {},
		"total_duration_ms": {}, "completed": {}, "metadata_recorded": {}, "thinking": {},
		"tokens": {}, "cost": {}, "cost_status": {}, "cost_reason": {}, "pricing_receipt": {}, "failed": {},
	}
	for index, event := range response.Events {
		for key := range event {
			if _, ok := allowed[key]; !ok {
				t.Fatalf("compact event[%d] returned detail-only field %q", index, key)
			}
		}
		for _, key := range []string{
			"timestamp", "model", "source", "auth_index", "api_key", "api_key_alias", "provider",
			"latency_ms", "request_id", "time_to_first_byte_ms", "total_duration_ms", "completed",
			"metadata_recorded", "thinking", "tokens", "cost", "cost_status", "pricing_receipt", "failed",
		} {
			if _, ok := event[key]; !ok {
				t.Fatalf("compact event[%d] missing field %q", index, key)
			}
		}
		var receipt usage.PricingReceipt
		if err := json.Unmarshal(event["pricing_receipt"], &receipt); err != nil {
			t.Fatalf("decode compact event[%d] pricing receipt: %v", index, err)
		}
		if receipt.SchemaVersion != usage.PricingReceiptSchemaVersion ||
			receipt.Provider != "compact-provider" ||
			receipt.Model != "compact-model" ||
			receipt.CanonicalModel != "compact-model" ||
			receipt.Source != "unconfigured" ||
			receipt.CostStatus != usage.CostStatusUnpriced {
			t.Fatalf("compact event[%d] pricing receipt = %#v", index, receipt)
		}
	}
	if strings.Contains(recorder.Body.String(), apiKey) {
		t.Fatalf("compact response returned raw API key: %s", recorder.Body.String())
	}
}

func TestGetUsageEventsRejectsInvalidView(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/usage/events?view=summary", nil)

	(&Handler{}).GetUsageEvents(ginContext)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}
