package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type modelDetailInterceptorHost struct {
	homeCatalogInterceptorHost
	body        string
	completions []pluginapi.RequestCompletion
}

func (h *modelDetailInterceptorHost) InterceptResponse(
	_ context.Context, _ pluginapi.ResponseInterceptRequest,
) pluginapi.ResponseInterceptResponse {
	return pluginapi.ResponseInterceptResponse{
		Headers: http.Header{"Content-Length": {"200"}}, Body: []byte(h.body),
	}
}

func (h *modelDetailInterceptorHost) CompleteRequest(
	_ context.Context, completion pluginapi.RequestCompletion,
) {
	h.completions = append(h.completions, completion)
}

func TestModelDetailUsesFilteredCatalogAndReportsCompletion(t *testing.T) {
	server := newTestServer(t)
	host := &modelDetailInterceptorHost{}
	server.handlers.SetPluginHost(host)
	for _, test := range []struct {
		name   string
		body   string
		model  string
		status int
	}{
		{"visible", `{"data":[{"id":"vendor/visible","object":"model"}]}`, "vendor/visible", http.StatusOK},
		{"missing", `{"data":[{"id":"vendor/visible"}]}`, "hidden", http.StatusNotFound},
		{"invalid", `not json`, "vendor/visible", http.StatusBadGateway},
		{"models slug", `{"models":[{"slug":"vendor/visible","name":"Visible"}]}`, "vendor/visible", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			host.body = test.body
			previous := len(host.completions)
			request := httptest.NewRequest(http.MethodGet, "/v1/models/"+test.model, nil)
			request.Header.Set("Authorization", "Bearer test-key")
			recorder := httptest.NewRecorder()
			server.engine.ServeHTTP(recorder, request)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.status, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Length"); got == "200" {
				t.Fatalf("stale catalog content length = %s", got)
			}
			if test.status == http.StatusOK && gjson.Get(recorder.Body.String(), "id").String() != test.model &&
				gjson.Get(recorder.Body.String(), "slug").String() != test.model {
				t.Fatalf("wrong model detail: %s", recorder.Body.String())
			}
			if len(host.completions) != previous+1 {
				t.Fatalf("completion count = %d, want %d", len(host.completions), previous+1)
			}
			completion := host.completions[previous]
			outcome := pluginapi.RequestCompletionSucceeded
			if test.status != http.StatusOK {
				outcome = pluginapi.RequestCompletionFailed
			}
			if completion.StatusCode != test.status || completion.Outcome != outcome {
				t.Fatalf("completion = %+v, want status=%d outcome=%s", completion, test.status, outcome)
			}
		})
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models/vendor/visible", nil)
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated detail status = %d", recorder.Code)
	}
}

func TestModelDetailFromHomeCatalogRespectsAPIKeyRules(t *testing.T) {
	catalog, errDecode := modelcatalog.DecodeHomeCatalog([]byte(`{
		"codex": [
			{"id": "test-home-visible", "context_length": 123456},
			{"id": "test-home-blocked", "context_length": 123456}
		]
	}`))
	if errDecode != nil {
		t.Fatalf("decode Home catalog: %v", errDecode)
	}

	clientID := "test-model-detail-home-only"
	registry.GetGlobalRegistry().RegisterClient(clientID, "openai", []*registry.ModelInfo{{ID: "test-local-only", Object: "model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientID) })

	server := newTestServer(t)
	updated := *server.configSnapshot()
	updated.APIKeyRules = map[string]config.APIKeyRule{
		"test-key": {BlockedModels: []string{"test-home-blocked"}},
	}
	server.UpdateClients(&updated)

	for _, test := range []struct {
		model  string
		status int
	}{
		{"test-home-visible", http.StatusOK},
		{"test-home-blocked", http.StatusNotFound},
		{"test-local-only", http.StatusNotFound},
	} {
		t.Run(test.model, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models/"+test.model+"?client_version=cpa", nil)
			ctx.Set("apiKey", "test-key")
			ctx.Set(handlers.ModelDetailIDContextKey, test.model)
			server.writeHomeCodexClientModels(ctx, catalog)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.status, recorder.Body.String())
			}
			if test.status == http.StatusOK && gjson.Get(recorder.Body.String(), "slug").String() != test.model {
				t.Fatalf("wrong Home model detail: %s", recorder.Body.String())
			}
		})
	}
}
