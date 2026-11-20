package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type deliveryContractLifecycleHost struct {
	*Host
	completed chan pluginapi.RequestCompletion
}

func (h *deliveryContractLifecycleHost) CompleteRequest(_ context.Context, result pluginapi.RequestCompletion) {
	h.completed <- result
}

func (h *deliveryContractLifecycleHost) CompleteRequestExcept(
	ctx context.Context,
	result pluginapi.RequestCompletion,
	_ string,
) {
	h.CompleteRequest(ctx, result)
}

func TestLegacyDeliveryNestedRPCUsesOwnCompletion(t *testing.T) {
	for _, mode := range []string{
		"two-turns", "overlapping-turns", "cancel-partial", "cancel-before-eof", "cancel-completed",
	} {
		t.Run(
			mode, func(t *testing.T) {
				const model = "gemini-3.7-flash"
				//goland:noinspection SpellCheckingInspection
				const signature = "EgYKBAECAwQ="
				multipleTurns := mode == "two-turns" || mode == "overlapping-turns"
				requests := make(chan []byte, 2)
				var visits atomic.Int32
				release := make(chan struct{})
				releaseFirst := sync.OnceFunc(func() { close(release) })
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							body, err := io.ReadAll(r.Body)
							if err != nil {
								t.Error(err)
								return
							}
							requests <- body
							requestNumber := visits.Add(1)
							finish := `,"finishReason":"STOP"`
							if mode == "cancel-partial" {
								finish = ""
							}
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(
								w,
								`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{`+
									`"functionCall":{"id":"native-call","name":"lookup","args":{"key":"value"}},`+
									`"thoughtSignature":"`+signature+`"}]}`+finish+
									`}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":22,`+
									`"totalTokenCount":33},"responseId":"nested"}}`+"\n\n",
							)
							w.(http.Flusher).Flush()
							if mode == "overlapping-turns" && requestNumber == 1 {
								select {
								case <-release:
								case <-r.Context().Done():
								}
							}
							if mode == "cancel-partial" || mode == "cancel-before-eof" {
								<-r.Context().Done()
							}
						},
					),
				)
				defer server.Close()
				defer releaseFirst()
				manager := coreauth.NewManager(nil, nil, nil)
				manager.RegisterExecutor(runtimeexecutor.NewAntigravityExecutor(&config.Config{}))
				auth := &coreauth.Auth{
					ID: t.Name(), Provider: "antigravity", Status: coreauth.StatusActive, ProxyURL: "direct",
					Attributes: map[string]string{"base_url": server.URL},
					Metadata: map[string]any{
						"access_token": "synthetic-token", "expired": time.Now().Add(time.Hour).Format(time.RFC3339),
						"project_id": "synthetic-project",
					},
				}
				if _, err := manager.Register(t.Context(), auth); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().
					RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
				host := New()
				host.SetModelExecutor(base)
				lifecycle := &deliveryContractLifecycleHost{
					Host: host, completed: make(chan pluginapi.RequestCompletion, 4),
				}
				base.SetPluginHost(lifecycle)
				instance := &hostCallbackInstance{id: 6332}
				caller := withHostCallbackInstance(
					withHostCallbackInstanceID(
						withHostCallbackPluginID(
							t.Context(), "delivery-contract-plugin",
						), instance.id,
					), instance,
				)
				outer, finishOuter := coreusage.WithStreamDelivery(caller)
				defer finishOuter(context.Canceled)
				outer = coreexecutor.WithStreamDeliveryAcknowledgement(outer)
				callbackID, closeCallback := host.openCallbackContextForPluginInstance(
					outer, "delivery-contract-plugin", instance,
				)
				defer closeCallback()
				var firstStreamID string
				call := func(body string) (string, error) {
					raw, err := json.Marshal(
						rpcHostModelExecutionRequest{
							HostCallbackID: callbackID,
							EntryProtocol:  "openai-response", ExitProtocol: "openai-response", Model: model,
							Stream: true, Body: []byte(body),
						},
					)
					if err != nil {
						return "", err
					}
					response, err := host.callFromPlugin(caller, pluginabi.MethodHostModelExecuteStream, raw)
					if err != nil {
						return "", err
					}
					stream, err := decodeRPCEnvelope[pluginapi.HostModelStreamResponse](response)
					if err != nil {
						return "", err
					}
					read, err := json.Marshal(pluginapi.HostModelStreamReadRequest{StreamID: stream.StreamID})
					if err != nil {
						return "", err
					}
					var output strings.Builder
					for {
						response, err := host.callFromPlugin(caller, pluginabi.MethodHostModelStreamRead, read)
						if err != nil {
							return "", err
						}
						chunk, err := decodeRPCEnvelope[pluginapi.HostModelStreamReadResponse](response)
						if err != nil {
							return "", err
						}
						if chunk.Error != "" {
							return "", fmt.Errorf("stream error: %s", chunk.Error)
						}
						output.Write(chunk.Payload)
						if mode == "overlapping-turns" && firstStreamID == "" {
							for line := range strings.SplitSeq(string(chunk.Payload), "\n") {
								if event := gjson.Parse(
									strings.TrimSpace(
										strings.TrimPrefix(
											line, "data:",
										),
									),
								); event.Get("type").String() == "response.completed" {
									firstStreamID = stream.StreamID
									return output.String(), nil
								}
							}
						}
						if (mode == "cancel-partial" || mode == "cancel-before-eof") && len(chunk.Payload) > 0 {
							closeCallback()
							return output.String(), nil
						}
						if chunk.Done {
							return output.String(), nil
						}
					}
				}
				result := make(chan error, 1)
				go func() {
					first := `{"model":"` + model + `","stream":true,"metadata":{"session_id":"` +
						t.Name() + `"},"input":[{"role":"user","content":"lookup"}]}`
					output, err := call(first)
					if err != nil || !multipleTurns {
						result <- err
						return
					}
					second := first
					var callID string
					for line := range strings.SplitSeq(output, "\n") {
						event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
						for _, item := range event.Get("response.output").Array() {
							if kind := item.Get("type").String(); kind == "reasoning" ||
								kind == "function_call" {
								second, err = sjson.SetRaw(second, "input.-1", item.Raw)
								if err != nil {
									result <- err
									return
								}
							}
							if item.Get("type").String() == "function_call" {
								callID = item.Get("call_id").String()
							}
						}
					}
					if callID == "" {
						result <- fmt.Errorf("missing delivered function call: %s", output)
						return
					}
					second, err = sjson.Set(
						second, "input.-1",
						map[string]any{"type": "function_call_output", "call_id": callID, "output": "found"},
					)
					if err == nil {
						_, err = call(second)
					}
					result <- err
				}()
				select {
				case err := <-result:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					closeCallback()
					finishOuter(context.Canceled)
					t.Fatal("nested RPC waited for the outer consumer receipt")
				}
				if mode == "overlapping-turns" && (firstStreamID == "" || visits.Load() != 2) {
					t.Fatal("second RPC turn did not complete while the first upstream remained open")
				}
				releaseFirst()
				if mode == "cancel-completed" {
					closeCallback()
				}
				if !coreusage.StreamDeliveryTracked(outer) ||
					!coreexecutor.StreamDeliveryAcknowledgement(outer) {
					t.Fatal("nested RPC changed the outer delivery ownership")
				}
				turns := 1
				if multipleTurns {
					turns = 2
				}
				for range turns {
					select {
					case completion := <-lifecycle.completed:
						want := pluginapi.RequestCompletionSucceeded
						if mode == "cancel-partial" || mode == "cancel-before-eof" {
							want = pluginapi.RequestCompletionCanceled
						}
						if completion.Outcome != want {
							t.Fatalf("nested completion=%+v, want %s", completion, want)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("nested completion waited for the outer receipt")
					}
				}
				if firstStreamID != "" {
					raw, err := json.Marshal(pluginapi.HostModelStreamCloseRequest{StreamID: firstStreamID})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := host.callFromPlugin(caller, pluginabi.MethodHostModelStreamClose, raw); err != nil {
						t.Fatal(err)
					}
				}
				<-requests
				if multipleTurns {
					if second := <-requests; !strings.Contains(string(second), signature) {
						t.Fatalf("second RPC turn lost the delivered signature: %s", second)
					}
				}
			},
		)
	}
}
