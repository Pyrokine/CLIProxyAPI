package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// noinspection GoDfaErrorMayBeNotNil
func codexConsumerError(
	t *testing.T, ctx context.Context, auth *coreauth.Auth,
	req core.Request, opts core.Options, websocketMode, stream bool,
) error {
	t.Helper()
	cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
	httpExecutor := newCodexExecutor(cfg)
	execute, executeStream := httpExecutor.Execute, httpExecutor.ExecuteStream
	if websocketMode {
		websocketExecutor := newCodexWebsocketsExecutor(cfg)
		execute, executeStream = websocketExecutor.Execute, websocketExecutor.ExecuteStream
	}
	if !stream {
		response, err := execute(ctx, auth, req, opts)
		if len(response.Payload) != 0 {
			t.Errorf("failed request returned success payload: %s", response.Payload)
		}
		return err
	}
	result, err := executeStream(ctx, auth, req, opts)
	if err != nil {
		return err
	}
	var terminalErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			if terminalErr != nil {
				t.Error("stream emitted more than one error")
			}
			terminalErr = chunk.Err
		}
	}
	return terminalErr
}

func assertCodexConsumerError(t *testing.T, err error, payload string, status int, wantRetry *time.Duration) {
	t.Helper()
	if err == nil || statusErrCode(err) != status {
		t.Fatalf("error=%v, status=%d, want %d", err, statusErrCode(err), status)
	}
	var original, actual map[string]any
	if json.Unmarshal([]byte(payload), &original) != nil ||
		json.Unmarshal([]byte(err.Error()), &actual) != nil {
		t.Fatalf("error lost JSON envelope: %v", err)
	}
	for key, value := range original {
		if !reflect.DeepEqual(actual[key], value) {
			t.Errorf("original envelope field %q changed: got=%v want=%v", key, actual[key], value)
		}
	}
	var typed core.ExecutionDispositionError
	if !errors.As(err, &typed) {
		t.Fatalf("error lost execution disposition: %T", err)
	}
	want := core.HTTPStatusDisposition(status, wantRetry != nil)
	if status == http.StatusTooManyRequests && wantRetry != nil && *wantRetry == 0 && strings.Contains(
		payload, `"websocket_connection_limit_reached"`,
	) {
		want.RetryDirective = core.RetryDirectiveNoRetry
		want.EffectScope = core.StateEffectScopeRequest
	}
	if got := typed.ExecutionDisposition(); got != want {
		t.Errorf("disposition=%+v, want %+v", got, want)
	}
	var retry interface{ RetryAfter() *time.Duration }
	if !errors.As(err, &retry) || !reflect.DeepEqual(retry.RetryAfter(), wantRetry) {
		t.Errorf("retry delay=%v, want %v", retry, wantRetry)
	}
}

func TestNewCodexStatusErrClassifiesKnownCodexFailures(t *testing.T) {
	for _, sample := range []struct {
		name, body, code, kind string
		status                 int
	}{
		{
			"context length status",
			`{"error":{"message":"context length exceeded","type":` +
				`"invalid_request_error","code":` +
				`"context_length_exceeded"}}`,
			"context_too_large", "invalid_request_error", http.StatusRequestEntityTooLarge,
		},
		{
			"thinking signature",
			`{"error":{"message":"Invalid signature in thinking block","type":"invalid_request_error","code":` +
				`"invalid_request_error"}}`,
			"thinking_signature_invalid", "invalid_request_error", http.StatusBadRequest,
		},
		{
			"previous response missing",
			`{"error":{"message":"No response found for previous_response_id resp_1` +
				`23","type":"invalid_request_error","code":"previous_response_not_found` +
				`"}}`,
			"previous_response_not_found", "invalid_request_error", http.StatusBadRequest,
		},
		{
			"auth unavailable",
			`{"error":{"message":"invalid or expired token","type":"authentication_error","code":"invalid_api_key"}}`,
			"auth_unavailable", "authentication_error", http.StatusUnauthorized,
		},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				assertError := func(t *testing.T, err error) {
					t.Helper()
					if err == nil || statusErrCode(err) != sample.status {
						t.Fatalf("error=%v, want status=%d", err, sample.status)
					}
					var original, actual struct {
						Error struct {
							Message string `json:"message"`
							Type    string `json:"type"`
							Code    string `json:"code"`
						} `json:"error"`
					}
					if json.Unmarshal([]byte(sample.body), &original) != nil || json.Unmarshal(
						[]byte(err.Error()), &actual,
					) != nil {
						t.Fatalf("error lost its JSON envelope: %v", err)
					}
					if actual.Error.Code != sample.code || actual.Error.Type != sample.kind ||
						actual.Error.Message != original.Error.Message {
						t.Fatalf(
							"canonical error=%v, want code=%s type=%s original message=%s", err, sample.code,
							sample.kind, original.Error.Message,
						)
					}
				}
				t.Run(
					"status",
					func(t *testing.T) { assertError(t, newCodexStatusErr(sample.status, []byte(sample.body))) },
				)
				for _, transport := range []string{"http", "sse", "websocket"} {
					for _, stream := range []bool{false, true} {
						t.Run(
							fmt.Sprintf("%s/stream=%t", transport, stream), func(t *testing.T) {
								ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
								defer cancel()
								ctx = home.WithClientScope(
									translator.WithPluginHooks(
										ctx,
										nil,
									),
									home.ClientScope{ID: t.Name(), Enabled: false},
								)
								var frame map[string]any
								if err := json.Unmarshal([]byte(sample.body), &frame); err != nil {
									t.Fatal(err)
								}
								frame["type"], frame["status"], frame["sequence_number"] = "error", sample.status, 37
								frame["cause"] = map[string]any{"id": "original-codex-cause"}
								payload, err := json.Marshal(frame)
								if err != nil {
									t.Fatal(err)
								}
								var attempts atomic.Int32
								auth := runtimeCodexAuth("https://codex-classification.invalid/v1")
								if transport == "websocket" {
									server := httptest.NewServer(
										http.HandlerFunc(
											func(w http.ResponseWriter, request *http.Request) {
												peer, err := (&websocket.Upgrader{}).Upgrade(w, request, nil)
												if err != nil {
													t.Error(err)
													return
												}
												defer func(closeResource func() error) {
													_ =
														closeResource()
												}(peer.Close)
												if _, _, err := peer.ReadMessage(); err != nil {
													t.Error(err)
													return
												}
												attempts.Add(1)
												if err := peer.WriteMessage(
													websocket.TextMessage, payload,
												); err != nil {
													t.Error(err)
												}
											},
										),
									)
									defer server.Close()
									auth = runtimeCodexAuth(server.URL)
								} else {
									ctx = util.WithRoundTripper(
										ctx, &auditClaudeContractTransport{
											respond: func(request *http.Request) (*http.Response, error) {
												attempts.Add(1)
												status, body := http.StatusOK, "data: "+string(payload)+"\n\n"
												if transport == "http" {
													status, body = sample.status, string(payload)
												}
												return &http.Response{
													StatusCode: status, Request: request,
													Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{
														"Content-Type": {"text/event-stream"},
														"X-Request-Id": {"original-codex-request"},
													},
												}, nil
											},
										},
									)
								}
								err = codexConsumerError(
									t, ctx, auth, core.Request{
										Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
									}, core.Options{SourceFormat: translator.FormatOpenAIResponse},
									transport == "websocket", stream,
								)
								assertError(t, err)
								var actual map[string]any
								if json.Unmarshal(
									[]byte(err.Error()), &actual,
								) != nil || actual["sequence_number"] != float64(37) || !reflect.DeepEqual(
									actual["cause"], frame["cause"],
								) || attempts.Load() != 1 {
									t.Fatalf(
										"error lost original status/cause or repeated the request: error=%v "+
											"attempts=%d",
										err,
										attempts.Load(),
									)
								}
							},
						)
					}
				}
			},
		)
	}
}

func TestCodexWebsocketsExecutePreservesPreviousResponseIDUpstream(t *testing.T) {
	captured := make(chan []byte, 1)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/responses" {
					t.Errorf("request path=%s, want /responses", request.URL.Path)
					return
				}
				peer, err := (&websocket.Upgrader{}).Upgrade(w, request, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
				kind, payload, err := peer.ReadMessage()
				if err != nil || kind != websocket.TextMessage {
					t.Errorf("request kind=%d error=%v", kind, err)
					return
				}
				captured <- append([]byte(nil), payload...)
				if err := peer.WriteMessage(
					websocket.TextMessage,
					[]byte(`{"type":"response.completed","response":{"id":"resp-2","output":[],"us`+
						`age":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`),
				); err != nil {
					t.Error(err)
				}
			},
		),
	)
	defer server.Close()
	cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = home.WithClientScope(
		translator.WithPluginHooks(ctx, nil),
		home.ClientScope{ID: t.Name(), Enabled: false},
	)
	auth := &coreauth.Auth{Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := core.Request{
		Model: "gpt-5-codex",
		Payload: []byte(
			`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-1"}]}`,
		),
	}
	if _, err := newCodexWebsocketsExecutor(cfg).Execute(
		ctx, auth, req, core.Options{SourceFormat: translator.FormatCodex},
	); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-captured:
		var actual struct {
			Type       string `json:"type"`
			PreviousID string `json:"previous_response_id"`
		}
		if err := json.Unmarshal(
			raw, &actual,
		); err != nil || actual.Type != "response.create" || actual.PreviousID != "resp-1" {
			t.Fatalf("original previous response did not reach upstream: %s error=%v", raw, err)
		}
	case <-ctx.Done():
		t.Fatal("upstream previous response payload was not captured")
	}
}

func TestNewCodexStatusErrPreservesUnclassifiedErrors(t *testing.T) {
	const body = `{"error":{"message":"documentation mentions too many tokens, but this ` +
		`is a billing configuration failure","type":"server_error","code":"bill` +
		`ing_config_error"}}`
	err := newCodexStatusErr(http.StatusBadGateway, []byte(body))
	if err.StatusCode() != http.StatusBadGateway || err.Error() != body {
		t.Fatalf("unclassified error changed: %v", err)
	}
}

func TestCodexErrorEnvelopeConsumers(t *testing.T) {
	retryDelay := 7 * time.Second
	immediate := time.Duration(0)
	for _, fixture := range []struct {
		name    string
		payload string
		status  int
		retry   *time.Duration
	}{
		{
			"plain HTTP capacity", `{"error":{"code":"model_at_capacity","message":"The model is at capacity"}}`, 429,
			nil,
		},
		{
			"capacity code", `{"type":"error","status":400,"sequence_number":17,"extension":{"trace":"synthetic"},` +
				`"error":{"code":"model_at_capacity","message":"The model is at capacity"}}`, 429, nil,
		},
		{
			"legacy capacity message", `{"type":"error","status":400,"sequence_number":18,` +
				`"error":{"message":"The requested model currently operates at capacity"}}`, 429, nil,
		},
		{
			"nested quota", `{"type":"response.failed","sequence_number":19,"extension":["synthetic"],` +
				`"response":{"id":"synthetic-failure","status":"failed","error":{"type":"usage_limit_reached",` +
				`"resets_in_seconds":7,"cause":{"reason":"synthetic-quota"}}}}`, 429, &retryDelay,
		},
		{
			"wrapped transient", `{"type":"error","status":429,"sequence_number":20,` +
				`"headers":{"X-Request-Id":"synthetic-envelope"},"body":{"error":{"type":"rate_limit_error",` +
				`"message":"try later","resets_in_seconds":7}}}`, 429, nil,
		},
		{
			"unknown execution", `{"type":"response.failed","status":502,"sequence_number":21,` +
				`"response":{"error":{"message":"synthetic unknown execution"}},` +
				`"cause":{"code":"synthetic-upstream"}}`, 502, nil,
		},
		{
			"legacy string error", `{"type":"error","status":400,"sequence_number":22,` +
				`"error":"synthetic legacy string"}`, 400, nil,
		},
		{"legacy missing details", `{"type":"response.failed","sequence_number":23,"response":{}}`, 502, nil},
		{
			name: "original connection limit", status: http.StatusTooManyRequests, retry: &immediate,
			payload: `{"type":"error","status":429,"error":{"code":"websocket_connection_limit_reached",` +
				`"message":"too many websockets"},` +
				`"headers":{"retry-after":"1","X-Request-Id":"synthetic-envelope"}}`,
		},
		{
			name: "original wrapped connection limit", status: http.StatusTooManyRequests, retry: &immediate,
			payload: `{"type":"error","status":429,"body":{"error":{"code":"websocket_connection_limit_reached",` +
				`"type":"server_error","message":"too many websocket connections"}},` +
				`"headers":{"X-Request-Id":"synthetic-envelope"}}`,
		},
		{
			name: "original status alias", status: http.StatusForbidden,
			payload: `{"type":"error","status_code":403,"body":{"error":{"type":"permission_error",` +
				`"message":"original permission error"}},"headers":{"X-Request-Id":"synthetic-envelope"}}`,
		},
	} {
		for _, transport := range []string{"http", "sse", "websocket"} {
			if fixture.name == "plain HTTP capacity" && transport != "http" {
				continue
			}
			if transport == "http" && fixture.name != "plain HTTP capacity" &&
				fixture.name != "capacity code" &&
				fixture.name != "legacy capacity message" {
				continue
			}
			for _, stream := range []bool{false, true} {
				name := fixture.name + "/" + transport
				if stream {
					name += "/stream"
				}
				t.Run(
					name, func(t *testing.T) {
						var attempts atomic.Int32
						ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
						defer cancel()
						ctx = home.WithClientScope(ctx, home.ClientScope{ID: t.Name(), Enabled: false})
						ctx = translator.WithPluginHooks(ctx, nil)
						auth := runtimeCodexAuth("https://codex-consumer.invalid/v1")
						if transport == "websocket" {
							server := httptest.NewServer(
								http.HandlerFunc(
									func(w http.ResponseWriter, r *http.Request) {
										peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
										if err != nil {
											t.Error(err)
											return
										}
										defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
										if _, _, err = peer.ReadMessage(); err != nil {
											t.Error(err)
											return
										}
										attempts.Add(1)
										if err = peer.WriteMessage(
											websocket.TextMessage, []byte(fixture.payload),
										); err != nil {
											t.Error(err)
										}
									},
								),
							)
							defer server.Close()
							auth = runtimeCodexAuth(server.URL)
						} else {
							ctx = util.WithRoundTripper(
								ctx, &auditClaudeContractTransport{
									respond: func(
										request *http.Request,
									) (*http.Response, error) {
										attempts.Add(1)
										status, body := http.StatusOK, "data: "+fixture.payload+"\n\n"
										if transport == "http" {
											status = http.StatusBadRequest
											body = fixture.payload
										}
										return &http.Response{
											StatusCode: status, Request: request,
											Body: io.NopCloser(strings.NewReader(body)),
											Header: http.Header{
												"Content-Type": {"text/event-stream"},
												"X-Request-Id": {"synthetic-http"},
											},
										}, nil
									},
								},
							)
						}
						req := core.Request{
							Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"synthetic"}`),
						}
						err := codexConsumerError(
							t,
							ctx,
							auth,
							req,
							core.Options{
								SourceFormat: translator.FormatOpenAIResponse,
							},
							transport == "websocket",
							stream,
						)
						assertCodexConsumerError(t, err, fixture.payload, fixture.status, fixture.retry)
						if attempts.Load() != 1 {
							t.Errorf("generation attempts=%d, want one", attempts.Load())
						}
						if transport == "sse" || transport == "websocket" && strings.Contains(
							fixture.payload, `"headers"`,
						) {
							var headers interface{ Headers() http.Header }
							if !errors.As(err, &headers) {
								t.Fatalf("event error lost headers: %T", err)
							}
							wantID := "synthetic-http"
							if transport == "websocket" {
								wantID = "synthetic-envelope"
							}
							if got := headers.Headers().Get("X-Request-Id"); got != wantID {
								t.Errorf("request id=%q, want %q", got, wantID)
							}
							headerCopy := headers.Headers()
							headerCopy.Set("X-Request-Id", "mutated")
							if headers.Headers().Get("X-Request-Id") != wantID {
								t.Error("caller mutated retained headers")
							}
						}
					},
				)
			}
		}
	}
}

type codexCleanupHomeClient struct {
	noCASCodexReplayHomeClient
	deletes atomic.Int32
	failure bool
}

func (*codexCleanupHomeClient) KVCompareAndSwap(
	context.Context, string, []byte, bool, []byte, time.Duration,
) (bool, error) {
	return true, nil
}

// noinspection GoErrorStringFormat
func (c *codexCleanupHomeClient) KVDel(context.Context, ...string) (int64, error) {
	c.deletes.Add(1)
	if c.failure {
		return 0, errors.New("Bearer synthetic-cleanup-secret")
	}
	return 1, nil
}

func TestCodexCanonicalSignatureCodeClearsReplay(t *testing.T) {
	client := &codexCleanupHomeClient{}
	ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, ID: t.Name(), Enabled: true})
	const payload = `{"error":{"code":"thinking_signature_invalid","type":` +
		`"invalid_request_error","message":"canonical ` +
		`rejection"}}`
	code, kind, classified := codexStatusErrorClassification(http.StatusBadRequest, []byte(payload))
	if !classified || code != "thinking_signature_invalid" || kind != "invalid_request_error" {
		t.Errorf(
			"canonical signature code was not recognized: code=%s type=%s classified=%t",
			code,
			kind,
			classified,
		)
	}
	scope := codexReasoningReplayScope{modelName: "gpt-5.5", sessionKey: "canonical-signature-session"}
	if err := clearCodexReasoningReplayOnInvalidSignature(
		ctx, scope, http.StatusBadRequest, []byte(payload),
	); err != nil || client.deletes.Load() != 1 {
		t.Fatalf(
			"canonical signature error did not clear its replay once: error=%v deletes=%d",
			err,
			client.deletes.Load(),
		)
	}
}

func TestCodexReplayCleanupPreservesUpstreamErrorConsumers(t *testing.T) {
	oldHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	defer log.StandardLogger().ReplaceHooks(oldHooks)
	hook := logtest.NewGlobal()
	const payload = `{"type":"error","status":400,"sequence_number":31,` +
		`"headers":{"X-Request-Id":"synthetic-signature"},"error":{"code":"invalid_encrypted_content",` +
		`"message":"Invalid signature in thinking block","cause":{"id":"synthetic-cause"}}}`
	for _, transport := range []string{"http", "sse", "websocket"} {
		for _, stream := range []bool{false, true} {
			for _, failDelete := range []bool{false, true} {
				name := transport
				if stream {
					name += "/stream"
				}
				if failDelete {
					name += "/delete-failure"
				}
				t.Run(
					name, func(t *testing.T) {
						hook.Reset()
						client := &codexCleanupHomeClient{failure: failDelete}
						ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
						defer cancel()
						ctx = home.WithClientScope(
							ctx,
							home.ClientScope{Client: client, ID: t.Name(), Enabled: true},
						)
						ctx = translator.WithPluginHooks(ctx, nil)
						var attempts atomic.Int32
						peerClosed := make(chan struct{}, 1)
						auth := runtimeCodexAuth("https://codex-cleanup.invalid/v1")
						if transport == "websocket" {
							server := httptest.NewServer(
								http.HandlerFunc(
									func(w http.ResponseWriter, r *http.Request) {
										peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
										if err != nil {
											t.Error(err)
											return
										}
										defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
										_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
										if _, _, err = peer.ReadMessage(); err != nil {
											t.Error(err)
											return
										}
										attempts.Add(1)
										if err = peer.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
											t.Error(err)
											return
										}
										if _, _, err = peer.ReadMessage(); err != nil {
											peerClosed <- struct{}{}
										}
									},
								),
							)
							defer server.Close()
							auth = runtimeCodexAuth(server.URL)
						} else {
							ctx = util.WithRoundTripper(
								ctx, &auditClaudeContractTransport{
									respond: func(
										request *http.Request,
									) (*http.Response, error) {
										attempts.Add(1)
										status, body := http.StatusOK, "data: "+payload+"\n\n"
										if transport == "http" {
											status, body = http.StatusBadRequest, payload
										}
										return &http.Response{
											StatusCode: status, Request: request,
											Body:   io.NopCloser(strings.NewReader(body)),
											Header: http.Header{"Content-Type": {"text/event-stream"}},
										}, nil
									},
								},
							)
						}
						req := core.Request{
							Model: "gpt-5.5",
							Payload: []byte(
								`{"model":"gpt-5.5","messages":[{"role":"user","content":"synthetic"}]}`,
							),
						}
						opts := core.Options{
							SourceFormat: translator.FormatClaude,
							Metadata: map[string]any{
								core.ExecutionSessionMetadataKey: "synthetic-cleanup-" + t.Name(),
							},
						}
						err := codexConsumerError(t, ctx, auth, req, opts, transport == "websocket", stream)
						canonical := strings.Replace(
							payload, `"code":"invalid_encrypted_content"`,
							`"code":"thinking_signature_invalid","type":"invalid_request_error"`, 1,
						)
						assertCodexConsumerError(t, err, canonical, http.StatusBadRequest, nil)
						if attempts.Load() != 1 || client.deletes.Load() != 1 {
							t.Fatalf(
								"generation attempts=%d, replay deletes=%d, want one each", attempts.Load(),
								client.deletes.Load(),
							)
						}
						if transport == "websocket" {
							select {
							case <-peerClosed:
							case <-time.After(time.Second):
								t.Fatal("failed named websocket remained open")
							}
						}
						warnings := 0
						for _, entry := range hook.AllEntries() {
							if strings.Contains(entry.Message, "synthetic-cleanup-secret") {
								t.Error("cleanup failure logged sensitive error detail")
							}
							if entry.Level == log.WarnLevel &&
								entry.Message == "codex reasoning replay cache delete failed" {
								warnings++
							}
						}
						if failDelete && warnings != 1 || !failDelete && warnings != 0 {
							t.Errorf("cleanup warnings=%d, delete failure=%t", warnings, failDelete)
						}
					},
				)
			}
		}
	}
}

func TestCodexConnectionLimitManagerKeepsRequestScope(t *testing.T) {
	const payload = `{"type":"error","status":429,"body":{"error":{"code":"` +
		`websocket_connection_limit_reached","type":"server_error","message":"too many websocket connections"` +
		`}},"headers":{"retry-after":"1","x-request-id":"req-1"}}`
	for _, stream := range []bool{false, true} {
		t.Run(
			fmt.Sprint(stream), func(t *testing.T) {
				cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				ctx = home.WithClientScope(
					translator.WithPluginHooks(ctx, nil), home.ClientScope{ID: t.Name(), Enabled: false},
				)
				var attempts atomic.Int32
				transport := &auditClaudeContractTransport{
					respond: func(request *http.Request) (*http.Response, error) {
						attempts.Add(1)
						return &http.Response{
							StatusCode: http.StatusOK, Request: request,
							Body: io.NopCloser(strings.NewReader("data: " + payload + "\n\n")),
							Header: http.Header{
								"Content-Type": {"text/event-stream"},
								"X-Request-Id": {"req-1"},
							},
						}, nil
					},
				}
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
				manager.SetConfig(cfg)
				manager.SetRetryConfig(2, 0, 1)
				manager.SetRoundTripperProvider(codexReplayTransportProvider{transport: transport})
				manager.RegisterExecutor(NewCodexAutoExecutor(cfg))
				var identities []string
				for _, id := range []string{"aa-connection-primary", "bb-connection-backup"} {
					auth := runtimeCodexAuth("https://codex-connection-limit.invalid/v1")
					auth.ID, auth.Status = id, coreauth.StatusActive
					registered, err := manager.Register(coreauth.WithSkipPersist(ctx), auth)
					if err != nil {
						t.Fatal(err)
					}
					identities = append(identities, registered.ID)
					registry.GetGlobalRegistry().RegisterClient(
						registered.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.5"}},
					)
					manager.RefreshSchedulerEntry(registered.ID)
				}
				t.Cleanup(
					func() {
						for _, id := range identities {
							registry.GetGlobalRegistry().UnregisterClient(id)
						}
						_ = manager.StopDeferredAuthRecreates(context.Background())
					},
				)
				req := core.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)}
				opts := core.Options{SourceFormat: translator.FormatOpenAIResponse}
				var err error
				if stream {
					var result *core.StreamResult
					result, err = manager.ExecuteStream(ctx, []string{"codex"}, req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = manager.Execute(ctx, []string{"codex"}, req, opts)
				}
				var status interface{ StatusCode() int }
				var retry interface{ RetryAfter() *time.Duration }
				var typed core.ExecutionDispositionError
				var headers interface{ Headers() http.Header }
				if !errors.As(err, &status) || status.StatusCode() != 429 || !errors.As(
					err, &retry,
				) || retry.RetryAfter() == nil || *retry.RetryAfter() != 0 || !errors.As(err, &typed) || !errors.As(
					err, &headers,
				) || headers.Headers().Get("X-Request-Id") != "req-1" {
					t.Fatalf(
						"Manager/SDK lost the original connection-limit status, zero hint, headers or typed state: %v",
						err,
					)
				}
				disposition := typed.ExecutionDisposition()
				if disposition.RetryDirective != core.RetryDirectiveNoRetry ||
					disposition.EffectScope != core.StateEffectScopeRequest ||
					attempts.Load() != 1 {
					t.Errorf(
						"connection limit changed credentials or cooling scope: disposition=%+v attempts=%d",
						disposition, attempts.Load(),
					)
				}
				for _, id := range identities {
					state, ok := manager.GetByID(id)
					if !ok || state.Unavailable || state.Quota.Exceeded || !state.NextRetryAfter.IsZero() {
						t.Fatalf("connection limit cooled credential %s: %+v", id, state)
					}
					if model := state.ModelStates[req.Model]; model != nil &&
						(model.Unavailable || model.Quota.Exceeded || !model.NextRetryAfter.IsZero()) {
						t.Fatalf("connection limit cooled model %s: %+v", id, model)
					}
				}
			},
		)
	}
}

func TestCodexCapacityManagerConsumer(t *testing.T) {
	for _, fixture := range []struct {
		name       string
		status     int
		body       string
		wantStatus int
	}{
		{"HTTP capacity", 400, `{"error":{"code":"model_at_capacity","message":"The model is at capacity"}}`, 429},
		{
			"SSE capacity", 200, "data: " + `{"type":"response.failed","status":400,"sequence_number":41,` +
				`"response":{"error":{"code":"model_at_capacity","message":"The model is at capacity"}}}` + "\n\n", 429,
		},
		{
			"SSE quota", 200, "data: " + `{"type":"response.failed",` +
				`"response":{"error":{"type":"usage_limit_reached","resets_in_seconds":7}}}` + "\n\n", 429,
		},
		{
			"SSE transient", 200, "data: " +
				`{"type":"error","error":{"type":"rate_limit_error","resets_in_seconds":7}}` + "\n\n", 429,
		},
		{
			"SSE unknown", 200, "data: " + `{"type":"response.failed","status":502,` +
				`"response":{"error":{"message":"synthetic unknown processing"}}}` + "\n\n", 502,
		},
	} {
		for _, stream := range []bool{false, true} {
			name := fixture.name
			if stream {
				name += "/stream"
			}
			t.Run(
				name, func(t *testing.T) {
					cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
					var attempts atomic.Int32
					transport := &auditClaudeContractTransport{
						respond: func(request *http.Request) (*http.Response, error) {
							attempts.Add(1)
							return &http.Response{
								StatusCode: fixture.status, Request: request,
								Body:   io.NopCloser(strings.NewReader(fixture.body)),
								Header: http.Header{"Content-Type": {"text/event-stream"}},
							}, nil
						},
					}
					ctx := home.WithClientScope(
						translator.WithPluginHooks(t.Context(), nil),
						home.ClientScope{ID: t.Name(), Enabled: false},
					)
					manager := coreauth.NewManager(nil, nil, nil)
					manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
					manager.SetConfig(cfg)
					manager.SetRetryConfig(0, 0, 1)
					manager.SetRoundTripperProvider(codexReplayTransportProvider{transport: transport})
					manager.RegisterExecutor(NewCodexAutoExecutor(cfg))
					auth := runtimeCodexAuth("https://codex-manager-consumer.invalid/v1")
					auth.ID = "synthetic-manager-" + t.Name()
					auth.Status = coreauth.StatusActive
					registered, err := manager.Register(coreauth.WithSkipPersist(ctx), auth)
					if err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(
						registered.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.5"}},
					)
					manager.RefreshSchedulerEntry(registered.ID)
					t.Cleanup(
						func() {
							registry.GetGlobalRegistry().UnregisterClient(registered.ID)
							_ = manager.StopDeferredAuthRecreates(context.Background())
						},
					)
					req := core.Request{
						Model:   "gpt-5.5",
						Payload: []byte(`{"model":"gpt-5.5","input":"synthetic"}`),
					}
					opts := core.Options{SourceFormat: translator.FormatOpenAIResponse}
					if stream {
						var result *core.StreamResult
						result, err = manager.ExecuteStream(ctx, []string{"codex"}, req, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						_, err = manager.Execute(ctx, []string{"codex"}, req, opts)
					}
					var status interface{ StatusCode() int }
					if err == nil || !errors.As(
						err, &status,
					) || status.StatusCode() != fixture.wantStatus || attempts.Load() != 1 {
						t.Fatalf("manager error=%v, status=%v, attempts=%d", err, status, attempts.Load())
					}
					state, ok := manager.GetByID(registered.ID)
					if !ok {
						t.Fatal("registered credential disappeared")
					}
					if fixture.wantStatus == 429 {
						model := state.ModelStates[req.Model]
						if model == nil || !model.Unavailable || !model.Quota.Exceeded ||
							model.EffectScope != core.StateEffectScopeQuotaFamily || model.NextRetryAfter.IsZero() {
							t.Fatalf("quota-family state=%+v", model)
						}
					} else {
						var typed core.ExecutionDispositionError
						if !errors.As(
							err, &typed,
						) || typed.ExecutionDisposition().Outcome != core.ExecutionOutcomeUnknown ||
							typed.ExecutionDisposition().RetryDirective != core.RetryDirectiveNoRetry {
							t.Fatalf("unknown processing result changed: %v", err)
						}
					}
				},
			)
		}
	}
}
