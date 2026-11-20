package store

import (
	"context"
	"errors"
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
	if req.Provider == "empty-plugin-provider" {
		return nil, true, nil
	}
	if req.Provider == "canceled-plugin-provider" {
		return nil, true, context.Canceled
	}
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

func TestRemoteFileStoresSinglePluginResults(t *testing.T) {
	readers := map[string]func(context.Context, string, string) (*coreauth.Auth, error){
		"git":    (&GitTokenStore{}).readAuthFile,
		"object": (&ObjectTokenStore{}).readAuthFile,
	}
	for name, read := range readers {
		t.Run(
			name, func(t *testing.T) {
				for _, tc := range []struct {
					provider string
					wantID   string
					wantErr  error
				}{
					{provider: "empty-plugin-provider"},
					{provider: "plugin-provider", wantID: "account-123"},
					{provider: "other-provider", wantID: "plugin.json"},
					{provider: "canceled-plugin-provider", wantErr: context.Canceled},
				} {
					t.Run(
						tc.provider, func(t *testing.T) {
							directory := t.TempDir()
							path := filepath.Join(directory, "plugin.json")
							raw := []byte(`{"type":"` + tc.provider + `"}`)
							if err := os.WriteFile(path, raw, 0o600); err != nil {
								t.Fatal(err)
							}
							ctx := sdkauth.WithPluginAuthParser(t.Context(), storedPluginAuthParser{})
							auth, err := read(ctx, path, directory)
							if !errors.Is(err, tc.wantErr) {
								t.Fatalf("read plugin auth: error=%v, want %v", err, tc.wantErr)
							}
							if tc.wantID == "" {
								if auth != nil {
									t.Fatalf("empty or failed plugin result produced an auth: %#v", auth)
								}
								return
							}
							if auth == nil || auth.ID != tc.wantID || auth.CreatedAt.IsZero() || auth.UpdatedAt.IsZero() {
								t.Fatalf("read plugin auth = %#v, want %s with file timestamps", auth, tc.wantID)
							}
						},
					)
				}
			},
		)
	}
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

type storedPluginMultiAuthParser struct{}

func (storedPluginMultiAuthParser) ParseAuth(
	context.Context,
	pluginapi.AuthParseRequest,
) (*coreauth.Auth, bool, error) {
	return nil, false, nil
}

func (storedPluginMultiAuthParser) ParseAuths(
	_ context.Context,
	req pluginapi.AuthParseRequest,
) ([]*coreauth.Auth, bool, error) {
	if req.Provider != "plugin-provider" {
		return nil, false, nil
	}
	return []*coreauth.Auth{
		{
			ID:       "account-a",
			Provider: req.Provider,
			FileName: req.FileName,
			Metadata: map[string]any{"type": req.Provider, "account": "a"},
		},
		nil,
		{
			ID:       "account-b",
			Provider: req.Provider,
			FileName: req.FileName,
			Metadata: map[string]any{"type": req.Provider, "account": "b"},
		},
	}, true, nil
}

func assertStoredPluginMultiAuths(t *testing.T, name, path string, auths []*coreauth.Auth) {
	t.Helper()
	if len(auths) != 2 {
		t.Fatalf("%s auth count = %d, want 2", name, len(auths))
	}
	for index, wantID := range []string{"account-a", "account-b"} {
		auth := auths[index]
		if auth == nil || auth.ID != wantID || !coreauth.IsPluginVirtualAuth(auth) {
			t.Fatalf("%s auth %d = %#v", name, index, auth)
		}
		if auth.Attributes[coreauth.AttributePath] != path ||
			auth.Attributes[coreauth.AttributeSource] != path ||
			auth.Attributes[coreauth.AttributeVirtualSource] != path {
			t.Fatalf("%s auth %s source attributes = %#v", name, auth.ID, auth.Attributes)
		}
	}
	if auths[0].EnsureIndex() == "" || auths[0].EnsureIndex() == auths[1].EnsureIndex() {
		t.Fatalf("%s auth indexes = %q/%q", name, auths[0].EnsureIndex(), auths[1].EnsureIndex())
	}
}

func TestRemoteStoresExpandPluginMultiAuths(t *testing.T) {
	ctx := sdkauth.WithPluginAuthParser(context.Background(), storedPluginMultiAuthParser{})
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.json")
	raw := []byte(`{"type":"plugin-provider"}`)
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	gitAuths, errGit := (&GitTokenStore{}).readAuthFiles(ctx, path, dir)
	if errGit != nil {
		t.Fatal(errGit)
	}
	assertStoredPluginMultiAuths(t, "git", path, gitAuths)

	objectAuths, errObject := (&ObjectTokenStore{}).readAuthFiles(ctx, path, dir)
	if errObject != nil {
		t.Fatal(errObject)
	}
	assertStoredPluginMultiAuths(t, "object", path, objectAuths)

	postgresStore, state := newPostgresStoreForTest(t)
	state.mu.Lock()
	state.id = "plugin.json"
	state.payload = string(raw)
	state.mu.Unlock()
	postgresAuths, errPostgres := postgresStore.List(ctx)
	if errPostgres != nil {
		t.Fatal(errPostgres)
	}
	assertStoredPluginMultiAuths(
		t,
		"postgres",
		filepath.Join(postgresStore.AuthDir(), "plugin.json"),
		postgresAuths,
	)
}
