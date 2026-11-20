package pluginhost_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/api"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
	logtest "github.com/sirupsen/logrus/hooks/test"
	_ "modernc.org/sqlite"
)

type mutationAuditHandler func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error)

func (handler mutationAuditHandler) HandleManagement(
	ctx context.Context,
	request pluginapi.ManagementRequest,
) (pluginapi.ManagementResponse, error) {
	return handler(ctx, request)
}

func TestDynamicPluginManagementMutationAuditConsumer(t *testing.T) {
	const credential = "fixture-management-authentication"
	const path = "/v0/management/private-fixture-token/fixture-secret.json"
	const body = `{"api_key":"fixture-body-value"}`
	directory := t.TempDir()
	t.Setenv("WRITABLE_PATH", directory)
	t.Setenv("MANAGEMENT_PASSWORD", credential)
	gin.SetMode(gin.TestMode)
	calls := 0
	handler := mutationAuditHandler(
		func(ctx context.Context, request pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
			calls++
			if request.Path != path || request.Query.Get("token") != "fixture-query-value" ||
				string(request.Body) != body {
				t.Fatalf("plugin request changed: %#v", request)
			}
			if request.Query.Get("fail") == "1" {
				return pluginapi.ManagementResponse{}, errors.New("fixture operation failed")
			}
			if request.Query.Get("confirm") == "1" {
				management.RecordMutationEffect(ctx, "plugin.persisted", "plugin")
				management.CompleteMutation(ctx)
			}
			return pluginapi.ManagementResponse{
				StatusCode: http.StatusAccepted,
				Headers:    http.Header{"Content-Type": {"application/json"}},
				Body:       []byte(`{"value":"<fixture> & unchanged"}`),
			}, nil
		},
	)
	var routes []pluginapi.ManagementRoute
	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodGet,
	} {
		routes = append(routes, pluginapi.ManagementRoute{Method: method, Path: path, Handler: handler})
	}
	host := pluginhost.NewManagementAuditTestHost(routes)
	var engine *gin.Engine
	cfg := &config.Config{AuthDir: filepath.Join(directory, "auth"), Debug: true}
	server := api.NewServer(
		cfg, auth.NewManager(nil, nil, nil), sdkaccess.NewManager(), filepath.Join(directory, "config.yaml"),
		api.WithPluginHost(host), api.WithEngineConfigurator(func(value *gin.Engine) { engine = value }),
	)
	t.Cleanup(func() { _ = server.ManagementHandler().Close() })
	db, err := sql.Open("sqlite", filepath.Join(directory, "management-state", "management-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	hook := logtest.NewGlobal()
	defer hook.Reset()
	audits := 0
	for _, check := range []struct {
		name, method, query    string
		authenticated, audited bool
		status                 int
	}{
		{"unauthenticated write", http.MethodPost, "", false, false, http.StatusUnauthorized},
		{"read", http.MethodGet, "", true, false, http.StatusAccepted},
		{"post", http.MethodPost, "", true, true, http.StatusAccepted},
		{"put", http.MethodPut, "", true, true, http.StatusAccepted},
		{"patch", http.MethodPatch, "", true, true, http.StatusAccepted},
		{"delete", http.MethodDelete, "", true, true, http.StatusAccepted},
		{"plugin failure", http.MethodPost, "&fail=1", true, true, http.StatusBadGateway},
		{"confirmed effect", http.MethodPost, "&confirm=1", true, true, http.StatusAccepted},
	} {
		t.Run(
			check.name, func(t *testing.T) {
				before := calls
				request := httptest.NewRequest(
					check.method, path+"?token=fixture-query-value"+check.query, strings.NewReader(body),
				)
				request.RemoteAddr = "127.0.0.1:12345"
				request.Header.Set("Content-Type", "application/json")
				if check.authenticated {
					request.Header.Set("Authorization", "Bearer "+credential)
				}
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				if response.Code != check.status {
					t.Fatalf("response = %d %s, want %d", response.Code, response.Body.String(), check.status)
				}
				if check.authenticated && calls != before+1 || !check.authenticated && calls != before {
					t.Fatalf("plugin calls = %d, previous %d", calls, before)
				}
				if check.status == http.StatusAccepted &&
					response.Body.String() != `{"value":"<fixture> & unchanged"}` {
					t.Fatalf("plugin response changed: %s", response.Body.String())
				}
				if check.audited {
					audits++
				}
				var count int
				if err := db.QueryRow("SELECT COUNT(*) FROM management_mutation_audit").Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != audits {
					t.Fatalf("audit records = %d, want %d", count, audits)
				}
				if !check.audited {
					if response.Header().Get(management.MutationResultHeader) != "" {
						t.Fatal("non-mutation response contains mutation result")
					}
					return
				}
				var result management.MutationResult
				if err := json.Unmarshal(
					[]byte(response.Header().Get(management.MutationResultHeader)), &result,
				); err != nil {
					t.Fatalf("mutation header: %v", err)
				}
				if !result.AuditAvailable || len(result.RequestID) != 32 {
					t.Fatalf("mutation audit result = %#v", result)
				}
				wantOutcome := "uncertain"
				if check.query == "&confirm=1" {
					wantOutcome = "succeeded"
				}
				if result.Outcome != wantOutcome || result.ConfirmedEffect != (check.query == "&confirm=1") {
					t.Fatalf("unconfirmed plugin effects reported as complete: %#v", result)
				}
				var requestID, actor, method, route, resource, target, storedResult string
				var status int
				if err := db.QueryRow(
					`SELECT request_id, actor_kind, method, route, resource, target, status_code, result_json
				FROM management_mutation_audit ORDER BY id DESC LIMIT 1`,
				).Scan(
					&requestID, &actor, &method, &route, &resource, &target, &status, &storedResult,
				); err != nil {
					t.Fatal(err)
				}
				if requestID != result.RequestID || actor != "management_key" || method != check.method ||
					route !=
						("/v0/management/plugins/:"+
							"route") ||
					resource !=
						"plugin" ||
					target !=
						"" ||
					status !=
						check.status {
					t.Fatalf(
						"stored audit = %s %s %s %s %s %s %d",
						requestID,
						actor,
						method,
						route,
						resource,
						target,
						status,
					)
				}
				foundLog := false
				for _, entry := range hook.AllEntries() {
					if entry.Data["request_id"] == requestID {
						foundLog = true
						break
					}
				}
				if !foundLog {
					t.Fatal("audit and actual request log do not share their generated request ID")
				}
				for _, secret := range []string{
					credential, path, "fixture-query-value", "fixture-body-value", "fixture-secret.json",
				} {
					if strings.Contains(route+target+storedResult, secret) {
						t.Fatalf("audit contains sensitive value %q", secret)
					}
				}
			},
		)
	}
	// Finish management persistence before testing triggers through the independent connection.
	if err := server.ManagementHandler().Close(); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"UPDATE management_mutation_audit SET target='changed'",
		"DELETE FROM management_mutation_audit",
	} {
		if _, err := db.Exec(statement); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("audit modification = %v, want append-only rejection", err)
		}
	}
}
