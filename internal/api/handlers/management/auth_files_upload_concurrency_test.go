package management

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	authfile "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type uploadedAuthParserFunc func(
	context.Context,
	pluginapi.AuthParseRequest,
) (*coreauth.Auth, bool, error)

func (f uploadedAuthParserFunc) ParseAuth(
	ctx context.Context,
	req pluginapi.AuthParseRequest,
) (*coreauth.Auth, bool, error) {
	return f(ctx, req)
}

type uploadedAuthStore struct {
	persistErr error
	persisted  chan string
}

func (*uploadedAuthStore) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, nil
}

func (*uploadedAuthStore) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", nil
}

func (*uploadedAuthStore) Delete(context.Context, string) error {
	return nil
}

func (s *uploadedAuthStore) PersistAuthFiles(_ context.Context, _ string, paths ...string) error {
	if len(paths) > 0 && s.persisted != nil {
		s.persisted <- paths[0]
	}
	return s.persistErr
}

type uploadedPluginHost struct {
	parse uploadedAuthParserFunc
}

func (*uploadedPluginHost) RegisteredPlugins() []pluginhost.RegisteredPluginInfo {
	return nil
}

func (*uploadedPluginHost) PluginBusy(string) bool {
	return false
}

func (*uploadedPluginHost) PluginRuntimeIdentity(string) pluginhost.PluginRuntimeIdentity {
	return pluginhost.PluginRuntimeIdentity{}
}

func (*uploadedPluginHost) UnloadPluginWithError(string) (bool, error) {
	return false, nil
}

func (*uploadedPluginHost) UnloadPluginIfCurrentWithError(pluginhost.PluginRuntimeIdentity) (bool, error) {
	return false, nil
}

func (h *uploadedPluginHost) ParseAuth(
	ctx context.Context,
	req pluginapi.AuthParseRequest,
) (*coreauth.Auth, bool, error) {
	return h.parse(ctx, req)
}

func TestAuthMatchesPathPreservesLinuxCaseSensitivity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file paths are case-insensitive")
	}
	root := t.TempDir()
	lower := filepath.Join(root, "a.json")
	upper := filepath.Join(root, "A.json")
	auth := &coreauth.Auth{Attributes: map[string]string{"path": lower, "source": lower}}
	if authMatchesPath(auth, upper) {
		t.Fatalf("case-distinct paths matched: %q and %q", lower, upper)
	}
}

func TestAuthIDForPathMatchesRelativeAuthDirectory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	path, errValidate := validateAuthFileName("auth", "shared.json")
	if errValidate != nil {
		t.Fatal(errValidate)
	}
	if got := authIDForPath(path, "auth"); got != "shared.json" {
		t.Fatalf("authIDForPath() = %q", got)
	}
	auth := &coreauth.Auth{Attributes: map[string]string{"path": filepath.Join("auth", "shared.json")}}
	if !authMatchesPath(auth, path) {
		t.Fatalf("relative auth path %q did not match %q", auth.Attributes["path"], path)
	}
}

func TestAuthFromUploadedFileUsesPluginStableID(t *testing.T) {
	parser := uploadedAuthParserFunc(
		func(_ context.Context, req pluginapi.AuthParseRequest) (*coreauth.Auth, bool, error) {
			if req.Provider != "plugin-provider" || filepath.Base(req.Path) != "plugin.json" {
				t.Fatalf("plugin parse request = %#v", req)
			}
			return &coreauth.Auth{
				ID:       "account-123",
				Provider: "plugin-provider",
				FileName: req.FileName,
				Attributes: map[string]string{
					"path":   req.Path,
					"source": req.Path,
				},
				Metadata: map[string]any{"type": "plugin-provider"},
			}, true, nil
		},
	)
	authDir := t.TempDir()
	path := filepath.Join(authDir, "plugin.json")
	auth, _, errParse := authFromUploadedFile(
		context.Background(),
		parser,
		authDir,
		path,
		[]byte(`{"type":"plugin-provider"}`),
	)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if auth == nil || auth.ID != "account-123" || auth.Provider != "plugin-provider" {
		t.Fatalf("parsed plugin auth = %#v", auth)
	}
}

func TestSaveUploadedAuthWaitsForRemotePersistence(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "remote.json")
	store := &uploadedAuthStore{persisted: make(chan string, 1)}
	manager := coreauth.NewManager(nil, nil, nil)
	handler := NewHandlerWithoutConfigFilePath(
		&config.Config{AuthDir: authDir},
		manager,
		WithTokenStore(store),
	)
	if errSave := handler.saveUploadedAuth(
		context.Background(), authDir, path, []byte(`{"type":"codex","token":"saved"}`),
	); errSave != nil {
		t.Fatal(errSave)
	}
	select {
	case persistedPath := <-store.persisted:
		if persistedPath != path {
			t.Fatalf("persisted path = %q", persistedPath)
		}
	default:
		t.Fatal("saveUploadedAuth returned before remote persistence")
	}
}

func TestSaveUploadedAuthRollsBackLocalFileWhenRemotePersistenceFails(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "remote.json")
	original := []byte(`{"type":"codex","token":"original"}`)
	if errWrite := authfile.WriteTokenFileAtomic(path, original); errWrite != nil {
		t.Fatal(errWrite)
	}
	store := &uploadedAuthStore{persistErr: errors.New("remote rejected")}
	manager := coreauth.NewManager(nil, nil, nil)
	handler := NewHandlerWithoutConfigFilePath(
		&config.Config{AuthDir: authDir},
		manager,
		WithTokenStore(store),
	)
	errSave := handler.saveUploadedAuth(
		context.Background(), authDir, path, []byte(`{"type":"codex","token":"replacement"}`),
	)
	if errSave == nil || !strings.Contains(errSave.Error(), "remote rejected") {
		t.Fatalf("saveUploadedAuth() error = %v", errSave)
	}
	persisted, errRead := os.ReadFile(path)
	if errRead != nil || string(persisted) != string(original) {
		t.Fatalf("rolled back file = %q, %v", persisted, errRead)
	}
	if _, published := manager.GetByID("remote.json"); published {
		t.Fatal("failed remote upload was published")
	}
}

func TestSaveUploadedAuthPublishesPluginStableID(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "plugin.json")
	manager := coreauth.NewManager(nil, nil, nil)
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	handler.pluginHost = &uploadedPluginHost{parse: uploadedAuthParserFunc(
		func(_ context.Context, req pluginapi.AuthParseRequest) (*coreauth.Auth, bool, error) {
			return &coreauth.Auth{
				ID:       "account-123",
				Provider: "plugin-provider",
				FileName: req.FileName,
				Attributes: map[string]string{
					"path":   req.Path,
					"source": req.Path,
				},
				Metadata: map[string]any{"type": "plugin-provider"},
			}, true, nil
		},
	)}
	if errSave := handler.saveUploadedAuth(
		context.Background(),
		authDir,
		path,
		[]byte(`{"type":"plugin-provider"}`),
	); errSave != nil {
		t.Fatal(errSave)
	}
	current, ok := manager.GetByID("account-123")
	if !ok || current == nil || current.Provider != "plugin-provider" {
		t.Fatalf("plugin auth was not published: %#v", current)
	}
	if _, duplicate := manager.GetByID("plugin.json"); duplicate {
		t.Fatal("file-name plugin auth was published alongside the plugin stable ID")
	}
}

func TestSaveUploadedAuthClearsReplacedGeminiRuntime(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "gemini.json")
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(
		context.Background(),
		&coreauth.Auth{
			ID:       "gemini.json",
			FileName: "gemini.json",
			Provider: "gemini-cli",
			Attributes: map[string]string{
				"path":   path,
				"source": path,
			},
			Metadata: map[string]any{
				"type": "gemini",
				"token": map[string]any{
					"refresh_token": "account-a-refresh",
				},
			},
			Runtime: &struct{ account string }{account: "a"},
		},
	); errRegister != nil {
		t.Fatal(errRegister)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	if errSave := handler.saveUploadedAuth(
		context.Background(),
		authDir,
		path,
		[]byte(`{"type":"gemini","token":{"refresh_token":"account-b-refresh"}}`),
	); errSave != nil {
		t.Fatal(errSave)
	}
	current, ok := manager.GetByID("gemini.json")
	if !ok || current == nil || current.Runtime != nil || refreshTokenValue(current.Metadata) != "account-b-refresh" {
		t.Fatalf("uploaded replacement auth = %#v", current)
	}
}

func TestSaveUploadedAuthDoesNotOverwriteConcurrentPublishedMutationAfterCancellation(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "shared.json")
	original := []byte(`{"type":"codex","token":"original"}`)
	if errWrite := authfile.WriteTokenFileAtomic(path, original); errWrite != nil {
		t.Fatalf("write original auth: %v", errWrite)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	originalAuth, _, errParse := authFromFile(authDir, path, original)
	if errParse != nil {
		t.Fatalf("parse original auth: %v", errParse)
	}
	if _, errRegister := manager.Register(context.Background(), originalAuth); errRegister != nil {
		t.Fatalf("register original auth: %v", errRegister)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	replacement := []byte(`{"type":"codex","token":"replacement"}`)
	replacementAuth, _, errParseReplacement := authFromFile(authDir, path, replacement)
	if errParseReplacement != nil {
		t.Fatalf("parse replacement auth: %v", errParseReplacement)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	replacementDone := make(chan error, 1)
	go func() {
		_, errMutate := manager.MutatePersistentAuth(
			context.Background(),
			replacementAuth.ID,
			func(context.Context) (*coreauth.Auth, error) {
				close(entered)
				<-release
				if errWrite := authfile.WriteTokenFileAtomic(path, replacement); errWrite != nil {
					return nil, errWrite
				}
				return replacementAuth, nil
			},
		)
		replacementDone <- errMutate
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("replacement mutation did not acquire auth lock")
	}

	ctx, cancel := context.WithCancel(context.Background())
	uploadDone := make(chan error, 1)
	go func() {
		uploadDone <- handler.saveUploadedAuth(
			ctx,
			authDir,
			path,
			[]byte(`{"type":"codex","token":"cancelled-upload"}`),
		)
	}()
	cancel()
	select {
	case errUpload := <-uploadDone:
		if !errors.Is(errUpload, context.Canceled) {
			t.Fatalf("cancelled upload error = %v, want context.Canceled", errUpload)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled upload remained blocked")
	}
	close(release)
	select {
	case errReplacement := <-replacementDone:
		if errReplacement != nil {
			t.Fatalf("replacement mutation error = %v", errReplacement)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement mutation did not finish")
	}

	persisted, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read persisted auth: %v", errRead)
	}
	if string(persisted) != string(replacement) {
		t.Fatalf("persisted auth = %s, want %s", persisted, replacement)
	}
	current, ok := manager.GetByID(replacementAuth.ID)
	if !ok || current.Metadata["token"] != "replacement" {
		t.Fatalf("current auth = %#v, want replacement", current)
	}
}
