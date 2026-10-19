package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type homeCapabilityCaptureExecutor struct {
	roundExecutor
	requests []cliproxyexecutor.Request
}

func (e *homeCapabilityCaptureExecutor) Execute(
	_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.requests = append(e.requests, req)
	return cliproxyexecutor.Response{}, nil
}

func (e *homeCapabilityCaptureExecutor) CountTokens(
	_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.requests = append(e.requests, req)
	return cliproxyexecutor.Response{}, nil
}

func (e *homeCapabilityCaptureExecutor) ExecuteStream(
	_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.requests = append(e.requests, req)
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"ok":true}`)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func TestHomeDispatchModelCapabilitiesReachEveryExecutionPath(t *testing.T) {
	for _, kind := range []string{"execute", "count", "stream"} {
		for _, compat := range []bool{false, true} {
			t.Run(kind+"/compat="+map[bool]string{false: "false", true: "true"}[compat], func(t *testing.T) {
				const credentialID = "home-model-capabilities"
				modelRegistry := registry.GetGlobalRegistry()
				modelRegistry.RegisterClient(credentialID, "test", []*registry.ModelInfo{{
					ID:                         "public-model",
					CatalogName:                "upstream-model",
					IsCompat:                   true,
					SupportConfigurationUpdate: true,
					Thinking:                   &registry.ThinkingSupport{Levels: []string{"low"}},
				}})
				t.Cleanup(func() { modelRegistry.UnregisterClient(credentialID) })
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				executor := &homeCapabilityCaptureExecutor{}
				manager.RegisterExecutor(executor)
				webSearch := false
				support := false
				wire := homeAuthDispatchResponse{
					Model: "upstream-model(high)",
					Auth: Auth{
						ID: credentialID, Provider: "test", Prefix: "tenant",
						Attributes: map[string]string{"api_key": "fixture-key", "auth_kind": "apikey"},
						Metadata: map[string]any{"credential_options": map[string]any{"models": []internalconfig.OpenAICompatibilityModel{
							{Name: "upstream-model", Alias: "public-model", IsCompat: compat},
						}}},
					},
					ModelInfo: &homeDispatchModelInfo{
						ID: "upstream-model", Type: "test", ContextLength: 32768,
						Thinking:                   &registry.ThinkingSupport{Levels: []string{"high", "none"}, ZeroAllowed: true},
						NativeCapabilities:         &registry.NativeCapabilities{WebSearch: &webSearch},
						SupportConfigurationUpdate: &support, UserDefined: true,
					},
				}
				payload, errMarshal := json.Marshal(wire)
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				dispatcher := &testHomeDispatcher{heartbeat: true, payload: payload}
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
				request := cliproxyexecutor.Request{Model: "tenant/public-model(high)"}
				options := cliproxyexecutor.Options{}
				switch kind {
				case "execute":
					_, errMarshal = manager.Execute(context.Background(), []string{"test"}, request, options)
				case "count":
					_, errMarshal = manager.ExecuteCount(context.Background(), []string{"test"}, request, options)
				case "stream":
					var stream *cliproxyexecutor.StreamResult
					stream, errMarshal = manager.ExecuteStream(context.Background(), []string{"test"}, request, options)
					if errMarshal == nil {
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								errMarshal = chunk.Err
							}
						}
					}
				}
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				if len(executor.requests) != 1 || dispatcher.calls != 1 {
					t.Fatalf("dispatches=%d, attempts=%d", dispatcher.calls, len(executor.requests))
				}
				received := executor.requests[0]
				if received.Model != "upstream-model(high)" {
					t.Fatalf("selected model = %q", received.Model)
				}
				selected, ok := ResolvedModelInfo(received)
				if !ok || selected.ID != "upstream-model" || selected.ContextLength != 32768 ||
					selected.IsCompat != compat || selected.SupportConfigurationUpdate || !selected.UserDefined ||
					selected.Thinking == nil || !selected.Thinking.ZeroAllowed ||
					selected.NativeCapabilities == nil || selected.NativeCapabilities.WebSearch == nil ||
					*selected.NativeCapabilities.WebSearch {
					t.Fatalf("selected capabilities = %+v", selected)
				}
				modelOptions, configured := ResolvedHomeModelOptions(received)
				if !configured || modelOptions.Name != "upstream-model" || modelOptions.IsCompat != compat {
					t.Fatalf("selected credential model options = (%+v, %t)", modelOptions, configured)
				}
				local, localBound := ResolvedAPIKeyModelInfo(received)
				if !localBound || local.ID != "upstream-model" || !local.IsCompat || !local.SupportConfigurationUpdate {
					t.Fatalf("selected local model = (%+v, %t)", local, localBound)
				}
			})
		}
	}
}

func TestHomeContext1MUsesDispatchedModelCapability(t *testing.T) {
	const credentialID = "home-context-1m"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(credentialID, "test", []*registry.ModelInfo{{
		ID: "public-model", CatalogName: "upstream-model", ContextLength: 32768,
	}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(credentialID) })
	for _, tc := range []struct {
		name         string
		contextLimit int
		allow        bool
	}{
		{name: "remote supports 1m", contextLimit: context1MMinimumTokens, allow: true},
		{name: "remote lacks 1m", contextLimit: 32768},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
			executor := &homeCapabilityCaptureExecutor{}
			manager.RegisterExecutor(executor)
			payload, errMarshal := json.Marshal(homeAuthDispatchResponse{
				Model:     "upstream-model[1m]",
				Auth:      Auth{ID: credentialID, Provider: "test", Attributes: map[string]string{"api_key": "fixture-key"}},
				ModelInfo: &homeDispatchModelInfo{ID: "upstream-model", ContextLength: tc.contextLimit},
			})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			manager.PublishHomeDispatch(&testHomeDispatcher{heartbeat: true, payload: payload}, executionregistry.New(), 1)
			_, errExecute := manager.Execute(
				context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "public-model[1m]"}, cliproxyexecutor.Options{},
			)
			if !tc.allow {
				requireContext1MModelSupportError(t, errExecute)
				if len(executor.requests) != 0 {
					t.Fatalf("unsupported 1m request reached executor: %d", len(executor.requests))
				}
				return
			}
			if errExecute != nil || len(executor.requests) != 1 {
				t.Fatalf("1m request attempts = %d, error = %v", len(executor.requests), errExecute)
			}
			resolved, ok := ResolvedModelInfo(executor.requests[0])
			if !ok || resolved.ContextLength != context1MMinimumTokens {
				t.Fatalf("selected 1m capability = %+v", resolved)
			}
		})
	}
}

func TestHomeModelInfoUsesExplicitSupportAndRejectsMalformedWire(t *testing.T) {
	local := &registry.ModelInfo{ID: "upstream-model", SupportConfigurationUpdate: true}
	for _, tc := range []struct {
		name     string
		value    *bool
		expected bool
	}{
		{name: "omitted", expected: true},
		{name: "explicit false", value: new(bool)},
		{name: "explicit true", value: func() *bool { v := true; return &v }(), expected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &registry.ModelInfo{ID: "upstream-model"}
			request := cliproxyexecutor.Request{Model: "upstream-model", Metadata: map[string]any{
				resolvedAPIKeyModelInfoMetadataKey: local,
			}}
			request = attachResolvedHomeModelInfo(request, nil, "upstream-model", info, tc.value)
			resolved, ok := ResolvedModelInfo(request)
			if !ok || resolved.SupportConfigurationUpdate != tc.expected || info.SupportConfigurationUpdate {
				t.Fatalf("support configuration update = %+v, source = %+v", resolved, info)
			}
		})
	}

	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&homeCapabilityCaptureExecutor{})
	manager.PublishHomeDispatch(&testHomeDispatcher{
		heartbeat: true, payload: []byte(`{"auth":{"id":"home-malformed","provider":"test"},"model_info":{"context_length":32768}}`),
	}, executionregistry.New(), 1)
	selection, err := manager.pickHomeDispatchSelection(context.Background(), "public-model", cliproxyexecutor.Options{})
	if selection != nil {
		selection.End("invalid test payload")
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("malformed Home metadata returned selection=%+v, error=%v", selection, err)
	}
}
