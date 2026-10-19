package cliproxy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestOAuthSettingsRegistrationKeepsAPIKeyMetadata(t *testing.T) {
	cfg := &config.Config{OAuthSettings: map[string][]config.OAuthModelSetting{
		"codex": {{Name: "gpt-5.6-luna", MaxContextLength: 524288}},
	}}
	service := &Service{cfg: cfg}
	for _, tc := range []struct {
		id       string
		authKind string
		want     int
	}{
		{id: "oauth-settings-oauth", authKind: "oauth", want: 524288},
		{id: "oauth-settings-api-key", authKind: "api_key", want: 0},
	} {
		auth := &coreauth.Auth{
			ID: tc.id, Provider: "codex", Status: coreauth.StatusActive,
			Attributes: map[string]string{"auth_kind": tc.authKind, "plan_type": "free"},
		}
		service.registerModelsForAuthWithConfig(auth, cfg)
		t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(tc.id) })
		found := false
		for _, model := range GlobalModelRegistry().GetModelsForClient(auth.ID) {
			if model == nil || model.ID != "gpt-5.6-luna" {
				continue
			}
			found = true
			if model.MaxContextLength != tc.want {
				t.Fatalf("%s max context = %d, want %d", tc.authKind, model.MaxContextLength, tc.want)
			}
		}
		if !found {
			t.Fatalf("%s Codex free catalog omitted gpt-5.6-luna", tc.authKind)
		}
	}
}

func TestOAuthSettingsAliasesDoNotMutateCatalog(t *testing.T) {
	cfg := &config.Config{OAuthSettings: map[string][]config.OAuthModelSetting{
		"codex": {
			{Name: "original", MaxContextLength: 262144},
			{Name: "original", Alias: "public", MaxContextLength: 524288},
		},
	}}
	original := &ModelInfo{ID: "original", ContextLength: 100000}
	alias := &ModelInfo{ID: "public", CatalogName: "original", ContextLength: 100000}
	out := applyOAuthSettings(cfg, "codex", "oauth", []*ModelInfo{original, alias})
	if len(out) != 2 || out[0].MaxContextLength != 262144 || out[1].MaxContextLength != 524288 {
		t.Fatalf("OAuth model metadata overrides: %+v", out)
	}
	if original.ContextLength != 100000 || alias.ContextLength != 100000 {
		t.Fatal("OAuth settings mutated shared catalog entries")
	}
	unmodified := applyOAuthSettings(cfg, "codex", "api_key", []*ModelInfo{original})
	if len(unmodified) != 1 || unmodified[0] != original {
		t.Fatal("OAuth-only settings reached API-key credential")
	}

	before, err := modelRefreshMappingRevision(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.OAuthSettings["codex"][0].MaxContextLength++
	after, err := modelRefreshMappingRevision(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("model refresh identity ignored OAuth settings change")
	}
}

type codexPlanRefreshFixtureExecutor struct {
	*customOwnedExecutor
	plan string
}

func (e *codexPlanRefreshFixtureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	refreshed := auth.Clone()
	refreshed.Metadata["access_token"] = "fixture-refreshed-access"
	refreshed.Metadata["plan_type"] = e.plan
	refreshed.Attributes["plan_type"] = e.plan
	return refreshed, nil
}

func TestCodexRefreshReprojectsPlanEligibleModels(t *testing.T) {
	for _, tc := range []struct {
		name, from, to      string
		wasAstra, wantAstra bool
	}{
		{name: "downgrade", from: "pro", to: "free", wasAstra: true},
		{name: "upgrade", from: "free", to: "team", wantAstra: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			cfg := &config.Config{AuthDir: t.TempDir()}
			service, err := NewBuilder().WithConfig(cfg).
				WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
				WithCoreAuthManager(manager).Build()
			if err != nil {
				t.Fatal(err)
			}
			manager.RegisterExecutor(&codexPlanRefreshFixtureExecutor{
				customOwnedExecutor: &customOwnedExecutor{id: "codex"}, plan: tc.to,
			})
			auth, err := manager.Register(context.Background(), &coreauth.Auth{
				ID: "codex-plan-refresh-" + tc.name, Provider: "codex", Status: coreauth.StatusActive,
				Attributes: map[string]string{"auth_kind": "oauth", "plan_type": tc.from},
				Metadata:   map[string]any{"access_token": "fixture-old-access", "plan_type": tc.from},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
			service.registerModelsForAuthWithConfig(auth, cfg)
			if got := GlobalModelRegistry().ClientSupportsModel(auth.ID, "gpt-6-astra"); got != tc.wasAstra {
				t.Fatalf("before refresh, Astra support = %t, want %t", got, tc.wasAstra)
			}
			if _, err := manager.ForceRefreshAuth(context.Background(), auth.ID); err != nil {
				t.Fatalf("fixture credential refresh: %v", err)
			}
			if got := GlobalModelRegistry().ClientSupportsModel(auth.ID, "gpt-6-astra"); got != tc.wantAstra {
				t.Fatalf("after refresh, Astra support = %t, want %t", got, tc.wantAstra)
			}
			current, exists := manager.GetByID(auth.ID)
			if !exists || current.Attributes["plan_type"] != tc.to {
				t.Fatalf("refreshed credential plan: %+v", current)
			}
		})
	}
}
