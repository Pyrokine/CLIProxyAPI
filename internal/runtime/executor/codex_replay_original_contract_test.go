package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexOriginalReplaySessionInputs(t *testing.T) {
	for _, sample := range []struct{ name, userID, want string }{
		{
			"JSON session", `{"device_id":"device-a","account_uuid":"","session_id":"session-json-1"}`,
			"claude:session-json-1:agent:main",
		},
		{"bare user ID", "same-user-across-chats", ""},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				request := core.Request{
					Model: "gpt-5.4", Payload: []byte(fmt.Sprintf(
						`{"metadata":{"user_id":%q},"messages":[{"role":"user","content":"next"}]}`,
						sample.userID,
					)),
				}
				if got := codexReasoningReplaySessionKey(
					t.Context(), translator.FormatClaude, request, core.Options{SourceFormat: translator.FormatClaude},
					[]byte(`{"model":"gpt-5.4","input":[]}`),
				); got != sample.want {
					t.Fatalf("session key = %q, want %q", got, sample.want)
				}
			},
		)
	}
	for _, name := range []string{"Session_id", "session_id", "Session-Id"} {
		if got := codexReasoningReplaySessionKeyFromHeaders(http.Header{name: {"session-alias"}}); got !=
			"session-id:session-alias" {
			t.Fatalf("%s key = %q", name, got)
		}
	}
	payloadKey := codexReasoningReplaySessionKeyFromPayload(
		[]byte(`{"client_metadata":{"x-codex-window-id":"window-1"}}`),
	)
	headerKey := codexReasoningReplaySessionKeyFromHeaders(http.Header{"X-Codex-Window-Id": {"window-1"}})
	if payloadKey != "window:window-1" || payloadKey != headerKey {
		t.Fatalf("window keys = %q / %q", payloadKey, headerKey)
	}
}

func TestCodexOriginalReplayStorageAndHistory(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, scenario := range []string{
			"native-inject", "native-store", "client-reasoning", "assistant-history", "tool-result", "short-tool-res" +
				"ult",
		} {
			t.Run(
				fmt.Sprintf("stream=%t/%s", stream, scenario), func(t *testing.T) {
					internalcache.ClearCodexReasoningReplayCache()
					t.Cleanup(internalcache.ClearCodexReasoningReplayCache)
					cached := validCodexReplayEncryptedContentForTest(5)
					client := validCodexReplayEncryptedContentForTest(6)
					callID, resultID := "call_1", "call_1"
					if scenario == "short-tool-result" {
						callID = "call_" + strings.Repeat("a", 62)
						sum := sha256.Sum256([]byte(callID))
						resultID = callID[:47] + "_" + hex.EncodeToString(sum[:8])
					}
					messages := `[{"role":"user","content":"first"},{"role":"assistant","content":` +
						`"answer"},{"role":"user","content":` +
						`"next"}]`
					if scenario == "client-reasoning" {
						messages = `[{"role":"assistant","content":[{"type":"thinking","thinking":"client ` +
							`summary","signature":` +
							`"` +
							client +
							`"},{"type":"text","text":"answer"}]},{"role":"user","content":"next"}]`
					} else if strings.HasSuffix(scenario, "tool-result") {
						messages = `[{"role":"user","content":"call lookup"},{"role":"user","content":` +
							`[{"type":"tool_result","tool_use_id":` +
							`"` +
							resultID +
							`","content":"sunny"}]}]`
					}
					request := core.Request{
						Model: "gpt-5.4",
						Payload: []byte(
							`{"metadata":{"user_id":"{\"device_id\":\"device-test\",\"account_uuid\` +
								`":\"\",\"session_id\":\"original-session\"}"},"messages":` +
								messages +
								(`,"tools":` +
									`[{"name":` +
									`"lookup",` +
									`"input_schema"` +
									`:{"type":` +
									`"object",` +
									`"properties":` +
									`{"q":{"type":` +
									`"string"}}}}]}`),
						),
					}
					options := core.Options{SourceFormat: translator.FormatClaude}
					if strings.HasPrefix(scenario, "native-") {
						request.Payload = []byte(
							`{"model":"gpt-5.4","prompt_cache_key":"native-session","input":` +
								`[{"role":"user","content":` +
								`"native"}]}`,
						)
						options.SourceFormat = translator.FormatOpenAIResponse
					}
					auth := &cliproxyauth.Auth{
						ID: t.Name(), Attributes: map[string]string{
							"api_key": "synthetic-upstream-key", "base_url": "https://original-replay.invalid",
						},
					}
					ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name(), Enabled: false})
					cacheScope := codexReasoningReplayScopeFromRequest(
						ctx, auth, translator.FormatClaude, request, options, request.Payload,
					)
					if !cacheScope.valid() {
						t.Fatal("original cache input has no model/session scope")
					}
					items := [][]byte{
						[]byte(
							`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"` + cached + `"}`,
						),
					}
					if scenario != "native-store" && !strings.HasSuffix(
						scenario, "tool-result",
					) && !internalcache.AppendCodexReasoningReplayItemsBestEffort(
						ctx, cacheScope.modelName, cacheScope.sessionKey, items,
					) {
						t.Fatal("original replay input was not stored")
					}
					var body []byte
					requestCount := 0
					ctx = util.WithRoundTripper(
						ctx, devinTestRoundTripper(
							func(upstream *http.Request) (*http.Response, error) {
								requestCount++
								var err error
								body, err = io.ReadAll(upstream.Body)
								if err != nil {
									return nil, err
								}
								output := "[]"
								if scenario == "native-store" {
									output = `[{"type":"reasoning","summary":[],"encrypted_content":"` + client + `"}]`
								}
								response := `data: {"type":"response.completed","response":{"id":` +
									`"response-fixture","status":"completed","output":` +
									output +
									`}}` +
									"\n\n"
								if strings.HasSuffix(scenario, "tool-result") && requestCount == 1 {
									response = `data: {"type":"response.output_item.done","item":{"type":` +
										`"reasoning","summary":[],"encrypted_content":` +
										`"` +
										cached +
										`"},"output_index":0}` +
										"\n\n" +
										(`data: {"type":"response.output_item.added","item":{"id":` +
											`"fc-fixture","type":"function_call","call_id":` +
											`"`) +
										callID +
										(`","name":` +
											`"lookup",` +
											`"arguments":` +
											`"{\"q\":` +
											`\"weather\"}",` +
											`"status":` +
											`"in_progress"}` +
											`,"output_index` +
											`":1}`) +
										"\n\n" +
										(`data: {"type":"response.output_item.done","item":{"id":"fc-fixture",` +
											`"type":"function_call","call_id":` +
											`"`) +
										callID +
										(`","name":` +
											`"lookup",` +
											`"arguments":` +
											`"{\"q\":` +
											`\"weather\"}",` +
											`"status":` +
											`"completed"},` +
											`"output_index"` +
											`:1}`) +
										"\n\n" +
										response
								}
								return &http.Response{
									StatusCode: 200, Request: upstream,
									Header: http.Header{"Content-Type": {"text/event-stream"}},
									Body:   io.NopCloser(strings.NewReader(response)),
								}, nil
							},
						),
					)
					executor := newCodexExecutor(&config.Config{})
					attempts := 1
					if strings.HasSuffix(scenario, "tool-result") {
						attempts = 2
					}
					for attempt := 0; attempt < attempts; attempt++ {
						selected := request
						if attempts == 2 && attempt == 0 {
							payload, err := sjson.SetRawBytes(
								request.Payload,
								"messages",
								[]byte(`[{"role":"user","content":"call lookup"}]`),
							)
							if err != nil {
								t.Fatal(err)
							}
							selected.Payload = payload
						}
						if stream {
							result, err := executor.ExecuteStream(ctx, auth, selected, options)
							if err != nil {
								t.Fatal(err)
							}
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
								if chunk.Acknowledge != nil {
									chunk.Acknowledge(nil)
								}
							}
						} else if _, err := executor.Execute(ctx, auth, selected, options); err != nil {
							t.Fatal(err)
						}
					}
					if requestCount != attempts {
						t.Fatalf("upstream request count = %d, want %d", requestCount, attempts)
					}
					input := gjson.GetBytes(body, "input").Array()
					switch scenario {
					case "native-inject", "native-store":
						if len(input) != 1 || input[0].Get("role").String() != "user" || strings.Contains(
							string(body), cached,
						) {
							t.Fatalf("native input received replay: %s", body)
						}
						if scenario == "native-store" {
							if _, found, err := internalcache.GetCodexReasoningReplayItemsRequired(
								ctx, cacheScope.modelName, cacheScope.sessionKey,
							); err != nil || found {
								t.Fatalf("native response populated replay: found=%t error=%v", found, err)
							}
						}
					case "client-reasoning":
						count := 0
						for _, item := range input {
							if item.Get("type").String() == "reasoning" {
								count++
								if item.Get("encrypted_content").String() != client {
									t.Fatalf("client reasoning was replaced: %s", body)
								}
							}
						}
						if count != 1 {
							t.Fatalf("reasoning count = %d, want one: %s", count, body)
						}
					case "assistant-history":
						if len(input) != 4 || input[0].Get("role").String() != "user" ||
							input[1].Get("encrypted_content").String() != cached ||
							input[2].Get("role").String() != "assistant" ||
							input[3].Get("role").String() != "user" {
							t.Fatalf("assistant history order changed: %s", body)
						}
					case "tool-result", "short-tool-result":
						if len(input) != 4 || input[0].Get("type").String() != "message" ||
							input[1].Get("encrypted_content").String() != cached ||
							input[2].Get("type").String() != "function_call" ||
							input[2].Get("call_id").String() != resultID ||
							input[2].Get("name").String() != "lookup" ||
							input[2].Get("arguments").String() != `{"q":"weather"}` ||
							input[3].Get("type").String() != "function_call_output" ||
							input[3].Get("call_id").String() != resultID {
							t.Fatalf("tool result lost its replayed call: %s", body)
						}
					}
				},
			)
		}
	}
}

func TestCodexOriginalReplayInvalidSignatureClearsOwnedScope(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
				internalcache.ClearCodexReasoningReplayCache()
				t.Cleanup(internalcache.ClearCodexReasoningReplayCache)
				ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name(), Enabled: false})
				auth := &cliproxyauth.Auth{
					ID: t.Name(), Attributes: map[string]string{
						"api_key": "synthetic-upstream-key", "base_url": "https://original-replay.invalid",
					},
				}
				request := core.Request{
					Model: "gpt-5.4",
					Payload: []byte(
						`{"metadata":{"user_id":"{\"session_id\":\"session-invalid\"}"},` +
							`"messages":[{"role":"user","content":` +
							`"next"}]}`,
					),
				}
				options := core.Options{SourceFormat: translator.FormatClaude}
				scope := codexReasoningReplayScopeFromRequest(
					ctx, auth, options.SourceFormat, request, options, []byte(`{"model":"gpt-5.4"}`),
				)
				item := []byte(
					`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"` +
						validCodexReplayEncryptedContentForTest(
							9,
						) + `"}`,
				)
				for _, key := range []string{scope.sessionKey, scope.sessionKey + ":other"} {
					if !internalcache.AppendCodexReasoningReplayItemsBestEffort(
						ctx, scope.modelName, key, [][]byte{item},
					) {
						t.Fatal("initial replay write failed")
					}
				}
				ctx = util.WithRoundTripper(
					ctx, devinTestRoundTripper(
						func(upstream *http.Request) (*http.Response, error) {
							response := "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\"," +
								"\"status\":\"failed\",\"error\":{\"message\":\"Invalid signature in " +
								"thinking block\",\"type\":\"invalid_request_error\",\"code\":\"" +
								"invalid_request_error\"}}}\n\n"
							return &http.Response{
								StatusCode: 200, Request: upstream,
								Header: http.Header{"Content-Type": {"text/event-stream"}},
								Body:   io.NopCloser(strings.NewReader(response)),
							}, nil
						},
					),
				)
				executor := newCodexExecutor(&config.Config{})
				var failure error
				if stream {
					result, err := executor.ExecuteStream(ctx, auth, request, options)
					if err != nil {
						failure = err
					} else {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								failure = chunk.Err
							}
						}
					}
				} else {
					_, failure = executor.Execute(ctx, auth, request, options)
				}
				if failure == nil || !strings.Contains(failure.Error(), "Invalid signature in thinking block") {
					t.Fatalf("original signature failure = %v", failure)
				}
				if _, found, err := internalcache.GetCodexReasoningReplayItemsRequired(
					ctx, scope.modelName, scope.sessionKey,
				); err != nil || found {
					t.Fatalf("owned replay survived invalid signature: found=%t error=%v", found, err)
				}
				if _, found, err := internalcache.GetCodexReasoningReplayItemsRequired(
					ctx, scope.modelName, scope.sessionKey+":other",
				); err != nil || !found {
					t.Fatalf("unrelated replay was removed: found=%t error=%v", found, err)
				}
			},
		)
	}
}
