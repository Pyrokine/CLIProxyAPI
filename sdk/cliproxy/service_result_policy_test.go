package cliproxy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestBuilderAndServiceResultPolicy(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	policy := coreauth.ResultPolicyFunc(func(_ context.Context, result coreauth.Result) coreauth.Result {
		return result
	})
	service, errBuild := NewBuilder().
		WithConfig(&config.Config{AuthDir: t.TempDir()}).
		WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
		WithCoreAuthManager(manager).
		WithResultPolicy(policy).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}
	if service == nil || service.ResultPolicy() == nil || manager.ResultPolicy() == nil {
		t.Fatalf("builder result policy propagation = service:%#v service-policy:%#v manager-policy:%#v", service, service.ResultPolicy(), manager.ResultPolicy())
	}

	updated := coreauth.ResultPolicyFunc(func(_ context.Context, result coreauth.Result) coreauth.Result {
		result.Success = false
		return result
	})
	service.SetResultPolicy(updated)
	if service.ResultPolicy() == nil || manager.ResultPolicy() == nil {
		t.Fatal("Service.SetResultPolicy() did not update the core manager")
	}
	service.SetResultPolicy(nil)
	if service.ResultPolicy() != nil || manager.ResultPolicy() != nil {
		t.Fatal("Service.SetResultPolicy(nil) did not clear the core manager policy")
	}

	var nilService *Service
	nilService.SetResultPolicy(policy)
	if nilService.ResultPolicy() != nil {
		t.Fatal("nil Service returned a result policy")
	}
}
