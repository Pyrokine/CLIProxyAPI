package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type managedXAIReplayTestExecutor struct {
	*XAIExecutor
	ctx   context.Context
	scope xaiReasoningReplayScope
}

func (e *managedXAIReplayTestExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.ctx = ctx
	e.scope = xaiReasoningReplayScopeFromRequest(ctx, auth, opts.SourceFormat, req, opts, req.Payload)
	previous := []byte(`{"type":"function_call","call_id":"previous-call","name":"lookup","arguments":"{}"}`)
	if status := internalcache.StoreXAIReasoningReplayItems(ctx, e.scope.modelName, e.scope.sessionKey, [][]byte{previous}); status != internalcache.XAIReasoningReplayStored {
		return nil, fmt.Errorf("seed replay cache: status=%v", status)
	}
	return e.XAIExecutor.ExecuteStream(ctx, auth, req, opts)
}

func TestXAIManagedStreamReplayRequiresTerminalAcknowledgement(t *testing.T) {
	for _, outcome := range []string{"success", "delivery failure", "cancellation"} {
		t.Run(outcome, func(t *testing.T) {
			internalcache.ClearXAIReasoningReplayCache()
			t.Cleanup(internalcache.ClearXAIReasoningReplayCache)
			ctx, cancel := context.WithCancel(cliproxyexecutor.WithStreamDeliveryAcknowledgement(xaiC03ContextWithAPIKey("transport-ack-caller")))
			defer cancel()
			const terminal = `{"type":"response.completed","response":{"id":"fixture-response","status":"completed","output":[{"type":"function_call","call_id":"completed-call","name":"lookup","arguments":"{}"}],"usage":{"input_tokens":8,"output_tokens":3,"total_tokens":11}}}`
			ctx = util.WithRoundTripper(ctx, xaiMediaRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "xai.test" {
					return nil, fmt.Errorf("unexpected host %s", req.URL.Host)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader("data: " + terminal + "\n\n")), Request: req}, nil
			}))
			wrapped := &managedXAIReplayTestExecutor{XAIExecutor: newXAIExecutor(&config.Config{})}
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.SetHomeClientProvider(func() cliproxyauth.HomeClient { return nil })
			manager.RegisterExecutor(wrapped)
			auth := &cliproxyauth.Auth{ID: "transport-ack-auth", Provider: "xai", Status: cliproxyauth.StatusActive,
				Attributes: map[string]string{"api_key": "offline-fixture", "base_url": "https://xai.test"}}
			if _, err := manager.Register(ctx, auth); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "grok-4.6"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			payload := []byte(`{"model":"grok-4.6","prompt_cache_key":"transport-ack-session","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"input":[{"role":"user","content":"hi"}]}`)
			result, err := manager.ExecuteStream(ctx, []string{"xai"}, cliproxyexecutor.Request{Model: "grok-4.6", Payload: payload},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			assertCachedCall := func(want string) {
				t.Helper()
				items, exists, errCache := internalcache.GetXAIReasoningReplayItemsRequired(wrapped.ctx, wrapped.scope.modelName, wrapped.scope.sessionKey)
				if errCache != nil {
					t.Fatal(errCache)
				}
				if !exists || len(items) != 1 || gjson.GetBytes(items[0], "call_id").String() != want {
					t.Fatalf("cached call: exists=%t items=%q want=%s", exists, items, want)
				}
			}
			deadline := time.After(3 * time.Second)
			sawTerminal := false
			for !sawTerminal {
				select {
				case chunk, open := <-result.Chunks:
					if !open || chunk.Err != nil || chunk.Acknowledge == nil {
						t.Fatalf("invalid stream chunk: open=%t err=%v acknowledged=%t", open, chunk.Err, chunk.Acknowledge != nil)
					}
					assertCachedCall("previous-call")
					if !chunk.Terminal {
						chunk.Acknowledge(nil)
						continue
					}
					sawTerminal = true
					switch outcome {
					case "delivery failure":
						chunk.Acknowledge(errors.New("fixture final write or flush failed"))
					case "cancellation":
						cancel()
						chunk.Acknowledge(nil)
					case "success":
						chunk.Acknowledge(nil)
					}
				case <-deadline:
					t.Fatal("timed out receiving transport terminal")
				}
			}
			for range result.Chunks {
			}
			want := "previous-call"
			if outcome == "success" {
				want = "completed-call"
			}
			assertCachedCall(want)
		})
	}
}
