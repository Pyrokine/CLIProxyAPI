package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

type requestLogStateResponse struct {
	Error     string                     `json:"error"`
	RequestID string                     `json:"request_id"`
	State     logging.RequestLogState    `json:"request_log_state"`
	Location  logging.RequestLogLocation `json:"request_log_location"`
	Reason    string                     `json:"reason"`
}

func newRequestLogTestHandler(t *testing.T) (*Handler, *usage.Persister, string) {
	t.Helper()
	logsDir := t.TempDir()
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	if !persister.IsReady() {
		t.Fatal("usage persister did not start")
	}
	t.Cleanup(persister.Stop)
	handler := NewHandler(
		&config.Config{},
		"",
		nil,
		withEnvSecret(""),
		withUsagePersister(persister),
		withLogDir(logsDir),
	)
	t.Cleanup(func() { _ = handler.Close() })
	return handler, persister, logsDir
}

func submitRequestLogArtifactAndWait(
	t *testing.T,
	persister *usage.Persister,
	artifact logging.RequestLogArtifact,
) logging.RequestLogArtifact {
	t.Helper()
	if !persister.SubmitRequestLogArtifact(artifact) {
		t.Fatal("request log artifact was not accepted")
	}
	return waitRequestLogArtifactState(t, persister, artifact.RequestID, artifact.State)
}

func waitRequestLogArtifactState(
	t *testing.T,
	persister *usage.Persister,
	requestID string,
	state logging.RequestLogState,
) logging.RequestLogArtifact {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		stored, found, err := persister.QueryRequestLogArtifactContext(context.Background(), requestID)
		if err == nil && found && stored.State == state {
			return stored
		}
		if time.Now().After(deadline) {
			t.Fatalf("request log artifact state was not persisted: found=%t artifact=%#v error=%v", found, stored, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func performRequestLogLookup(t *testing.T, handler *Handler, requestID, query string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	target := "/v0/management/request-log-by-id/" + requestID
	if query != "" {
		target += "?" + query
	}
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	c.Params = gin.Params{{Key: "id", Value: requestID}}
	handler.GetRequestLogByID(c)
	return recorder
}

func decodeRequestLogStateResponse(t *testing.T, recorder *httptest.ResponseRecorder) requestLogStateResponse {
	t.Helper()
	var response requestLogStateResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode request log state response: %v; body=%s", err, recorder.Body.String())
	}
	return response
}

func TestGetRequestLogByIDMapsLifecycleStates(t *testing.T) {
	handler, persister, _ := newRequestLogTestHandler(t)
	tests := []struct {
		state      logging.RequestLogState
		wantStatus int
	}{
		{state: logging.RequestLogPending, wantStatus: http.StatusAccepted},
		{state: logging.RequestLogDisabled, wantStatus: http.StatusConflict},
		{state: logging.RequestLogNotCaptured, wantStatus: http.StatusConflict},
		{state: logging.RequestLogPublishFailed, wantStatus: http.StatusInternalServerError},
		{state: logging.RequestLogExpired, wantStatus: http.StatusGone},
		{state: logging.RequestLogLegacyUnknown, wantStatus: http.StatusConflict},
	}
	for _, test := range tests {
		t.Run(
			string(test.state), func(t *testing.T) {
				requestID := "request-" + string(test.state)
				submitRequestLogArtifactAndWait(
					t, persister, logging.RequestLogArtifact{
						RequestID: requestID,
						State:     test.state,
						Reason:    "stored reason",
					},
				)
				recorder := performRequestLogLookup(t, handler, requestID, "lines=10")
				if recorder.Code != test.wantStatus {
					t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.wantStatus, recorder.Body.String())
				}
				response := decodeRequestLogStateResponse(t, recorder)
				if response.RequestID != requestID || response.State != test.state || response.Reason != "stored reason" {
					t.Fatalf("response = %#v, want request ID, state, and stored reason", response)
				}
				if test.state == logging.RequestLogPending && recorder.Header().Get("Retry-After") != "1" {
					t.Fatalf("Retry-After = %q, want 1", recorder.Header().Get("Retry-After"))
				}
			},
		)
	}
}

func TestGetRequestLogByIDServesIndexedLocalArtifact(t *testing.T) {
	handler, persister, logsDir := newRequestLogTestHandler(t)
	const requestID = "request-available"
	const fileName = "request-available.log"
	content := strings.Join([]string{"first", "second", "third"}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(logsDir, fileName), []byte(content), 0o600); err != nil {
		t.Fatalf("write request log: %v", err)
	}
	submitRequestLogArtifactAndWait(
		t, persister, logging.RequestLogArtifact{
			RequestID: requestID,
			State:     logging.RequestLogAvailable,
			Location:  logging.RequestLogLocationLocal,
			FileName:  fileName,
		},
	)

	recorder := performRequestLogLookup(t, handler, requestID, "lines=2")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	var preview struct {
		Content    string `json:"content"`
		Truncated  bool   `json:"truncated"`
		TotalLines int    `json:"total_lines"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.Content != "second\nthird" || !preview.Truncated || preview.TotalLines != 3 {
		t.Fatalf("preview = %#v, want last two of three lines", preview)
	}
}

func TestGetRequestLogByIDExpiresMissingLocalArtifact(t *testing.T) {
	handler, persister, _ := newRequestLogTestHandler(t)
	const requestID = "request-missing-local"
	submitRequestLogArtifactAndWait(
		t, persister, logging.RequestLogArtifact{
			RequestID: requestID,
			State:     logging.RequestLogAvailable,
			Location:  logging.RequestLogLocationLocal,
			FileName:  "missing.log",
		},
	)

	recorder := performRequestLogLookup(t, handler, requestID, "lines=10")
	if recorder.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410; body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeRequestLogStateResponse(t, recorder)
	if response.State != logging.RequestLogExpired || !strings.Contains(response.Reason, "no longer exists") {
		t.Fatalf("response = %#v, want expired missing-file state", response)
	}
	stored := waitRequestLogArtifactState(t, persister, requestID, logging.RequestLogExpired)
	if !strings.Contains(stored.Reason, "no longer exists") {
		t.Fatalf("stored artifact = %#v, want missing-file reason", stored)
	}
}

func TestGetRequestLogByIDDistinguishesLegacyAndUnknownRequests(t *testing.T) {
	handler, persister, logsDir := newRequestLogTestHandler(t)

	t.Run(
		"duplicate legacy files", func(t *testing.T) {
			const requestID = "legacyduplicate"
			for _, name := range []string{
				"error-v1-responses-2026-08-30T100000-" + requestID + ".log",
				"error-v1-responses-2026-08-30T100001-" + requestID + ".log",
			} {
				if err := os.WriteFile(filepath.Join(logsDir, name), []byte("legacy"), 0o600); err != nil {
					t.Fatalf("write legacy log: %v", err)
				}
			}
			recorder := performRequestLogLookup(t, handler, requestID, "lines=10")
			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
			}
			response := decodeRequestLogStateResponse(t, recorder)
			if response.State != logging.RequestLogLegacyUnknown || !strings.Contains(response.Error, "multiple") {
				t.Fatalf("response = %#v, want duplicate legacy conflict", response)
			}
		},
	)

	t.Run(
		"legacy event without lifecycle row", func(t *testing.T) {
			const requestID = "legacyevent"
			persister.Record(
				usage.FlatDetail{
					Timestamp: time.Now().UTC(),
					RequestID: requestID,
					Model:     "legacy-model",
				},
			)
			deadline := time.Now().Add(2 * time.Second)
			for {
				exists, err := persister.HasEventRequestIDContext(context.Background(), requestID)
				if err == nil && exists {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("legacy event was not persisted: exists=%t error=%v", exists, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			recorder := performRequestLogLookup(t, handler, requestID, "lines=10")
			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
			}
			if response := decodeRequestLogStateResponse(
				t, recorder,
			); response.State != logging.RequestLogLegacyUnknown {
				t.Fatalf("response = %#v, want legacy_unknown", response)
			}
		},
	)

	t.Run(
		"unknown request ID", func(t *testing.T) {
			recorder := performRequestLogLookup(t, handler, "unknownrequest", "lines=10")
			if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "unknown request ID") {
				t.Fatalf("status = %d, body=%s; want unknown 404", recorder.Code, recorder.Body.String())
			}
		},
	)
}
