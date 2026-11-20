package auth

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestExplicitOAuthWithoutEmailUsesOAuthAlias(t *testing.T) {
	for _, mode := range []string{
		"email control", "no legacy key control", "attribute OAuth with legacy key", "metadata OAuth with legacy key",
	} {
		t.Run(
			mode, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				manager.SetConfig(
					&config.Config{
						ClaudeKey: []config.ClaudeKey{
							{
								APIKey: "synthetic-legacy-key",
								Models: []config.ClaudeModel{{Name: "api-upstream", Alias: "public-model"}},
							},
						},
					},
				)
				manager.SetOAuthModelAlias(
					map[string][]config.OAuthModelAlias{
						"claude": {{Name: "oauth-upstream", Alias: "public-model"}},
					},
				)
				executor := &homeCapabilityCaptureExecutor{provider: "claude"}
				manager.RegisterExecutor(executor)
				auth := &Auth{
					ID: t.Name(), Provider: "claude", Status: StatusActive,
					Attributes: map[string]string{
						AttributeAuthKind: AuthKindOAuth, AttributeAPIKey: "synthetic-legacy-key",
					},
					Metadata: map[string]any{"access_token": "synthetic-oauth-token"},
				}
				switch mode {
				case "email control":
					auth.Metadata["email"] = "synthetic@example.invalid"
				case "no legacy key control":
					delete(auth.Attributes, AttributeAPIKey)
				case "metadata OAuth with legacy key":
					delete(auth.Attributes, AttributeAuthKind)
					auth.Metadata[AttributeAuthKind] = AuthKindOAuth
				}
				registered, err := manager.Register(t.Context(), auth)
				if err != nil {
					t.Fatal(err)
				}
				kind, label := registered.AccountInfo()
				if registered.AuthKind() != AuthKindOAuth || kind != "oauth" ||
					(mode != "email control" && label != "") {
					t.Fatalf("OAuth identity = %s/%s/%q", registered.AuthKind(), kind, label)
				}
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(
					auth.ID, auth.Provider, []*registry.ModelInfo{
						{ID: "public-model"}, {ID: "oauth-upstream"}, {ID: "api-upstream"},
					},
				)
				t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
				manager.RefreshSchedulerEntry(auth.ID)
				if _, err := manager.Execute(
					t.Context(), []string{"claude"}, core.Request{Model: "public-model"}, core.Options{},
				); err != nil {
					t.Fatal(err)
				}
				if len(executor.requests) != 1 || executor.requests[0].Model != "oauth-upstream" {
					t.Fatalf("actual alias requests = %+v, want one oauth-upstream", executor.requests)
				}
			},
		)
	}
}

func TestExplicitOAuthLegacyKeyKeepsAliasAndRefreshIdentity(t *testing.T) {
	for _, source := range []string{"attribute-oauth", "metadata-oauth", "legacy-api-key"} {
		t.Run(
			source, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				cfg := &config.Config{
					CodexKey: []config.CodexKey{
						{
							APIKey: "synthetic-retained-key",
							Models: []config.CodexModel{{Name: "key-target", Alias: "client"}},
						},
					},
				}
				manager.SetConfig(cfg)
				manager.SetOAuthModelAlias(
					map[string][]config.OAuthModelAlias{
						"codex": {{Name: "oauth-target", Alias: "client"}},
					},
				)
				refresher := &executorKeyRefreshExecutor{id: "codex"}
				manager.RegisterExecutor(refresher)
				auth := &Auth{
					ID: t.Name(), Provider: "codex", Status: StatusActive,
					Attributes: map[string]string{"api_key": "synthetic-retained-key"},
					Metadata: map[string]any{
						"access_token": "old-token", "refresh_token": "synthetic-refresh",
						"refresh_interval_seconds": 1, "email": "synthetic@example.invalid",
					},
				}
				wantTarget, wantKind, wantRefresh := "oauth-target", AuthKindOAuth, int32(1)
				switch source {
				case "attribute-oauth":
					auth.Attributes[AttributeAuthKind] = AuthKindOAuth
				case "metadata-oauth":
					auth.Metadata[AttributeAuthKind] = AuthKindOAuth
				case "legacy-api-key":
					wantTarget, wantKind, wantRefresh = "key-target", AuthKindAPIKey, 0
				}
				registered, err := manager.Register(WithSkipPersist(t.Context()), auth)
				if err != nil {
					t.Fatal(err)
				}
				if registered.AuthKind() != wantKind {
					t.Fatalf("credential kind=%q, want %q", registered.AuthKind(), wantKind)
				}
				result := manager.resolveExecutionAliasResultForRequested(registered, "client(8192)")
				if result.UpstreamModel != wantTarget+"(8192)" {
					t.Fatalf("selected alias=%+v, want %s", result, wantTarget)
				}
				manager.checkRefreshes(WithSkipPersist(t.Context()))
				manager.refreshWorkers.Wait()
				if got := refresher.refreshCalls.Load(); got != wantRefresh {
					t.Fatalf("automatic refresh calls=%d, want %d", got, wantRefresh)
				}
				current, ok := manager.GetByID(auth.ID)
				if !ok || current.AuthKind() != wantKind ||
					current.Attributes["api_key"] != "synthetic-retained-key" ||
					current.Metadata["email"] != "synthetic@example.invalid" {
					t.Fatal("refresh changed the credential kind or retained display/legacy fields")
				}
				if wantRefresh == 1 && (current.Metadata["access_token"] != "refreshed-token" ||
					current.Generation == registered.Generation) {
					t.Fatal("OAuth refresh did not publish the new token generation")
				}
				if result := manager.resolveExecutionAliasResultForRequested(
					current, "client",
				); result.UpstreamModel != wantTarget {
					t.Fatalf("refresh changed alias identity: %+v", result)
				}
			},
		)
	}
}
