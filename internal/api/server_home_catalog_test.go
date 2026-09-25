package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
)

type homeCatalogInterceptorHost struct {
	calls        int
	sourceFormat string
}

func (*homeCatalogInterceptorHost) InterceptRequestBeforeAuth(
	context.Context,
	pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{}
}

func (*homeCatalogInterceptorHost) InterceptRequestAfterAuth(
	context.Context,
	pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{}
}

func (h *homeCatalogInterceptorHost) InterceptResponse(
	_ context.Context,
	request pluginapi.ResponseInterceptRequest,
) pluginapi.ResponseInterceptResponse {
	h.calls++
	h.sourceFormat = request.SourceFormat
	return pluginapi.ResponseInterceptResponse{
		Headers: http.Header{"X-Home-Catalog-Intercepted": {"true"}},
		Body:    request.Body,
	}
}

func (*homeCatalogInterceptorHost) InterceptStreamChunk(
	context.Context,
	pluginapi.StreamChunkInterceptRequest,
) pluginapi.StreamChunkInterceptResponse {
	return pluginapi.StreamChunkInterceptResponse{}
}

func TestWriteHomeCodexClientModelsUsesCatalogRoutesAndClientVersion(t *testing.T) {
	catalog, errDecode := modelcatalog.DecodeHomeCatalog([]byte(`{
		"codex": [{
			"id": "shared-home-model",
			"context_length": 123456,
			"native_capabilities": {"web_search": true}
		}],
		"gemini": [{
			"id": "shared-home-model",
			"context_length": 654321,
			"native_capabilities": {"web_search": true}
		}],
		"kimi": [{"id": "kimi-k3"}]
	}`))
	if errDecode != nil {
		t.Fatalf("DecodeHomeCatalog() error = %v", errDecode)
	}

	server := newTestServer(t)
	interceptor := &homeCatalogInterceptorHost{}
	server.handlers.SetPluginHost(interceptor)

	cpaResponse := writeHomeCatalogResponse(t, server, catalog, "cpa")
	if interceptor.calls != 1 || interceptor.sourceFormat != constant.OpenAI {
		t.Fatalf("interceptor calls/source = (%d, %q)", interceptor.calls, interceptor.sourceFormat)
	}
	shared := uniqueHomeCodexModel(t, cpaResponse, "shared-home-model")
	if contextWindow := jsonInt(shared["context_window"]); contextWindow != 123456 {
		t.Fatalf("shared context_window = %d, want 123456", contextWindow)
	}
	capabilities, ok := shared["cpa_capabilities"].(map[string]any)
	if !ok || capabilities["web_search"] != false {
		t.Fatalf("shared cpa_capabilities = %#v, want web_search=false", shared["cpa_capabilities"])
	}
	kimi := uniqueHomeCodexModel(t, cpaResponse, "kimi-k3")
	if contextWindow := jsonInt(kimi["context_window"]); contextWindow != 1048576 {
		t.Fatalf("Kimi context_window = %d, want 1048576", contextWindow)
	}
	if maxTokens := jsonInt(kimi["max_tokens"]); maxTokens != 65536 {
		t.Fatalf("Kimi max_tokens = %d, want 65536", maxTokens)
	}
	assertHomeCodexReasoningEfforts(t, kimi, []string{"low", "high", "max"})

	legacyResponse := writeHomeCatalogResponse(t, server, catalog, "0.143.9")
	if interceptor.calls != 2 {
		t.Fatalf("interceptor calls = %d, want 2", interceptor.calls)
	}
	legacyShared := uniqueHomeCodexModel(t, legacyResponse, "shared-home-model")
	if _, exposed := legacyShared["cpa_capabilities"]; exposed {
		t.Fatalf("legacy client exposed cpa_capabilities: %#v", legacyShared["cpa_capabilities"])
	}
	legacyKimi := uniqueHomeCodexModel(t, legacyResponse, "kimi-k3")
	assertHomeCodexReasoningEfforts(t, legacyKimi, []string{"low", "high"})
}

func writeHomeCatalogResponse(
	t *testing.T,
	server *Server,
	catalog *modelcatalog.HomeCatalog,
	clientVersion string,
) []map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version="+clientVersion, nil)
	server.writeHomeCodexClientModels(ctx, catalog)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Home-Catalog-Intercepted") != "true" {
		t.Fatalf("interceptor header = %q", recorder.Header().Get("X-Home-Catalog-Intercepted"))
	}
	var response struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response.Models
}

func uniqueHomeCodexModel(t *testing.T, models []map[string]any, slug string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, model := range models {
		if model["slug"] != slug {
			continue
		}
		if found != nil {
			t.Fatalf("model %q appeared more than once", slug)
		}
		found = model
	}
	if found == nil {
		t.Fatalf("model %q missing from response", slug)
	}
	return found
}

func assertHomeCodexReasoningEfforts(t *testing.T, model map[string]any, want []string) {
	t.Helper()
	raw, ok := model["supported_reasoning_levels"].([]any)
	if !ok || len(raw) != len(want) {
		t.Fatalf("supported_reasoning_levels = %#v, want %v", model["supported_reasoning_levels"], want)
	}
	for index, item := range raw {
		level, ok := item.(map[string]any)
		if !ok || level["effort"] != want[index] {
			t.Fatalf("reasoning level %d = %#v, want %q", index, item, want[index])
		}
	}
}

func jsonInt(value any) int {
	number, _ := value.(float64)
	return int(number)
}
