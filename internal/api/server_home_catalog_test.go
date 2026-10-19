package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	claudemodels "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/claude/models"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
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
	if !ok || capabilities["web_search"] != true {
		t.Fatalf("shared cpa_capabilities = %#v, want web_search=true", shared["cpa_capabilities"])
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

func TestWriteHomeCodexClientModelsReasoningUpdatesIgnoreLocalRoutes(t *testing.T) {
	catalog, err := modelcatalog.DecodeHomeCatalog([]byte(`{
		"codex": [{"id":"gpt-6-astra","support_configuration_update":true}, {"id":"gpt-6-sol","support_configuration_update":true}, {"id":"gpt-6-luna","support_configuration_update":true}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t)
	server.handlers.SetPluginHost(&homeCatalogInterceptorHost{})
	reg := registry.GetGlobalRegistry()
	clientID := "home-reasoning-unrelated-local-route"
	reg.RegisterClient(clientID, "openai", []*registry.ModelInfo{{ID: "gpt-6-astra"}})
	t.Cleanup(func() { reg.UnregisterClient(clientID) })
	for _, version := range []string{"cpa", "0.162.0"} {
		models := writeHomeCatalogResponse(t, server, catalog, version)
		for _, id := range []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"} {
			model := uniqueHomeCodexModel(t, models, id)
			if got := model["supports_reasoning_effort_updates"]; got != true {
				t.Fatalf("Home %s reasoning updates = %v, want true", id, got)
			}
		}
	}
}

func TestWriteHomeCodexClientModelsReasoningUpdatesFollowHomeRoutes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		catalog string
		id      string
		want    bool
	}{
		{name: "Astra supported", catalog: `{"codex":[{"id":"gpt-6-astra","support_configuration_update":true}]}`, id: "gpt-6-astra", want: true},
		{name: "Astra unsupported", catalog: `{"codex":[{"id":"gpt-6-astra","support_configuration_update":false}]}`, id: "gpt-6-astra"},
		{name: "unknown support", catalog: `{"codex":[{"id":"gpt-6-astra"}]}`, id: "gpt-6-astra"},
		{name: "malformed support", catalog: `{"codex":[{"id":"gpt-6-astra","support_configuration_update":"true"}]}`, id: "gpt-6-astra"},
		{name: "Sol unsupported", catalog: `{"codex":[{"id":"gpt-6-sol","support_configuration_update":false}]}`, id: "gpt-6-sol"},
		{name: "Luna unsupported", catalog: `{"codex":[{"id":"gpt-6-luna","support_configuration_update":false}]}`, id: "gpt-6-luna"},
		{name: "all routes supported", catalog: `{"codex":[{"id":"gpt-6-astra","support_configuration_update":true}],"codex-plus":[{"id":"gpt-6-astra","support_configuration_update":true}]}`, id: "gpt-6-astra", want: true},
		{name: "unsupported first", catalog: `{"codex":[{"id":"gpt-6-astra","support_configuration_update":false},{"id":"gpt-6-astra","support_configuration_update":true}]}`, id: "gpt-6-astra"},
		{name: "unsupported last", catalog: `{"codex":[{"id":"gpt-6-astra","support_configuration_update":true},{"id":"gpt-6-astra","support_configuration_update":false}]}`, id: "gpt-6-astra"},
		{name: "one unknown route", catalog: `{"codex":[{"id":"gpt-6-astra","support_configuration_update":true}],"codex-plus":[{"id":"gpt-6-astra"}]}`, id: "gpt-6-astra"},
		{name: "mixed providers", catalog: `{"codex":[{"id":"gpt-6-astra","support_configuration_update":true}],"xai":[{"id":"gpt-6-astra","support_configuration_update":true}]}`, id: "gpt-6-astra"},
		{name: "prefixed exact ID", catalog: `{"codex":[{"id":"team/gpt-6-astra","support_configuration_update":true}]}`, id: "team/gpt-6-astra", want: true},
		{name: "alias exact ID", catalog: `{"codex":[{"id":"home-reasoning-alias","catalog_name":"gpt-6-astra","support_configuration_update":true}]}`, id: "home-reasoning-alias", want: true},
		{name: "different ID does not veto", catalog: `{"codex":[{"id":"gpt-6-astra","support_configuration_update":true}],"openai":[{"id":"other-model","support_configuration_update":false}]}`, id: "gpt-6-astra", want: true},
	} {
		for _, version := range []string{"cpa", "0.162.0"} {
			t.Run(tc.name+"/"+version, func(t *testing.T) {
				catalog, err := modelcatalog.DecodeHomeCatalog([]byte(tc.catalog))
				if err != nil {
					t.Fatal(err)
				}
				server := newTestServer(t)
				server.handlers.SetPluginHost(&homeCatalogInterceptorHost{})
				reg := registry.GetGlobalRegistry()
				clientID := "home-unrelated-supported-local-route"
				reg.RegisterClient(clientID, "codex", []*registry.ModelInfo{{ID: tc.id, SupportConfigurationUpdate: true}})
				t.Cleanup(func() { reg.UnregisterClient(clientID) })
				model := uniqueHomeCodexModel(t, writeHomeCatalogResponse(t, server, catalog, version), tc.id)
				if got := model["supports_reasoning_effort_updates"]; got != tc.want {
					t.Fatalf("Home %s reasoning updates = %v, want %t", tc.id, got, tc.want)
				}
			})
		}
	}
}

func TestWriteHomeCodexClientModelsLabelsDevinAndUsesCompactJSON(t *testing.T) {
	catalog, errDecode := modelcatalog.DecodeHomeCatalog([]byte(`{
		"codex": [{"id":"test-home-devin-alias", "display_name":"Alias <2>"}],
		"devin": [{"id":"test-home-devin-alias", "display_name":"Alias <2>"}],
		"openai": [{"id":"test-home-regular", "display_name":"Regular"}]
	}`))
	if errDecode != nil {
		t.Fatalf("DecodeHomeCatalog() error = %v", errDecode)
	}
	server := newTestServer(t)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.156.0", nil)
	server.writeHomeCodexClientModels(ctx, catalog)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `Alias <2> (Devin)`) ||
		strings.Contains(recorder.Body.String(), "u003c") || strings.HasSuffix(recorder.Body.String(), "\n") {
		t.Fatalf("Home Codex catalog status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := uniqueHomeCodexModel(t, response.Models, "test-home-regular")["display_name"]; got != "Regular" {
		t.Fatalf("regular display_name = %q, want Regular", got)
	}
}

type homePatchExecutor struct {
	coreauth.ProviderExecutor
	provider  string
	supported bool
}

func (e *homePatchExecutor) Identifier() string       { return e.provider }
func (e *homePatchExecutor) SupportsApplyPatch() bool { return e.supported }

func TestHomeCodexApplyPatchUsesCatalogRoutesAndCurrentConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		catalog string
		support map[string]bool
		want    bool
	}{
		{name: "supported", catalog: `{"fixture-provider":[{"id":"gpt-6-sol"}]}`, support: map[string]bool{"fixture-provider": true}, want: true},
		{name: "unsupported", catalog: `{"codex":[{"id":"gpt-6-sol"}]}`, support: map[string]bool{"codex": false}},
		{name: "missing", catalog: `{"codex":[{"id":"gpt-6-sol"}]}`},
		{name: "mixed supported", catalog: `{"codex":[{"id":"gpt-6-sol"}],"xai":[{"id":"gpt-6-sol"}]}`, support: map[string]bool{"codex": true, "xai": true}, want: true},
		{name: "mixed unsupported", catalog: `{"codex":[{"id":"gpt-6-sol"}],"xai":[{"id":"gpt-6-sol"}]}`, support: map[string]bool{"codex": true, "xai": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := modelcatalog.DecodeHomeCatalog([]byte(tc.catalog))
			if err != nil {
				t.Fatal(err)
			}
			manager := coreauth.NewManager(nil, nil, nil)
			for provider, supported := range tc.support {
				manager.RegisterExecutor(&homePatchExecutor{provider: provider, supported: supported})
			}
			server := &Server{handlers: handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager)}
			server.handlers.SetPluginHost(&homeCatalogInterceptorHost{})
			for _, enabled := range []bool{false, true, false} {
				cfg := &config.Config{}
				cfg.Client.Codex.EnableApplyPatch = enabled
				server.setConfig(cfg)
				server.handlers.UpdateClients(effectiveSDKConfig(cfg))
				entry := uniqueHomeCodexModel(t, writeHomeCatalogResponse(t, server, catalog, "cpa"), "gpt-6-sol")
				got, exists := entry["apply_patch_tool_type"]
				if !exists || enabled && tc.want && got != "freeform" || (!enabled || !tc.want) && got != nil {
					t.Fatalf("enabled=%t support=%t capability=%#v exists=%t", enabled, tc.want, got, exists)
				}
			}
		})
	}
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

func TestHomeModelRoutesPreserveCredentialErrors(t *testing.T) {
	for _, test := range []struct {
		name, raw, message string
		status             int
	}{
		{name: "no credentials", raw: `{"error":{"type":"no_credentials","message":"Missing API key"}}`, message: "Missing API key", status: http.StatusUnauthorized},
		{name: "invalid credential", raw: `{"error":{"type":"invalid_credential","message":"Invalid API key"}}`, message: "Invalid API key", status: http.StatusUnauthorized},
		{name: "internal error", raw: `{"error":{"type":"internal_error","message":"boom"}}`, message: "home models request failed", status: http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newHomeCatalogTestClient(t, test.raw)
			server := newTestServerWithOptions(t, WithHomeClientProvider(func() *home.Client { return client }))
			server.cfg.Home.Enabled = true
			var registered gin.HandlerFunc
			for _, route := range server.engine.Routes() {
				if route.Method == http.MethodGet && route.Path == "/v1/models" {
					registered = route.HandlerFunc
				}
			}
			if registered == nil {
				t.Fatal("model-list route is not registered")
			}
			for _, uri := range []string{"/v1/models", "/v1/models?client_version=cpa"} {
				t.Run(uri, func(t *testing.T) {
					recorder := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(recorder)
					ctx.Request = httptest.NewRequest(http.MethodGet, uri, nil)
					ctx.Request.Header.Set("Authorization", "Bearer test-key")
					ctx.Set("apiKey", "test-key")
					registered(ctx)
					var response handlers.ErrorResponse
					if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if recorder.Code != test.status || response.Error.Message != test.message {
						t.Fatalf("Home model error status=%d body=%s", recorder.Code, recorder.Body.String())
					}
				})
			}
		})
	}
}

func newHomeCatalogTestClient(t *testing.T, payload string) *home.Client {
	t.Helper()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen: %v", errListen)
	}
	done := make(chan struct{})
	var connections sync.WaitGroup
	go func() {
		defer close(done)
		for {
			conn, errAccept := listener.Accept()
			if errAccept != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer func() {
					if errClose := conn.Close(); errClose != nil {
						t.Errorf("close Home test connection: %v", errClose)
					}
				}()
				reader := bufio.NewReader(conn)
				for {
					args, errReadRESPArrayOfBulkStrings := readRESPArrayOfBulkStrings(reader)
					if errReadRESPArrayOfBulkStrings != nil || len(args) == 0 {
						return
					}
					response := "+OK\r\n"
					switch strings.ToUpper(string(args[0])) {
					case "HELLO":
						response = "-ERR unknown command 'hello'\r\n"
					case "GET":
						var request struct {
							Type string `json:"type"`
						}
						if len(args) != 2 {
							t.Error("invalid Home GET request")
							return
						}
						if errUnmarshal := json.Unmarshal(args[1], &request); errUnmarshal != nil || request.Type != "models" {
							t.Errorf("unexpected Home request %q: %v", args[1], errUnmarshal)
							return
						}
						response = fmt.Sprintf("$%d\r\n%s\r\n", len(payload), payload)
					}
					if _, errWriteString := io.WriteString(conn, response); errWriteString != nil {
						return
					}
				}
			}()
		}
	}()
	host, portText, errSplitHostPort := net.SplitHostPort(listener.Addr().String())
	if errSplitHostPort != nil {
		t.Fatalf("split Home test address: %v", errSplitHostPort)
	}
	port, errAtoi := strconv.Atoi(portText)
	if errAtoi != nil {
		t.Fatalf("parse Home test port: %v", errAtoi)
	}
	client := home.New(config.HomeConfig{Enabled: true, Host: host, Port: port, DisableClusterDiscovery: true})
	t.Cleanup(func() {
		client.Close()
		if errClose := listener.Close(); errClose != nil {
			t.Errorf("close Home test listener: %v", errClose)
		}
		<-done
		connections.Wait()
	})
	return client
}

func TestHomeCodexModelsNormalizeMaxContextAliases(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fields    string
		wantMax   int
		wantLimit int
	}{
		{name: "camelCase", fields: `,"maxContextLength":524288`, wantMax: 524288, wantLimit: 524288},
		{name: "snake_case", fields: `,"max_context_length":655360`, wantMax: 655360, wantLimit: 655360},
		{name: "snake wins", fields: `,"max_context_length":786432,"maxContextLength":524288`, wantMax: 786432, wantLimit: 786432},
		{name: "ordinary context", wantLimit: 272000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(fmt.Sprintf(`{"codex":[{"id":"home-max-alias-fixture","context_length":272000%s}]}`, tc.fields))
			catalog, err := modelcatalog.DecodeHomeCatalog(payload)
			if err != nil {
				t.Fatal(err)
			}
			if models := catalog.Models(); len(models) != 1 || modelMapInt(models[0], "max_context_length") != tc.wantMax {
				t.Fatalf("decoded max_context_length = %#v, want %d", models, tc.wantMax)
			}
			server := newTestServer(t)
			server.handlers.SetPluginHost(&homeCatalogInterceptorHost{})
			for _, version := range []string{"cpa", "0.143.9"} {
				model := uniqueHomeCodexModel(t, writeHomeCatalogResponse(t, server, catalog, version), "home-max-alias-fixture")
				if got := jsonInt(model["context_window"]); got != tc.wantLimit {
					t.Fatalf("client=%s context_window=%d, want %d", version, got, tc.wantLimit)
				}
			}
		})
	}
}

func TestHomeClaudeModelMetadataThroughRegisteredRoute(t *testing.T) {
	for _, test := range []struct {
		name, payload, id, display, created string
		input, output                       int64
	}{
		{name: "provided metadata", payload: `{"claude":[{"id":"claude-sonnet-4-6","created":1771372800,"owned_by":"anthropic","display_name":"Claude 4.6 Sonnet","context_length":200000,"max_completion_tokens":64000}]}`, id: "claude-sonnet-4-6", display: "Claude 4.6 Sonnet", created: "2026-02-18T00:00:00Z", input: 200000, output: 64000},
		{name: "missing metadata", payload: `{"claude":[{"id":"claude-no-limits"}]}`, id: "claude-no-limits", display: "claude-no-limits", input: 200000, output: 64000},
		{name: "custom model", payload: `{"claude":[{"id":"gpt-4o","display_name":"GPT-4o"}]}`, id: "gpt-4o", display: "GPT-4o", input: 200000, output: 64000},
		{name: "custom limits", payload: `{"claude":[{"id":"claude-home-custom-limits","context_length":131072,"max_completion_tokens":32768}]}`, id: "claude-home-custom-limits", display: "claude-home-custom-limits", input: 131072, output: 32768},
	} {
		for _, disabled := range []bool{false, true} {
			mode := "cloaked"
			if disabled {
				mode = "uncloaked"
			}
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				client := newHomeCatalogTestClient(t, test.payload)
				server := newTestServerWithOptions(t, WithHomeClientProvider(func() *home.Client { return client }))
				cfg := *server.configSnapshot()
				cfg.Home.Enabled = true
				cfg.ClaudeCode.DisableCloakingModelList = disabled
				server.UpdateClients(&cfg)
				var registered gin.HandlerFunc
				for _, route := range server.engine.Routes() {
					if route.Method == http.MethodGet && route.Path == "/v1/models" {
						registered = route.HandlerFunc
					}
				}
				if registered == nil {
					t.Fatal("model-list route is not registered")
				}
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
				ctx.Request.Header.Set("Authorization", "Bearer test-key")
				ctx.Request.Header.Set("User-Agent", "claude-cli/test")
				ctx.Set("apiKey", "test-key")
				registered(ctx)
				body := recorder.Body.String()
				if recorder.Code != http.StatusOK || len(gjson.Get(body, "data").Array()) != 1 {
					t.Fatalf("catalog status=%d body=%s", recorder.Code, body)
				}
				model := gjson.Get(body, "data.0")
				wantID := test.id
				if !disabled {
					wantID = claudemodels.ClientModelID(test.id)
				}
				if model.Get("id").String() != wantID || model.Get("type").String() != "model" || model.Get("display_name").String() != test.display {
					t.Errorf("model identity/schema=%s, want id=%s type=model display=%s", model.Raw, wantID, test.display)
				}
				if got := model.Get("created_at"); got.String() != test.created || test.created == "" && got.Exists() {
					t.Errorf("created_at=%s, want %q", got.Raw, test.created)
				}
				if model.Get("max_input_tokens").Int() != test.input || model.Get("max_tokens").Int() != test.output {
					t.Errorf("token metadata=%s, want %d/%d", model.Raw, test.input, test.output)
				}
			})
		}
	}
}
