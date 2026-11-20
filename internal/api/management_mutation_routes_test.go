package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/google/uuid"
)

func TestManagementMutationResultRouteAndCORS(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")
	server := newTestServer(t)
	mutationID := uuid.NewString()
	path := "/v0/management/mutations/" + mutationID
	request := func(method, path, body string, authorized bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Origin", "https://management.test")
		req.Header.Set(management.MutationIDHeader, mutationID)
		if authorized {
			req.Header.Set("Authorization", "Bearer test-management-password")
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		return recorder
	}
	if result := request(http.MethodGet, path, "", false); result.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized result = %d: %s", result.Code, result.Body.String())
	}
	if result := request(
		http.MethodGet, path, "", true,
	); result.Code != http.StatusNotFound || !strings.Contains(result.Body.String(), `"status":"unknown"`) {
		t.Fatalf("unknown mutation = %d: %s", result.Code, result.Body.String())
	}
	written := request(http.MethodPut, "/v0/management/debug", `{"value":false}`, true)
	if written.Code != http.StatusOK {
		t.Fatalf("write = %d: %s", written.Code, written.Body.String())
	}
	var result management.MutationResult
	if err := json.Unmarshal([]byte(written.Header().Get(management.MutationResultHeader)), &result); err != nil {
		t.Fatalf("mutation result header: %v", err)
	}
	if result.MutationID != mutationID || result.RequestID == "" || !result.AuditAvailable {
		t.Fatalf("mutation identity/audit = %#v", result)
	}
	readback := request(http.MethodGet, path, "", true)
	if readback.Code != http.StatusOK {
		t.Fatalf("readback = %d: %s", readback.Code, readback.Body.String())
	}
	var stored management.MutationResult
	if err := json.Unmarshal(readback.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.MutationID != result.MutationID || stored.RequestID != result.RequestID ||
		stored.Outcome != result.Outcome ||
		stored.ConfirmedEffect != result.ConfirmedEffect {
		t.Fatalf("readback = %#v, header = %#v", stored, result)
	}
	for _, header := range []string{management.MutationIDHeader, management.MutationResultHeader} {
		if !strings.Contains(written.Header().Get("Access-Control-Expose-Headers"), header) {
			t.Errorf("browser cannot read %s", header)
		}
	}

	preflight := httptest.NewRequest(http.MethodOptions, "/v0/management/debug", nil)
	preflight.Header.Set("Origin", "https://management.test")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPut)
	preflight.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type, "+management.MutationIDHeader)
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, preflight)
	if recorder.Code != http.StatusNoContent || !strings.Contains(
		recorder.Header().Get("Access-Control-Allow-Headers"), management.MutationIDHeader,
	) {
		t.Fatalf("mutation preflight = %d, headers = %v", recorder.Code, recorder.Header())
	}
}
