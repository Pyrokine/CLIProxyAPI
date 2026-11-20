package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestABIRegistrationUsesHostStreamCapabilityField(t *testing.T) {
	raw, errMarshal := abiOKEnvelope(
		abiRegistration{
			Capabilities: abiCapabilities{
				RequestInterceptor:     true,
				ResponseInterceptor:    true,
				StreamChunkInterceptor: true,
			},
		},
	)
	if errMarshal != nil {
		t.Fatalf("abiOKEnvelope() error = %v", errMarshal)
	}

	var envelope abiEnvelope
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("json.Unmarshal(envelope) error = %v", errUnmarshal)
	}
	var result struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if errUnmarshal := json.Unmarshal(envelope.Result, &result); errUnmarshal != nil {
		t.Fatalf("json.Unmarshal(result) error = %v", errUnmarshal)
	}
	if !result.Capabilities["response_stream_interceptor"] {
		t.Fatalf("response_stream_interceptor capability was not advertised: %v", result.Capabilities)
	}
	if _, exists := result.Capabilities["stream_chunk_interceptor"]; exists {
		t.Fatalf("legacy stream_chunk_interceptor field should not be advertised: %v", result.Capabilities)
	}
}

func TestABIStreamContractRetainsStatelessScriptInputs(t *testing.T) {
	pluginDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(t.TempDir(), "stream_contract.js")
	script := `var calls = 0;
function on_after_stream_response(ctx) {
    if (++calls !== 1 || JSON.parse(ctx.req.body).model !== "fixture-model" || !Object.isFrozen(ctx.history_chunks)) {
        throw new Error("stream hook requires an isolated VM, request body and frozen history");
    }
    ctx.headers["X-Fixture-Stream-Inputs"] = "present";
    return ctx;
}`
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPaths, err := json.Marshal([]string{scriptPath})
	if err != nil {
		t.Fatal(err)
	}
	registrationRequest, err := json.Marshal(
		abiLifecycleRequest{
			ConfigYAML: []byte("enabled: true\nscript_paths: " + string(scriptPaths) + "\n"), PluginDir: pluginDir,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := handleJSHandlerABIMethod(context.Background(), pluginabi.MethodPluginRegister, registrationRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer JSHandlerPluginShutdown()
	var envelope abiEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var registration abiRegistration
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		t.Fatal(err)
	}
	if registration.SchemaVersion >= pluginabi.SchemaVersionStreamChunkOmitRequestBody ||
		registration.SchemaVersion >= pluginabi.SchemaVersionStreamChunkOmitHistory {
		t.Errorf(
			"stateless JS hooks require request/history payloads, advertised schema = %d", registration.SchemaVersion,
		)
	}
	request := pluginapi.StreamChunkInterceptRequest{
		Model: "fixture-model", SourceFormat: "openai", RequestBody: []byte(`{"model":"fixture-model"}`),
		Body: []byte(
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0}]},\"finish_reason\":\"stop\"}]}\n\n",
		),
	}
	for index := range 2 {
		request.ChunkIndex = index
		if index == 1 {
			request.HistoryChunks = [][]byte{request.Body}
			request.Body = []byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		}
		wireRequest := request
		if registration.SchemaVersion >= pluginabi.SchemaVersionStreamChunkOmitRequestBody {
			wireRequest.OriginalRequest = nil
			wireRequest.RequestBody = nil
		}
		if registration.SchemaVersion >= pluginabi.SchemaVersionStreamChunkOmitHistory {
			wireRequest.HistoryChunks = nil
		}
		payload, err := json.Marshal(abiStreamChunkInterceptRequest{StreamChunkInterceptRequest: wireRequest})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := handleJSHandlerABIMethod(
			context.Background(), pluginabi.MethodResponseInterceptStreamChunk, payload,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		var response pluginapi.StreamChunkInterceptResponse
		if err := json.Unmarshal(envelope.Result, &response); err != nil {
			t.Fatal(err)
		}
		if response.Headers.Get("X-Fixture-Stream-Inputs") != "present" {
			t.Errorf("ABI stream chunk %d did not expose the request body and frozen history in a fresh VM", index)
		}
		want := `"finish_reason":null`
		if index == 1 {
			want = `"finish_reason":"tool_calls"`
		}
		if response.DropChunk || !strings.Contains(string(response.Body), want) {
			t.Fatalf("ABI stream chunk %d = %s, want %s", index, response.Body, want)
		}
	}
}
