package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestProtocolAuthSelectionKeepsClientKeyPolicy(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, allowed := range []bool{false, true} {
			name := "non-stream"
			if stream {
				name = "stream"
			}
			if allowed {
				name += "/allowed"
			} else {
				name += "/blocked"
			}
			t.Run(
				name, func(t *testing.T) {
					const actualModel = "unlisted-wire-model"
					const selectionModel = "listed-selection-model"
					calls := 0
					executor := &lifecycleCaptureExecutor{
						provider: "codex",
						execute: func(
							_ context.Context,
							_ *coreauth.Auth,
							req coreexecutor.Request,
							_ coreexecutor.Options,
						) (coreexecutor.Response, error) {
							calls++
							if req.Model != actualModel {
								t.Errorf("wire model=%s", req.Model)
							}
							return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
						},
						stream: func(
							_ context.Context,
							_ *coreauth.Auth,
							req coreexecutor.Request,
							_ coreexecutor.Options,
						) (*coreexecutor.StreamResult, error) {
							calls++
							if req.Model != actualModel {
								t.Errorf("wire model=%s", req.Model)
							}
							chunks := make(chan coreexecutor.StreamChunk, 1)
							chunks <- coreexecutor.StreamChunk{Payload: []byte("data: [DONE]\n\n")}
							close(chunks)
							return &coreexecutor.StreamResult{Chunks: chunks}, nil
						},
					}
					seed := newLifecycleHandler(t, selectionModel, executor)
					rule := internalconfig.APIKeyRule{Providers: []string{"codex"}}
					if !allowed {
						rule.BlockedModels = []string{actualModel}
					}
					handler := NewBaseAPIHandlers(
						&sdkconfig.SDKConfig{
							APIKeyRules: map[string]internalconfig.APIKeyRule{"synthetic-client-key": rule},
						},
						seed.AuthManager,
					)
					ctx := apiKeyPolicyContext("synthetic-client-key", "protocol-selection-policy")
					req := ProtocolExecutionRequest{
						EntryProtocol: "openai", Model: actualModel, AuthSelectionModel: selectionModel,
						ForcedProvider: "codex", Stream: stream,
					}
					var err *ErrorMessage
					if stream {
						var response ModelExecutionStream
						response, err = handler.ExecuteProtocolStreamWithAuthManager(ctx, req)
						if err == nil {
							for range response.Chunks {
							}
						}
					} else {
						_, err = handler.ExecuteProtocolWithAuthManager(ctx, req)
					}
					if allowed {
						if err != nil || calls != 1 {
							t.Fatalf("allowed explicit selection: calls=%d error=%v", calls, err)
						}
					} else if err == nil || err.StatusCode != http.StatusForbidden || calls != 0 {
						t.Fatalf("selection bypassed actual-model policy: calls=%d error=%v", calls, err)
					}
				},
			)
		}
	}
}

func TestProtocolAndInternalModelExecutionKeepTheirContracts(t *testing.T) {
	for _, entry := range []string{"internal", "protocol", "protocol auth selection"} {
		for _, stream := range []bool{false, true} {
			name := entry + "/non-stream"
			if stream {
				name = entry + "/stream"
			}
			t.Run(
				name, func(t *testing.T) {
					const model = "protocol-contract-wire-model"
					const selectionModel = "protocol-contract-selection-model"
					type observation struct {
						authID string
						req    coreexecutor.Request
						opts   coreexecutor.Options
					}
					seen := make(chan observation, 1)
					executor := &lifecycleCaptureExecutor{
						provider: "codex",
						execute: func(
							_ context.Context,
							auth *coreauth.Auth,
							req coreexecutor.Request,
							opts coreexecutor.Options,
						) (coreexecutor.Response, error) {
							seen <- observation{auth.ID, req, opts}
							return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
						},
						stream: func(
							_ context.Context,
							auth *coreauth.Auth,
							req coreexecutor.Request,
							opts coreexecutor.Options,
						) (*coreexecutor.StreamResult, error) {
							seen <- observation{auth.ID, req, opts}
							chunks := make(chan coreexecutor.StreamChunk, 1)
							chunks <- coreexecutor.StreamChunk{Payload: []byte("data: [DONE]\n\n")}
							close(chunks)
							return &coreexecutor.StreamResult{Chunks: chunks}, nil
						},
					}
					handler := newLifecycleHandler(t, model, executor)
					expectedAuth := "handler-lifecycle-" + model
					if entry == "protocol auth selection" {
						expectedAuth = "protocol-contract-selection-auth"
						_, err := handler.AuthManager.Register(
							context.Background(), &coreauth.Auth{
								ID: expectedAuth, Provider: "codex", Status: coreauth.StatusActive,
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						registry.GetGlobalRegistry().RegisterClient(
							expectedAuth, "codex", []*registry.ModelInfo{{ID: selectionModel}},
						)
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(expectedAuth) })
					}
					body := []byte(`{"model":"protocol-contract-wire-model","input":"synthetic"}`)
					headers := http.Header{"X-Fixture": {"original"}}
					query := url.Values{"fixture": {"original"}}
					ctx := context.Background()
					if entry == "internal" {
						req := ModelExecutionRequest{
							EntryProtocol: "openai", Model: model, Body: body, Headers: headers, Query: query,
							Stream: stream,
						}
						if stream {
							response, err := handler.ExecuteModelStream(ctx, req)
							if err != nil {
								t.Fatal(err)
							}
							for range response.Chunks {
							}
						} else if _, err := handler.ExecuteModel(ctx, req); err != nil {
							t.Fatal(err)
						}
					} else {
						req := ProtocolExecutionRequest{
							EntryProtocol: "openai", ExitProtocol: "openai", Model: model, Stream: stream,
							Body: body, Headers: headers, Query: query, ForcedProvider: "codex", Path: "/v1/responses",
						}
						if entry == "protocol auth selection" {
							req.AuthSelectionModel = selectionModel
						}
						if stream {
							response, err := handler.ExecuteProtocolStreamWithAuthManager(ctx, req)
							if err != nil {
								t.Fatal(err)
							}
							for range response.Chunks {
							}
						} else if _, err := handler.ExecuteProtocolWithAuthManager(ctx, req); err != nil {
							t.Fatal(err)
						}
					}
					observed := <-seen
					if observed.authID != expectedAuth || observed.req.Model != model {
						t.Fatalf(
							"auth-only model changed dispatch: auth=%s request model=%s", observed.authID,
							observed.req.Model,
						)
					}
					if observed.opts.Metadata[coreexecutor.RequestedModelMetadataKey] != model {
						t.Fatalf("requested model changed: %v", observed.opts.Metadata)
					}
					if entry == "internal" {
						if observed.opts.Metadata["source"] != "plugin_host_model_callback" {
							t.Fatalf("internal source missing: %v", observed.opts.Metadata)
						}
					} else {
						if _, exists := observed.opts.Metadata["source"]; exists {
							t.Fatalf("route execution acquired internal source: %v", observed.opts.Metadata)
						}
						if observed.opts.Metadata[coreexecutor.RequestPathMetadataKey] != "/v1/responses" {
							t.Fatalf("protocol path was not propagated: %v", observed.opts.Metadata)
						}
					}
					if entry == "protocol auth selection" &&
						observed.opts.Metadata[coreexecutor.AuthSelectionModelMetadataKey] != selectionModel {
						t.Fatalf("selection metadata missing: %v", observed.opts.Metadata)
					}
					observed.req.Payload[0] = '!'
					observed.opts.Headers.Set("X-Fixture", "changed")
					observed.opts.Query.Set("fixture", "changed")
					if !bytes.HasPrefix(
						body, []byte("{"),
					) || headers.Get("X-Fixture") != "original" || query.Get("fixture") != "original" {
						t.Fatal("execution changed caller-owned input containers")
					}
				},
			)
		}
	}
}
