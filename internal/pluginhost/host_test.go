package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

type blockingInitializationLoader struct {
	entered chan struct{}
	release chan struct{}
	client  pluginClient
}

func (l *blockingInitializationLoader) Open(pluginFile, *Host) (pluginClient, error) {
	close(l.entered)
	<-l.release
	return l.client, nil
}

func TestHostApplyConfigPreservesPublishedRuntimeOnRootFailure(t *testing.T) {
	host := newHostWithRecords(capabilityRecord{id: "existing"})
	previousConfig := &config.Config{Debug: true}
	host.mu.Lock()
	host.runtimeConfig = previousConfig
	host.mu.Unlock()
	blockedRoot := filepath.Join(t.TempDir(), "plugins")
	if errWrite := os.WriteFile(blockedRoot, []byte("not a directory"), 0o600); errWrite != nil {
		t.Fatalf("write blocked root: %v", errWrite)
	}
	errApply := host.ApplyConfig(
		context.Background(),
		&config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: blockedRoot}},
	)
	if errApply == nil {
		t.Fatal("ApplyConfig() error = nil")
	}
	snapshot := host.Snapshot()
	if snapshot == nil || len(snapshot.records) != 1 || snapshot.records[0].id != "existing" {
		t.Fatalf("runtime snapshot changed after root failure: %#v", snapshot)
	}
	if current := host.currentRuntimeConfig(); current != previousConfig {
		t.Fatalf("runtime config = %p, want previous %p", current, previousConfig)
	}
}

func TestHostApplyConfigUsesPluginRootLock(t *testing.T) {
	t.Run("waits for external mutation", func(t *testing.T) {
		dir := t.TempDir()
		rootLock, errLock := pluginstore.LockRoot(dir)
		if errLock != nil {
			t.Fatalf("LockRoot() error = %v", errLock)
		}
		host := NewForTest(newTestSymbolLoader())
		finished := make(chan struct{})
		go func() {
			host.ApplyConfig(
				context.Background(),
				&config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: dir}},
			)
			close(finished)
		}()
		select {
		case <-finished:
			t.Fatal("ApplyConfig completed while another mutation held the root lock")
		case <-time.After(20 * time.Millisecond):
		}
		rootLock.Unlock()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("ApplyConfig did not finish after the root lock was released")
		}
	})
}

func TestHostApplyConfig_DisabledGlobalSkipsSnapshot(t *testing.T) {
	loader := newTestSymbolLoader()
	h := NewForTest(loader)

	h.ApplyConfig(
		context.Background(), &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: false,
				Dir:     makePluginDir(t, "alpha"),
			},
		},
	)

	if loader.openCalls != 0 {
		t.Fatalf("Open calls = %d, want 0", loader.openCalls)
	}
	snap := h.Snapshot()
	if snap.enabled || len(snap.records) != 0 {
		t.Fatalf("Snapshot() = %+v, want empty disabled snapshot", snap)
	}
}

func TestHostApplyConfig_DisabledPluginSkipsCapability(t *testing.T) {
	enabled := false
	loader := newTestSymbolLoader()
	plugin := &testPlugin{
		registerResult:    validTestPlugin("alpha"),
		reconfigureResult: validTestPlugin("alpha"),
	}
	loader.lookups["alpha"] = newTestSymbolLookup(plugin)
	h := NewForTest(loader)

	h.ApplyConfig(
		context.Background(), &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true,
				Dir:     makePluginDir(t, "alpha"),
				Configs: map[string]config.PluginInstanceConfig{
					"alpha": {Enabled: &enabled},
				},
			},
		},
	)

	if plugin.registerCalls != 0 || plugin.reconfigureCalls != 0 {
		t.Fatalf("calls = register %d reconfigure %d, want 0", plugin.registerCalls, plugin.reconfigureCalls)
	}
	if loader.openCalls != 0 {
		t.Fatalf("Open calls = %d, want 0", loader.openCalls)
	}
	if len(h.Snapshot().records) != 0 {
		t.Fatalf("Snapshot records = %d, want 0", len(h.Snapshot().records))
	}
}

func TestHostApplyConfigClearsResourceRoutesWhenPluginDisabled(t *testing.T) {
	enabled := false
	loader := newTestSymbolLoader()
	plugin := &testPlugin{
		registerResult:    validTestPlugin("alpha"),
		reconfigureResult: validTestPlugin("alpha"),
	}
	loader.lookups["alpha"] = newTestSymbolLookup(plugin)
	h := NewForTest(loader)
	dir := makePluginDir(t, "alpha")

	h.mu.Lock()
	h.resourceRoutes[managementRouteKey(http.MethodGet, "/v0/resource/plugins/alpha/status")] = resourceRouteRecord{
		pluginID: "alpha",
		route: pluginapi.ResourceRoute{
			Path: "/v0/resource/plugins/alpha/status",
			Handler: managementHandlerFunc(
				func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
					return pluginapi.ManagementResponse{Body: []byte("status")}, nil
				},
			),
		},
	}
	h.mu.Unlock()

	h.ApplyConfig(
		context.Background(), &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true,
				Dir:     dir,
				Configs: map[string]config.PluginInstanceConfig{
					"alpha": {Enabled: &enabled},
				},
			},
		},
	)
	if h.HasResourceRoute(http.MethodGet, "/v0/resource/plugins/alpha/status") {
		t.Fatal("resource route remained registered after plugin was disabled")
	}
}

func TestHostApplyConfigRegistersPluginThinkingApplier(t *testing.T) {
	loader := newTestSymbolLoader()
	plugin := &testPlugin{
		registerResult:    validTestPlugin("alpha"),
		reconfigureResult: validTestPlugin("alpha"),
	}
	plugin.registerResult.Capabilities.ThinkingApplier = testThinkingCapability{provider: "plugin-thinking"}
	plugin.reconfigureResult.Capabilities.ThinkingApplier = testThinkingCapability{provider: "plugin-thinking"}
	loader.lookups["alpha"] = newTestSymbolLookup(plugin)
	h := NewForTest(loader)
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     makePluginDir(t, "alpha"),
		},
	}
	t.Cleanup(
		func() {
			h.ApplyConfig(
				context.Background(), &config.Config{
					Plugins: config.PluginsConfig{
						Enabled: false,
						Dir:     cfg.Plugins.Dir,
					},
				},
			)
		},
	)

	h.ApplyConfig(context.Background(), cfg)

	out, errApply := thinking.ApplyThinking(
		[]byte(`{"model":"plugin-model"}`), "plugin-model(10240)", "openai", "plugin-thinking", "plugin-thinking",
	)
	if errApply != nil {
		t.Fatalf("ApplyThinking() error = %v", errApply)
	}
	if got := gjson.GetBytes(out, "thinking_budget").Int(); got != 10240 {
		t.Fatalf("thinking_budget = %d, want 10240; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "plugin").String(); got != "plugin-thinking" {
		t.Fatalf("plugin = %q, want plugin-thinking; body=%s", got, string(out))
	}
}

func TestHostApplyConfigAllowsRegisterCallbackToUseHost(t *testing.T) {
	loader := newTestSymbolLoader()
	plugin := &testPlugin{
		registerResult:    validTestPlugin("alpha"),
		reconfigureResult: validTestPlugin("alpha"),
	}
	lookup := newTestSymbolLookup(plugin)
	loader.lookups["alpha"] = lookup
	h := NewForTest(loader)
	lookup.registerOverride = func([]byte) pluginapi.Plugin {
		if h.currentRuntimeConfig() == nil {
			t.Error("currentRuntimeConfig() = nil during register callback")
		}
		return validTestPlugin("alpha")
	}

	dir := makePluginDir(t, "alpha")
	done := make(chan struct{})
	go func() {
		h.ApplyConfig(
			context.Background(), &config.Config{
				Plugins: config.PluginsConfig{
					Enabled: true,
					Dir:     dir,
				},
			},
		)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ApplyConfig deadlocked when register callback used host")
	}
}

func TestHostApplyConfigReleasesRootLockBeforeRegister(t *testing.T) {
	loader := newTestSymbolLoader()
	plugin := &testPlugin{registerResult: validTestPlugin("alpha")}
	lookup := newTestSymbolLookup(plugin)
	registerStarted := make(chan struct{})
	releaseRegister := make(chan struct{})
	lookup.registerOverride = func([]byte) pluginapi.Plugin {
		close(registerStarted)
		<-releaseRegister
		return validTestPlugin("alpha")
	}
	loader.lookups["alpha"] = lookup
	h := NewForTest(loader)
	dir := makePluginDir(t, "alpha")
	applyDone := make(chan struct{})
	go func() {
		h.ApplyConfig(
			context.Background(),
			&config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: dir}},
		)
		close(applyDone)
	}()
	select {
	case <-registerStarted:
	case <-time.After(time.Second):
		t.Fatal("plugin register did not start")
	}
	rootLock, errLock := pluginstore.LockRoot(dir)
	if errLock != nil {
		t.Fatalf("LockRoot() error = %v", errLock)
	}
	rootLock.Unlock()
	close(releaseRegister)
	select {
	case <-applyDone:
	case <-time.After(time.Second):
		t.Fatal("ApplyConfig did not finish after register release")
	}
}

func TestHostShutdownAllContextReturnsWhileRegisterStalls(t *testing.T) {
	loader := newTestSymbolLoader()
	plugin := &testPlugin{registerResult: validTestPlugin("alpha")}
	lookup := newTestSymbolLookup(plugin)
	registerStarted := make(chan struct{})
	releaseRegister := make(chan struct{})
	lookup.registerOverride = func([]byte) pluginapi.Plugin {
		close(registerStarted)
		<-releaseRegister
		return validTestPlugin("alpha")
	}
	lookup.shutdownStarted = make(chan struct{})
	loader.lookups["alpha"] = lookup
	h := NewForTest(loader)
	applyDone := make(chan struct{})
	go func() {
		h.ApplyConfig(
			context.Background(),
			&config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: makePluginDir(t, "alpha")}},
		)
		close(applyDone)
	}()
	select {
	case <-registerStarted:
	case <-time.After(time.Second):
		t.Fatal("plugin register did not start")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Millisecond)
	errShutdown := h.ShutdownAllContext(shutdownCtx)
	cancelShutdown()
	if !errors.Is(errShutdown, context.DeadlineExceeded) {
		t.Fatalf("ShutdownAllContext() error = %v, want deadline exceeded", errShutdown)
	}
	if snap := h.Snapshot(); snap.enabled || len(snap.records) != 0 {
		t.Fatalf("Snapshot() = %+v, want empty snapshot after shutdown", snap)
	}

	close(releaseRegister)
	select {
	case <-applyDone:
	case <-time.After(time.Second):
		t.Fatal("ApplyConfig did not finish after stalled register was released")
	}
	select {
	case <-lookup.shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("plugin shutdown did not resume after register finished")
	}
	if snap := h.Snapshot(); snap.enabled || len(snap.records) != 0 {
		t.Fatalf("stale ApplyConfig republished snapshot: %+v", snap)
	}
}

func TestHostUnloadPluginContextReturnsWhileRegisterStalls(t *testing.T) {
	loader := newTestSymbolLoader()
	plugin := &testPlugin{registerResult: validTestPlugin("alpha")}
	lookup := newTestSymbolLookup(plugin)
	registerStarted := make(chan struct{})
	releaseRegister := make(chan struct{})
	lookup.registerOverride = func([]byte) pluginapi.Plugin {
		close(registerStarted)
		<-releaseRegister
		return validTestPlugin("alpha")
	}
	lookup.shutdownStarted = make(chan struct{})
	loader.lookups["alpha"] = lookup
	h := NewForTest(loader)
	applyDone := make(chan struct{})
	go func() {
		h.ApplyConfig(
			context.Background(),
			&config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: makePluginDir(t, "alpha")}},
		)
		close(applyDone)
	}()
	select {
	case <-registerStarted:
	case <-time.After(time.Second):
		t.Fatal("plugin register did not start")
	}

	unloadCtx, cancelUnload := context.WithTimeout(context.Background(), 30*time.Millisecond)
	unloaded, errUnload := h.UnloadPluginContext(unloadCtx, "alpha")
	cancelUnload()
	if !unloaded {
		t.Fatal("UnloadPluginContext() did not detach the registering plugin")
	}
	if !errors.Is(errUnload, context.DeadlineExceeded) {
		t.Fatalf("UnloadPluginContext() error = %v, want deadline exceeded", errUnload)
	}

	close(releaseRegister)
	select {
	case <-applyDone:
	case <-time.After(time.Second):
		t.Fatal("ApplyConfig did not finish after stalled register was released")
	}
	select {
	case <-lookup.shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("plugin shutdown did not resume after register finished")
	}
	deadline := time.Now().Add(time.Second)
	for h.PluginLoaded("alpha") {
		if time.Now().After(deadline) {
			t.Fatal("stale ApplyConfig or completed shutdown remained tracked")
		}
		time.Sleep(time.Millisecond)
	}
	if len(h.Snapshot().records) != 0 {
		t.Fatal("stale ApplyConfig republished the unloaded plugin")
	}
}

func TestHostApplyConfigRegistersInterceptorOnlyPlugin(t *testing.T) {
	loader := newTestSymbolLoader()
	plugin := &testPlugin{
		registerResult: pluginapi.Plugin{
			Metadata: pluginapi.Metadata{
				Name:             "alpha",
				Version:          "1.0.0",
				Author:           "test",
				GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			},
			Capabilities: pluginapi.Capabilities{
				RequestInterceptor: requestInterceptorFunc(
					func(
						ctx context.Context,
						req pluginapi.RequestInterceptRequest,
					) (pluginapi.RequestInterceptResponse, error) {
						return pluginapi.RequestInterceptResponse{Body: []byte("registered")}, nil
					},
				),
			},
		},
	}
	loader.lookups["alpha"] = newTestSymbolLookup(plugin)
	h := NewForTest(loader)

	h.ApplyConfig(
		context.Background(), &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true,
				Dir:     makePluginDir(t, "alpha"),
			},
		},
	)

	if len(h.Snapshot().records) != 1 {
		t.Fatalf("Snapshot records = %d, want 1", len(h.Snapshot().records))
	}
}

func TestHostApplyConfigDispatchesInterceptorRPCMethods(t *testing.T) {
	loader := newTestSymbolLoader()
	plugin := &testPlugin{
		registerResult: pluginapi.Plugin{
			Metadata: pluginapi.Metadata{
				Name:             "alpha",
				Version:          "1.0.0",
				Author:           "test",
				GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			},
			Capabilities: pluginapi.Capabilities{
				RequestInterceptor: requestInterceptorFunc(
					func(
						ctx context.Context,
						req pluginapi.RequestInterceptRequest,
					) (pluginapi.RequestInterceptResponse, error) {
						return pluginapi.RequestInterceptResponse{Body: []byte("request|rpc")}, nil
					},
				),
				ResponseInterceptor: responseInterceptorFunc{
					interceptResponse: func(
						ctx context.Context,
						req pluginapi.ResponseInterceptRequest,
					) (pluginapi.ResponseInterceptResponse, error) {
						return pluginapi.ResponseInterceptResponse{Body: []byte("response|rpc")}, nil
					},
				},
				StreamChunkInterceptor: responseInterceptorFunc{
					interceptStreamChunk: func(
						ctx context.Context,
						req pluginapi.StreamChunkInterceptRequest,
					) (pluginapi.StreamChunkInterceptResponse, error) {
						return pluginapi.StreamChunkInterceptResponse{Body: []byte("chunk|rpc")}, nil
					},
				},
			},
		},
	}
	loader.lookups["alpha"] = newTestSymbolLookup(plugin)
	h := NewForTest(loader)

	h.ApplyConfig(
		context.Background(), &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true,
				Dir:     makePluginDir(t, "alpha"),
			},
		},
	)

	if len(h.Snapshot().records) != 1 {
		t.Fatalf("Snapshot records = %d, want 1", len(h.Snapshot().records))
	}

	caps := h.Snapshot().records[0].plugin.Capabilities
	reqResp, errReq := caps.RequestInterceptor.InterceptRequestBeforeAuth(
		context.Background(), pluginapi.RequestInterceptRequest{Body: []byte("request")},
	)
	if errReq != nil {
		t.Fatalf("InterceptRequestBeforeAuth() error = %v", errReq)
	}
	if got := string(reqResp.Body); got != "request|rpc" {
		t.Fatalf("InterceptRequestBeforeAuth() body = %q, want request|rpc", got)
	}

	respResp, errResp := caps.ResponseInterceptor.InterceptResponse(
		context.Background(), pluginapi.ResponseInterceptRequest{Body: []byte("response")},
	)
	if errResp != nil {
		t.Fatalf("InterceptResponse() error = %v", errResp)
	}
	if got := string(respResp.Body); got != "response|rpc" {
		t.Fatalf("InterceptResponse() body = %q, want response|rpc", got)
	}

	chunkResp, errChunk := caps.StreamChunkInterceptor.InterceptStreamChunk(
		context.Background(), pluginapi.StreamChunkInterceptRequest{Body: []byte("chunk")},
	)
	if errChunk != nil {
		t.Fatalf("InterceptStreamChunk() error = %v", errChunk)
	}
	if got := string(chunkResp.Body); got != "chunk|rpc" {
		t.Fatalf("InterceptStreamChunk() body = %q, want chunk|rpc", got)
	}
}

func TestInterceptorHelpersReturnErrorsWhenCallbackMissing(t *testing.T) {
	if _, errReq := (requestInterceptorFunc(nil)).InterceptRequestBeforeAuth(
		context.Background(), pluginapi.RequestInterceptRequest{},
	); errReq == nil {
		t.Fatal("InterceptRequestBeforeAuth() error = nil, want missing request interceptor callback")
	}
	if _, errReq := (requestInterceptorFunc(nil)).InterceptRequestAfterAuth(
		context.Background(), pluginapi.RequestInterceptRequest{},
	); errReq == nil {
		t.Fatal("InterceptRequestAfterAuth() error = nil, want missing request interceptor callback")
	}
	if _, errResp := (responseInterceptorFunc{interceptResponse: nil}).InterceptResponse(
		context.Background(), pluginapi.ResponseInterceptRequest{},
	); errResp == nil {
		t.Fatal("InterceptResponse() error = nil, want missing response interceptor callback")
	}
	if _, errChunk := (responseInterceptorFunc{interceptStreamChunk: nil}).InterceptStreamChunk(
		context.Background(), pluginapi.StreamChunkInterceptRequest{},
	); errChunk == nil {
		t.Fatal("InterceptStreamChunk() error = nil, want missing stream chunk interceptor callback")
	}
}

func TestRPCInterceptorsIncludeHostCallbackID(t *testing.T) {
	client := &capturePluginClient{}
	adapter := &rpcPluginAdapter{
		host:   New(),
		client: client,
	}

	if _, errReq := adapter.InterceptRequestBeforeAuth(
		context.Background(), pluginapi.RequestInterceptRequest{Body: []byte("request")},
	); errReq != nil {
		t.Fatalf("InterceptRequestBeforeAuth() error = %v", errReq)
	}
	var req rpcRequestInterceptRequest
	if errDecode := json.Unmarshal(client.requests[pluginabi.MethodRequestInterceptBefore], &req); errDecode != nil {
		t.Fatalf("decode request interceptor request: %v", errDecode)
	}
	if req.HostCallbackID == "" {
		t.Fatal("request interceptor before-auth host_callback_id is empty")
	}

	if _, errReq := adapter.InterceptRequestAfterAuth(
		context.Background(), pluginapi.RequestInterceptRequest{Body: []byte("request")},
	); errReq != nil {
		t.Fatalf("InterceptRequestAfterAuth() error = %v", errReq)
	}
	var reqAfter rpcRequestInterceptRequest
	if errDecode := json.Unmarshal(
		client.requests[pluginabi.MethodRequestInterceptAfter], &reqAfter,
	); errDecode != nil {
		t.Fatalf("decode after-auth request interceptor request: %v", errDecode)
	}
	if reqAfter.HostCallbackID == "" {
		t.Fatal("request interceptor after-auth host_callback_id is empty")
	}

	if _, errResp := adapter.InterceptResponse(
		context.Background(), pluginapi.ResponseInterceptRequest{Body: []byte("response")},
	); errResp != nil {
		t.Fatalf("InterceptResponse() error = %v", errResp)
	}
	var resp rpcResponseInterceptRequest
	if errDecode := json.Unmarshal(client.requests[pluginabi.MethodResponseInterceptAfter], &resp); errDecode != nil {
		t.Fatalf("decode response interceptor request: %v", errDecode)
	}
	if resp.HostCallbackID == "" {
		t.Fatal("response interceptor host_callback_id is empty")
	}

	if _, errChunk := adapter.InterceptStreamChunk(
		context.Background(), pluginapi.StreamChunkInterceptRequest{Body: []byte("chunk")},
	); errChunk != nil {
		t.Fatalf("InterceptStreamChunk() error = %v", errChunk)
	}
	var chunk rpcStreamChunkInterceptRequest
	if errDecode := json.Unmarshal(
		client.requests[pluginabi.MethodResponseInterceptStreamChunk], &chunk,
	); errDecode != nil {
		t.Fatalf("decode stream chunk interceptor request: %v", errDecode)
	}
	if chunk.HostCallbackID == "" {
		t.Fatal("stream chunk interceptor host_callback_id is empty")
	}
}

func TestRPCExecuteStreamKeepsHostCallbackScopeUntilStreamEnds(t *testing.T) {
	client := &capturePluginClient{}
	host := New()
	adapter := &rpcPluginAdapter{host: host, client: client}
	ctx, cancel := context.WithCancel(context.Background())
	stream, errExecute := adapter.ExecuteStream(ctx, pluginapi.ExecutorRequest{})
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	var request rpcExecutorRequest
	if errDecode := json.Unmarshal(client.requests[pluginabi.MethodExecutorExecuteStream], &request); errDecode != nil {
		t.Fatalf("decode executor stream request: %v", errDecode)
	}
	if request.HostCallbackID == "" {
		t.Fatal("executor stream host_callback_id is empty")
	}
	host.callbackContexts.mu.RLock()
	_, exists := host.callbackContexts.contexts[request.HostCallbackID]
	host.callbackContexts.mu.RUnlock()
	if !exists {
		t.Fatal("executor stream host callback scope closed before stream completion")
	}

	cancel()
	select {
	case _, ok := <-stream.Chunks:
		if ok {
			t.Fatal("canceled executor stream returned an unexpected chunk")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled executor stream did not close")
	}
	deadline := time.Now().Add(time.Second)
	for {
		host.callbackContexts.mu.RLock()
		_, exists = host.callbackContexts.contexts[request.HostCallbackID]
		host.callbackContexts.mu.RUnlock()
		if !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("executor stream host callback scope remained after stream completion")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRPCManagementIncludesHostCallbackID(t *testing.T) {
	client := &capturePluginClient{}
	host := New()
	adapter := &rpcPluginAdapter{
		host:   host,
		client: client,
	}

	if _, errHandle := adapter.HandleManagement(
		context.Background(), pluginapi.ManagementRequest{
			Method: http.MethodGet,
			Path:   "/v0/management/plugins/test/status",
			Body:   []byte("request"),
		},
	); errHandle != nil {
		t.Fatalf("HandleManagement() error = %v", errHandle)
	}
	var req rpcManagementRequest
	if errDecode := json.Unmarshal(client.requests[pluginabi.MethodManagementHandle], &req); errDecode != nil {
		t.Fatalf("decode management request: %v", errDecode)
	}
	if req.HostCallbackID == "" {
		t.Fatal("management handle host_callback_id is empty")
	}
	if req.Method != http.MethodGet || req.Path != "/v0/management/plugins/test/status" || string(req.Body) != "request" {
		t.Fatalf("management request = %#v, want forwarded request fields", req.ManagementRequest)
	}

	host.callbackContexts.mu.RLock()
	_, exists := host.callbackContexts.contexts[req.HostCallbackID]
	host.callbackContexts.mu.RUnlock()
	if exists {
		t.Fatal("management host_callback_id scope was not closed")
	}
}

func TestSanitizePluginRequestRemovesNonJSONMetadata(t *testing.T) {
	req := pluginapi.RequestInterceptRequest{
		Metadata: map[string]any{
			"keep":     "value",
			"callback": func(string) {},
			"nested": map[string]any{
				"keep": "nested",
				"drop": func() {},
			},
			"list": []any{"item", func() {}},
		},
	}
	raw, errMarshal := json.Marshal(sanitizePluginRequest(req))
	if errMarshal != nil {
		t.Fatalf("Marshal(sanitized request interceptor) error = %v", errMarshal)
	}
	var decoded pluginapi.RequestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("Unmarshal(sanitized request interceptor) error = %v", errUnmarshal)
	}
	if decoded.Metadata["keep"] != "value" {
		t.Fatalf("metadata keep = %#v, want value", decoded.Metadata)
	}
	if _, ok := decoded.Metadata["callback"]; ok {
		t.Fatalf("metadata callback survived sanitize: %#v", decoded.Metadata)
	}
	nested, ok := decoded.Metadata["nested"].(map[string]any)
	if !ok || nested["keep"] != "nested" {
		t.Fatalf("nested metadata = %#v, want keep", decoded.Metadata["nested"])
	}
	if _, ok := nested["drop"]; ok {
		t.Fatalf("nested metadata function survived sanitize: %#v", nested)
	}

	execReq := rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Metadata: map[string]any{
				"keep":     "value",
				"callback": func(string) {},
			},
		},
	}
	if _, errMarshalExec := json.Marshal(sanitizePluginRequest(execReq)); errMarshalExec != nil {
		t.Fatalf("Marshal(sanitized executor request) error = %v", errMarshalExec)
	}

	wrappedReq := rpcRequestInterceptRequest{
		RequestInterceptRequest: pluginapi.RequestInterceptRequest{
			Metadata: map[string]any{
				"keep":     "value",
				"callback": func(string) {},
			},
		},
		HostCallbackID: "callback-1",
	}
	if _, errMarshalWrapped := json.Marshal(sanitizePluginRequest(wrappedReq)); errMarshalWrapped != nil {
		t.Fatalf("Marshal(sanitized wrapped request interceptor) error = %v", errMarshalWrapped)
	}
}

func TestHostApplyConfigReusesPublishedPluginWhenConfigUnchanged(t *testing.T) {
	loader := newTestSymbolLoader()
	plugin := &testPlugin{
		registerResult:    validTestPlugin("alpha"),
		reconfigureResult: validTestPlugin("alpha"),
	}
	loader.lookups["alpha"] = newTestSymbolLookup(plugin)
	h := NewForTest(loader)
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     makePluginDir(t, "alpha"),
		},
	}

	h.ApplyConfig(context.Background(), cfg)
	h.ApplyConfig(context.Background(), cfg)

	if plugin.registerCalls != 1 {
		t.Fatalf("Register calls = %d, want 1", plugin.registerCalls)
	}
	if plugin.reconfigureCalls != 0 {
		t.Fatalf("Reconfigure calls = %d, want 0", plugin.reconfigureCalls)
	}
	if loader.openCalls != 1 {
		t.Fatalf("Open calls = %d, want 1", loader.openCalls)
	}
	if len(h.Snapshot().records) != 1 {
		t.Fatalf("Snapshot records = %d, want 1", len(h.Snapshot().records))
	}
}

func TestHostApplyConfigDoesNotHoldRootLockDuringCandidateRegister(t *testing.T) {
	loader := newTestSymbolLoader()
	oldPlugin := &testPlugin{registerResult: validTestPlugin("alpha")}
	loader.lookups["alpha"] = newTestSymbolLookup(oldPlugin)
	host := NewForTest(loader)
	dir := makePluginDir(t, "alpha")
	cfg := &config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: dir}}
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply != nil {
		t.Fatalf("initial ApplyConfig() error = %v", errApply)
	}
	t.Cleanup(host.ShutdownAll)

	registerEntered := make(chan struct{})
	releaseRegister := make(chan struct{})
	newLookup := newTestSymbolLookup(&testPlugin{registerResult: validTestPlugin("alpha")})
	newLookup.registerOverride = func([]byte) pluginapi.Plugin {
		close(registerEntered)
		<-releaseRegister
		return validTestPlugin("alpha")
	}
	loader.lookups["alpha"] = newLookup
	enabled := true
	cfg.Plugins.Configs = map[string]config.PluginInstanceConfig{
		"alpha": {Enabled: &enabled, Priority: 1},
	}
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- host.ApplyConfig(context.Background(), cfg)
	}()
	select {
	case <-registerEntered:
	case <-time.After(time.Second):
		t.Fatal("candidate Register did not start")
	}
	rootLockResult := make(chan struct {
		lock *pluginstore.RootLock
		err  error
	}, 1)
	go func() {
		rootLock, errLock := pluginstore.LockRoot(dir)
		rootLockResult <- struct {
			lock *pluginstore.RootLock
			err  error
		}{lock: rootLock, err: errLock}
	}()
	select {
	case result := <-rootLockResult:
		if result.err != nil {
			close(releaseRegister)
			t.Fatalf("LockRoot() while Register blocked: %v", result.err)
		}
		result.lock.Unlock()
	case <-time.After(time.Second):
		close(releaseRegister)
		t.Fatal("LockRoot waited for candidate Register")
	}
	close(releaseRegister)
	select {
	case errApply := <-applyDone:
		if errApply != nil {
			t.Fatalf("reload ApplyConfig() error = %v", errApply)
		}
	case <-time.After(time.Second):
		t.Fatal("ApplyConfig did not finish after candidate Register resumed")
	}
	if oldPlugin.reconfigureCalls != 0 {
		t.Fatalf("old Reconfigure calls = %d, want 0", oldPlugin.reconfigureCalls)
	}
}

func TestRegisteredPluginsIncludesMetadataAndOAuthCapability(t *testing.T) {
	loader := newTestSymbolLoader()
	plugin := &testPlugin{
		registerResult:    validTestPlugin("alpha"),
		reconfigureResult: validTestPlugin("alpha"),
	}
	plugin.registerResult.Metadata.Logo = "https://example.com/logo.svg"
	plugin.registerResult.Metadata.ConfigFields = []pluginapi.ConfigField{
		{
			Name:        "mode",
			Type:        pluginapi.ConfigFieldTypeEnum,
			EnumValues:  []string{"safe", "fast"},
			Description: "Execution mode.",
		},
	}
	plugin.registerResult.Capabilities.AuthProvider = fakeAuthProvider{identifier: "alpha"}
	loader.lookups["alpha"] = newTestSymbolLookup(plugin)
	h := NewForTest(loader)

	h.ApplyConfig(
		context.Background(), &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true,
				Dir:     makePluginDir(t, "alpha"),
			},
		},
	)

	infos := h.RegisteredPlugins()
	if len(infos) != 1 {
		t.Fatalf("RegisteredPlugins() len = %d, want 1; infos=%#v", len(infos), infos)
	}
	if !infos[0].SupportsOAuth {
		t.Fatalf("RegisteredPlugins()[0].SupportsOAuth = false, want true; infos=%#v", infos)
	}
	if infos[0].Metadata.Logo == "" || len(infos[0].Metadata.ConfigFields) != 1 {
		t.Fatalf("RegisteredPlugins()[0].Metadata = %#v, want logo and config fields", infos[0].Metadata)
	}
}

func TestHostApplyConfig_InvalidMetadataOrNoCapabilitiesSkipped(t *testing.T) {
	loader := newTestSymbolLoader()
	loader.lookups["empty-name"] = newTestSymbolLookup(
		&testPlugin{
			registerResult:    validTestPlugin(""),
			reconfigureResult: validTestPlugin(""),
		},
	)
	loader.lookups["no-caps"] = newTestSymbolLookup(
		&testPlugin{
			registerResult:    validTestPlugin("no-caps"),
			reconfigureResult: validTestPlugin("no-caps"),
		},
	)
	loader.lookups["no-caps"].registerOverride = func([]byte) pluginapi.Plugin {
		return pluginapi.Plugin{
			Metadata: pluginapi.Metadata{
				Name:             "no-caps",
				Version:          "1.0.0",
				Author:           "test",
				GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			},
		}
	}
	h := NewForTest(loader)

	h.ApplyConfig(
		context.Background(), &config.Config{
			Plugins: config.PluginsConfig{
				Enabled: true,
				Dir:     makePluginDir(t, "empty-name", "no-caps"),
			},
		},
	)

	if len(h.Snapshot().records) != 0 {
		t.Fatalf("Snapshot records = %d, want 0", len(h.Snapshot().records))
	}
}

func TestHostApplyConfigPreservesActivePluginWhenReplacementRegisterFails(t *testing.T) {
	loader := newTestSymbolLoader()
	activePlugin := &testPlugin{registerResult: validTestPlugin("alpha")}
	activeLookup := newTestSymbolLookup(activePlugin)
	loader.lookups["alpha"] = activeLookup
	host := NewForTest(loader)
	dir := makePluginDir(t, "alpha")
	cfg := &config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: dir}}
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply != nil {
		t.Fatalf("initial ApplyConfig() error = %v", errApply)
	}
	t.Cleanup(host.ShutdownAll)

	rejectedLookup := newTestSymbolLookup(&testPlugin{})
	rejectedLookup.registerOverride = func([]byte) pluginapi.Plugin { return pluginapi.Plugin{} }
	loader.lookups["alpha"] = rejectedLookup
	enabled := true
	cfg.Plugins.Configs = map[string]config.PluginInstanceConfig{
		"alpha": {Enabled: &enabled, Priority: 1},
	}
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply == nil {
		t.Fatal("replacement ApplyConfig() error = nil")
	}
	records := host.activeRecords()
	if len(records) != 1 || records[0].id != "alpha" || host.loadedPlugin("alpha") == nil {
		t.Fatalf("active plugin changed after rejected replacement: records=%#v loaded=%#v", records, host.loadedPlugin("alpha"))
	}
	if activeLookup.shutdownCalls != 0 || rejectedLookup.shutdownCalls != 1 {
		t.Fatalf("Shutdown calls active=%d rejected=%d, want 0/1", activeLookup.shutdownCalls, rejectedLookup.shutdownCalls)
	}
}

func TestHostApplyConfigPreservesActivePluginWhenReplacementLoadFails(t *testing.T) {
	loader := newTestSymbolLoader()
	activeLookup := newTestSymbolLookup(&testPlugin{registerResult: validTestPlugin("alpha")})
	loader.lookups["alpha"] = activeLookup
	host := NewForTest(loader)
	cfg := &config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: makePluginDir(t, "alpha")}}
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply != nil {
		t.Fatalf("initial ApplyConfig() error = %v", errApply)
	}
	t.Cleanup(host.ShutdownAll)

	loader.lookups["alpha"] = nil
	cfg.Plugins.Dir = makePluginDir(t, "alpha")
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply == nil ||
		!strings.Contains(errApply.Error(), "load replacement plugin alpha") {
		t.Fatalf("replacement ApplyConfig() error = %v, want load failure", errApply)
	}
	records := host.activeRecords()
	if len(records) != 1 || records[0].id != "alpha" || host.loadedPlugin("alpha") == nil {
		t.Fatalf("active plugin changed after failed replacement load: records=%#v loaded=%#v", records, host.loadedPlugin("alpha"))
	}
	if activeLookup.shutdownCalls != 0 {
		t.Fatalf("active plugin shutdown calls = %d, want 0", activeLookup.shutdownCalls)
	}
}

func TestHostApplyConfig_PanicFusesPluginForProcessLifetime(t *testing.T) {
	loader := newTestSymbolLoader()
	activePlugin := &testPlugin{registerResult: validTestPlugin("alpha")}
	loader.lookups["alpha"] = newTestSymbolLookup(activePlugin)
	h := NewForTest(loader)
	cfg := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     makePluginDir(t, "alpha"),
		},
	}

	h.ApplyConfig(context.Background(), cfg)
	candidate := &testPlugin{registerResult: validTestPlugin("alpha"), panicOnRegister: true}
	loader.lookups["alpha"] = newTestSymbolLookup(candidate)
	enabled := true
	cfg.Plugins.Configs = map[string]config.PluginInstanceConfig{
		"alpha": {Enabled: &enabled, Priority: 1},
	}
	h.ApplyConfig(context.Background(), cfg)
	candidate.panicOnRegister = false
	cfg.Plugins.Configs["alpha"] = config.PluginInstanceConfig{Enabled: &enabled, Priority: 2}
	h.ApplyConfig(context.Background(), cfg)

	if activePlugin.registerCalls != 1 || candidate.registerCalls != 1 {
		t.Fatalf(
			"Register calls active=%d candidate=%d, want 1/1",
			activePlugin.registerCalls,
			candidate.registerCalls,
		)
	}
	if activePlugin.reconfigureCalls != 0 || candidate.reconfigureCalls != 0 {
		t.Fatalf(
			"Reconfigure calls active=%d candidate=%d, want 0/0",
			activePlugin.reconfigureCalls,
			candidate.reconfigureCalls,
		)
	}
	if len(h.Snapshot().records) != 0 {
		t.Fatalf("Snapshot records = %d, want 0 after fuse", len(h.Snapshot().records))
	}
}

func TestHostRejectedCandidateRemainsManagedWhileLaterRegisterBlocks(t *testing.T) {
	loader := newTestSymbolLoader()
	alphaLookup := newTestSymbolLookup(&testPlugin{})
	alphaLookup.registerOverride = func([]byte) pluginapi.Plugin {
		return pluginapi.Plugin{}
	}
	betaEntered := make(chan struct{})
	betaRelease := make(chan struct{})
	betaLookup := newTestSymbolLookup(&testPlugin{})
	betaLookup.registerOverride = func([]byte) pluginapi.Plugin {
		close(betaEntered)
		<-betaRelease
		return validTestPlugin("beta")
	}
	loader.lookups["alpha"] = alphaLookup
	loader.lookups["beta"] = betaLookup
	host := NewForTest(loader)
	dir := makePluginDir(t, "alpha", "beta")
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- host.ApplyConfig(
			context.Background(),
			&config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: dir}},
		)
	}()
	select {
	case <-betaEntered:
	case <-time.After(time.Second):
		t.Fatal("later candidate Register did not start")
	}
	if alphaLookup.shutdownCalls != 1 {
		t.Fatalf("rejected candidate Shutdown calls = %d, want 1", alphaLookup.shutdownCalls)
	}
	close(betaRelease)
	select {
	case errApply := <-applyDone:
		if errApply != nil {
			t.Fatalf("ApplyConfig() error = %v", errApply)
		}
	case <-time.After(time.Second):
		t.Fatal("ApplyConfig did not finish")
	}
	host.ShutdownAll()
}

func TestHostDiscardedCandidateRemainsBusyUntilShutdownCompletes(t *testing.T) {
	shutdownStarted := make(chan struct{})
	shutdownRelease := make(chan struct{})
	lookup := newTestSymbolLookup(&testPlugin{})
	lookup.registerOverride = func([]byte) pluginapi.Plugin { return pluginapi.Plugin{} }
	lookup.shutdownStarted = shutdownStarted
	lookup.shutdownRelease = shutdownRelease
	loader := newTestSymbolLoader()
	loader.lookups["alpha"] = lookup
	host := NewForTest(loader)
	dir := makePluginDir(t, "alpha")
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- host.ApplyConfig(
			context.Background(),
			&config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: dir}},
		)
	}()
	select {
	case <-shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("discarded candidate shutdown did not start")
	}
	if !host.PluginBusy("alpha") {
		t.Fatal("PluginBusy(alpha) = false after pending candidate entered shutdown")
	}
	close(shutdownRelease)
	select {
	case errApply := <-applyDone:
		if errApply != nil {
			t.Fatalf("ApplyConfig() error = %v", errApply)
		}
	case <-time.After(time.Second):
		t.Fatal("ApplyConfig did not finish after discarded candidate shutdown")
	}
	deadline := time.Now().Add(time.Second)
	for host.PluginBusy("alpha") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if host.PluginBusy("alpha") {
		t.Fatal("PluginBusy(alpha) remained true after discarded candidate shutdown")
	}
}

func TestHostInitializationRemainsBusyUntilLoaderReturns(t *testing.T) {
	lookup := newTestSymbolLookup(&testPlugin{registerResult: validTestPlugin("alpha")})
	loader := &blockingInitializationLoader{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		client:  lookup,
	}
	host := NewForTest(loader)
	dir := makePluginDir(t, "alpha")
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- host.ApplyConfig(
			context.Background(),
			&config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: dir}},
		)
	}()
	select {
	case <-loader.entered:
	case <-time.After(time.Second):
		t.Fatal("plugin initialization did not start")
	}
	if !host.PluginBusy("alpha") {
		t.Fatal("PluginBusy(alpha) = false during initialization")
	}
	unloaded, errUnload := host.UnloadPluginWithError("alpha")
	if !unloaded || !errors.Is(errUnload, errPluginInitializationInProgress) {
		t.Fatalf("UnloadPluginWithError() = (%v, %v), want restart-required initialization error", unloaded, errUnload)
	}
	if !host.PluginBusy("alpha") {
		t.Fatal("PluginBusy(alpha) = false after unresolved initialization unload")
	}
	if errShutdown := host.ShutdownAllContext(context.Background()); !errors.Is(errShutdown, errPluginInitializationInProgress) {
		t.Fatalf("ShutdownAllContext() error = %v, want initialization error", errShutdown)
	}
	if !host.PluginBusy("alpha") {
		t.Fatal("PluginBusy(alpha) = false before initialization returned")
	}
	close(loader.release)
	select {
	case errApply := <-applyDone:
		if errApply != nil {
			t.Fatalf("ApplyConfig() error = %v", errApply)
		}
	case <-time.After(time.Second):
		t.Fatal("ApplyConfig did not finish after initialization returned")
	}
	deadline := time.Now().Add(time.Second)
	for host.PluginBusy("alpha") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if host.PluginBusy("alpha") {
		t.Fatal("PluginBusy(alpha) remained true after initialization cleanup")
	}
	if lookup.shutdownCalls != 1 {
		t.Fatalf("stale initialized client Shutdown calls = %d, want 1", lookup.shutdownCalls)
	}
}

func TestHostUnloadPluginRemovesRuntimeStateAndShutsDownClient(t *testing.T) {
	usageEntered := make(chan struct{})
	usageRelease := make(chan struct{})
	registerResult := validTestPlugin("alpha")
	registerResult.Capabilities.UsagePlugin = usagePluginFunc(
		func(_ context.Context, record pluginapi.UsageRecord) {
			if record.Provider != "unload-lifecycle" {
				return
			}
			close(usageEntered)
			<-usageRelease
		},
	)
	loader := newTestSymbolLoader()
	plugin := &testPlugin{
		registerResult:    registerResult,
		reconfigureResult: registerResult,
	}
	lookup := newTestSymbolLookup(plugin)
	lookup.shutdownStarted = make(chan struct{})
	loader.lookups["alpha"] = lookup
	h := NewForTest(loader)
	t.Cleanup(func() {
		select {
		case <-usageRelease:
		default:
			close(usageRelease)
		}
		h.ShutdownAll()
	})
	dir := makePluginDir(t, "alpha")

	h.ApplyConfig(context.Background(), &config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: dir}})
	if !h.PluginLoaded("alpha") {
		t.Fatal("PluginLoaded(alpha) = false, want true")
	}
	if len(h.activeRecords()) != 1 {
		t.Fatalf("activeRecords() len = %d, want 1", len(h.activeRecords()))
	}
	coreusage.PublishRecord(context.Background(), coreusage.Record{Provider: "unload-lifecycle"})
	select {
	case <-usageEntered:
	case <-time.After(time.Second):
		t.Fatal("usage callback did not start")
	}

	unloaded := make(chan bool, 1)
	go func() {
		unloaded <- h.UnloadPlugin("alpha")
	}()
	select {
	case result := <-unloaded:
		t.Fatalf("UnloadPlugin returned %v before the usage callback completed", result)
	case <-lookup.shutdownStarted:
		t.Fatal("plugin client shut down before the usage callback completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(usageRelease)
	select {
	case result := <-unloaded:
		if !result {
			t.Fatal("UnloadPlugin(alpha) = false, want true")
		}
	case <-time.After(time.Second):
		t.Fatal("UnloadPlugin did not finish after the usage callback completed")
	}
	select {
	case <-lookup.shutdownStarted:
	default:
		t.Fatal("plugin client was not shut down after the usage callback completed")
	}
	if h.PluginLoaded("alpha") || h.PluginBusy("alpha") {
		t.Fatal("plugin remained loaded or busy after unload")
	}
	if len(h.activeRecords()) != 0 {
		t.Fatalf("activeRecords() len = %d, want 0", len(h.activeRecords()))
	}
	if lookup.shutdownCalls != 1 {
		t.Fatalf("Shutdown calls = %d, want 1", lookup.shutdownCalls)
	}
}

func TestHostApplyConfigPreservesOldPluginWhenFinalRootLockFails(t *testing.T) {
	loader := newTestSymbolLoader()
	oldPlugin := &testPlugin{
		registerResult:    validTestPlugin("alpha"),
		reconfigureResult: validTestPlugin("alpha"),
	}
	newPlugin := &testPlugin{
		registerResult:    validTestPlugin("alpha"),
		reconfigureResult: validTestPlugin("alpha"),
	}
	oldLookup := newTestSymbolLookup(oldPlugin)
	newLookup := newTestSymbolLookup(newPlugin)
	loader.lookups["alpha"] = oldLookup
	host := NewForTest(loader)
	dir := makePluginDir(t, "alpha-v1.0.0")
	previousConfig := &config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: dir}}
	if errApply := host.ApplyConfig(context.Background(), previousConfig); errApply != nil {
		t.Fatalf("initial ApplyConfig() error = %v", errApply)
	}
	t.Cleanup(host.ShutdownAll)

	loader.lookups["alpha"] = newLookup
	makePluginFile(t, dir, "alpha-v1.1.0")
	backupDir := dir + ".backup"
	newLookup.registerOverride = func([]byte) pluginapi.Plugin {
		if errRename := os.Rename(dir, backupDir); errRename != nil {
			t.Fatalf("rename plugin root: %v", errRename)
		}
		if errWrite := os.WriteFile(dir, []byte("blocks final root lock"), 0o600); errWrite != nil {
			t.Fatalf("block plugin root: %v", errWrite)
		}
		return validTestPlugin("alpha")
	}
	nextConfig := &config.Config{Debug: true, Plugins: config.PluginsConfig{Enabled: true, Dir: dir}}
	errApply := host.ApplyConfig(context.Background(), nextConfig)
	if errApply == nil {
		t.Fatal("ApplyConfig() error = nil")
	}
	if errRemove := os.Remove(dir); errRemove != nil {
		t.Fatalf("remove blocked plugin root: %v", errRemove)
	}
	if errRestore := os.Rename(backupDir, dir); errRestore != nil {
		t.Fatalf("restore plugin root: %v", errRestore)
	}

	records := host.activeRecords()
	if len(records) != 1 || records[0].id != "alpha" || records[0].version != "1.0.0" {
		t.Fatalf("active records after failed reload = %#v", records)
	}
	loaded := host.loadedPlugin("alpha")
	if loaded == nil || loaded.version != "1.0.0" {
		t.Fatalf("loaded plugin after failed reload = %#v", loaded)
	}
	if oldLookup.shutdownCalls != 0 || newLookup.shutdownCalls != 1 {
		t.Fatalf("shutdown calls old=%d new=%d, want 0/1", oldLookup.shutdownCalls, newLookup.shutdownCalls)
	}
	if current := host.currentRuntimeConfig(); current != previousConfig {
		t.Fatalf("runtime config = %p, want previous %p", current, previousConfig)
	}
}

func TestHostConditionalUnloadRejectsReplacedRuntimeIdentity(t *testing.T) {
	loader := newTestSymbolLoader()
	loader.lookups["alpha"] = newTestSymbolLookup(&testPlugin{registerResult: validTestPlugin("alpha")})
	host := NewForTest(loader)
	dir := makePluginDir(t, "alpha")
	cfg := &config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: dir}}
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply != nil {
		t.Fatalf("initial ApplyConfig() error = %v", errApply)
	}
	t.Cleanup(host.ShutdownAll)
	identity := host.PluginRuntimeIdentity("alpha")

	loader.lookups["alpha"] = newTestSymbolLookup(&testPlugin{registerResult: validTestPlugin("alpha")})
	enabled := true
	cfg.Plugins.Configs = map[string]config.PluginInstanceConfig{
		"alpha": {Enabled: &enabled, Priority: 1},
	}
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply != nil {
		t.Fatalf("replacement ApplyConfig() error = %v", errApply)
	}
	unloaded, errUnload := host.UnloadPluginIfCurrentWithError(identity)
	if unloaded || !errors.Is(errUnload, ErrPluginRuntimeSuperseded) {
		t.Fatalf("UnloadPluginIfCurrentWithError() = (%v, %v), want superseded identity", unloaded, errUnload)
	}
	if !host.PluginLoaded("alpha") {
		t.Fatal("conditional unload removed the replacement runtime")
	}
}

func TestHostApplyConfigHotReloadsChangedVersionAndShutsDownOldClient(t *testing.T) {
	usageEntered := make(chan struct{})
	usageRelease := make(chan struct{})
	newRecords := make(chan pluginapi.UsageRecord, 1)
	oldResult := validTestPlugin("alpha")
	oldResult.Capabilities.UsagePlugin = usagePluginFunc(
		func(_ context.Context, record pluginapi.UsageRecord) {
			if record.Provider != "hot-reload-lifecycle" {
				return
			}
			close(usageEntered)
			<-usageRelease
		},
	)
	newResult := validTestPlugin("alpha")
	newResult.Capabilities.UsagePlugin = usagePluginFunc(
		func(_ context.Context, record pluginapi.UsageRecord) {
			if record.Provider == "hot-reload-new-generation" {
				newRecords <- record
			}
		},
	)
	loader := newTestSymbolLoader()
	oldPlugin := &testPlugin{
		registerResult:    oldResult,
		reconfigureResult: oldResult,
	}
	newPlugin := &testPlugin{
		registerResult:    newResult,
		reconfigureResult: newResult,
	}
	oldLookup := newTestSymbolLookup(oldPlugin)
	oldLookup.shutdownStarted = make(chan struct{})
	newLookup := newTestSymbolLookup(newPlugin)
	loader.lookups["alpha"] = oldLookup
	h := NewForTest(loader)
	t.Cleanup(func() {
		select {
		case <-usageRelease:
		default:
			close(usageRelease)
		}
		h.ShutdownAll()
	})
	dir := makePluginDir(t, "alpha-v1.0.0")
	cfg := &config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: dir}}

	h.ApplyConfig(context.Background(), cfg)
	coreusage.PublishRecord(context.Background(), coreusage.Record{Provider: "hot-reload-lifecycle"})
	select {
	case <-usageEntered:
	case <-time.After(time.Second):
		t.Fatal("old usage callback did not start")
	}

	loader.lookups["alpha"] = newLookup
	makePluginFile(t, dir, "alpha-v1.1.0")
	reloaded := make(chan struct{})
	go func() {
		h.ApplyConfig(context.Background(), cfg)
		close(reloaded)
	}()
	select {
	case <-reloaded:
		t.Fatal("ApplyConfig returned before the old usage callback completed")
	case <-oldLookup.shutdownStarted:
		t.Fatal("old plugin client shut down before its usage callback completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(usageRelease)
	select {
	case <-reloaded:
	case <-time.After(time.Second):
		t.Fatal("ApplyConfig did not finish after the old usage callback completed")
	}
	select {
	case <-oldLookup.shutdownStarted:
	default:
		t.Fatal("old plugin client was not shut down after its usage callback completed")
	}

	if oldPlugin.registerCalls != 1 || newPlugin.registerCalls != 1 {
		t.Fatalf("register calls old=%d new=%d, want 1/1", oldPlugin.registerCalls, newPlugin.registerCalls)
	}
	if oldPlugin.reconfigureCalls != 0 {
		t.Fatalf("old reconfigure calls = %d, want 0", oldPlugin.reconfigureCalls)
	}
	if !h.PluginBusy("alpha") {
		t.Fatal("PluginBusy(alpha) = false, want true while active client exists")
	}
	if oldLookup.shutdownCalls != 1 {
		t.Fatalf("old Shutdown calls = %d after hot reload, want 1", oldLookup.shutdownCalls)
	}
	coreusage.PublishRecord(context.Background(), coreusage.Record{Provider: "hot-reload-new-generation"})
	select {
	case <-newRecords:
	case <-time.After(time.Second):
		t.Fatal("new usage generation did not receive the post-reload record")
	}
	if !h.UnloadPlugin("alpha") {
		t.Fatal("UnloadPlugin(alpha) = false, want true")
	}
	if oldLookup.shutdownCalls != 1 || newLookup.shutdownCalls != 1 {
		t.Fatalf("shutdown calls old=%d new=%d, want 1/1", oldLookup.shutdownCalls, newLookup.shutdownCalls)
	}
}

func TestHostHotReloadPublishesExternalAdaptersBeforeOldClientShutdown(t *testing.T) {
	registrarEntered := make(chan struct{})
	registrarRelease := make(chan struct{})
	oldResult := validTestPlugin("alpha")
	oldResult.Capabilities.ModelRegistrar = staticModelRegistrar("alpha-provider", "old-model")
	newResult := validTestPlugin("alpha")
	newResult.Capabilities.ModelRegistrar = modelRegistrarFunc(
		func(
			ctx context.Context,
			_ pluginapi.ModelRegistrationRequest,
		) (pluginapi.ModelRegistrationResponse, error) {
			close(registrarEntered)
			select {
			case <-registrarRelease:
			case <-ctx.Done():
				return pluginapi.ModelRegistrationResponse{}, ctx.Err()
			}
			return pluginapi.ModelRegistrationResponse{
				Provider: "alpha-provider",
				Models:   []pluginapi.ModelInfo{{ID: "new-model"}},
			}, nil
		},
	)
	loader := newTestSymbolLoader()
	oldLookup := newTestSymbolLookup(&testPlugin{registerResult: oldResult})
	oldLookup.shutdownStarted = make(chan struct{})
	newLookup := newTestSymbolLookup(&testPlugin{registerResult: newResult})
	loader.lookups["alpha"] = oldLookup
	host := NewForTest(loader)
	t.Cleanup(func() {
		select {
		case <-registrarRelease:
		default:
			close(registrarRelease)
		}
		host.ShutdownAll()
	})
	dir := makePluginDir(t, "alpha-v1.0.0")
	cfg := &config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: dir}}
	host.ApplyConfig(context.Background(), cfg)
	modelRegistry := newFakeModelRegistry()
	host.RegisterModels(context.Background(), modelRegistry)

	loader.lookups["alpha"] = newLookup
	makePluginFile(t, dir, "alpha-v1.1.0")
	reloaded := make(chan struct{})
	go func() {
		host.ApplyConfig(context.Background(), cfg)
		close(reloaded)
	}()
	select {
	case <-registrarEntered:
	case <-time.After(time.Second):
		t.Fatal("new model registrar did not start")
	}
	select {
	case <-oldLookup.shutdownStarted:
		t.Fatal("old plugin client shut down before new model publication completed")
	case <-reloaded:
		t.Fatal("ApplyConfig returned while new model publication was blocked")
	case <-time.After(50 * time.Millisecond):
	}
	if _, okOld := modelRegistry.clients["plugin:alpha:alpha-provider"]; !okOld {
		t.Fatal("old model publication was removed while new registrar was blocked")
	}

	close(registrarRelease)
	select {
	case <-reloaded:
	case <-time.After(time.Second):
		t.Fatal("ApplyConfig did not finish after model publication completed")
	}
	select {
	case <-oldLookup.shutdownStarted:
	default:
		t.Fatal("old plugin client was not shut down after external publication completed")
	}
	client := modelRegistry.clients["plugin:alpha:alpha-provider"]
	if client == nil || len(client.models) != 1 || client.models[0].ID != "new-model" {
		t.Fatalf("new model publication = %#v", client)
	}
}

func TestSortRecordsPriorityDescendingAndIDTieBreak(t *testing.T) {
	records := []capabilityRecord{
		{id: "charlie", priority: 1},
		{id: "bravo", priority: 2},
		{id: "alpha", priority: 2},
	}

	sortRecords(records)

	want := []string{"alpha", "bravo", "charlie"}
	for index, id := range want {
		if records[index].id != id {
			t.Fatalf("records[%d].id = %q, want %q", index, records[index].id, id)
		}
	}
}

type capturePluginClient struct {
	requests map[string][]byte
}

func (c *capturePluginClient) Call(ctx context.Context, method string, request []byte) ([]byte, error) {
	if c.requests == nil {
		c.requests = make(map[string][]byte)
	}
	c.requests[method] = append([]byte(nil), request...)
	return marshalRPCResult(rpcEmptyResponse{})
}

func (c *capturePluginClient) Shutdown() {}
