package cliproxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestBuilderAuthDirContract(t *testing.T) {
	originalStore := sdkAuth.GetTokenStore()
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(originalStore) })
	for _, input := range []string{"~/tokens", "tokens", "."} {
		t.Run(
			input, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("HOME", root)
				t.Chdir(root)
				want := filepath.Join(root, "tokens")
				if input == "." {
					want = root
				}
				store := sdkAuth.NewFileTokenStore()
				sdkAuth.RegisterTokenStore(store)
				cfg := &config.Config{AuthDir: input}
				service, err := NewBuilder().WithConfig(cfg).
					WithConfigPath(filepath.Join(root, "config.yaml")).
					Build()
				if err != nil {
					t.Fatal(err)
				}
				if cfg.AuthDir != want || service.cfg.AuthDir != want {
					t.Fatalf("runtime auth directory = %q, want %q", service.cfg.AuthDir, want)
				}
				_, err = service.coreManager.Register(
					t.Context(), &coreauth.Auth{
						ID: "synthetic.json", Provider: "codex", FileName: "synthetic.json",
						Metadata: map[string]any{"type": "codex", "access_token": "synthetic-token"},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				auths, err := store.List(t.Context())
				if err != nil || len(auths) != 1 || auths[0].ID != "synthetic.json" ||
					auths[0].Attributes[coreauth.AttributePath] != filepath.Join(
						want, "synthetic.json",
					) {
					t.Fatalf("file-store readback = %#v, error = %v", auths, err)
				}
				info, err := os.Stat(filepath.Join(want, "synthetic.json"))
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("credential permissions: info = %v, error = %v", info, err)
				}
			},
		)
	}
}

func TestServiceEnsureAuthDirContract(t *testing.T) {
	for _, input := range []string{"~/tokens", "tokens"} {
		t.Run(
			input, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("HOME", root)
				t.Chdir(root)
				want := filepath.Join(root, "tokens")
				service := &Service{cfg: &config.Config{AuthDir: input}}
				if err := service.ensureAuthDir(); err != nil {
					t.Fatal(err)
				}
				if service.cfg.AuthDir != want {
					t.Fatalf("runtime auth directory = %q, want %q", service.cfg.AuthDir, want)
				}
				info, err := os.Stat(want)
				if err != nil || info.Mode().Perm() != 0o700 {
					t.Fatalf("directory permissions: info = %v, error = %v", info, err)
				}
				path := filepath.Join(want, "synthetic.json")
				if err := os.WriteFile(
					path, []byte(`{"type":"codex","access_token":"synthetic-old"}`), 0o644,
				); err != nil {
					t.Fatal(err)
				}
				if err := service.ensureAuthDir(); err != nil {
					t.Fatal(err)
				}
				store := sdkAuth.NewFileTokenStore()
				store.SetBaseDir(service.cfg.AuthDir)
				auths, err := store.List(t.Context())
				if err != nil || len(auths) != 1 {
					t.Fatalf("normalized directory readback = %#v, error = %v", auths, err)
				}
				info, err = os.Stat(path)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("existing credential permissions: info = %v, error = %v", info, err)
				}
				if _, err := os.Stat(filepath.Join(root, "~")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("literal tilde directory exists: %v", err)
				}
			},
		)
	}
}

type authDirContractTokenProvider func(context.Context, *config.Config) (*TokenClientResult, error)

func (load authDirContractTokenProvider) Load(
	ctx context.Context,
	cfg *config.Config,
) (*TokenClientResult, error) {
	return load(ctx, cfg)
}

func TestServiceRunAuthDirBeforeClientLoad(t *testing.T) {
	for _, input := range []string{"~/tokens", "tokens"} {
		t.Run(
			input, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("HOME", root)
				t.Chdir(root)
				want := filepath.Join(root, "tokens")
				stop := errors.New("synthetic loader stops before server construction")
				service := &Service{cfg: &config.Config{AuthDir: input}}
				service.tokenProvider = authDirContractTokenProvider(
					func(ctx context.Context, cfg *config.Config) (*TokenClientResult, error) {
						if cfg.AuthDir != want {
							t.Fatalf("client loader directory = %q, want %q", cfg.AuthDir, want)
						}
						store := sdkAuth.NewFileTokenStore()
						store.SetBaseDir(cfg.AuthDir)
						path, err := store.Save(
							ctx, &coreauth.Auth{
								ID: "synthetic.json", Provider: "codex",
								Metadata: map[string]any{"type": "codex", "access_token": "synthetic"},
							},
						)
						if err != nil || path != filepath.Join(want, "synthetic.json") {
							t.Fatalf("client loader save = %q, %v", path, err)
						}
						if auths, err := store.List(ctx); err != nil || len(auths) != 1 ||
							auths[0].ID != "synthetic.json" {
							t.Fatalf("client loader readback = %#v, %v", auths, err)
						}
						return nil, stop
					},
				)
				if err := service.Run(t.Context()); !errors.Is(err, stop) || service.server != nil {
					t.Fatalf(
						"service passed the synthetic loader: error = %v, server = %v",
						err,
						service.server,
					)
				}
			},
		)
	}
}

func TestServiceEnsureAuthDirRejectsUnsafePaths(t *testing.T) {
	for _, unsafe := range []string{"symlink-directory", "symlink-token", "file-directory", "empty"} {
		t.Run(
			unsafe, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("HOME", root)
				t.Chdir(root)
				dir := filepath.Join(root, "tokens")
				input := "~/tokens"
				switch unsafe {
				case "symlink-directory":
					if err := os.Symlink(t.TempDir(), dir); err != nil {
						t.Fatal(err)
					}
				case "symlink-token":
					if err := os.Mkdir(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(dir, "token.json")); err != nil {
						t.Fatal(err)
					}
				case "file-directory":
					if err := os.WriteFile(dir, []byte("synthetic"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "empty":
					input = ""
				}
				service := &Service{cfg: &config.Config{AuthDir: input}}
				if err := service.ensureAuthDir(); err == nil {
					t.Fatal("unsafe directory accepted")
				}
			},
		)
	}
}

func TestBuilderAuthDirResolutionFailureAndOptionalDirectory(t *testing.T) {
	t.Setenv("HOME", "")
	t.Chdir(t.TempDir())
	if service,
		err :=
		NewBuilder().WithConfig(&config.Config{AuthDir: "~/tokens"}).WithConfigPath("config.yaml").Build(); err ==
		nil ||
		service != nil {
		t.Fatalf("unresolvable directory: service = %v, error = %v", service, err)
	}
	if _, err := NewBuilder().WithConfig(&config.Config{}).WithCoreAuthManager(
		coreauth.NewManager(
			nil, nil, nil,
		),
	).WithConfigPath("config.yaml").Build(); err != nil {
		t.Fatalf("optional directory gained a home-directory prerequisite: %v", err)
	}
	service := &Service{cfg: &config.Config{AuthDir: "~/tokens"}}
	if err := service.ensureAuthDir(); err == nil {
		t.Fatal("service accepted an unresolvable directory")
	}
}
