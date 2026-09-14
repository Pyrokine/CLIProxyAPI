package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	proxyconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
)

type c02SearchExecutor struct {
	mu      sync.Mutex
	request *http.Request
	body    []byte
	authIDs []string
}

func (*c02SearchExecutor) Identifier() string { return "codex" }

func (*c02SearchExecutor) Execute(
	context.Context,
	*auth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (*c02SearchExecutor) ExecuteStream(
	context.Context,
	*auth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	return nil, nil
}

func (*c02SearchExecutor) Refresh(_ context.Context, current *auth.Auth) (*auth.Auth, error) {
	return current, nil
}

func (*c02SearchExecutor) CountTokens(
	context.Context,
	*auth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (*c02SearchExecutor) PrepareRequest(request *http.Request, selected *auth.Auth) error {
	if selected == nil {
		return nil
	}
	token, _ := selected.Metadata["access_token"].(string)
	if strings.TrimSpace(token) == "" && selected.Attributes != nil {
		token = selected.Attributes[auth.AttributeAPIKey]
	}
	if strings.TrimSpace(token) != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

func (e *c02SearchExecutor) HttpRequest(
	_ context.Context,
	selected *auth.Auth,
	request *http.Request,
) (*http.Response, error) {
	body, errRead := io.ReadAll(request.Body)
	if errRead != nil {
		return nil, errRead
	}
	e.mu.Lock()
	e.request = request.Clone(request.Context())
	e.body = append([]byte(nil), body...)
	if selected != nil {
		e.authIDs = append(e.authIDs, selected.ID)
	}
	e.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}, nil
}

func (e *c02SearchExecutor) snapshot() (*http.Request, []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.request, append([]byte(nil), e.body...)
}

func (e *c02SearchExecutor) selectedAuthIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.authIDs...)
}

type c02SelectionObserver struct {
	mu        sync.Mutex
	sawGin    bool
	sessionID string
}

func (s *c02SelectionObserver) Pick(
	ctx context.Context,
	_, _ string,
	opts coreexecutor.Options,
	candidates []*auth.Auth,
) (*auth.Auth, error) {
	s.mu.Lock()
	s.sawGin = util.GinContextValue(ctx) != nil
	s.sessionID = opts.Headers.Get("X-Session-ID")
	s.mu.Unlock()
	if len(candidates) == 0 {
		return nil, nil
	}
	return candidates[0], nil
}

func (s *c02SelectionObserver) snapshot() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sawGin, s.sessionID
}

type c02RequestRouter struct {
	request pluginapi.RequestInterceptRequest
}

func (*c02RequestRouter) HasRequestInterceptors() bool { return true }

func (r *c02RequestRouter) InterceptRequestBeforeAuth(
	_ context.Context,
	request pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	r.request = request
	headers := request.Headers.Clone()
	headers.Set("X-C02-Routed", "true")
	return pluginapi.RequestInterceptResponse{
		Headers: headers,
		Body:    []byte(`{"id":"routed-session","model":"routed-model","query":"go"}`),
	}
}

func (*c02RequestRouter) InterceptRequestAfterAuth(
	_ context.Context,
	request pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{Headers: request.Headers, Body: request.Body}
}

type c02LifecycleRouter struct {
	mu           sync.Mutex
	afterRequest pluginapi.RequestInterceptRequest
}

func (*c02LifecycleRouter) HasRequestInterceptors() bool { return true }

func (*c02LifecycleRouter) InterceptRequestBeforeAuth(
	_ context.Context,
	request pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	headers := request.Headers.Clone()
	headers.Del("X-Client-Request-Id")
	headers.Set("X-C02-Before", "before")
	return pluginapi.RequestInterceptResponse{
		Headers: headers,
		Body:    []byte(`{"id":"session-c02","model":"team-b/gpt-5.6-sol","query":"go"}`),
	}
}

func (r *c02LifecycleRouter) InterceptRequestAfterAuth(
	_ context.Context,
	request pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	r.mu.Lock()
	r.afterRequest = request
	r.afterRequest.Headers = request.Headers.Clone()
	r.afterRequest.Body = append([]byte(nil), request.Body...)
	r.mu.Unlock()
	headers := request.Headers.Clone()
	headers.Set("X-C02-After", "after")
	return pluginapi.RequestInterceptResponse{Headers: headers, Body: request.Body}
}

func (r *c02LifecycleRouter) snapshot() pluginapi.RequestInterceptRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	request := r.afterRequest
	request.Headers = request.Headers.Clone()
	request.Body = append([]byte(nil), request.Body...)
	return request
}

type c02RequestLogger struct {
	mu          sync.Mutex
	apiRequest  []byte
	apiResponse []byte
}

func (l *c02RequestLogger) LogRequest(
	_, _ string,
	_ map[string][]string,
	_ []byte,
	_ int,
	_ map[string][]string,
	_, _, apiRequest, apiResponse, _ []byte,
	_ []*interfaces.ErrorMessage,
	_ string,
	_, _ time.Time,
) error {
	l.mu.Lock()
	l.apiRequest = append([]byte(nil), apiRequest...)
	l.apiResponse = append([]byte(nil), apiResponse...)
	l.mu.Unlock()
	return nil
}

func (*c02RequestLogger) LogStreamingRequest(
	_, _ string,
	_ map[string][]string,
	_ []byte,
	_ string,
) (internallogging.StreamingLogWriter, error) {
	return nil, nil
}

func (*c02RequestLogger) IsEnabled() bool { return true }

func (l *c02RequestLogger) snapshot() ([]byte, []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.apiRequest...), append([]byte(nil), l.apiResponse...)
}

func registerC02Credential(t *testing.T, server *Server, credential *auth.Auth, models ...string) {
	t.Helper()
	if _, errRegister := server.handlers.AuthManager.Register(context.Background(), credential); errRegister != nil {
		t.Fatalf("register credential: %v", errRegister)
	}
	if len(models) == 0 {
		return
	}
	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model})
	}
	registry.GetGlobalRegistry().RegisterClient(credential.ID, credential.Provider, infos)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.ID) })
}

func TestCodexAlphaSearchStatusMapsContextErrors(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		fallback int
		want     int
	}{
		{
			name: "canceled", err: context.Canceled,
			fallback: http.StatusBadGateway, want: clienterror.StatusClientClosedRequest,
		},
		{
			name: "deadline", err: context.DeadlineExceeded,
			fallback: http.StatusBadGateway, want: http.StatusGatewayTimeout,
		},
		{
			name: "explicit status", err: &auth.Error{HTTPStatus: http.StatusTooManyRequests},
			fallback: http.StatusServiceUnavailable, want: http.StatusTooManyRequests,
		},
		{
			name: "fallback", err: errors.New("boom"),
			fallback: http.StatusBadRequest, want: http.StatusBadRequest,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				if got := codexAlphaSearchStatus(test.err, test.fallback); got != test.want {
					t.Fatalf("codexAlphaSearchStatus() = %d, want %d", got, test.want)
				}
			},
		)
	}
}

func TestCodexAlphaSearchRoutesPropagateGinContextAndSession(t *testing.T) {
	for _, path := range []string{"/v1/alpha/search", "/backend-api/codex/alpha/search"} {
		t.Run(
			path, func(t *testing.T) {
				server := newTestServer(t)
				executor := &c02SearchExecutor{}
				observer := &c02SelectionObserver{}
				server.handlers.AuthManager.RegisterExecutor(executor)
				server.handlers.AuthManager.SetSelector(observer)
				registerC02Credential(
					t, server, &auth.Auth{
						ID:       "codex-oauth",
						Provider: "codex",
						Status:   auth.StatusActive,
						Metadata: map[string]any{"access_token": "oauth-token"},
					},
				)

				request := httptest.NewRequest(
					http.MethodPost,
					path,
					strings.NewReader(`{"id":"session-c02","query":"golang"}`),
				)
				request.Header.Set("Authorization", "Bearer test-key")
				response := httptest.NewRecorder()
				server.engine.ServeHTTP(response, request)

				if response.Code != http.StatusOK {
					t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
				}
				traceParts := strings.Split(response.Header().Get(internallogging.CPATraceIDHeader), "-")
				if len(traceParts) != 3 || traceParts[1] == "" || len(traceParts[2]) != 32 {
					t.Fatalf(
						"trace ID = %q, want timestamp-authIndex-requestID",
						response.Header().Get(internallogging.CPATraceIDHeader),
					)
				}
				if _, errParse := time.Parse("20060102150405", traceParts[0]); errParse != nil {
					t.Fatalf("trace timestamp = %q: %v", traceParts[0], errParse)
				}
				sawGin, sessionID := observer.snapshot()
				if !sawGin {
					t.Fatal("selector did not receive Gin context")
				}
				if sessionID != "session-c02" {
					t.Fatalf("selection session = %q, want session-c02", sessionID)
				}
				upstreamRequest, _ := executor.snapshot()
				if upstreamRequest == nil || upstreamRequest.URL.String() != codexAlphaSearchOAuthURL {
					t.Fatalf("upstream request = %#v, want OAuth Alpha Search URL", upstreamRequest)
				}
			},
		)
	}
}

func TestCodexAlphaSearchUsesRequestIDForSessionAffinity(t *testing.T) {
	server := newTestServer(t)
	executor := &c02SearchExecutor{}
	selector := auth.NewSessionAffinitySelector(&auth.RoundRobinSelector{})
	t.Cleanup(selector.Stop)
	server.handlers.AuthManager.RegisterExecutor(executor)
	server.handlers.AuthManager.SetSelector(selector)
	for _, credentialID := range []string{"codex-oauth-a", "codex-oauth-b"} {
		registerC02Credential(
			t, server, &auth.Auth{
				ID:       credentialID,
				Provider: "codex",
				Status:   auth.StatusActive,
				Metadata: map[string]any{"access_token": credentialID},
			},
		)
	}

	for _, sessionID := range []string{"session-a", "session-b", "session-a"} {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/alpha/search",
			strings.NewReader(`{"id":"`+sessionID+`","query":"golang"}`),
		)
		request.Header.Set("Authorization", "Bearer test-key")
		response := httptest.NewRecorder()
		server.engine.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf(
				"session %s status = %d, want %d; body=%s", sessionID, response.Code, http.StatusOK,
				response.Body.String(),
			)
		}
	}

	selected := executor.selectedAuthIDs()
	if len(selected) != 3 {
		t.Fatalf("selected auth IDs = %v, want three selections", selected)
	}
	if selected[0] == selected[1] {
		t.Fatalf("different sessions selected the same auth %q", selected[0])
	}
	if selected[2] != selected[0] {
		t.Fatalf("session affinity selected %q, want %q", selected[2], selected[0])
	}
}

func TestRouteCodexAlphaSearchRequestUsesExistingInterceptorChain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	contextRecorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(contextRecorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(""))
	router := &c02RequestRouter{}

	body, headers, routing := routeCodexAlphaSearchRequest(
		router,
		context.Background(),
		ginContext,
		[]byte(`{"id":"original-session","model":"original-model"}`),
		codexAlphaSearchRouting{ID: "original-session", Model: "original-model"},
	)

	if routing.ID != "routed-session" || routing.Model != "routed-model" {
		t.Fatalf("routing = %#v, want routed values", routing)
	}
	if headers.Get("X-C02-Routed") != "true" {
		t.Fatalf("routed header = %q, want true", headers.Get("X-C02-Routed"))
	}
	if !strings.Contains(string(body), `"query":"go"`) {
		t.Fatalf("routed body = %s", body)
	}
	if router.request.SourceFormat != codexAlphaSearchSourceFormat {
		t.Fatalf("source format = %q, want %q", router.request.SourceFormat, codexAlphaSearchSourceFormat)
	}
}

func TestCodexAlphaSearchSeparatesSelectionModelAndAppliesInterceptorLifecycle(t *testing.T) {
	server := newTestServer(t)
	executor := &c02SearchExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	for _, credential := range []*auth.Auth{
		{
			ID:       "codex-team-a",
			Provider: "codex",
			Prefix:   "team-a",
			Status:   auth.StatusActive,
			Metadata: map[string]any{"access_token": "token-a"},
		},
		{
			ID:       "codex-team-b",
			Provider: "codex",
			Prefix:   "team-b",
			Status:   auth.StatusActive,
			Metadata: map[string]any{"access_token": "token-b"},
		},
	} {
		registerC02Credential(t, server, credential, credential.Prefix+"/gpt-5.6-sol")
	}
	router := &c02LifecycleRouter{}
	engine := gin.New()
	engine.POST(
		"/v1/alpha/search", func(c *gin.Context) {
			server.codexAlphaSearchWithRouter(c, router)
		},
	)

	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/alpha/search",
		strings.NewReader(`{"id":"session-c02","model":"gpt-5.6-sol","query":"golang"}`),
	)
	request.Header.Set("Authorization", "Bearer test-key")
	request.Header.Set("X-Client-Request-Id", "client-request")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	selected := executor.selectedAuthIDs()
	if len(selected) != 1 || selected[0] != "codex-team-b" {
		t.Fatalf("selected auth IDs = %v, want [codex-team-b]", selected)
	}
	upstreamRequest, upstreamBody := executor.snapshot()
	if upstreamRequest == nil {
		t.Fatal("upstream request is nil")
	}
	if got := upstreamRequest.Header.Get("X-C02-Before"); got != "before" {
		t.Fatalf("X-C02-Before = %q, want before", got)
	}
	if got := upstreamRequest.Header.Get("X-C02-After"); got != "after" {
		t.Fatalf("X-C02-After = %q, want after", got)
	}
	if got := upstreamRequest.Header.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("X-Client-Request-Id = %q, want cleared", got)
	}
	if !strings.Contains(string(upstreamBody), `"model":"gpt-5.6-sol"`) ||
		strings.Contains(string(upstreamBody), `"model":"team-b/gpt-5.6-sol"`) {
		t.Fatalf("upstream body = %s, want original unprefixed model", upstreamBody)
	}
	afterRequest := router.snapshot()
	if afterRequest.Model != "gpt-5.6-sol" || afterRequest.RequestedModel != "gpt-5.6-sol" {
		t.Fatalf("after-auth models = model %q requested %q", afterRequest.Model, afterRequest.RequestedModel)
	}
	if !strings.Contains(string(afterRequest.Body), `"model":"gpt-5.6-sol"`) {
		t.Fatalf("after-auth body = %s, want original unprefixed model", afterRequest.Body)
	}
}

func TestCodexAlphaSearchAPIKeyEndpointModelRewriteAndLogging(t *testing.T) {
	requestLogger := &c02RequestLogger{}
	requestLoggerFactory := func(*proxyconfig.Config, string) internallogging.RequestLogger {
		return requestLogger
	}
	server := newTestServerWithOptions(t, WithRequestLoggerFactory(requestLoggerFactory))
	cfg, errClone := proxyconfig.Clone(server.configSnapshot())
	if errClone != nil {
		t.Fatalf("clone config: %v", errClone)
	}
	cfg.RequestLog = true
	cfg.CodexKey = []proxyconfig.CodexKey{
		{
			APIKey:      "codex-alpha-secret",
			Prefix:      "vendor",
			BaseURL:     "https://codex.example.com/v1",
			AlphaSearch: true,
			Models: []proxyconfig.CodexModel{
				{
					Name:  "gpt-5.6-sol",
					Alias: "sol-alias",
				},
			},
		},
	}
	server.setConfig(cfg)
	server.handlers.AuthManager.SetConfig(cfg)
	executor := &c02SearchExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	registerC02Credential(
		t, server, &auth.Auth{
			ID:       "codex-alpha-api-key",
			Provider: "codex",
			Prefix:   "vendor",
			Status:   auth.StatusActive,
			Attributes: map[string]string{
				auth.AttributeAPIKey:           "codex-alpha-secret",
				auth.AttributeCodexAlphaSearch: "true",
				"base_url":                     "https://codex.example.com/v1",
			},
		}, "vendor/sol-alias",
	)

	requestBody := strings.Join(
		[]string{
			`{"id":"session-api-key","model":"vendor/sol-alias",`,
			`"prompt_cache_key":"cache","prompt_cache_retention":"24h","query":"go"}`,
		}, "",
	)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/alpha/search",
		strings.NewReader(requestBody),
	)
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	upstreamRequest, upstreamBody := executor.snapshot()
	if upstreamRequest == nil || upstreamRequest.URL.String() != "https://codex.example.com/v1/alpha/search" {
		t.Fatalf("upstream URL = %#v", upstreamRequest)
	}
	if upstreamRequest.Header.Get("Authorization") != "Bearer codex-alpha-secret" {
		t.Fatalf("Authorization = %q", upstreamRequest.Header.Get("Authorization"))
	}
	bodyText := string(upstreamBody)
	if !strings.Contains(bodyText, `"model":"gpt-5.6-sol"`) {
		t.Fatalf("upstream body did not rewrite model: %s", bodyText)
	}
	if strings.Contains(bodyText, "prompt_cache_key") || strings.Contains(bodyText, "prompt_cache_retention") {
		t.Fatalf("upstream body retained Responses-only fields: %s", bodyText)
	}
	apiRequest, apiResponse := requestLogger.snapshot()
	if !strings.Contains(string(apiRequest), "https://codex.example.com/v1/alpha/search") {
		t.Fatalf("request log missing upstream URL: %s", apiRequest)
	}
	if strings.Contains(string(apiRequest), "codex-alpha-secret") {
		t.Fatalf("request log exposed API key: %s", apiRequest)
	}
	if !strings.Contains(string(apiResponse), `{"ok":true}`) {
		t.Fatalf("response log missing upstream body: %s", apiResponse)
	}
}
