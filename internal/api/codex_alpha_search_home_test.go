package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	proxyconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

type homeSearchDispatcher struct{ calls atomic.Int32 }

func (*homeSearchDispatcher) HeartbeatOK() bool { return true }
func (d *homeSearchDispatcher) RPopAuth(context.Context, string, string, http.Header, int) ([]byte, error) {
	d.calls.Add(1)
	return []byte((`{"model":"gpt-5","auth_index":"synthetic-search-index","auth":{"id":"synthetic-search-` +
		`auth","provider":"codex","status":"active","attributes":{"auth_kind":"oauth"},"metadata":{"` +
		`access_token":"synthetic-search-token"}}}`)), nil
}

func (d *homeSearchDispatcher) RPopAuthWithPolicy(
	ctx context.Context,
	model, session string,
	headers http.Header,
	count int,
	policy string,
) ([]byte, error) {
	if policy != auth.CredentialPolicyCodexAlphaSearchV1 {
		return nil, errors.New("unexpected credential policy")
	}
	return d.RPopAuth(ctx, model, session, headers, count)
}

type homeSearchBody struct {
	io.Reader
	closes atomic.Int32
}

func (b *homeSearchBody) Close() error { b.closes.Add(1); return nil }

type homeSearchExecutor struct {
	c02SearchExecutor
	body         *homeSearchBody
	contentType  string
	calls        atomic.Int32
	refreshCalls atomic.Int32
	request      *http.Request
	beforeReturn func()
}

func (e *homeSearchExecutor) HttpRequest(_ context.Context, _ *auth.Auth, request *http.Request) (
	*http.Response,
	error,
) {
	e.calls.Add(1)
	e.request = request
	if e.beforeReturn != nil {
		e.beforeReturn()
	}
	return &http.Response{
		StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {e.contentType}}, Body: e.body,
	}, nil
}

func (e *homeSearchExecutor) Refresh(_ context.Context, current *auth.Auth) (*auth.Auth, error) {
	e.refreshCalls.Add(1)
	updated := current.Clone()
	updated.Metadata["access_token"] = "synthetic-refreshed-token"
	return updated, nil
}

type homeSearchUsageCapture struct{ records chan coreusage.Record }

func (p homeSearchUsageCapture) HandleUsage(_ context.Context, record coreusage.Record) {
	if p.records != nil && record.AuthID == "synthetic-search-auth" {
		p.records <- record
	}
}

func TestHomeCodexAlphaSearchReportsUnauthorizedBeforeEarlyReturn(t *testing.T) {
	const upstreamBody = `{"error":{"message":"access token expired"}}`
	for _, bindFailure := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("bind_failure=%t", bindFailure), func(t *testing.T) {
				server := newTestServer(t)
				cfg, err := proxyconfig.Clone(server.configSnapshot())
				if err != nil {
					t.Fatal(err)
				}
				cfg.RequestLog = true
				cfg.Home.Enabled = true
				server.setConfig(cfg)
				manager := server.handlers.AuthManager
				manager.SetConfig(cfg)
				dispatcher, registry := &homeSearchDispatcher{}, executionregistry.New()
				t.Cleanup(func() { _ = registry.Close() })
				manager.PublishHomeDispatch(dispatcher, registry, 1)
				var reader = io.MultiReader(
					strings.NewReader(upstreamBody), iotest.ErrReader(io.ErrUnexpectedEOF),
				)
				if bindFailure {
					reader = strings.NewReader(upstreamBody)
				}
				provider := &homeSearchExecutor{body: &homeSearchBody{Reader: reader}, contentType: "application/json"}
				if bindFailure {
					provider.beforeReturn = func() { _ = registry.Close() }
				}
				manager.RegisterExecutor(provider)
				records := make(chan coreusage.Record, 4)
				usage := coreusage.NewManager(4)
				usage.Register(homeSearchUsageCapture{records: records})
				usage.Start(t.Context())
				t.Cleanup(usage.Stop)
				server.handlers.SetUsageManager(usage)
				response := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(response)
				c.Set("apiKey", "test-key")
				c.Request = httptest.NewRequest(
					http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"model":"gpt-5-codex","query":"test"}`),
				)
				c.Request.Header.Set("Authorization", "Bearer test-key")
				c.Request = c.Request.WithContext(coreusage.WithManager(c.Request.Context(), usage))
				server.codexAlphaSearch(c)
				if err := usage.StopContext(t.Context()); err != nil {
					t.Fatal(err)
				}
				wantStatus, wantBody := http.StatusBadGateway, upstreamBody
				if bindFailure {
					wantStatus, wantBody = http.StatusServiceUnavailable, "upstream unauthorized"
				}
				if response.Code != wantStatus || provider.calls.Load() != 1 || provider.refreshCalls.Load() != 0 ||
					dispatcher.calls.Load() != 1 ||
					provider.body.closes.Load() != 1 {
					t.Fatalf(
						"search early return status=%d requests=%d refresh=%d dispatch=%d closes=%d", response.Code,
						provider.calls.Load(), provider.refreshCalls.Load(), dispatcher.calls.Load(),
						provider.body.closes.Load(),
					)
				}
				if len(records) != 1 {
					t.Fatalf("search early return usage records=%d, want one", len(records))
				}
				record := <-records
				if record.Fail.StatusCode != 401 || record.Fail.Body != wantBody ||
					coreusage.GenerateEnabled(record.Generate) ||
					record.Detail.TotalTokens != 0 {
					t.Fatalf("search early return lost observed 401: %+v", record)
				}
				if !bindFailure {
					raw, _ := c.Get("API_RESPONSE")
					buffer, ok := raw.(*logging.BoundedLogBuffer)
					if !ok || !strings.Contains(buffer.String(), upstreamBody) || !strings.Contains(
						buffer.String(), io.ErrUnexpectedEOF.Error(),
					) {
						t.Fatalf("search log lost partial body/read error: %v", raw)
					}
				}
			},
		)
	}
}

// noinspection GoDirectComparisonOfErrors
func TestCodexAlphaSearchHomeUnauthorizedPreservesBody(t *testing.T) {
	for _, test := range []struct{ name, body, contentType string }{
		{
			"json",
			"  {\"error\":{\"type\":\"authentication_error\",\"message\":\"synthetic expiry\",\"detail\":17}}\n",
			"application/json",
		},
		{"text", "synthetic upstream denied\n", "text/plain; charset=utf-8"},
		{"empty", "", "text/plain"},
		{"credential", "access token expired\naccess_token=synthetic-warning-secret", "text/plain"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				server := newTestServer(t)
				logger := log.StandardLogger()
				hooks, level := logger.ReplaceHooks(make(log.LevelHooks)), logger.GetLevel()
				logger.SetLevel(log.WarnLevel)
				hook := logtest.NewLocal(logger)
				t.Cleanup(func() { logger.ReplaceHooks(hooks); logger.SetLevel(level) })
				manager := server.handlers.AuthManager
				manager.SetConfig(&proxyconfig.Config{Home: proxyconfig.HomeConfig{Enabled: true}})
				dispatcher := &homeSearchDispatcher{}
				registry := executionregistry.New()
				t.Cleanup(func() { _ = registry.Close() })
				manager.PublishHomeDispatch(dispatcher, registry, 1)
				executor := &homeSearchExecutor{
					body: &homeSearchBody{Reader: strings.NewReader(test.body)}, contentType: test.contentType,
				}
				manager.RegisterExecutor(executor)
				usage := make(chan coreusage.Record, 8)
				const pluginName = "home-search-unauthorized-test"
				coreusage.RegisterNamedPlugin(pluginName, homeSearchUsageCapture{records: usage})
				t.Cleanup(func() { coreusage.RegisterNamedPlugin(pluginName, homeSearchUsageCapture{}) })

				request := httptest.NewRequest(
					http.MethodPost, "/v1/alpha/search",
					strings.NewReader(`{"id":"synthetic-home-search","model":"gpt-5","query":"synthetic query"}`),
				)
				request.Header.Set("Authorization", "Bearer test-key")
				response := httptest.NewRecorder()
				server.engine.ServeHTTP(response, request)
				if response.Code != http.StatusUnauthorized || response.Body.String() != test.body ||
					response.Header().Get("Content-Type") != test.contentType {
					t.Fatalf(
						"response status=%d content-type=%q body=%q", response.Code,
						response.Header().Get("Content-Type"), response.Body.String(),
					)
				}
				if executor.calls.Load() != 1 || executor.refreshCalls.Load() != 0 || dispatcher.calls.Load() != 1 ||
					executor.body.closes.Load() != 1 {
					t.Fatalf(
						"requests=%d refresh=%d dispatch=%d close=%d", executor.calls.Load(),
						executor.refreshCalls.Load(), dispatcher.calls.Load(), executor.body.closes.Load(),
					)
				}
				warnings := 0
				for _, entry := range hook.AllEntries() {
					const prefix = "codex alpha search upstream request failed: "
					if !strings.HasPrefix(entry.Message, prefix) {
						continue
					}
					warnings++
					if entry.Message != prefix+logging.SafeDiagnosticForLog(test.body) ||
						entry.Data["status"] != http.StatusUnauthorized ||
						strings.Contains(
							entry.Message, "synthetic-warning-secret",
						) {
						t.Fatalf("unsafe search warning: %q %v", entry.Message, entry.Data)
					}
				}
				if warnings != 1 {
					t.Fatalf("warnings=%d, want 1", warnings)
				}
				if executor.request == nil || executor.request.Context().Err() != context.Canceled {
					t.Fatal("Home attempt context was not released")
				}
				select {
				case record := <-usage:
					if record.Fail.StatusCode != http.StatusUnauthorized || record.Fail.Body != test.body ||
						record.AuthIndex != "synthetic-search-index" ||
						record.AccessTokenSHA256 == "" ||
						coreusage.GenerateEnabled(record.Generate) ||
						record.Detail.TotalTokens != 0 {
						t.Fatalf(
							"usage status=%d body=%q index=%q", record.Fail.StatusCode, record.Fail.Body,
							record.AuthIndex,
						)
					}
				case <-time.After(time.Second):
					t.Fatal("Home error result was not reported")
				}
			},
		)
	}
}
