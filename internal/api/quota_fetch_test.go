package api

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestQuotaCredentialTypeForProviderAcceptsGeminiFileType(t *testing.T) {
	credType, ok := quotaCredentialTypeForProvider("gemini")
	if !ok {
		t.Fatalf("gemini provider was not accepted for quota registration")
	}
	if credType != quota.TypeGeminiCli {
		t.Fatalf("credential type = %q, want %q", credType, quota.TypeGeminiCli)
	}
}

func TestRegisterAuthManagerForQuotaUsesRuntimeAuthIndex(t *testing.T) {
	authManager := coreauth.NewManager(nil, nil, nil)
	authPath := filepath.Join(t.TempDir(), "claude.json")
	auth := &coreauth.Auth{
		ID:       "auth-claude",
		Provider: "claude",
		FileName: filepath.Base(authPath),
		Attributes: map[string]string{
			"path":   authPath,
			"source": authPath,
		},
		Metadata: map[string]any{
			"type": "claude",
		},
	}
	registeredAuth, err := authManager.Register(context.Background(), auth)
	if err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	legacyIndex := computeAuthIndex(filepath.Base(authPath))
	if registeredAuth.Index == legacyIndex {
		t.Fatalf("runtime auth index unexpectedly matches legacy filename hash: %s", registeredAuth.Index)
	}

	scheduler := quota.NewScheduler(quota.DefaultConfig(), nil)
	if count := registerAuthManagerForQuota(scheduler, authManager); count != 1 {
		t.Fatalf("registered quota auth count = %d, want 1", count)
	}

	entry := scheduler.GetStatus().Credentials[filepath.Base(authPath)]
	if entry == nil {
		t.Fatalf("quota entry for %q was not registered", filepath.Base(authPath))
	}
	if entry.AuthIndex != registeredAuth.Index {
		t.Fatalf("auth index = %q, want runtime auth index %q", entry.AuthIndex, registeredAuth.Index)
	}
}
