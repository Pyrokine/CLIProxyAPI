package cliproxy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestBuilderAppliesInitialSessionAffinitySubagentSetting(t *testing.T) {
	subagentAffinity := false
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.Routing.Strategy = "round-robin"
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionAffinitySubagents = &subagentAffinity

	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		Build()
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	affinity, ok := service.coreManager.Selector().(*coreauth.SessionAffinitySelector)
	if !ok || affinity == nil {
		t.Fatalf("startup selector = %#v, want session affinity selector", service.coreManager.Selector())
	}
	defer affinity.Stop()

	auths := []*coreauth.Auth{
		{ID: "a", Provider: "gemini", Status: coreauth.StatusActive},
		{ID: "b", Provider: "gemini", Status: coreauth.StatusActive},
	}
	rootOptions := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"session_id":"root"}`),
		Metadata:        map[string]any{},
	}
	root, errRoot := affinity.Pick(context.Background(), "gemini", "model", rootOptions, auths[:1])
	if errRoot != nil || root == nil || root.ID != "a" {
		t.Fatalf("root Pick() = %#v, %v", root, errRoot)
	}
	childOptions := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"session_id":"child","parent_session_id":"root"}`),
		Metadata:        map[string]any{},
	}
	child, errChild := affinity.Pick(context.Background(), "gemini", "model", childOptions, auths)
	if errChild != nil || child == nil || child.ID != "b" {
		t.Fatalf("child Pick() = %#v, %v, want fallback auth b", child, errChild)
	}
}
