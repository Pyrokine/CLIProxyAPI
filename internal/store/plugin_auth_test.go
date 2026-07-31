package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	sdkauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type storedPluginAuthParser struct{}

func (storedPluginAuthParser) ParseAuth(
	_ context.Context,
	req pluginapi.AuthParseRequest,
) (*coreauth.Auth, bool, error) {
	if req.Provider != "plugin-provider" {
		return nil, false, nil
	}
	return &coreauth.Auth{
		ID:       "account-123",
		Provider: req.Provider,
		FileName: req.FileName,
		Metadata: map[string]any{"type": req.Provider},
	}, true, nil
}

func TestRemoteFileStoresUsePluginStableAuthID(t *testing.T) {
	ctx := sdkauth.WithPluginAuthParser(context.Background(), storedPluginAuthParser{})
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.json")
	if errWrite := os.WriteFile(path, []byte(`{"type":"plugin-provider"}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	gitAuth, errGit := (&GitTokenStore{}).readAuthFile(ctx, path, dir)
	if errGit != nil {
		t.Fatal(errGit)
	}
	objectAuth, errObject := (&ObjectTokenStore{}).readAuthFile(ctx, path, dir)
	if errObject != nil {
		t.Fatal(errObject)
	}
	for name, auth := range map[string]*coreauth.Auth{"git": gitAuth, "object": objectAuth} {
		if auth == nil || auth.ID != "account-123" || auth.Attributes["path"] != path {
			t.Fatalf("%s auth = %#v", name, auth)
		}
	}
}
