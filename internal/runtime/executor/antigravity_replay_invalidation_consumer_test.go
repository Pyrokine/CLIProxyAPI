package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	homekv "github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

func TestAntigravityReplayInvalidationFailureKeepsActualManagerResult(t *testing.T) {
	const nativeID = "native-reused-response"
	const args = `{"file_path":"/tmp/a"}`
	stableID := util.GeminiClaudeToolUseID(nativeID, "Read", args)
	//goland:noinspection SpellCheckingInspection
	payload := []byte(`{"sessionId":"sess-replay-pairing-failure",` +
		`"request":{"contents":[` +
		`{"role":"model","parts":[{"functionCall":{"name":"unknown",` +
		`"args":{"phase":1}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"id":"` +
		stableID +
		`","name":"unknown","response":{"result":"old"}}}]},` +
		`{"role":"model","parts":[{"thoughtSignature":"skip_thought_s` +
		`ignature_validator",` +
		`"functionCall":{"id":"` + stableID + `","name":"Read","args":` + args + `}}]},` +
		(`{"role":"user","parts":[{"functionResponse":{"id":` +
			`"`) +
		stableID +
		(`","name":"Read","response":` +
			`{"result":"new"}}}]}`) +
		`]}}`)
	if err := signature.ValidateGeminiFunctionCallPairing(payload); err != nil {
		t.Fatalf("original payload pairing: %v", err)
	}
	item := []byte(`{"type":"function_call_part","contentIndex":2,"partIndex":0,` +
		`"targetOccurrence":0,"name":"Read","call_id":"` +
		nativeID +
		`","args":` +
		args +
		`,"thoughtSignature":"pairing-failure-signature-123456789"}`)
	oldValue, err := json.Marshal([][]byte{item})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct {
		name, model, path string
		stream            bool
	}{
		{"generate", "gemini-3.6-flash-high", antigravityGeneratePath, false},
		{"claude nonstream route", "gemini-3-pro-preview", antigravityStreamPath, false},
		{"stream", "gemini-3.6-flash-high", antigravityStreamPath, true},
	} {
		for _, homeDispatch := range []bool{false, true} {
			for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
				t.Run(
					fmt.Sprintf("%s/home=%t/status=%d", route.name, homeDispatch, status), func(t *testing.T) {
						const privateError = "synthetic-private-cache-error"
						credential := antigravityCountTokensAuth("https://synthetic.invalid", t.Name())
						credential.Status = coreauth.StatusActive
						home := &dispatchedPrimaryErrorHome{
							primaryErrorHome: &primaryErrorHome{
								t: t, value: bytes.Clone(oldValue),
								casErr: fmt.Errorf("%s: %w", privateError, homekv.ErrNotConnected),
							},
							credential: credential,
						}
						cfg := &config.Config{Home: config.HomeConfig{Enabled: homeDispatch}}
						hook := &primaryErrorResults{}
						manager := coreauth.NewManager(nil, nil, hook)
						t.Cleanup(
							func() {
								if err := manager.StopDeferredAuthRecreates(context.Background()); err != nil {
									t.Error(err)
								}
							},
						)
						manager.SetConfig(cfg)
						manager.SetHomeClientProvider(func() coreauth.HomeClient { return home })
						manager.RegisterExecutor(NewAntigravityExecutor(cfg))
						inflight := executionregistry.New()
						releases := make(chan executionregistry.ReleaseGroup, 2)
						if homeDispatch {
							inflight.SetReleaseSink(
								func(
									group executionregistry.ReleaseGroup,
									_ int64,
								) {
									releases <- group
								},
							)
							manager.PublishHomeDispatch(home, inflight, 1)
						} else {
							if _, err := manager.Register(
								coreauth.WithSkipPersist(t.Context()), credential,
							); err != nil {
								t.Fatal(err)
							}
							models := registry.GetGlobalRegistry()
							models.RegisterClient(
								credential.ID, "antigravity", []*registry.ModelInfo{{ID: route.model}},
							)
							t.Cleanup(func() { models.UnregisterClient(credential.ID) })
						}
						logger := log.StandardLogger()
						oldLevel := logger.GetLevel()
						logger.SetLevel(log.WarnLevel)
						capture := &logtest.Hook{}
						oldHooks := logger.ReplaceHooks(log.LevelHooks{})
						logger.AddHook(capture)
						t.Cleanup(func() { logger.ReplaceHooks(oldHooks); logger.SetLevel(oldLevel) })
						requests := 0
						ctx := util.WithRoundTripper(
							t.Context(),
							antigravityCountTokensRoundTripFunc(
								func(request *http.Request) (*http.Response, error) {
									requests++
									wire, err := io.ReadAll(request.Body)
									if err != nil {
										return nil, err
									}
									_ = request.Body.Close()
									if request.URL.Host != "synthetic.invalid" ||
										request.URL.Path != route.path {
										t.Error("request reached an unexpected endpoint")
									}
									if home.casCalls != 1 || !bytes.Equal(home.value, oldValue) {
										t.Error(
											"request did not continue after one failed snapshot invalidation",
										)
									}
									contents := gjson.GetBytes(wire, "request.contents").Array()
									original := gjson.GetBytes(payload, "request.contents").Array()
									if len(contents) < len(original) {
										t.Fatalf("original history was removed: %s", wire)
									}
									contents = contents[len(contents)-len(original):]
									for i := range original {
										for _, kind := range []string{"functionCall", "functionResponse"} {
											path := "parts.0." + kind
											if contents[i].Get(path).Raw != original[i].Get(path).Raw {
												t.Fatalf(
													"original tool history changed at content %d: %s",
													i,
													wire,
												)
											}
										}
									}
									if err := signature.ValidateGeminiFunctionCallPairing(wire); err != nil {
										t.Fatalf("continued payload pairing: %v", err)
									}
									body := `{"response":{"candidates":[{"content":{"role":"model",` +
										`"parts":[{"text":"ok"}]},` +
										`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,` +
										`"candidatesTokenCount":1,"totalTokenCount":2}}}`
									contentType := "application/json"
									if route.path == antigravityStreamPath {
										body, contentType = "data: "+body+"\n\n", "text/event-stream"
									}
									if status == http.StatusBadRequest {
										body, contentType = `{"error":{"code":400,`+
											`"message":"synthetic provider request failure"}}`, "application/json"
									}
									return &http.Response{
										StatusCode: status,
										Header:     http.Header{"Content-Type": {contentType}},
										Body:       io.NopCloser(strings.NewReader(body)),
										Request:    request,
									}, nil
								},
							),
						)
						request := core.Request{Model: route.model, Payload: bytes.Clone(payload)}
						opts := core.Options{Stream: route.stream, SourceFormat: translator.FormatAntigravity}
						var executeErr error
						if route.stream {
							var stream *core.StreamResult
							stream, executeErr = manager.ExecuteStream(
								ctx,
								[]string{"antigravity"},
								request,
								opts,
							)
							if executeErr == nil {
								for chunk := range stream.Chunks {
									if chunk.Acknowledge != nil {
										chunk.Acknowledge(nil)
									}
									if chunk.Err != nil {
										executeErr = chunk.Err
									}
								}
							}
						} else {
							_, executeErr = manager.Execute(ctx, []string{"antigravity"}, request, opts)
						}
						success := status == http.StatusOK
						if (executeErr == nil) != success || errors.Is(
							executeErr, homekv.ErrNotConnected,
						) || requests != 1 {
							t.Fatalf(
								"actual provider result changed: status=%d requests=%d error=%v",
								status,
								requests,
								executeErr,
							)
						}
						if len(hook.results) != 1 || hook.results[0].Success != success {
							t.Fatalf("Manager final results: %+v", hook.results)
						}
						if !success &&
							(hook.results[0].Error == nil || hook.results[0].Error.HTTPStatus != status) {
							t.Fatalf("Manager lost actual provider status: %+v", hook.results[0])
						}
						if !bytes.Equal(home.value, oldValue) || !home.HeartbeatOK() {
							t.Error("failed invalidation changed the stored version or Home availability")
						}
						if homeDispatch {
							if home.dispatches != 1 ||
								len(inflight.FreezeInFlight(time.Now()).Executions) != 0 ||
								len(releases) != 1 {
								t.Fatal("final provider result did not release the single Home admission")
							}
						} else {
							after, _ := manager.GetByID(credential.ID)
							if after == nil || after.Unavailable || after.Disabled ||
								after.LastError != nil || !after.NextRetryAfter.IsZero() {
								t.Fatal("replay invalidation changed credential availability")
							}
							if success &&
								(after.Success !=
									1 ||
									after.Failed !=
										0) ||
								!success &&
									(after.Success !=
										0 ||
										after.Failed !=
											1) {
								t.Fatal("credential accounting did not use the actual provider result")
							}
						}
						invalidations := 0
						for _, entry := range capture.AllEntries() {
							if strings.Contains(entry.Message, "reasoning replay invalidate failed") {
								invalidations++
							}
							if strings.Contains(entry.Message, privateError) || strings.Contains(
								fmt.Sprint(entry.Data), privateError,
							) {
								t.Error("cache diagnostic exposed the raw Home error")
							}
						}
						if invalidations != 1 {
							t.Fatalf("invalidation warnings=%d, want one", invalidations)
						}
					},
				)
			}
		}
	}
}
