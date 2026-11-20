package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
)

//goland:noinspection DuplicatedCode
func TestCodexTerminalQuotaCoolsAccountAcrossModels(t *testing.T) {
	for _, transport := range []string{"sse", "websocket-error", "websocket-failed"} {
		t.Run(
			transport, func(t *testing.T) {
				const model, siblingModel = "gpt-5.4", "gpt-5.4-mini"
				const family, siblingFamily = "codex-account", "codex-account"
				const created = `{"type":"response.created","response":{"id":"quota-test-response"}}`
				const quota = `{"type":"usage_limit_reached","message":"You've hit your usage ` +
					`limit.","resets_in_seconds":` +
					`3600}`
				const completed = `{"type":"response.completed","response":{"id":"quota-test-success","` +
					`status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"` +
					`total_tokens":2}}}`
				attempts := make(chan string, 8)
				upgrader := websocket.Upgrader{}
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
							attempts <- account
							terminal := completed
							if account == "quota-high" {
								terminal = `{"type":"error","status":429,"error":` + quota + `}`
								if transport == "websocket-failed" {
									terminal = `{"type":"response.failed","response":{"error":` + quota + `}}`
								}
							}
							if transport == "sse" {
								w.Header().Set("Content-Type", "text/event-stream")
								if _, errWrite := fmt.Fprintf(
									w, "data: %s\n\ndata: %s\n\n", created, terminal,
								); errWrite != nil {
									t.Errorf("write SSE: %v", errWrite)
								}
								return
							}
							conn, errUpgrade := upgrader.Upgrade(w, r, nil)
							if errUpgrade != nil {
								t.Errorf("upgrade websocket: %v", errUpgrade)
								return
							}
							defer func() {
								if errClose := conn.Close(); errClose != nil {
									t.Errorf("close websocket: %v", errClose)
								}
							}()
							if _, _, errRead := conn.ReadMessage(); errRead != nil {
								t.Errorf("read websocket request: %v", errRead)
								return
							}
							for _, event := range []string{created, terminal} {
								if errWrite := conn.WriteMessage(
									websocket.TextMessage, []byte(event),
								); errWrite != nil {
									t.Errorf("write websocket event: %v", errWrite)
									return
								}
							}
						},
					),
				)
				defer server.Close()
				manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
				manager.SetRetryConfig(0, 0, 0)
				manager.SetHomeClientProvider(func() cliproxyauth.HomeClient { return nil })
				cfg := &config.Config{}
				if transport == "sse" {
					manager.RegisterExecutor(newCodexExecutor(cfg))
				} else {
					manager.RegisterExecutor(newCodexWebsocketsExecutor(cfg))
				}
				highID, lowID := "quota-high-"+transport, "quota-low-"+transport
				for i, id := range []string{highID, lowID} {
					key := "quota-high"
					if i == 1 {
						key = "quota-low"
					}
					registry.GetGlobalRegistry().RegisterClient(
						id, "codex", []*registry.ModelInfo{{ID: model}, {ID: siblingModel}},
					)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					if _, errRegister := manager.Register(
						context.Background(), &cliproxyauth.Auth{
							ID: id, Provider: "codex", Status: cliproxyauth.StatusActive,
							Attributes: map[string]string{
								"priority": fmt.Sprint(4 - i), "base_url": server.URL, "api_key": key,
							},
							Metadata: map[string]any{"disable_cooling": false},
							ModelStates: map[string]*cliproxyauth.ModelState{
								model: {
									Status: cliproxyauth.StatusActive, Generation: 1,
									Quota: cliproxyauth.QuotaState{Family: family, Generation: 1},
								},
								siblingModel: {
									Status: cliproxyauth.StatusActive, Generation: 1,
									Quota: cliproxyauth.QuotaState{Family: siblingFamily, Generation: 1},
								},
							},
						},
					); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				run := func(requestModel string) ([]byte, error) {
					t.Helper()
					result, errStream := manager.ExecuteStream(
						context.Background(), []string{"codex"}, cliproxyexecutor.Request{
							Model:   requestModel,
							Payload: []byte(fmt.Sprintf(`{"model":%q,"input":"hello"}`, requestModel)),
						}, cliproxyexecutor.Options{
							Stream: true, SourceFormat: sdktranslator.FromString("openai-response"),
						},
					)
					if errStream != nil {
						t.Fatalf("stream must start before the terminal quota error: %v", errStream)
					}
					var payload []byte
					var terminalErr error
					for chunk := range result.Chunks {
						payload = append(payload, chunk.Payload...)
						if chunk.Err != nil {
							terminalErr = chunk.Err
						}
					}
					return payload, terminalErr
				}
				before := time.Now()
				payload, quotaErr := run(model)
				if !strings.Contains(string(payload), "response.created") || quotaErr == nil {
					t.Fatalf(
						"expected payload then terminal quota error: payload=%s error=%v",
						payload,
						quotaErr,
					)
				}
				var scoped cliproxyexecutor.ExecutionDispositionError
				if !errors.As(
					quotaErr, &scoped,
				) || scoped.ExecutionDisposition().EffectScope != cliproxyexecutor.StateEffectScopeQuotaFamily {
					t.Fatalf("real Codex quota scope = %T %v, want typed quota family", quotaErr, quotaErr)
				}
				high, found := manager.GetByID(highID)
				if !found {
					t.Fatal("quota credential disappeared")
				}
				for _, stateModel := range []string{model, siblingModel} {
					state := high.ModelStates[stateModel]
					if state == nil || !state.Unavailable || !state.Quota.Exceeded ||
						state.Quota.Family != family ||
						state.EffectScope !=
							cliproxyexecutor.StateEffectScopeQuotaFamily ||
						state.EffectKey !=
							family ||
						state.Generation !=
							2 ||
						state.Quota.Generation !=
							2 ||
						state.ResetAuthority !=
							cliproxyauth.StateResetAuthoritySuccess ||
						state.Quota.NextRecoverAt.Before(before.Add(time.Hour)) {
						t.Fatalf("quota family state for %s = %+v", stateModel, state)
					}
				}
				payload, errSibling := run(siblingModel)
				if errSibling != nil || !strings.Contains(string(payload), "response.completed") {
					t.Errorf(
						"sibling model did not fail over to the healthy account: payload=%s error=%v", payload,
						errSibling,
					)
				}
				for _, want := range []string{"quota-high", "quota-low"} {
					select {
					case got := <-attempts:
						if got != want {
							t.Errorf("upstream account = %s, want %s", got, want)
						}
					default:
						t.Fatalf("missing upstream attempt for %s", want)
					}
				}
				if len(attempts) != 0 {
					t.Errorf("unexpected extra upstream attempts: %d", len(attempts))
				}
			},
		)
	}
}

//goland:noinspection DuplicatedCode
func TestCodexModelLevelCoolingPreservesSiblingModel(t *testing.T) {
	for _, transport := range []string{"sse", "websocket-error", "websocket-failed"} {
		t.Run(
			transport, func(t *testing.T) {
				const model, siblingModel = "gpt-5.3-codex-spark", "gpt-5.6-sol"
				const family, siblingFamily = model, siblingModel
				const created = `{"type":"response.created","response":{"id":"quota-test-response"}}`
				const quota = `{"type":"usage_limit_reached","message":"You've hit your usage ` +
					`limit.","resets_in_seconds":` +
					`3600}`
				const completed = `{"type":"response.completed","response":{"id":"quota-test-success","` +
					`status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"` +
					`total_tokens":2}}}`
				attempts := make(chan string, 8)
				upgrader := websocket.Upgrader{}
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
							attempts <- account
							terminal := completed
							var reqText string
							if transport == "sse" {
								bodyBytes, _ := io.ReadAll(r.Body)
								reqText = string(bodyBytes)
							}
							if transport != "sse" {
								conn, errUpgrade := upgrader.Upgrade(w, r, nil)
								if errUpgrade != nil {
									t.Errorf("upgrade websocket: %v", errUpgrade)
									return
								}
								defer func() {
									if errClose := conn.Close(); errClose != nil {
										t.Errorf("close websocket: %v", errClose)
									}
								}()
								_, msg, errRead := conn.ReadMessage()
								if errRead != nil {
									t.Errorf("read websocket request: %v", errRead)
									return
								}
								reqText = string(msg)
								if account == "quota-high" && strings.Contains(reqText, model) {
									terminal = `{"type":"error","status":429,"error":` + quota + `}`
									if transport == "websocket-failed" {
										terminal = `{"type":"response.failed","response":{"error":` + quota + `}}`
									}
								}
								for _, event := range []string{created, terminal} {
									if errWrite := conn.WriteMessage(
										websocket.TextMessage, []byte(event),
									); errWrite != nil {
										t.Errorf("write websocket event: %v", errWrite)
										return
									}
								}
								return
							}
							if account == "quota-high" && strings.Contains(reqText, model) {
								terminal = `{"type":"error","status":429,"error":` + quota + `}`
							}
							w.Header().Set("Content-Type", "text/event-stream")
							if _, errWrite := fmt.Fprintf(
								w, "data: %s\n\ndata: %s\n\n", created, terminal,
							); errWrite != nil {
								t.Errorf("write SSE: %v", errWrite)
							}
						},
					),
				)
				defer server.Close()
				manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
				manager.SetRetryConfig(0, 0, 0)
				manager.SetHomeClientProvider(func() cliproxyauth.HomeClient { return nil })
				cfg, errParse := config.ParseConfigBytes([]byte("codex:\n  model-level-cooling: true\n"))
				if errParse != nil {
					t.Fatalf("parse config: %v", errParse)
				}
				if transport == "sse" {
					manager.RegisterExecutor(newCodexExecutor(cfg))
				} else {
					manager.RegisterExecutor(newCodexWebsocketsExecutor(cfg))
				}
				highID, lowID := "quota-high-"+transport, "quota-low-"+transport
				for i, id := range []string{highID, lowID} {
					key := "quota-high"
					if i == 1 {
						key = "quota-low"
					}
					registry.GetGlobalRegistry().RegisterClient(
						id, "codex", []*registry.ModelInfo{{ID: model}, {ID: siblingModel}},
					)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					if _, errRegister := manager.Register(
						context.Background(), &cliproxyauth.Auth{
							ID: id, Provider: "codex", Status: cliproxyauth.StatusActive,
							Attributes: map[string]string{
								"priority": fmt.Sprint(4 - i), "base_url": server.URL, "api_key": key,
							},
							Metadata: map[string]any{"disable_cooling": false},
							ModelStates: map[string]*cliproxyauth.ModelState{
								model: {
									Status: cliproxyauth.StatusActive, Generation: 1,
									Quota: cliproxyauth.QuotaState{Family: family, Generation: 1},
								},
								siblingModel: {
									Status: cliproxyauth.StatusActive, Generation: 1,
									Quota: cliproxyauth.QuotaState{Family: siblingFamily, Generation: 1},
								},
							},
						},
					); errRegister != nil {
						t.Fatal(errRegister)
					}
				}
				run := func(requestModel string) ([]byte, error) {
					t.Helper()
					result, errStream := manager.ExecuteStream(
						context.Background(), []string{"codex"}, cliproxyexecutor.Request{
							Model:   requestModel,
							Payload: []byte(fmt.Sprintf(`{"model":%q,"input":"hello"}`, requestModel)),
						}, cliproxyexecutor.Options{
							Stream: true, SourceFormat: sdktranslator.FromString("openai-response"),
						},
					)
					if errStream != nil {
						t.Fatalf("stream must start before the terminal quota error: %v", errStream)
					}
					var payload []byte
					var terminalErr error
					for chunk := range result.Chunks {
						payload = append(payload, chunk.Payload...)
						if chunk.Err != nil {
							terminalErr = chunk.Err
						}
					}
					return payload, terminalErr
				}
				before := time.Now()
				payload, quotaErr := run(model)
				if !strings.Contains(string(payload), "response.created") || quotaErr == nil {
					t.Fatalf(
						"expected payload then terminal quota error: payload=%s error=%v",
						payload,
						quotaErr,
					)
				}
				var scoped cliproxyexecutor.ExecutionDispositionError
				if !errors.As(
					quotaErr, &scoped,
				) || scoped.ExecutionDisposition().EffectScope != cliproxyexecutor.StateEffectScopeQuotaFamily {
					t.Fatalf("real Codex quota scope = %T %v, want typed quota family", quotaErr, quotaErr)
				}
				high, found := manager.GetByID(highID)
				if !found {
					t.Fatal("quota credential disappeared")
				}
				state, sibling := high.ModelStates[model], high.ModelStates[siblingModel]
				if state == nil || !state.Unavailable || !state.Quota.Exceeded || state.Quota.Family != family ||
					state.EffectScope != cliproxyexecutor.StateEffectScopeQuotaFamily ||
					state.EffectKey != family ||
					state.Generation !=
						2 ||
					state.Quota.Generation !=
						2 ||
					state.Quota.NextRecoverAt.Before(before.Add(time.Hour)) {
					t.Fatalf("rejected model quota state = %+v", state)
				}
				if sibling == nil || sibling.Unavailable || sibling.Quota.Exceeded ||
					sibling.Quota.Family != siblingFamily ||
					sibling.Generation != 1 {
					t.Fatalf("independent sibling quota state = %+v", sibling)
				}
				payload, errSibling := run(siblingModel)
				if errSibling != nil || !strings.Contains(string(payload), "response.completed") {
					t.Errorf("sibling model failed unexpectedly: payload=%s error=%v", payload, errSibling)
				}
				for _, want := range []string{"quota-high", "quota-high"} {
					select {
					case got := <-attempts:
						if got != want {
							t.Errorf("upstream account = %s, want %s", got, want)
						}
					default:
						t.Fatalf("missing upstream attempt for %s", want)
					}
				}
				if len(attempts) != 0 {
					t.Errorf("unexpected extra upstream attempts: %d", len(attempts))
				}
			},
		)
	}
}
