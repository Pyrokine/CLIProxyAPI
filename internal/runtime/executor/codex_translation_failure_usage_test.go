package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestCodexTranslationRejectionPublishesFailureUsage(t *testing.T) {
	format := sdktranslator.Format("codex-contract-rejected-response")
	for _, upstream := range []sdktranslator.Format{sdktranslator.FormatCodex, sdktranslator.FormatOpenAIResponse} {
		sdktranslator.Register(format, upstream, nil, sdktranslator.ResponseTransform{
			NonStream: func(_ context.Context, _ string, _, _, _ []byte, state *any) []byte {
				*state = runtimeCodexTranslationState{}
				return nil
			},
		})
	}
	for _, mode := range []string{"http-terminal", "http-recovered", "websocket", "compact"} {
		t.Run(mode, func(t *testing.T) {
			capture := newCodexContractUsageCapture(t)
			completed := `{"type":"response.completed","response":{"id":"synthetic-rejected","model":"gpt-5.5","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12},"tool_usage":{"image_gen":{"total_tokens":30}}}}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "websocket" {
					peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer peer.Close()
					if _, _, err = peer.ReadMessage(); err != nil {
						t.Error(err)
						return
					}
					if err = peer.WriteMessage(websocket.TextMessage, []byte(completed)); err != nil {
						t.Error(err)
					}
					return
				}
				if mode == "compact" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, gjson.Get(completed, "response").Raw)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if mode == "http-recovered" {
					_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"complete\"}\n\ndata: {\"type\":\"response.content_part.done\"}\n\n")
					return
				}
				_, _ = fmt.Fprintf(w, "data: %s\n\n", completed)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if mode == "websocket" {
				ctx = core.WithDownstreamWebsocket(ctx)
			}
			auth := runtimeCodexAuth(server.URL)
			auth.ID = capture.authID
			opts := core.Options{SourceFormat: sdktranslator.FormatCodex, ResponseFormat: format}
			if mode == "compact" {
				opts.Alt = "responses/compact"
			}
			response, err := NewCodexAutoExecutor(&config.Config{}).Execute(ctx, auth, core.Request{
				Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"synthetic"}`),
			}, opts)
			if err == nil || len(response.Payload) != 0 {
				t.Fatalf("response=%s, error=%v", response.Payload, err)
			}
			if record := capture.next(t); !record.Failed || record.Model != "gpt-5.5" {
				t.Fatalf("translation rejection recorded as success: %+v", record)
			}
		})
	}
}
