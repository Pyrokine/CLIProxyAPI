package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

type e8AlphaSearchRouter struct {
	target   string
	reject   bool
	requests []pluginapi.RequestInterceptRequest
	queries  []url.Values
}

func (*e8AlphaSearchRouter) HasRequestInterceptors() bool { return true }

func (r *e8AlphaSearchRouter) InterceptRequestBeforeAuth(
	ctx context.Context, request pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	captured := request
	captured.Headers = request.Headers.Clone()
	captured.Body = bytes.Clone(request.Body)
	r.requests = append(r.requests, captured)
	if c, ok := util.GinContextValue(ctx).(*gin.Context); ok && c != nil {
		r.queries = append(r.queries, c.Request.URL.Query())
	}
	if r.reject {
		return pluginapi.RequestInterceptResponse{
			Terminate: true, StatusCode: http.StatusServiceUnavailable,
			ResponseBody: []byte(`{"error":"unsupported plugin route target"}`),
		}
	}
	if r.target == "" {
		return pluginapi.RequestInterceptResponse{}
	}
	body, err := sjson.SetBytes(request.Body, "model", r.target)
	if err != nil {
		return pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: http.StatusInternalServerError}
	}
	return pluginapi.RequestInterceptResponse{Body: body}
}

func (*e8AlphaSearchRouter) InterceptRequestAfterAuth(
	_ context.Context, request pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{Headers: request.Headers, Body: request.Body}
}

func e8AlphaSearchEngine(server *Server, router codexAlphaSearchRequestRouter) *gin.Engine {
	engine := gin.New()
	for _, path := range []string{"/v1/alpha/search", "/backend-api/codex/alpha/search"} {
		engine.POST(path, func(c *gin.Context) { server.codexAlphaSearchWithRouter(c, router) })
	}
	return engine
}

func TestCodexAlphaSearchUsesPluginProviderTargetModel(t *testing.T) {
	server := newTestServer(t)
	executor := &c02SearchExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	router := &e8AlphaSearchRouter{target: "team-b/gpt-5.6-sol"}
	for _, credential := range []*auth.Auth{
		{
			ID: "codex-team-a", Provider: "codex", Prefix: "team-a", Status: auth.StatusActive,
			Metadata: map[string]any{"access_token": "token-a"},
		},
		{
			ID: "codex-team-b", Provider: "codex", Prefix: "team-b", Status: auth.StatusActive,
			Metadata: map[string]any{"access_token": "token-b"},
		},
	} {
		registerC02Credential(t, server, credential, credential.Prefix+"/gpt-5.6-sol")
	}
	engine := e8AlphaSearchEngine(server, router)
	payload := `{"id":"session-123","model":"gpt-5.6-sol","commands":{"search_query":[{"q":"golang"}]}}`
	paths := []string{"/v1/alpha/search?key=test-key", "/backend-api/codex/alpha/search?key=test-key"}
	for _, path := range paths {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
		request.Header.Set("Authorization", "Bearer test-key")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want %d; body=%s", path, response.Code, http.StatusOK, response.Body.String())
		}
	}
	if got := executor.selectedAuthIDs(); len(got) != 2 || got[0] != "codex-team-b" || got[1] != "codex-team-b" {
		t.Fatalf("selected auth IDs = %v, want [codex-team-b codex-team-b]", got)
	}
	_, body := executor.snapshot()
	if string(body) != payload {
		t.Fatalf("upstream body = %q, want original unprefixed body %q", body, payload)
	}
	if len(router.requests) != 2 || len(router.queries) != 2 {
		t.Fatalf("before-auth requests=%d queries=%d, want two of each", len(router.requests), len(router.queries))
	}
	for index, request := range router.requests {
		if request.SourceFormat != "codex-alpha-search" || request.RequestedModel != "gpt-5.6-sol" {
			t.Fatalf("route source/model changed: %+v", request)
		}
		if request.Headers.Get("Authorization") != "Bearer test-key" || router.queries[index].Get("key") != "test-key" {
			t.Fatal("route header or query changed")
		}
		if got := request.Metadata[coreexecutor.RequestPathMetadataKey]; got != strings.SplitN(
			paths[index], "?", 2,
		)[0] {
			t.Fatalf("route path = %#v", got)
		}
		if string(request.Body) != payload {
			t.Fatalf("route body = %q, want %q", request.Body, payload)
		}
	}
}

func TestCodexAlphaSearchFallsBackWhenPluginDoesNotHandleRoute(t *testing.T) {
	server := newTestServer(t)
	executor := &c02SearchExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	credential := &auth.Auth{
		ID: "codex-auth", Provider: "codex", Status: auth.StatusActive,
		Metadata: map[string]any{"access_token": "codex-token"},
	}
	registerC02Credential(t, server, credential, "gpt-5.6-sol")
	router := &e8AlphaSearchRouter{}
	engine := e8AlphaSearchEngine(server, router)
	payload := `{"model":"gpt-5.6-sol"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(payload))
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if got := executor.selectedAuthIDs(); len(got) != 1 || got[0] != credential.ID {
		t.Fatalf("selected auth IDs = %v, want [%s]", got, credential.ID)
	}
	_, body := executor.snapshot()
	if string(body) != payload || len(router.requests) != 1 {
		t.Fatalf("body=%q router requests=%d", body, len(router.requests))
	}
}

func TestCodexAlphaSearchRejectsUnsupportedPluginRouteTarget(t *testing.T) {
	server := newTestServer(t)
	executor := &c02SearchExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	registerC02Credential(
		t, server, &auth.Auth{
			ID: "codex-auth", Provider: "codex", Status: auth.StatusActive,
			Metadata: map[string]any{"access_token": "codex-token"},
		}, "gpt-5.6-sol",
	)
	engine := e8AlphaSearchEngine(server, &e8AlphaSearchRouter{reject: true})
	request := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"model":"gpt-5.6-sol"}`))
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if request, _ := executor.snapshot(); request != nil {
		t.Fatal("unsupported plugin route sent an upstream request")
	}
}

func TestRestoreCodexAlphaSearchModelKeepsIndependentRoutedFields(t *testing.T) {
	for _, test := range []struct {
		name, original, routed, model, query string
		cache                                bool
	}{
		{
			name: "model-only", original: `{ "id":"session", "model":"original", "n":9007199254740993 }`,
			routed: `{ "id":"session", "model":"team-b/original", "n":9007199254740993 }`, model: `"original"`,
		},
		{
			name: "independent edit", original: `{"model":"original","query":"old","n":9007199254740993}`,
			routed: `{"model":"team-b/original","query":"new","n":9007199254740993}`, model: `"original"`, query: "new",
		},
		{
			name: "missing original model", original: `{ "query":"original", "n":9007199254740993 }`,
			routed: `{ "model":"team-b/original", "query":"original", "n":9007199254740993 }`, query: "original",
		},
		{
			name: "explicit null", original: `{"model":null,"n":9007199254740993}`,
			routed: `{"model":"team-b/original","n":9007199254740993}`, model: `null`,
		},
		{
			name: "removed cache controls", original: `{"model":"original","query":"original",` +
				`"prompt_cache_key":"private-cache","prompt_cache_retention":"24h","n":9007199254740993}`,
			routed: `{"model":"team-b/original","query":"original","n":9007199254740993}`,
			model:  `"original"`, query: "original", cache: true,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				original, routed := []byte(test.original), []byte(test.routed)
				before := bytes.Clone(routed)
				result := restoreCodexAlphaSearchModel(routed, original)
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(result, &fields); err != nil {
					t.Fatal(err)
				}
				if string(fields["model"]) != test.model || string(fields["n"]) != "9007199254740993" {
					t.Fatalf("model or unrelated integer changed: %s", result)
				}
				if test.query != "" && string(fields["query"]) != `"`+test.query+`"` {
					t.Fatalf("independent routed query changed: %s", result)
				}
				if test.name == "model-only" && !bytes.Equal(result, original) {
					t.Fatalf("original body bytes changed: %s", result)
				}
				if test.cache && (fields["prompt_cache_key"] != nil || fields["prompt_cache_retention"] != nil) {
					t.Fatalf("original cache controls were restored: %s", result)
				}
				if !bytes.Equal(routed, before) || string(original) != test.original {
					t.Fatal("caller request bytes changed")
				}
			},
		)
	}
}

func TestCodexAlphaSearchForwardsRequest(t *testing.T) {
	server := newTestServer(t)
	executor := &c02SearchExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	credential := &auth.Auth{
		ID: "codex-auth", Provider: "codex", Status: auth.StatusActive,
		Metadata: map[string]any{"access_token": "codex-token", "account_id": "account-123"},
	}
	registerC02Credential(t, server, credential)
	request := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"query":"GPT-5.6"}`))
	request.Header.Set("Authorization", "Bearer test-key")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Session_id", "session-123")
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	upstream, body := executor.snapshot()
	if upstream == nil {
		t.Fatal("Codex executor did not receive a request")
	}
	if upstream.URL.String() != "https://chatgpt.com/backend-api/codex/alpha/search" ||
		string(body) != `{"query":"GPT-5.6"}` {
		t.Fatalf("upstream URL=%s body=%s", upstream.URL, body)
	}
	for header, want := range map[string]string{
		"Authorization": "Bearer codex-token", "Chatgpt-Account-Id": "account-123", "Session_id": "session-123",
	} {
		if got := upstream.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response Content-Type = %q", response.Header().Get("Content-Type"))
	}
	traceID := response.Header().Get(internallogging.CPATraceIDHeader)
	parts := strings.Split(traceID, "-")
	if len(parts) != 3 || parts[1] != credential.Index || len(parts[2]) != 32 {
		t.Fatalf("trace ID = %q, want timestamp-%s-requestID", traceID, credential.Index)
	}
	if _, err := time.Parse("20060102150405", parts[0]); err != nil {
		t.Fatalf("trace timestamp = %q: %v", parts[0], err)
	}
}
