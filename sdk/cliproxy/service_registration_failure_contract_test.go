package cliproxy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestAuthRegistrationSaveFailurePreservesPublishedModels(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "register"
		if existing {
			name = "update"
		}
		t.Run(
			name, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
				service := &Service{cfg: &config.Config{}, coreManager: manager}
				auth := &coreauth.Auth{
					ID: t.Name() + ".json", FileName: "synthetic.json", Provider: "qwen", Status: coreauth.StatusActive,
					Metadata: map[string]any{"type": "qwen", "access_token": "synthetic", "note": "original"},
				}
				var before *coreauth.Auth
				if existing {
					var err error
					before, err = manager.Register(t.Context(), auth)
					if err != nil {
						t.Fatal(err)
					}
				}
				reg := GlobalModelRegistry()
				reg.RegisterClient(auth.ID, auth.Provider, []*ModelInfo{{ID: "previous-model"}})
				t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
				blocker := filepath.Join(t.TempDir(), "not-a-directory")
				if err := os.WriteFile(blocker, []byte("synthetic"), 0o600); err != nil {
					t.Fatal(err)
				}
				store := sdkAuth.NewFileTokenStore()
				store.SetBaseDir(blocker)
				manager.SetStore(store)
				update := auth.Clone()
				update.Metadata["note"] = "rejected"
				if err := service.applyCoreAuthAddOrUpdate(t.Context(), update); err == nil ||
					!strings.Contains(err.Error(), name+" auth ") || !strings.Contains(
					err.Error(), "secure dir failed",
				) {
					t.Fatalf("save failure was not returned by Service: %v", err)
				}
				current, ok := manager.GetByID(auth.ID)
				models := reg.GetModelsForClient(auth.ID)
				if existing {
					if !ok || !reflect.DeepEqual(
						current, before,
					) || len(models) != 1 || models[0].ID != "previous-model" {
						t.Fatalf("failed update changed published auth/models: auth=%#v models=%#v", current, models)
					}
				} else if ok || len(models) != 0 {
					t.Fatalf("failed registration retained auth or stale models: auth=%#v models=%#v", current, models)
				}
			},
		)
	}
}
