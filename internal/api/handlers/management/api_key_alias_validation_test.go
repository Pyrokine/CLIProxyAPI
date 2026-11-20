package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestPutAPIKeyAliasesUsesTheSameValidationAsPatch(t *testing.T) {
	for _, test := range []struct {
		name    string
		aliases map[string]string
		valid   bool
	}{
		{"short-key", map[string]string{"short": "valid"}, false},
		{"whitespace-key", map[string]string{"invalid client key": "valid"}, false},
		{"non-ascii-key", map[string]string{"synthetic-客户端-key": "valid"}, false},
		{"alias-punctuation", map[string]string{"synthetic-client-key": "invalid/alias"}, false},
		{"alias-length", map[string]string{"synthetic-client-key": strings.Repeat("a", 21)}, false},
		{"valid", map[string]string{"synthetic-client-key": "valid-alias_1"}, true},
		{"clear", map[string]string{}, true},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "config.yaml")
				original := []byte("port: 8317\napi-key-aliases:\n  original-client-key: original\n")
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
				before := map[string]string{"original-client-key": "original"}
				h := &Handler{cfg: &config.Config{Port: 8317, APIKeyAliases: before}, configFilePath: path}
				engine := gin.New()
				engine.PUT("/aliases", h.PutAPIKeyAliases)
				body, err := json.Marshal(test.aliases)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPut, "/aliases", strings.NewReader(string(body)))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				if test.valid {
					if response.Code != http.StatusOK {
						t.Fatalf("valid aliases status=%d body=%s", response.Code, response.Body.String())
					}
					loaded, err := config.LoadConfig(path)
					if err != nil {
						t.Fatal(err)
					}
					if len(loaded.APIKeyAliases) != len(test.aliases) {
						t.Fatalf("persisted aliases=%+v want %+v", loaded.APIKeyAliases, test.aliases)
					}
					for key, alias := range test.aliases {
						if loaded.APIKeyAliases[key] != alias {
							t.Fatalf("persisted aliases=%+v want %+v", loaded.APIKeyAliases, test.aliases)
						}
					}
					return
				}
				if response.Code != http.StatusBadRequest {
					t.Fatalf("invalid aliases status=%d body=%s", response.Code, response.Body.String())
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != string(original) {
					t.Fatalf("rejected save changed file data=%q err=%v", data, err)
				}
				if !reflect.DeepEqual(h.configSnapshot().APIKeyAliases, before) {
					t.Fatal("rejected aliases changed the published configuration")
				}
			},
		)
	}
}
