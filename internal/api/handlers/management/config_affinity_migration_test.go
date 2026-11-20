package management

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"gopkg.in/yaml.v3"
)

func TestSessionAffinityRetirementManagementSave(t *testing.T) {
	for _, tc := range []struct {
		name, routing string
		enabled       bool
	}{
		{"legacy enabled", "claude-code-session-affinity: true", true},
		{"legacy disabled", "claude-code-session-affinity: false", false},
		{"explicit false", "claude-code-session-affinity: true, session-affinity: false", false},
		{"explicit true", "claude-code-session-affinity: false, session-affinity: true", true},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				handler := NewHandler(&config.Config{Port: 8317}, path, nil, withEnvSecret(""))
				t.Cleanup(func() { _ = handler.Close() })
				body := "port: 8317\nrouting: {" + tc.routing + ", future: retained}\n"
				response := runConfigListRequest(handler.PutConfigYAML, http.MethodPut, "/config.yaml", body)
				if response.Code != http.StatusOK {
					t.Fatalf("save status=%d body=%s", response.Code, response.Body.String())
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var saved map[string]any
				if err := yaml.Unmarshal(raw, &saved); err != nil {
					t.Fatal(err)
				}
				routing := saved["routing"].(map[string]any)
				if _, exists := routing["claude-code-session-affinity"]; exists ||
					routing["session-affinity"] != tc.enabled ||
					routing["future"] != "retained" {
					t.Fatalf("saved routing=%#v", routing)
				}
				if handler.cfg.Routing.SessionAffinity != tc.enabled {
					t.Fatal("published routing differs from the migrated file")
				}
			},
		)
	}
}
