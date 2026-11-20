package auth

import (
	"context"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestAuthSelectionModelKeepsExecutionModelAcrossAllPaths(t *testing.T) {
	const selectionModel = "selection-model-only"
	const executionModel = "actual-execution-model"
	for _, mode := range []string{"local", "home"} {
		for _, kind := range []string{"execute", "count", "stream"} {
			t.Run(
				mode+"/"+kind, func(t *testing.T) {
					manager := NewManager(nil, nil, nil)
					executor := &homeCapabilityCaptureExecutor{}
					manager.RegisterExecutor(executor)
					auth := &Auth{ID: "auth-selection-contract", Provider: "test", Status: StatusActive}
					registry.GetGlobalRegistry().RegisterClient(
						auth.ID, "test", []*registry.ModelInfo{{ID: selectionModel}},
					)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					var dispatcher *accountedHomeExecutionDispatcher
					if mode == "home" {
						manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
						dispatcher = &accountedHomeExecutionDispatcher{auths: []Auth{*auth}}
						manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
					} else if _, err := manager.Register(context.Background(), auth); err != nil {
						t.Fatal(err)
					}
					req := cliproxyexecutor.Request{Model: executionModel}
					opts := cliproxyexecutor.Options{
						Metadata: map[string]any{
							cliproxyexecutor.AuthSelectionModelMetadataKey: []byte(" " + selectionModel + " "),
						},
					}
					var err error
					switch kind {
					case "execute":
						_, err = manager.Execute(context.Background(), []string{"test"}, req, opts)
					case "count":
						_, err = manager.ExecuteCount(context.Background(), []string{"test"}, req, opts)
					case "stream":
						var stream *cliproxyexecutor.StreamResult
						stream, err = manager.ExecuteStream(context.Background(), []string{"test"}, req, opts)
						if err == nil {
							for chunk := range stream.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					if len(executor.requests) != 1 || executor.requests[0].Model != executionModel {
						t.Fatalf("selection changed the actual execution model: %+v", executor.requests)
					}
					if opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey] != nil {
						t.Fatal("manager changed the caller-owned metadata")
					}
					if dispatcher != nil && dispatcher.calls.Load() != 1 {
						t.Fatalf("Home dispatch calls=%d", dispatcher.calls.Load())
					}
				},
			)
		}
	}
}
