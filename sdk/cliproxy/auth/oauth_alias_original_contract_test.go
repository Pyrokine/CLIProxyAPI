package auth

import (
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalOAuthAliasBypassesBlockedRouteAndRecordsTarget(t *testing.T) {
	for _, mode := range []string{"single", "mixed", "execute", "count", "stream"} {
		t.Run(
			mode, func(t *testing.T) {
				const provider = "antigravity"
				const route, target = "claude-opus-4-6", "claude-opus-4-6-thinking"
				hook := &resultCaptureHook{}
				manager := NewManager(nil, nil, hook)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				capture := &originalOAuthAliasExecutor{id: provider}
				manager.RegisterExecutor(capture)
				manager.SetOAuthModelAlias(
					map[string][]config.OAuthModelAlias{
						provider: {{Name: target, Alias: route, Fork: true}},
					},
				)
				deadline := time.Now().Add(time.Hour)
				credential, err := manager.Register(
					t.Context(), &Auth{
						ID: "oauth-alias-auth", Provider: provider, Status: StatusActive,
						ModelStates: map[string]*ModelState{
							route: {
								Unavailable: true, Status: StatusError, NextRetryAfter: deadline,
							},
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				reg := registerProjectionTestClient(
					t, credential.ID, provider,
					&registry.ModelInfo{ID: route}, &registry.ModelInfo{ID: target},
				)
				manager.RefreshSchedulerEntry(credential.ID)
				request := core.Request{Model: route}
				var selected *Auth
				switch mode {
				case "single":
					selected, _, err = manager.PickNext(t.Context(), provider, route, core.Options{}, nil)
				case "mixed":
					selected, _, _, err = manager.pickNextMixed(
						t.Context(), []string{provider}, route, core.Options{}, nil,
					)
				case "execute":
					_, err = manager.Execute(t.Context(), []string{provider}, request, core.Options{})
				case "count":
					_, err = manager.ExecuteCount(t.Context(), []string{provider}, request, core.Options{})
				case "stream":
					var stream *core.StreamResult
					stream, err = manager.ExecuteStream(t.Context(), []string{provider}, request, core.Options{})
					if err == nil {
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
								break
							}
						}
					}
				}
				if err != nil {
					t.Fatalf("healthy alias target was blocked by the old route cooldown: %v", err)
				}
				if mode == "single" || mode == "mixed" {
					if selected == nil || selected.ID != credential.ID {
						t.Fatalf("selected credential=%#v", selected)
					}
				} else {
					if len(capture.calls) != 1 || capture.calls[0].model != target || capture.calls[0].alias != route ||
						capture.calls[0].authID != credential.ID {
						t.Fatalf(
							"actual upstream requests=%#v, want one target=%q alias=%q request", capture.calls, target,
							route,
						)
					}
					results := hook.Results()
					if len(results) != 1 || !results[0].Success || results[0].Model != target ||
						results[0].AuthID != credential.ID || results[0].AuthGeneration != credential.Generation {
						t.Fatalf("result did not retain the selected target and credential generation: %#v", results)
					}
				}
				current, _ := manager.GetByID(credential.ID)
				if state := current.ModelStates[route]; state == nil || !state.Unavailable ||
					!state.NextRetryAfter.Equal(deadline) {
					t.Fatalf("using a different target changed the original route state: %#v", state)
				}
				if reg.IsModelSuspendedForClient(credential.ID, route) {
					t.Fatal("registry still exposes the old route cooldown for a healthy alias target")
				}
			},
		)
	}
}
