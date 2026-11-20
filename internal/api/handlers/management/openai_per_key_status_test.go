package management

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestOpenAICompatibilityPerKeyStatusPreservesWeightIdentityAndReload(t *testing.T) {
	firstWeight, secondWeight := 7, 11
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "synthetic-compat", BaseURL: "https://synthetic.invalid",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "synthetic-shared-key", Weight: &firstWeight, ProxyURL: "direct"},
					{APIKey: "synthetic-shared-key", Weight: &secondWeight, ProxyURL: "direct"},
				},
			},
		},
	}
	synthesize := func(actual *config.Config) []*coreauth.Auth {
		t.Helper()
		auths, err := synthesizer.NewConfigSynthesizer().Synthesize(
			&synthesizer.SynthesisContext{
				Config: actual, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		return auths
	}
	original := synthesize(cfg)
	if len(original) != 2 || original[0].ID == original[1].ID {
		t.Fatal("fixture does not expose two stable same-key entries")
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range original {
		if _, err := manager.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
	}
	h := newConfigListTestHandler(t, cfg)
	h.authManager = manager
	h.configCommitted = func(ctx context.Context, commit ConfigCommit) error {
		for _, auth := range synthesize(commit.Config) {
			if _, err := manager.Update(coreauth.WithSkipPersist(ctx), auth); err != nil {
				return err
			}
		}
		return nil
	}
	for _, step := range []struct {
		index    int
		disabled bool
		bulk     bool
	}{
		{0, true, false}, {1, true, false}, {0, false, false}, {1, false, false},
		{0, true, true}, {1, true, true}, {0, false, true}, {1, false, true},
	} {
		body := fmt.Sprintf(
			`{"name":%q,"auth_index":%q,"disabled":%t}`, original[step.index].ID, original[step.index].EnsureIndex(),
			step.disabled,
		)
		operation, path := h.PatchAuthFileStatus, "/auth-files/status"
		if step.bulk {
			body = fmt.Sprintf(`{"names":[%q]}`, original[step.index].ID)
			operation, path = h.PatchAuthFilesBulkEnable, "/auth-files/bulk-enable"
			if step.disabled {
				operation, path = h.PatchAuthFilesBulkDisable, "/auth-files/bulk-disable"
			}
		}
		response := runConfigListRequest(operation, http.MethodPatch, path, body)
		if response.Code != http.StatusOK {
			t.Fatalf(
				"key=%d disabled=%v status=%d body=%s",
				step.index,
				step.disabled,
				response.Code,
				response.Body.String(),
			)
		}
		loaded, err := config.LoadConfig(h.configFilePath)
		if err != nil {
			t.Fatal(err)
		}
		group := loaded.OpenAICompatibility[0]
		if group.Disabled || len(group.APIKeyEntries) != 2 || *group.APIKeyEntries[0].Weight != 7 ||
			*group.APIKeyEntries[1].Weight != 11 ||
			group.APIKeyEntries[0].ProxyURL != "direct" ||
			group.APIKeyEntries[1].ProxyURL != "direct" {
			t.Fatalf("per-key status changed group/weight/proxy: %+v", group)
		}
		if group.APIKeyEntries[step.index].Disabled != step.disabled {
			t.Fatalf("disabled flag was not persisted for key %d", step.index)
		}
		regenerated := synthesize(loaded)
		if len(regenerated) != 2 {
			t.Fatal("disabled key disappeared from the editable synthesized list")
		}
		for i, auth := range regenerated {
			if auth.ID != original[i].ID || auth.Disabled != group.APIKeyEntries[i].Disabled ||
				(auth.Status == coreauth.StatusDisabled) != auth.Disabled {
				t.Fatalf("reload key=%d id=%q disabled=%v status=%s", i, auth.ID, auth.Disabled, auth.Status)
			}
			current, ok := manager.GetByID(auth.ID)
			if !ok || current.Disabled != auth.Disabled {
				t.Fatalf("runtime and persisted state differ for key %d", i)
			}
		}
	}
}
