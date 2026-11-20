package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexReasoningReplayFinalDoneKeepsCredentialBoundary(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, switchAuth := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("stream=%t/switch_auth=%t", stream, switchAuth), func(t *testing.T) {
					internalcache.ClearCodexReasoningReplayCache()
					t.Cleanup(internalcache.ClearCodexReasoningReplayCache)
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					t.Cleanup(cancel)
					ctx = home.WithClientScope(ctx, home.ClientScope{ID: t.Name(), Enabled: false})
					added := validCodexReplayEncryptedContentForTest(1)
					done := validCodexReplayEncryptedContentForTest(2)
					session := "session-1"
					firstID, secondID := "auth-replay-1", "auth-replay-1"
					firstKey, secondKey := "test", "test"
					if switchAuth {
						done = validCodexReplayEncryptedContentForTest(12)
						session = "session-auth-switch"
						firstID, secondID = "auth-replay-session-auth-a", "auth-replay-session-auth-b"
						firstKey, secondKey = "test-a", "test-b"
					}
					var bodies [][]byte
					ctx = util.WithRoundTripper(
						ctx, devinTestRoundTripper(
							func(r *http.Request) (*http.Response, error) {
								body, err := io.ReadAll(r.Body)
								if err != nil {
									return nil, err
								}
								bodies = append(bodies, body)
								wantKey := firstKey
								if len(bodies) == 2 {
									wantKey = secondKey
								}
								if r.Header.Get("Authorization") != "Bearer "+wantKey {
									return nil, fmt.Errorf(
										"request %d did not use its selected credential", len(bodies),
									)
								}
								response := `data: {"type":"response.output_item.added","item":{"id":"rs_added",` +
									(`"type":"reasoning","status":"in_progress","summary":[],` +
										`"encrypted_content":` +
										`"`) +
									added +
									`"},"output_index":0}` + "\n\n" +
									(`data: {"type":"response.output_item.done","item":{"id":"rs_done",` +
										`"type":"reasoning",`) +
									`"summary":[],"encrypted_content":"` + done + `"},"output_index":0}` + "\n\n" +
									(`data: {"type":"response.completed","response":{"id":"resp_1","object":` +
										`"response",`) +
									`"created_at":0,"status":"completed","model":"gpt-5.4","output":[],` +
									`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
								return &http.Response{
									StatusCode: http.StatusOK, Request: r,
									Header: http.Header{"Content-Type": {"text/event-stream"}},
									Body:   io.NopCloser(strings.NewReader(response)),
								}, nil
							},
						),
					)
					executor := newCodexExecutor(&config.Config{})
					for i, turn := range []struct{ text, id, key string }{
						{"hello", firstID, firstKey}, {"next", secondID, secondKey},
					} {
						payload := fmt.Sprintf(
							`{"model":"gpt-5.4","metadata":{"user_id":`+
								`"{\"device_id\":\"device-test\",\"account_uuid\":\"\",\"session_id\":\"%s\"}"},`+
								`"messages":[{"role":"user","content":[{"type":"text","text":"%s"}]}]}`, session,
							turn.text,
						)
						req := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(payload)}
						auth := &cliproxyauth.Auth{
							ID: turn.id,
							Attributes: map[string]string{
								"base_url": "https://codex-replay.invalid", "api_key": turn.key,
							},
						}
						opts := cliproxyexecutor.Options{
							SourceFormat: sdktranslator.FormatClaude, Stream: stream && i == 0,
						}
						if opts.Stream {
							result, err := executor.ExecuteStream(ctx, auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
							}
						} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
							t.Fatal(err)
						}
						if i == 0 {
							scope := codexReasoningReplayScopeFromRequest(
								ctx, auth, opts.SourceFormat, req, opts, bodies[0],
							)
							items, found, err := internalcache.GetCodexReasoningReplayItemsRequired(
								ctx, scope.modelName, scope.sessionKey,
							)
							if err != nil || !found {
								t.Fatalf(
									"first completion did not publish replay: found=%t error=%v",
									found,
									err,
								)
							}
							foundDone := false
							for _, item := range items {
								if gjson.GetBytes(item, "type").String() == "reasoning" {
									foundDone = true
									if gjson.GetBytes(item, "encrypted_content").String() != done {
										t.Fatalf("replay cached an unfinished reasoning item: %s", item)
									}
								}
							}
							if !foundDone {
								t.Fatal("replay did not cache the final reasoning item")
							}
						}
					}
					if len(bodies) != 2 {
						t.Fatalf("upstream request count = %d, want 2", len(bodies))
					}
					if switchAuth {
						if gjson.GetBytes(
							bodies[1], "input.0.role",
						).String() != "user" || strings.Contains(string(bodies[1]), done) {
							t.Fatalf(
								"next credential received the previous credential's reasoning: %s",
								bodies[1],
							)
						}
					} else if gjson.GetBytes(bodies[1], "input.0.type").String() != "reasoning" ||
						gjson.GetBytes(bodies[1], "input.0.encrypted_content").String() != done ||
						gjson.GetBytes(bodies[1], "input.1.role").String() != "user" {
						t.Fatalf("next request lost final reasoning for the same credential: %s", bodies[1])
					}
				},
			)
		}
	}
}
