package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type geminiSearchRouteExecutor struct {
	cliproxyauth.ProviderExecutor
	prepare  func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options, bool) ([]byte, error)
	requests []cliproxyexecutor.Request
	bodies   [][]byte
}

func (e *geminiSearchRouteExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.requests = append(e.requests, req)
	body, err := e.prepare(ctx, auth, req, opts, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	e.bodies = append(e.bodies, body)
	return cliproxyexecutor.Response{Payload: body}, nil
}

func (e *geminiSearchRouteExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.requests = append(e.requests, req)
	body, err := e.prepare(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}
	e.bodies = append(e.bodies, body)
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: body}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *geminiSearchRouteExecutor) Refresh(_ context.Context, _ *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return nil, errors.New("offline native search fixture cannot refresh credentials")
}

func (e *geminiSearchRouteExecutor) HttpRequest(_ context.Context, _ *cliproxyauth.Auth, _ *http.Request) (*http.Response, error) {
	return nil, errors.New("offline native search fixture cannot send HTTP requests")
}

func newGeminiSearchRouteExecutor(cfg *config.Config, provider string) *geminiSearchRouteExecutor {
	e := &geminiSearchRouteExecutor{}
	switch provider {
	case "gemini":
		inner := NewGeminiExecutor(cfg)
		e.ProviderExecutor = inner
		e.prepare = func(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) ([]byte, error) {
			prepared, err := inner.prepareGeminiRequest(ctx, auth, req, opts, stream)
			if err != nil {
				return nil, err
			}
			_ = prepared.httpReq.Body.Close()
			return prepared.body, nil
		}
	case "vertex":
		inner := NewGeminiVertexExecutor(cfg)
		e.ProviderExecutor = inner
		e.prepare = func(ctx context.Context, _ *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) ([]byte, error) {
			prepared, err := inner.prepareVertexGeminiBody(ctx, req, opts, req.Model, stream)
			if err != nil {
				return nil, err
			}
			return prepared.body, nil
		}
	case "gemini-cli":
		inner := NewGeminiCLIExecutor(cfg)
		e.ProviderExecutor = inner
		e.prepare = func(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) ([]byte, error) {
			prepared, err := inner.prepareGeminiCLIRequest(ctx, auth, req, opts, stream)
			if err != nil {
				return nil, err
			}
			return prepared.basePayload, nil
		}
	case "aistudio":
		inner := NewAIStudioExecutor(cfg, provider, nil)
		e.ProviderExecutor = inner
		e.prepare = func(ctx context.Context, _ *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) ([]byte, error) {
			body, _, err := inner.translateRequest(ctx, req, opts, stream)
			return body, err
		}
	default:
		panic("unknown native search fixture provider: " + provider)
	}
	return e
}

func TestGeminiResponsesWebSearchMappedRoutes(t *testing.T) {
	for _, provider := range []string{"gemini", "vertex", "gemini-cli", "aistudio"} {
		for _, prefix := range []string{"", "tenant"} {
			for _, capability := range []string{"true", "false", "nil-capabilities", "nil-web-search"} {
				for _, choice := range []struct {
					name    string
					value   string
					tools   string
					search  bool
					invalid bool
				}{
					{name: "allowed-search", value: `{"type":"allowed_tools","mode":"auto","tools":[{"type":"web_search"},{"type":"function","name":"lookup"}]}`, search: true},
					{name: "allowed-function", value: `{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"lookup"}]}`},
					{name: "none", value: `"none"`},
					{name: "invalid-domain-array", value: `"auto"`, tools: `[{"type":"web_search","filters":{"allowed_domains":[]}}]`, search: true, invalid: true},
					{name: "invalid-domain-value", value: `"auto"`, tools: `[{"type":"web_search","filters":{"allowed_domains":["example.com",42]}}]`, search: true, invalid: true},
					{name: "multiple-search-tools", value: `"auto"`, tools: `[{"type":"web_search"},{"type":"web_search_preview"}]`, search: true, invalid: true},
				} {
					for _, stream := range []bool{false, true} {
						name := fmt.Sprintf("%s/prefix=%s/%s/%s/stream=%t", provider, prefix, capability, choice.name, stream)
						t.Run(name, func(t *testing.T) {
							const upstream = "gemini-2.5-flash"
							const alias = "gemini-native-search-route-fixture"
							public := alias
							if prefix != "" {
								public = prefix + "/" + alias
							}
							authID := "native-search-route-" + provider
							info := &registry.ModelInfo{ID: public, CatalogName: upstream, UserDefined: true}
							value := capability != "false"
							switch capability {
							case "true", "false":
								info.NativeCapabilities = &registry.NativeCapabilities{WebSearch: &value}
							case "nil-web-search":
								info.NativeCapabilities = &registry.NativeCapabilities{}
							}
							models := registry.GetGlobalRegistry()
							models.RegisterClient(authID, provider, []*registry.ModelInfo{info})
							t.Cleanup(func() { models.UnregisterClient(authID) })
							advertised := models.GetResponsesWebSearchCapability(public)
							if capability == "true" || capability == "false" {
								if advertised == nil || *advertised != value {
									t.Fatalf("advertised capability = %v, want %t", advertised, value)
								}
							} else if advertised != nil {
								t.Fatalf("unknown metadata advertised as known: %v", advertised)
							}
							cfg := &config.Config{
								GeminiKey:          []config.GeminiKey{{APIKey: "fixture-key", Models: []config.GeminiModel{{Name: upstream, Alias: alias}}}},
								VertexCompatAPIKey: []config.VertexCompatKey{{APIKey: "fixture-key", Models: []config.VertexCompatModel{{Name: upstream, Alias: alias}}}},
								OAuthModelAlias:    map[string][]config.OAuthModelAlias{provider: {{Name: upstream, Alias: alias}}},
							}
							manager := cliproxyauth.NewManager(nil, nil, nil)
							manager.SetConfig(cfg)
							manager.SetOAuthModelAlias(cfg.OAuthModelAlias)
							executor := newGeminiSearchRouteExecutor(cfg, provider)
							manager.RegisterExecutor(executor)
							auth := &cliproxyauth.Auth{ID: authID, Provider: provider, Prefix: prefix, Status: cliproxyauth.StatusActive}
							if provider == "gemini" || provider == "vertex" {
								auth.Attributes = map[string]string{"api_key": "fixture-key"}
							} else {
								auth.Attributes = map[string]string{"auth_kind": "oauth"}
								auth.Metadata = map[string]any{"access_token": "fixture-access", "expiry": time.Now().Add(time.Hour).Format(time.RFC3339)}
							}
							ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							defer cancel()
							if _, err := manager.Register(ctx, auth); err != nil {
								t.Fatal(err)
							}
							manager.RefreshSchedulerEntry(authID)
							payload := []byte(`{"input":"search","tools":[{"type":"web_search","filters":{"allowed_domains":[" example.com ","docs.example.com"]}},{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
							payload, err := sjson.SetBytes(payload, "model", public)
							if err != nil {
								t.Fatal(err)
							}
							payload, err = sjson.SetRawBytes(payload, "tool_choice", []byte(choice.value))
							if err != nil {
								t.Fatal(err)
							}
							if choice.tools != "" {
								payload, err = sjson.SetRawBytes(payload, "tools", []byte(choice.tools))
								if err != nil {
									t.Fatal(err)
								}
							}
							request := cliproxyexecutor.Request{Model: public, Payload: payload, Metadata: map[string]any{"fixture": "unchanged"}}
							opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload}
							var body []byte
							if stream {
								var result *cliproxyexecutor.StreamResult
								result, err = manager.ExecuteStream(ctx, []string{provider}, request, opts)
								if err == nil {
									for chunk := range result.Chunks {
										if chunk.Err != nil {
											err = chunk.Err
										}
										body = append(body, chunk.Payload...)
									}
								}
							} else {
								var result cliproxyexecutor.Response
								result, err = manager.Execute(ctx, []string{provider}, request, opts)
								body = result.Payload
							}
							if len(executor.requests) != 1 || executor.requests[0].Model != upstream {
								t.Fatalf("routed requests = %+v, error = %v", executor.requests, err)
							}
							if !reflect.DeepEqual(request.Metadata, map[string]any{"fixture": "unchanged"}) {
								t.Fatalf("caller request metadata mutated: %+v", request.Metadata)
							}
							if choice.invalid && value {
								var status cliproxyexecutor.StatusError
								var scoped cliproxyexecutor.RequestScopedError
								if err == nil || len(executor.bodies) != 0 || !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest || !errors.As(err, &scoped) || !scoped.IsRequestScoped() {
									t.Fatalf("invalid request: body count=%d, error=%T %v; want request-scoped 400 before payload acceptance", len(executor.bodies), err, err)
								}
								current, ok := manager.GetByID(authID)
								if !ok || current.Status != cliproxyauth.StatusActive || current.Unavailable || current.Quota.Exceeded {
									t.Fatalf("invalid client request changed credential availability: %+v", current)
								}
								return
							}
							if err != nil {
								t.Fatal(err)
							}
							searchPath := "tools"
							if provider == "gemini-cli" {
								searchPath = "request.tools"
							}
							var searches []gjson.Result
							for _, tool := range gjson.GetBytes(body, searchPath).Array() {
								if search := tool.Get("googleSearch"); search.Exists() {
									searches = append(searches, search)
								}
							}
							wantSearch := choice.search && value
							if (len(searches) == 1) != wantSearch || len(searches) > 1 {
								t.Fatalf("native search count=%d, want search=%t: %s", len(searches), wantSearch, body)
							}
							if wantSearch && searches[0].Get("includedDomains").Raw != `["example.com","docs.example.com"]` {
								t.Fatalf("domain restriction missing or changed: %s", searches[0].Raw)
							}
							if strings.Contains(string(body), `"model":"`+public+`"`) {
								t.Fatalf("public alias leaked into upstream model field: %s", body)
							}
						})
					}
				}
			}
		}
	}
}
