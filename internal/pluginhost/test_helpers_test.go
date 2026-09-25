package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type testSymbolLoader struct {
	openCalls int
	lookups   map[string]*testSymbolLookup
}

func newTestSymbolLoader() *testSymbolLoader {
	return &testSymbolLoader{lookups: make(map[string]*testSymbolLookup)}
}

func (l *testSymbolLoader) Open(file pluginFile, _ *Host) (pluginClient, error) {
	l.openCalls++
	lookup := l.lookups[file.ID]
	if lookup == nil {
		return nil, fmt.Errorf("missing test plugin for %s", file.Path)
	}
	return lookup, nil
}

type testSymbolLookup struct {
	plugin              *testPlugin
	active              pluginapi.Plugin
	shutdownCalls       int
	shutdownStarted     chan struct{}
	shutdownRelease     chan struct{}
	shutdownOnce        sync.Once
	registerOverride    func([]byte) pluginapi.Plugin
	reconfigureOverride func([]byte) pluginapi.Plugin
	methodHandlers      map[string]func(context.Context, []byte) ([]byte, error)
	shutdownHook        func()
	schemaVersion       uint32
	schemaVersionSet    bool
	lastLifecycle       rpcLifecycleRequest
}

func newTestSymbolLookup(plugin *testPlugin) *testSymbolLookup {
	return &testSymbolLookup{plugin: plugin}
}

func (l *testSymbolLookup) Call(ctx context.Context, method string, request []byte) ([]byte, error) {
	if handler := l.methodHandlers[method]; handler != nil {
		return handler(ctx, request)
	}
	switch method {
	case pluginabi.MethodPluginRegister:
		return l.callLifecycle(request, false)
	case pluginabi.MethodPluginReconfigure:
		return l.callLifecycle(request, true)
	case pluginabi.MethodThinkingIdentifier:
		if l.active.Capabilities.ThinkingApplier == nil {
			return nil, fmt.Errorf("missing thinking applier")
		}
		return marshalRPCResult(rpcIdentifierResponse{Identifier: l.active.Capabilities.ThinkingApplier.Identifier()})
	case pluginabi.MethodThinkingApply:
		var req pluginapi.ThinkingApplyRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		resp, errApply := l.active.Capabilities.ThinkingApplier.ApplyThinking(ctx, req)
		if errApply != nil {
			return nil, errApply
		}
		return marshalRPCResult(resp)
	case pluginabi.MethodRequestInterceptBefore:
		if l.active.Capabilities.RequestInterceptor == nil {
			return nil, fmt.Errorf("missing request interceptor")
		}
		var req pluginapi.RequestInterceptRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		resp, errIntercept := l.active.Capabilities.RequestInterceptor.InterceptRequestBeforeAuth(ctx, req)
		if errIntercept != nil {
			return nil, errIntercept
		}
		return marshalRPCResult(resp)
	case pluginabi.MethodRequestInterceptAfter:
		if l.active.Capabilities.RequestInterceptor == nil {
			return nil, fmt.Errorf("missing request interceptor")
		}
		var req pluginapi.RequestInterceptRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		resp, errIntercept := l.active.Capabilities.RequestInterceptor.InterceptRequestAfterAuth(ctx, req)
		if errIntercept != nil {
			return nil, errIntercept
		}
		return marshalRPCResult(resp)
	case pluginabi.MethodRequestComplete:
		if l.active.Capabilities.RequestLifecyclePlugin == nil {
			return nil, fmt.Errorf("missing request lifecycle plugin")
		}
		var req pluginapi.RequestCompletion
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		if errComplete := l.active.Capabilities.RequestLifecyclePlugin.HandleRequestComplete(
			ctx, req,
		); errComplete != nil {
			return nil, errComplete
		}
		return marshalRPCResult(rpcEmptyResponse{})
	case pluginabi.MethodResponseInterceptAfter:
		if l.active.Capabilities.ResponseInterceptor == nil {
			return nil, fmt.Errorf("missing response interceptor")
		}
		var req pluginapi.ResponseInterceptRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		resp, errIntercept := l.active.Capabilities.ResponseInterceptor.InterceptResponse(ctx, req)
		if errIntercept != nil {
			return nil, errIntercept
		}
		return marshalRPCResult(resp)
	case pluginabi.MethodResponseInterceptStreamChunk:
		if l.active.Capabilities.StreamChunkInterceptor == nil {
			return nil, fmt.Errorf("missing stream chunk interceptor")
		}
		var req pluginapi.StreamChunkInterceptRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		resp, errIntercept := l.active.Capabilities.StreamChunkInterceptor.InterceptStreamChunk(ctx, req)
		if errIntercept != nil {
			return nil, errIntercept
		}
		return marshalRPCResult(resp)
	case pluginabi.MethodAuthIdentifier:
		if l.active.Capabilities.AuthProvider == nil {
			return nil, fmt.Errorf("missing auth provider")
		}
		return marshalRPCResult(rpcIdentifierResponse{Identifier: l.active.Capabilities.AuthProvider.Identifier()})
	case pluginabi.MethodModelRegister:
		if l.active.Capabilities.ModelRegistrar == nil {
			return nil, fmt.Errorf("missing model registrar")
		}
		var req pluginapi.ModelRegistrationRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		resp, errRegister := l.active.Capabilities.ModelRegistrar.RegisterModels(ctx, req)
		if errRegister != nil {
			return nil, errRegister
		}
		return marshalRPCResult(resp)
	case pluginabi.MethodSchedulerPick:
		if l.active.Capabilities.Scheduler == nil {
			return nil, fmt.Errorf("missing scheduler")
		}
		var req pluginapi.SchedulerPickRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		resp, errPick := l.active.Capabilities.Scheduler.Pick(ctx, req)
		if errPick != nil {
			return nil, errPick
		}
		return marshalRPCResult(resp)
	case pluginabi.MethodUsageHandle:
		if l.active.Capabilities.UsagePlugin == nil {
			return marshalRPCResult(rpcEmptyResponse{})
		}
		var record pluginapi.UsageRecord
		if errUnmarshal := json.Unmarshal(request, &record); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		l.active.Capabilities.UsagePlugin.HandleUsage(ctx, record)
		return marshalRPCResult(rpcEmptyResponse{})
	case pluginabi.MethodPluginQuiesce:
		return marshalRPCError("method_not_found", "plugin quiesce is unsupported"), nil
	default:
		return nil, fmt.Errorf("missing test method %s", method)
	}
}

func (l *testSymbolLookup) Shutdown() {
	l.shutdownCalls++
	if l.shutdownHook != nil {
		l.shutdownHook()
	}
	if l.shutdownStarted != nil {
		l.shutdownOnce.Do(
			func() {
				close(l.shutdownStarted)
			},
		)
	}
	if l.shutdownRelease != nil {
		<-l.shutdownRelease
	}
}

func (l *testSymbolLookup) callLifecycle(request []byte, reload bool) ([]byte, error) {
	var req rpcLifecycleRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	l.lastLifecycle = req
	var plugin pluginapi.Plugin
	if reload {
		if l.reconfigureOverride != nil {
			plugin = l.reconfigureOverride(req.ConfigYAML)
		} else {
			plugin = l.plugin.Reconfigure(req.ConfigYAML)
		}
	} else {
		if l.registerOverride != nil {
			plugin = l.registerOverride(req.ConfigYAML)
		} else {
			plugin = l.plugin.Register(req.ConfigYAML)
		}
	}
	l.active = plugin
	schemaVersion := l.schemaVersion
	if schemaVersion == 0 && !l.schemaVersionSet {
		schemaVersion = pluginabi.SchemaVersion
	}
	return marshalRPCResult(
		rpcRegistration{
			SchemaVersion: schemaVersion,
			Metadata:      plugin.Metadata,
			Capabilities:  rpcCapabilitiesFromPlugin(plugin),
		},
	)
}

type testPlugin struct {
	registerCalls     int
	reconfigureCalls  int
	registerResult    pluginapi.Plugin
	reconfigureResult pluginapi.Plugin
	panicOnRegister   bool
	panicOnReload     bool
}

func (p *testPlugin) Register(_ []byte) pluginapi.Plugin {
	p.registerCalls++
	if p.panicOnRegister {
		panic("register panic")
	}
	return p.registerResult
}

func (p *testPlugin) Reconfigure(_ []byte) pluginapi.Plugin {
	p.reconfigureCalls++
	if p.panicOnReload {
		panic("reconfigure panic")
	}
	return p.reconfigureResult
}

func validTestPlugin(name string) pluginapi.Plugin {
	return pluginapi.Plugin{
		Metadata: pluginapi.Metadata{
			Name:             name,
			Version:          "1.0.0",
			Author:           "test",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
		},
		Capabilities: pluginapi.Capabilities{
			UsagePlugin: testUsageCapability{},
		},
	}
}

type testUsageCapability struct{}

func (testUsageCapability) HandleUsage(context.Context, pluginapi.UsageRecord) {}

type requestLifecyclePluginFunc func(context.Context, pluginapi.RequestCompletion)

func (f requestLifecyclePluginFunc) HandleRequestComplete(
	ctx context.Context,
	completion pluginapi.RequestCompletion,
) error {
	f(ctx, completion)
	return nil
}

type requestInterceptorFunc func(
	context.Context,
	pluginapi.RequestInterceptRequest,
) (pluginapi.RequestInterceptResponse, error)

func (f requestInterceptorFunc) InterceptRequestBeforeAuth(
	ctx context.Context,
	req pluginapi.RequestInterceptRequest,
) (pluginapi.RequestInterceptResponse, error) {
	if f == nil {
		return pluginapi.RequestInterceptResponse{}, fmt.Errorf("missing request interceptor callback")
	}
	return f(ctx, req)
}

func (f requestInterceptorFunc) InterceptRequestAfterAuth(
	ctx context.Context,
	req pluginapi.RequestInterceptRequest,
) (pluginapi.RequestInterceptResponse, error) {
	if f == nil {
		return pluginapi.RequestInterceptResponse{}, fmt.Errorf("missing request interceptor callback")
	}
	return f(ctx, req)
}

type schedulerFunc func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error)

func (f schedulerFunc) Pick(ctx context.Context, req pluginapi.SchedulerPickRequest) (
	pluginapi.SchedulerPickResponse,
	error,
) {
	if f == nil {
		return pluginapi.SchedulerPickResponse{}, fmt.Errorf("missing scheduler callback")
	}
	return f(ctx, req)
}

type responseInterceptorFunc struct {
	interceptResponse func(context.Context, pluginapi.ResponseInterceptRequest) (
		pluginapi.ResponseInterceptResponse,
		error,
	)
	interceptStreamChunk func(
		context.Context,
		pluginapi.StreamChunkInterceptRequest,
	) (pluginapi.StreamChunkInterceptResponse, error)
}

func (f responseInterceptorFunc) InterceptResponse(
	ctx context.Context,
	req pluginapi.ResponseInterceptRequest,
) (pluginapi.ResponseInterceptResponse, error) {
	if f.interceptResponse == nil {
		return pluginapi.ResponseInterceptResponse{}, fmt.Errorf("missing response interceptor callback")
	}
	return f.interceptResponse(ctx, req)
}

func (f responseInterceptorFunc) InterceptStreamChunk(
	ctx context.Context,
	req pluginapi.StreamChunkInterceptRequest,
) (pluginapi.StreamChunkInterceptResponse, error) {
	if f.interceptStreamChunk == nil {
		return pluginapi.StreamChunkInterceptResponse{}, fmt.Errorf("missing stream chunk interceptor callback")
	}
	return f.interceptStreamChunk(ctx, req)
}

func makePluginDir(t *testing.T, ids ...string) string {
	t.Helper()
	root := t.TempDir()
	archDir := filepath.Join(root, runtime.GOOS, runtime.GOARCH)
	if errMkdirAll := os.MkdirAll(archDir, 0o755); errMkdirAll != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdirAll)
	}
	for _, id := range ids {
		path := filepath.Join(archDir, id+pluginExtension(runtime.GOOS))
		if errWriteFile := os.WriteFile(path, []byte("x"), 0o644); errWriteFile != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, errWriteFile)
		}
	}
	return root
}
