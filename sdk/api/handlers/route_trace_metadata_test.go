package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
)

func TestRequestExecutionMetadataAddsRouteTraceCallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	trace := logging.NewRouteTrace()
	ginCtx.Set(logging.RouteTraceGinKey, trace)
	ctx := util.WithGinContext(context.Background(), ginCtx)

	meta := requestExecutionMetadata(ctx)
	callback, ok := meta[coreexecutor.RouteTraceCallbackMetadataKey].(func(coreexecutor.RouteTraceEvent))
	if !ok || callback == nil {
		t.Fatal("route trace callback missing from execution metadata")
	}
	truncatedCallback, ok := meta[coreexecutor.RouteTraceTruncatedCallbackMetadataKey].(func(int))
	if !ok || truncatedCallback == nil {
		t.Fatal("route trace truncated callback missing from execution metadata")
	}
	callback(coreexecutor.RouteTraceEvent{Action: "attempt.completed", Provider: "codex"})
	truncatedCallback(2)
	meta["business_key"] = "business_value"
	meta["__route_trace_attempt_state"] = "private"

	snapshot := trace.Snapshot()
	if len(snapshot.Events) != 1 || snapshot.Events[0].Provider != "codex" || snapshot.TruncatedEvents != 2 {
		t.Fatalf("route trace snapshot = %#v", snapshot)
	}
	pluginMetadata := pluginExecutionMetadata(meta)
	if _, exists := pluginMetadata[coreexecutor.RouteTraceCallbackMetadataKey]; exists {
		t.Fatal("route trace callback leaked to plugin metadata")
	}
	if _, exists := pluginMetadata[coreexecutor.RouteTraceTruncatedCallbackMetadataKey]; exists {
		t.Fatal("route trace truncated callback leaked to plugin metadata")
	}
	if _, exists := pluginMetadata["__route_trace_attempt_state"]; exists {
		t.Fatal("route trace internal state leaked to plugin metadata")
	}
	if got := pluginMetadata["business_key"]; got != "business_value" {
		t.Fatalf("business metadata = %#v", got)
	}
	if _, exists := meta[coreexecutor.RouteTraceCallbackMetadataKey]; !exists {
		t.Fatal("plugin metadata filtering mutated execution metadata")
	}
}

func TestRequestLifecycleCompletionHidesRouteTraceMetadata(t *testing.T) {
	metadata := map[string]any{
		"business_key": "business_value",
		coreexecutor.RouteTraceCallbackMetadataKey:          func(coreexecutor.RouteTraceEvent) {},
		coreexecutor.RouteTraceTruncatedCallbackMetadataKey: func(int) {},
		"__route_trace_attempt_state":                       "private",
	}
	var captured pluginapi.RequestCompletion
	host := &lifecycleTestHost{
		completeRequest: func(_ context.Context, completion pluginapi.RequestCompletion) {
			captured = completion
		},
	}
	handler := &BaseAPIHandler{PluginHost: host}
	tracker := handler.newRequestLifecycleTracker(
		context.Background(), "", "openai", "gpt-5", "gpt-5", false, metadata, "",
	)
	tracker.complete(pluginapi.RequestCompletionSucceeded, http.StatusOK, nil)

	if got := captured.Metadata["business_key"]; got != "business_value" {
		t.Fatalf("business metadata = %#v", got)
	}
	for _, key := range []string{
		coreexecutor.RouteTraceCallbackMetadataKey,
		coreexecutor.RouteTraceTruncatedCallbackMetadataKey,
		"__route_trace_attempt_state",
	} {
		if _, exists := captured.Metadata[key]; exists {
			t.Fatalf("route trace metadata %q leaked to lifecycle plugin", key)
		}
	}
	if _, exists := metadata[coreexecutor.RouteTraceCallbackMetadataKey]; !exists {
		t.Fatal("lifecycle completion filtering mutated execution metadata")
	}
}
