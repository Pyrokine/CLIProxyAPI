package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestAuditXAIAggregatedReasoningAcrossPublicTransports(t *testing.T) {
	for _, transport := range []string{"http-execute", "http-stream", "websocket-stream"} {
		for _, separateItem := range []bool{false, true} {
			for _, shape := range []string{"content", "summary", "mixed"} {
				t.Run(
					fmt.Sprintf("%s/item=%t/%s", transport, separateItem, shape), func(t *testing.T) {
						proof := validGrokEncryptedContentForExecutorTest()
						fields := `"content":[{"type":"reasoning_text","text":"first","note":"retained"},` +
							`{"type":"reasoning_text","text":` +
							`"second"}]`
						if shape == "summary" {
							fields = `"summary":[{"type":"reasoning_text","text":"first","note":"retained"},` +
								`{"type":"summary_text","text":` +
								`"second"}]`
						} else if shape == "mixed" {
							fields = `"summary":[{"type":"summary_text","text":"first","note":"retained"}],"` +
								`content":[{"type":"reasoning_text","text":"second"},{"type":"opaque","` +
								`value":9007199254740993}]`
						}
						item := fmt.Sprintf(
							`{"id":"reasoning-a","type":"reasoning","encrypted_content":%q,%s}`, proof, fields,
						)
						output := "[" + item + "]"
						var events []string
						if separateItem {
							events = append(
								events, `{"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
							)
							output = "[]"
						}
						events = append(
							events,
							`{"type":"response.completed","response":{"id":"synthetic-summary",`+
								`"status":"completed","model":"grok-4.3","output":`+
								output+
								(`,"usage":{"input_tokens":9007199254740993,"output_tokens":2,"total_tokens":`+
									`9007199254740995}}}`),
						)
						server := httptest.NewServer(
							http.HandlerFunc(
								func(w http.ResponseWriter, r *http.Request) {
									if websocket.IsWebSocketUpgrade(r) {
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
										for _, event := range events {
											if err = peer.WriteMessage(
												websocket.TextMessage, []byte(event),
											); err != nil {
												t.Error(err)
												return
											}
										}
										return
									}
									w.Header().Set("Content-Type", "text/event-stream")
									for _, event := range events {
										_, _ = io.WriteString(w, "data: "+event+"\n\n")
									}
								},
							),
						)
						defer server.Close()
						credential := &auth.Auth{
							ID: t.Name(), Provider: "xai",
							Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL},
						}
						ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
						defer cancel()
						request := core.Request{
							Model: "grok-4.3", Payload: []byte(`{"model":"grok-4.3","input":"synthetic"}`),
						}
						opts := core.Options{
							SourceFormat:   translator.FormatOpenAIResponse,
							ResponseFormat: translator.FormatOpenAIResponse,
						}
						var execute auth.ProviderExecutor = newXAIExecutor(&config.Config{})
						if transport == "websocket-stream" {
							execute = newXAIWebsocketsExecutor(&config.Config{})
						}
						assertItem := func(value gjson.Result) {
							if value.Get("summary.#").Int() != 2 ||
								value.Get("summary.0.type").String() != "summary_text" ||
								value.Get("summary.1.type").String() != "summary_text" ||
								value.Get("summary.0.text").String() != "first" ||
								value.Get("summary.1.text").String() != "second" ||
								value.Get("summary.0.note").String() != "retained" ||
								value.Get("encrypted_content").String() != proof {
								t.Fatalf("aggregated reasoning changed: %s", value.Raw)
							}
							if shape == "mixed" {
								if value.Get("content.#").Int() != 1 ||
									value.Get("content.0.type").String() != "opaque" ||
									value.Get("content.0.value").Raw != "9007199254740993" {
									t.Fatalf("non-reasoning content changed: %s", value.Raw)
								}
							} else if value.Get("content").Exists() {
								t.Fatalf("reasoning text remained in content: %s", value.Raw)
							}
						}
						var response []byte
						if transport == "http-execute" {
							result, err := execute.Execute(ctx, credential, request, opts)
							if err != nil {
								t.Fatal(err)
							}
							response = result.Payload
						} else {
							result, err := execute.ExecuteStream(ctx, credential, request, opts)
							if err != nil {
								t.Fatal(err)
							}
							for chunk := range result.Chunks {
								if chunk.Acknowledge != nil {
									chunk.Acknowledge(nil)
								}
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
								for line := range bytes.SplitSeq(chunk.Payload, []byte("\n")) {
									line = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
									switch gjson.GetBytes(line, "type").String() {
									case "response.output_item.done":
										assertItem(gjson.GetBytes(line, "item"))
									case "response.completed":
										response = []byte(gjson.GetBytes(line, "response").Raw)
									}
								}
							}
						}
						assertItem(gjson.GetBytes(response, "output.0"))
						if gjson.GetBytes(response, "status").String() != "completed" || gjson.GetBytes(
							response, "usage.input_tokens",
						).Raw != "9007199254740993" || strings.Contains(string(response), `"reasoning_text"`) {
							t.Fatalf("final response metadata changed: %s", response)
						}
					},
				)
			}
		}
	}
}
