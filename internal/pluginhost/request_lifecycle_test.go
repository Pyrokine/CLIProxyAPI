package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestRequestInterceptorTerminationStopsChain(t *testing.T) {
	lowCalls := 0
	host := newHostWithRecords(
		capabilityRecord{
			id:       "high",
			priority: 20,
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					RequestInterceptor: requestInterceptorFunc(
						func(
							context.Context,
							pluginapi.RequestInterceptRequest,
						) (pluginapi.RequestInterceptResponse, error) {
							return pluginapi.RequestInterceptResponse{
								Terminate:       true,
								StatusCode:      http.StatusForbidden,
								ResponseHeaders: http.Header{"Content-Type": {"application/json"}},
								ResponseBody:    []byte(`{"error":"blocked"}`),
							}, nil
						},
					),
				},
			},
		},
		capabilityRecord{
			id:       "low",
			priority: 10,
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					RequestInterceptor: requestInterceptorFunc(
						func(
							context.Context,
							pluginapi.RequestInterceptRequest,
						) (pluginapi.RequestInterceptResponse, error) {
							lowCalls++
							return pluginapi.RequestInterceptResponse{}, nil
						},
					),
				},
			},
		},
	)

	response := host.InterceptRequestBeforeAuth(
		context.Background(),
		pluginapi.RequestInterceptRequest{RequestID: "request-1"},
	)
	if !response.Terminate || response.StatusCode != http.StatusForbidden {
		t.Fatalf("termination response = %#v", response)
	}
	if response.ResponseHeaders.Get("Content-Type") != "application/json" ||
		string(response.ResponseBody) != `{"error":"blocked"}` {
		t.Fatalf("termination payload = %#v", response)
	}
	if lowCalls != 0 {
		t.Fatalf("lower-priority interceptor calls = %d, want 0", lowCalls)
	}
}

func TestCompleteRequestUsesUncancelledContextAndClonesMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	originalNested := map[string]any{"value": "original"}
	var got pluginapi.RequestCompletion
	var callbackContextError error
	done := make(chan struct{})
	host := newHostWithRecords(
		capabilityRecord{
			id: "lifecycle",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					RequestLifecyclePlugin: requestLifecyclePluginFunc(
						func(
							callbackCtx context.Context,
							completion pluginapi.RequestCompletion,
						) {
							callbackContextError = callbackCtx.Err()
							got = completion
							completion.Metadata["nested"].(map[string]any)["value"] = "mutated"
							close(done)
						},
					),
				},
			},
		},
	)

	host.CompleteRequest(
		ctx, pluginapi.RequestCompletion{
			RequestID:   "request-1",
			Outcome:     pluginapi.RequestCompletionCanceled,
			StartedAt:   time.Now().Add(-time.Second),
			CompletedAt: time.Now(),
			Metadata:    map[string]any{"nested": originalNested},
		},
	)
	<-done

	if callbackContextError != nil {
		t.Fatalf("callback context error = %v", callbackContextError)
	}
	if got.RequestID != "request-1" || got.Outcome != pluginapi.RequestCompletionCanceled {
		t.Fatalf("completion = %#v", got)
	}
	if originalNested["value"] != "original" {
		t.Fatalf("input metadata was mutated: %#v", originalNested)
	}
}

func TestCompleteRequestDoesNotWaitForBlockingPlugin(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	host := newHostWithRecords(
		capabilityRecord{
			id: "blocking-lifecycle",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					RequestLifecyclePlugin: requestLifecyclePluginFunc(
						func(
							context.Context,
							pluginapi.RequestCompletion,
						) {
							close(started)
							<-release
						},
					),
				},
			},
		},
	)

	returned := make(chan struct{})
	go func() {
		host.CompleteRequest(
			context.Background(),
			pluginapi.RequestCompletion{RequestID: "request-blocking"},
		)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("CompleteRequest blocked on lifecycle plugin")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("lifecycle plugin was not invoked")
	}
	close(release)
}

func TestRPCCapabilitiesAndAdapterIncludeRequestLifecycle(t *testing.T) {
	var got pluginapi.RequestCompletion
	plugin := validTestPlugin("request-lifecycle")
	plugin.Capabilities.RequestLifecyclePlugin = requestLifecyclePluginFunc(
		func(
			_ context.Context,
			completion pluginapi.RequestCompletion,
		) {
			got = completion
		},
	)
	caps := rpcCapabilitiesFromPlugin(plugin)
	if !caps.RequestLifecyclePlugin {
		t.Fatal("RequestLifecyclePlugin = false, want true")
	}
	rawCaps, errMarshal := json.Marshal(caps)
	if errMarshal != nil {
		t.Fatalf("Marshal() error = %v", errMarshal)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(rawCaps, &decoded); errUnmarshal != nil {
		t.Fatalf("Unmarshal() error = %v", errUnmarshal)
	}
	if decoded["request_lifecycle_plugin"] != true {
		t.Fatalf("request_lifecycle_plugin = %#v", decoded["request_lifecycle_plugin"])
	}

	lookup := newTestSymbolLookup(&testPlugin{registerResult: plugin})
	registered, errRegister := registerRPCPlugin(
		context.Background(), nil, "request-lifecycle", lookup, pluginabi.MethodPluginRegister, nil,
	)
	if errRegister != nil {
		t.Fatalf("registerRPCPlugin() error = %v", errRegister)
	}
	if lookup.lastLifecycle.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf(
			"registration schema version = %d, want %d",
			lookup.lastLifecycle.SchemaVersion,
			pluginabi.SchemaVersion,
		)
	}
	if registered.Capabilities.RequestLifecyclePlugin == nil {
		t.Fatal("RequestLifecyclePlugin = nil, want RPC adapter")
	}
	channel := make(chan struct{})
	metadata := map[string]any{"keep": "value", "channel": channel, "function": func() {}}
	if errComplete := registered.Capabilities.RequestLifecyclePlugin.HandleRequestComplete(
		context.Background(),
		pluginapi.RequestCompletion{
			RequestID: "request-rpc",
			Outcome:   pluginapi.RequestCompletionSucceeded,
			Metadata:  metadata,
		},
	); errComplete != nil {
		t.Fatalf("HandleRequestComplete() error = %v", errComplete)
	}
	if got.RequestID != "request-rpc" || got.Outcome != pluginapi.RequestCompletionSucceeded ||
		len(got.Metadata) != 1 || got.Metadata["keep"] != "value" {
		t.Fatalf("RPC completion = %#v", got)
	}
	if len(metadata) != 3 || metadata["channel"] != channel {
		t.Fatal("RPC completion sanitization changed the original metadata")
	}
	if _, ok := metadata["function"].(func()); !ok {
		t.Fatal("RPC completion sanitization removed the caller's function")
	}
}

func TestRequestCompletionMetadataPreservesRPCEnvelopeAndCaller(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(
			fmt.Sprint(wrapped), func(t *testing.T) {
				channel := make(chan struct{})
				nested := map[string]any{"keep": "nested", "channel": channel}
				metadata := map[string]any{
					"keep":     "value",
					"channel":  channel,
					"function": func() {},
					"nested":   nested,
				}
				completion := pluginapi.RequestCompletion{
					RequestID: "request-1", Outcome: pluginapi.RequestCompletionSucceeded, Metadata: metadata,
				}
				var request any = completion
				callbackID := ""
				if wrapped {
					callbackID = "callback-1"
					request = rpcRequestCompletion{RequestCompletion: completion, HostCallbackID: callbackID}
				}
				raw, err := json.Marshal(sanitizePluginRequest(request))
				if err != nil {
					t.Fatalf("completion metadata could not cross the JSON envelope: %v", err)
				}
				var decoded rpcRequestCompletion
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Fatal(err)
				}
				if decoded.RequestID !=
					completion.RequestID ||
					decoded.Outcome !=
						completion.Outcome ||
					decoded.HostCallbackID !=
						callbackID ||
					len(decoded.Metadata) != 2 ||
					decoded.Metadata["keep"] != "value" {
					t.Fatalf("completion envelope = %s", raw)
				}
				if child, ok := decoded.Metadata["nested"].(map[string]any); !ok || len(child) != 1 ||
					child["keep"] != "nested" {
					t.Fatalf("nested completion metadata = %s", raw)
				}
				if len(metadata) != 4 || metadata["channel"] != channel || len(nested) != 2 ||
					nested["channel"] != channel {
					t.Fatal("completion sanitization mutated the caller's metadata")
				}
				if _, ok := metadata["function"].(func()); !ok {
					t.Fatal("completion sanitization removed the caller's function")
				}
			},
		)
	}
}

func TestValidPluginAcceptsRequestLifecycleOnly(t *testing.T) {
	plugin := validTestPlugin("request-lifecycle-only")
	plugin.Capabilities = pluginapi.Capabilities{
		RequestLifecyclePlugin: requestLifecyclePluginFunc(
			func(context.Context, pluginapi.RequestCompletion) {},
		),
	}
	if !validPlugin(plugin) {
		t.Fatal("request-lifecycle-only plugin was rejected")
	}
}

func TestRPCRegistrationNegotiatesSchemaVersionsZeroThroughSix(t *testing.T) {
	for schemaVersion := uint32(0); schemaVersion <= pluginabi.SchemaVersion; schemaVersion++ {
		t.Run(
			fmt.Sprintf("schema-%d", schemaVersion), func(t *testing.T) {
				lookup := newTestSymbolLookup(&testPlugin{registerResult: validTestPlugin("schema")})
				lookup.schemaVersion = schemaVersion
				lookup.schemaVersionSet = true
				registered, errRegister := registerRPCPlugin(
					context.Background(), nil, "schema", lookup, pluginabi.MethodPluginRegister, nil,
				)
				if errRegister != nil {
					t.Fatalf("registerRPCPlugin() error = %v", errRegister)
				}
				want := schemaVersion
				if want == 0 {
					want = 1
				}
				if registered.SchemaVersion != want {
					t.Fatalf("SchemaVersion = %d, want %d", registered.SchemaVersion, want)
				}
			},
		)
	}
}

func TestRPCRegistrationRejectsNewerSchemaVersion(t *testing.T) {
	lookup := newTestSymbolLookup(&testPlugin{registerResult: validTestPlugin("future-schema")})
	lookup.schemaVersion = pluginabi.SchemaVersion + 1
	if _, errRegister := registerRPCPlugin(
		context.Background(), nil, "future-schema", lookup, pluginabi.MethodPluginRegister, nil,
	); errRegister == nil {
		t.Fatal("registerRPCPlugin() error = nil, want unsupported schema error")
	}
}
