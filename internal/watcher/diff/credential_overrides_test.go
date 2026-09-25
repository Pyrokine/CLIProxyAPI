package diff

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestCredentialOverrideDiffUsesValuesAndPresence(t *testing.T) {
	for _, provider := range []string{"claude-api-key", "codex-api-key", "xai-api-key", "gemini-api-key", "interactions-api-key", "vertex-api-key", "openai-compatibility"} {
		t.Run(provider, func(t *testing.T) {
			parse := func(fields string) *config.Config {
				t.Helper()
				var cfg config.Config
				if err := json.Unmarshal([]byte(fmt.Sprintf(`{"%s":[{"name":"example","api-key":"synthetic-key"%s}]}`, provider, fields)), &cfg); err != nil {
					t.Fatal(err)
				}
				return &cfg
			}
			previous := parse("")
			for _, fields := range []string{`,"disable-cooling":false,"request-retry":0`, `,"disable-cooling":true,"request-retry":2`, ""} {
				next := parse(fields)
				changes := strings.Join(BuildConfigChangeDetails(previous, next), "\n")
				if !strings.Contains(changes, "disable-cooling") || !strings.Contains(changes, "request-retry") {
					t.Fatalf("override change missing: %q", changes)
				}
				if strings.Contains(changes, "synthetic-key") || strings.Contains(changes, "0x") {
					t.Fatalf("diff contains credential or pointer address: %q", changes)
				}
				if equal := BuildConfigChangeDetails(next, parse(fields)); len(equal) != 0 {
					t.Fatalf("equal values at distinct pointers changed: %v", equal)
				}
				previous = next
			}
		})
	}
}
