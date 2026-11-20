package management

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"gopkg.in/yaml.v3"
)

func TestOAuthExcludedModelsKeepFirstSpellingAcrossSaveAndReload(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		t.Run(
			method, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte("port: 8317\n"), 0600); err != nil {
					t.Fatal(err)
				}
				h := NewHandler(&config.Config{Port: 8317}, path, nil, withEnvSecret(""))
				body := `{"Claude":[" Claude-* ","claude-*","Custom/Model-X"]}`
				handler := h.PutOAuthExcludedModels
				if method == http.MethodPatch {
					body = `{"provider":"Claude","models":[" Claude-* ","claude-*","Custom/Model-X"]}`
					handler = h.PatchOAuthExcludedModels
				}
				recorder := runConfigListRequest(handler, method, "/oauth-excluded-models", body)
				if recorder.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				readback := runConfigListRequest(h.GetOAuthExcludedModels, http.MethodGet, "/oauth-excluded-models", "")
				var response map[string]map[string][]string
				if err := json.Unmarshal(readback.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				want := []string{"Claude-*", "Custom/Model-X"}
				if !reflect.DeepEqual(response["oauth-excluded-models"]["claude"], want) {
					t.Fatalf("readback=%s", readback.Body.String())
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var saved config.Config
				if err := yaml.Unmarshal(data, &saved); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(saved.OAuthExcludedModels["claude"], want) {
					t.Fatalf("saved=%s", data)
				}
				loaded, err := config.LoadConfig(path)
				if err != nil || !reflect.DeepEqual(loaded.OAuthExcludedModels["claude"], want) {
					t.Fatalf("loaded=%+v error=%v", loaded, err)
				}
			},
		)
	}
}
