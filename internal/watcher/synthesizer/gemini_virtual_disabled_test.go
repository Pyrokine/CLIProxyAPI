package synthesizer

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestGeminiVirtualCredentialsPreserveFileDisabledState(t *testing.T) {
	ctx := &SynthesisContext{Config: &config.Config{}, AuthDir: t.TempDir(), Now: time.Now()}
	path := filepath.Join(ctx.AuthDir, "gemini.json")
	for _, disabled := range []bool{false, true, false} {
		t.Run(
			fmt.Sprintf("disabled=%t", disabled), func(t *testing.T) {
				payload := []byte(fmt.Sprintf(
					`{"type":"gemini","project_id":"project-a,project-b","disabled":%t}`, disabled,
				))
				auths, err := SynthesizeAuthFile(ctx, path, payload)
				if err != nil || len(auths) != 3 {
					t.Fatalf("synthesize virtual credentials count=%d, error=%v", len(auths), err)
				}
				if !auths[0].Disabled || auths[0].Status != coreauth.StatusDisabled {
					t.Fatal("multi-project primary credential must remain disabled")
				}
				for _, auth := range auths[1:] {
					wantStatus := coreauth.StatusActive
					if disabled {
						wantStatus = coreauth.StatusDisabled
					}
					if auth.Disabled != disabled || auth.Status != wantStatus {
						t.Errorf("virtual credential %s disabled=%t status=%s", auth.ID, auth.Disabled, auth.Status)
					}
					if value, _ := auth.Metadata["disabled"].(bool); value != disabled {
						t.Errorf("virtual disabled metadata=%v", auth.Metadata["disabled"])
					}
				}
				selector := &coreauth.RoundRobinSelector{}
				selected, err := selector.Pick(
					t.Context(), "gemini-cli", "fixture-model", coreexecutor.Options{}, auths,
				)
				if disabled {
					if err == nil || selected != nil {
						t.Fatalf("disabled file remained selectable: selected=%+v, error=%v", selected, err)
					}
				} else if err != nil || selected == nil || selected.Attributes["gemini_virtual_parent"] != auths[0].ID {
					t.Fatalf("enabled file lost its project route: selected=%+v, error=%v", selected, err)
				}
			},
		)
	}
}
