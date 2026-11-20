package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalCustomHeaderMetadataInputs(t *testing.T) {
	t.Run(
		"extract", func(t *testing.T) {
			metadata := map[string]any{
				"headers": map[string]any{
					" X-Test ": " value ", "": "ignored", "X-Empty": "   ", "X-Num": float64(1),
				},
			}
			if got := ExtractCustomHeadersFromMetadata(metadata); !reflect.DeepEqual(
				got, map[string]string{"X-Test": "value"},
			) {
				t.Fatalf("original headers=%v", got)
			}
		},
	)
	t.Run(
		"apply", func(t *testing.T) {
			credential := &Auth{
				Metadata:   map[string]any{"headers": map[string]string{"X-Test": "new", "X-Empty": "   "}},
				Attributes: map[string]string{"header:X-Test": "old", "keep": "1"},
			}
			if err := ApplyCustomHeadersFromMetadata(credential); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(
				credential.Attributes,
				map[string]string{"header:X-Test": "new", "keep": "1"},
			) {
				t.Fatalf("original attributes=%v", credential.Attributes)
			}
		},
	)
	t.Run(
		"session placeholder", func(t *testing.T) {
			credential := &Auth{
				Metadata: map[string]any{"headers": map[string]string{"X-CPA-Session": "$CPA-SESSION-ID"}},
			}
			if err := ApplyCustomHeadersFromMetadata(credential); err != nil {
				t.Fatal(err)
			}
			if credential.Attributes["header:X-CPA-Session"] != "$CPA-SESSION-ID" {
				t.Fatalf("original session placeholder=%v", credential.Attributes)
			}
		},
	)
}

type originalCustomHeadersExecutor struct {
	schedulerTestExecutor
	headers http.Header
	model   string
	payload string
	calls   int
}

func (*originalCustomHeadersExecutor) Identifier() string { return "test-provider" }
func (e *originalCustomHeadersExecutor) Execute(
	ctx context.Context,
	credential *Auth,
	request core.Request,
	options core.Options,
) (core.Response, error) {
	outgoing := httptest.NewRequest(http.MethodPost, "https://api.example.com", nil).WithContext(ctx)
	outgoing.Header.Set("Authorization", "Bearer fixture-credential")
	util.ApplyCustomHeadersFromAttrs(outgoing, credential.Attributes, options.Headers)
	e.headers, e.model, e.payload = outgoing.Header.Clone(), request.Model, string(request.Payload)
	e.calls++
	return core.Response{Payload: []byte(`{"ok":true}`)}, nil
}
func (e *originalCustomHeadersExecutor) CountTokens(
	ctx context.Context,
	credential *Auth,
	request core.Request,
	options core.Options,
) (core.Response, error) {
	return e.Execute(ctx, credential, request, options)
}
func (e *originalCustomHeadersExecutor) ExecuteStream(
	ctx context.Context,
	credential *Auth,
	request core.Request,
	options core.Options,
) (*core.StreamResult, error) {
	if _, err := e.Execute(ctx, credential, request, options); err != nil {
		return nil, err
	}
	chunks := make(chan core.StreamChunk, 1)
	chunks <- core.StreamChunk{Payload: []byte(`{"id":"1"}`)}
	close(chunks)
	return &core.StreamResult{Chunks: chunks}, nil
}

func TestOriginalCustomHeadersSessionInputs(t *testing.T) {
	for _, affinity := range []bool{false, true} {
		for _, kind := range []string{"execute", "count", "stream"} {
			for _, sample := range []string{"header", "claude", "clear-derived", "clear-explicit", "clear-empty"} {
				t.Run(
					map[bool]string{false: "affinity=false", true: "affinity=true"}[affinity]+"/"+kind+"/"+sample,
					func(t *testing.T) {
						var selector Selector = &RoundRobinSelector{}
						if affinity {
							selector = NewSessionAffinitySelector(&RoundRobinSelector{})
						}
						manager := NewManager(nil, selector, nil)
						manager.SetHomeClientProvider(func() HomeClient { return nil })
						executor := &originalCustomHeadersExecutor{}
						manager.RegisterExecutor(executor)
						credential := &Auth{
							ID: "auth-1", Provider: "test-provider", Status: StatusActive,
							Attributes: map[string]string{
								"header:X-Forwarded-Session": "$CPA-SESSION-ID",
								"header:Authorization":       "Bearer $CPA-SESSION-ID",
								"header:Static-Header":       "keep-me",
							},
						}
						payload := `{"messages":[{"role":"user","content":"hello"}]}`
						options := core.Options{Headers: http.Header{"X-Session-ID": {"aff-toggle-sess-1"}}}
						want, header := "aff-toggle-sess-1", "X-Forwarded-Session"
						ctx := t.Context()
						if sample == "claude" {
							credential.ID = "auth-stream-1"
							credential.Attributes["header:X-Stream-Session"] = "$CPA-SESSION-ID"
							payload = `{"messages":[{"role":"user","content":"stream hello"}]}`
							options.Headers = http.Header{"X-Claude-Code-Session-Id": {"claude-stream-sess-42"}}
							want, header = "claude:claude-stream-sess-42", "X-Stream-Session"
						} else if sample != "header" {
							credential.ID = "auth-clear-1"
							options.Headers = http.Header{"X-Session-ID": {"stale-session-123"}}
							want = ""
							if sample == "clear-explicit" {
								want = "header:stale-session-123"
								ctx = util.WithSessionID(ctx, want)
							} else if sample == "clear-empty" {
								ctx = util.WithSessionID(util.WithSessionID(ctx, "header:stale-session-123"), "")
							}
							options.RequestAfterAuthInterceptor = func(
								_ context.Context,
								input core.RequestAfterAuthInterceptRequest,
							) core.RequestAfterAuthInterceptResponse {
								next := input.Options
								next.Headers.Del("X-Session-ID")
								for _, key := range []string{
									core.CanonicalSessionIDMetadataKey, core.LCPAffinitySessionIDMetadataKey,
									core.ExecutionSessionMetadataKey, core.DerivedSessionIDMetadataKey,
								} {
									delete(next.Metadata, key)
								}
								return core.RequestAfterAuthInterceptResponse{
									Handled: true, Request: input.Request, Options: next,
								}
							}
						}
						registerRequestScopedTestAuth(t, manager, credential, "test-model")
						request := core.Request{Model: "test-model", Payload: []byte(payload)}
						var err error
						switch kind {
						case "execute":
							_, err = manager.Execute(ctx, []string{"test-provider"}, request, options)
						case "count":
							_, err = manager.ExecuteCount(ctx, []string{"test-provider"}, request, options)
						case "stream":
							var result *core.StreamResult
							result, err = manager.ExecuteStream(
								ctx,
								[]string{"test-provider"},
								request,
								options,
							)
							if err == nil {
								for chunk := range result.Chunks {
									if chunk.Err != nil {
										t.Fatal(chunk.Err)
									}
								}
							}
						}
						if err !=
							nil ||
							executor.calls !=
								1 ||
							executor.headers.Get(header) !=
								want ||
							executor.model !=
								request.Model ||
							executor.payload !=
								payload ||
							executor.headers.Get("Static-Header") != "keep-me" ||
							executor.headers.Get("Authorization") != "Bearer fixture-credential" {
							t.Fatalf(
								"headers=%v model=%q payload=%q calls=%d error=%v, want session=%q",
								executor.headers,
								executor.model,
								executor.payload,
								executor.calls,
								err,
								want,
							)
						}
					},
				)
			}
		}
	}
}

func TestOriginalCustomHeaderResolverKeepsConcurrentRequestIdentity(t *testing.T) {
	previous := util.SessionIDResolver
	t.Cleanup(func() { util.SessionIDResolver = previous })
	util.SessionIDResolver = func(_ context.Context, incoming http.Header) string {
		return incoming.Get("X-Session-Id")
	}
	results := make(chan bool, 2)
	for _, session := range []string{"resolver-a", "resolver-b"} {
		go func() {
			outgoing := httptest.NewRequest(http.MethodPost, "https://api.example.com", nil)
			util.ApplyCustomHeadersFromAttrs(
				outgoing, map[string]string{"header:X-CPA-Session": "$CPA-SESSION-ID"},
				http.Header{"X-Session-Id": {session}},
			)
			results <- outgoing.Header.Get("X-CPA-Session") == session
		}()
	}
	for range 2 {
		if !<-results {
			t.Error("concurrent request lost its own session identity")
		}
	}
}

func TestOriginalCustomHeaderResolverAndMetadataInputs(t *testing.T) {
	previous := util.SessionIDResolver
	t.Cleanup(func() { util.SessionIDResolver = previous })
	for _, sample := range []string{"resolver", "metadata", "explicit", "empty"} {
		t.Run(
			sample, func(t *testing.T) {
				calls := 0
				util.SessionIDResolver = func(_ context.Context, incoming http.Header) string {
					calls++
					return "resolved:" + incoming.Get("X-Session-ID")
				}
				ctx := t.Context()
				want := "resolved:direct-session"
				if sample != "resolver" {
					ctx = logging.WithClientRequestMetadata(
						ctx, logging.ClientRequestMetadata{SessionID: "metadata-session"},
					)
					want = "metadata-session"
				}
				if sample == "explicit" {
					ctx = util.WithSessionID(ctx, "explicit-session")
					want = "explicit-session"
				} else if sample == "empty" {
					ctx = util.WithSessionID(ctx, "")
					want = ""
				}
				for _, session := range []string{"direct-session", "other-session"} {
					outgoing := httptest.NewRequest(http.MethodPost, "https://api.example.com", nil).
						WithContext(ctx)
					util.ApplyCustomHeadersFromAttrs(
						outgoing, map[string]string{"header:X-CPA-Session": "$CPA-SESSION-ID"},
						http.Header{"X-Session-Id": {session}},
					)
					if sample == "resolver" {
						want = "resolved:" + session
					}
					if outgoing.Header.Get("X-CPA-Session") != want {
						t.Fatalf("session=%q, want %q", outgoing.Header.Get("X-CPA-Session"), want)
					}
				}
				if sample == "resolver" && calls != 2 || sample != "resolver" && calls != 0 {
					t.Fatalf("resolver calls=%d for %s", calls, sample)
				}
			},
		)
	}
}
