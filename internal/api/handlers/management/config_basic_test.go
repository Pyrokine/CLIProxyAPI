package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestGetVersionIncludesFeatureCapabilities(t *testing.T) {
	h := NewHandler(&config.Config{}, "", nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/version", nil)

	h.GetVersion(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("GetVersion status = %d", rec.Code)
	}
	var body struct {
		Features map[string]bool `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal version response: %v", err)
	}
	if !body.Features["plugins"] {
		t.Fatalf("features.plugins = false, want true")
	}
}

func newConfigBasicTestHandler(t *testing.T, cfg *config.Config) *Handler {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	handler := NewHandler(cfg, path, nil, withEnvSecret(""))
	t.Cleanup(
		func() {
			if err := handler.Close(); err != nil {
				t.Errorf("close management test handler: %v", err)
			}
		},
	)
	return handler
}

func TestConfigYAMLConditionalWriteRejectsStaleETag(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("port: 8317\ndebug: false\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	handler := NewHandler(&config.Config{Port: 8317}, configPath, nil, withEnvSecret(""))
	defer func() { _ = handler.Close() }()

	getRecorder := httptest.NewRecorder()
	getContext, _ := gin.CreateTestContext(getRecorder)
	getContext.Request = httptest.NewRequest(http.MethodGet, "/config.yaml", nil)
	handler.GetConfigYAML(getContext)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("GetConfigYAML status = %d, body = %s", getRecorder.Code, getRecorder.Body.String())
	}
	etag := getRecorder.Header().Get("ETag")
	redactionKey, errKey := handler.configYAMLRedactionKey()
	if errKey != nil {
		t.Fatalf("configYAMLRedactionKey: %v", errKey)
	}
	if etag != configYAMLETag(original, redactionKey) {
		t.Fatalf("ETag = %q, want keyed config digest", etag)
	}

	external := []byte("port: 8317\ndebug: true\n")
	if err := os.WriteFile(configPath, external, 0o600); err != nil {
		t.Fatalf("write external config: %v", err)
	}
	putRecorder := httptest.NewRecorder()
	putContext, _ := gin.CreateTestContext(putRecorder)
	putContext.Request = httptest.NewRequest(
		http.MethodPut,
		"/config.yaml",
		strings.NewReader("port: 8317\ndebug: false\nproxy-url: https://example.com\n"),
	)
	putContext.Request.Header.Set("Content-Type", "application/yaml")
	putContext.Request.Header.Set("If-Match", etag)
	handler.PutConfigYAML(putContext)
	if putRecorder.Code != http.StatusPreconditionFailed {
		t.Fatalf(
			"PutConfigYAML status = %d, want %d: %s", putRecorder.Code, http.StatusPreconditionFailed,
			putRecorder.Body.String(),
		)
	}
	stored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !bytes.Equal(stored, external) {
		t.Fatalf("stale write changed config: %q", stored)
	}
}

func TestConfigYAMLConditionalWriteAcceptsCurrentETag(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("port: 8317\ndebug: false\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	handler := NewHandler(&config.Config{Port: 8317}, configPath, nil, withEnvSecret(""))
	defer func() { _ = handler.Close() }()

	putRecorder := httptest.NewRecorder()
	putContext, _ := gin.CreateTestContext(putRecorder)
	putContext.Request = httptest.NewRequest(
		http.MethodPut,
		"/config.yaml",
		strings.NewReader("port: 8317\ndebug: true\n"),
	)
	putContext.Request.Header.Set("Content-Type", "application/yaml")
	redactionKey, errKey := handler.configYAMLRedactionKey()
	if errKey != nil {
		t.Fatalf("configYAMLRedactionKey: %v", errKey)
	}
	putContext.Request.Header.Set("If-Match", configYAMLETag(original, redactionKey))
	handler.PutConfigYAML(putContext)
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("PutConfigYAML status = %d, want %d: %s", putRecorder.Code, http.StatusOK, putRecorder.Body.String())
	}
}

func TestConfigYAMLRedactionKeyPersistsAcrossHandlers(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	const rawSecret = "sk-persistent-redaction-key-secret"
	if err := os.WriteFile(
		configPath,
		[]byte("port: 8317\napi-keys:\n  - "+rawSecret+"\n"),
		0o600,
	); err != nil {
		t.Fatalf("write config: %v", err)
	}

	handler := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/config.yaml", nil)
	handler.GetConfigYAML(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GetConfigYAML status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	redacted := recorder.Body.String()
	if strings.Contains(redacted, rawSecret) || !strings.Contains(redacted, configYAMLPlaceholderPrefix) {
		t.Fatalf("GetConfigYAML redaction mismatch: %s", redacted)
	}
	firstKey, err := handler.configYAMLRedactionKey()
	if err != nil {
		t.Fatalf("configYAMLRedactionKey: %v", err)
	}
	if err = handler.Close(); err != nil {
		t.Fatalf("close first handler: %v", err)
	}

	keyPath, err := configYAMLRedactionKeyPath(configPath)
	if err != nil {
		t.Fatalf("configYAMLRedactionKeyPath: %v", err)
	}
	if os.PathSeparator == '/' {
		info, err := os.Stat(keyPath)
		if err != nil {
			t.Fatalf("stat redaction key: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("redaction key mode = %o, want 600", got)
		}
	}

	restarted := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	defer func() { _ = restarted.Close() }()
	secondKey, err := restarted.configYAMLRedactionKey()
	if err != nil {
		t.Fatalf("restarted configYAMLRedactionKey: %v", err)
	}
	if firstKey != secondKey {
		t.Fatal("restarted handler loaded a different redaction key")
	}
	putRecorder := httptest.NewRecorder()
	putContext, _ := gin.CreateTestContext(putRecorder)
	putContext.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", strings.NewReader(redacted))
	putContext.Request.Header.Set("Content-Type", "application/yaml")
	restarted.PutConfigYAML(putContext)
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("PutConfigYAML status = %d, want %d: %s", putRecorder.Code, http.StatusOK, putRecorder.Body.String())
	}
	stored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read stored config: %v", err)
	}
	if !strings.Contains(string(stored), rawSecret) {
		t.Fatalf("stored config omitted restored secret: %s", stored)
	}
}

func TestConfigYAMLRedactionKeyUsesPrivateStateWithoutChangingConfigDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	configDir := t.TempDir()
	if err := os.Chmod(configDir, 0o775); err != nil {
		t.Fatalf("chmod config directory: %v", err)
	}
	configPath := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := loadOrCreateConfigYAMLRedactionKey(configPath); err != nil {
		t.Fatalf("loadOrCreateConfigYAMLRedactionKey() error = %v", err)
	}
	if os.PathSeparator == '/' {
		configDirInfo, err := os.Stat(configDir)
		if err != nil {
			t.Fatalf("stat config directory: %v", err)
		}
		if got := configDirInfo.Mode().Perm(); got != 0o775 {
			t.Fatalf("config directory mode = %o, want 775", got)
		}
	}
	keyPath, err := configYAMLRedactionKeyPath(configPath)
	if err != nil {
		t.Fatalf("configYAMLRedactionKeyPath() error = %v", err)
	}
	if os.PathSeparator == '/' {
		stateInfo, err := os.Stat(filepath.Dir(keyPath))
		if err != nil {
			t.Fatalf("stat redaction state directory: %v", err)
		}
		if got := stateInfo.Mode().Perm(); got != 0o700 {
			t.Fatalf("redaction state directory mode = %o, want 700", got)
		}
	}
}

func TestLoadOrCreateConfigYAMLRedactionKeyRejectsAncestorSymlink(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(base, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink config ancestor: %v", err)
	}
	configPath := filepath.Join(link, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config through symlink: %v", err)
	}

	if _, err := loadOrCreateConfigYAMLRedactionKey(configPath); err == nil {
		t.Fatal("loadOrCreateConfigYAMLRedactionKey accepted symlink ancestor")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("read symlink target: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".cliproxy-management-") {
			t.Fatalf("symlink target received redaction state %q", entry.Name())
		}
	}
}

func TestConfigYAMLRedactionKeyUsesDistinctStateDirectoriesPerConfig(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.yaml")
	secondPath := filepath.Join(dir, "second.yaml")
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
			t.Fatalf("write config %q: %v", path, err)
		}
	}
	firstKeyPath, err := configYAMLRedactionKeyPath(firstPath)
	if err != nil {
		t.Fatalf("first configYAMLRedactionKeyPath() error = %v", err)
	}
	secondKeyPath, err := configYAMLRedactionKeyPath(secondPath)
	if err != nil {
		t.Fatalf("second configYAMLRedactionKeyPath() error = %v", err)
	}
	if filepath.Dir(firstKeyPath) == filepath.Dir(secondKeyPath) {
		t.Fatalf("configs share state directory %q", filepath.Dir(firstKeyPath))
	}
	if _, err = loadOrCreateConfigYAMLRedactionKey(firstPath); err != nil {
		t.Fatalf("load first redaction key: %v", err)
	}
	if _, err = loadOrCreateConfigYAMLRedactionKey(secondPath); err != nil {
		t.Fatalf("load second redaction key: %v", err)
	}
}

func TestConfigYAMLRedactionKeySupportsLongConfigBasename(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	configPath := filepath.Join(t.TempDir(), strings.Repeat("c", 250))
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := loadOrCreateConfigYAMLRedactionKey(configPath); err != nil {
		t.Fatalf("loadOrCreateConfigYAMLRedactionKey() error = %v", err)
	}
	keyPath, err := configYAMLRedactionKeyPath(configPath)
	if err != nil {
		t.Fatalf("configYAMLRedactionKeyPath() error = %v", err)
	}
	if len(filepath.Base(keyPath)) >= 255 {
		t.Fatalf("redaction key basename length = %d", len(filepath.Base(keyPath)))
	}
	if _, err = os.Stat(keyPath); err != nil {
		t.Fatalf("stat redaction key: %v", err)
	}
}

func TestConfigYAMLRedactionKeyRejectsInvalidFiles(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")

	for _, test := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{
			name: "invalid length",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
					t.Fatalf("write invalid key: %v", err)
				}
			},
		},
		{
			name: "symlink",
			prepare: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(filepath.Dir(path), "key-target")
				if err := os.WriteFile(target, make([]byte, 32), 0o600); err != nil {
					t.Fatalf("write key target: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink key: %v", err)
				}
			},
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				configPath := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
					t.Fatalf("write config: %v", err)
				}
				keyPath, err := configYAMLRedactionKeyPath(configPath)
				if err != nil {
					t.Fatalf("configYAMLRedactionKeyPath: %v", err)
				}
				if err = os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
					t.Fatalf("mkdir redaction state: %v", err)
				}
				test.prepare(t, keyPath)
				handler := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
				defer func() { _ = handler.Close() }()
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodGet, "/config.yaml", nil)
				handler.GetConfigYAML(c)
				if recorder.Code != http.StatusInternalServerError {
					t.Fatalf(
						"status = %d, want %d: %s", recorder.Code, http.StatusInternalServerError,
						recorder.Body.String(),
					)
				}
				if strings.Contains(recorder.Body.String(), keyPath) || strings.Contains(
					recorder.Body.String(), configPath,
				) {
					t.Fatalf("response leaked internal path: %s", recorder.Body.String())
				}
			},
		)
	}
}

func TestPutConfigYAMLWithoutPlaceholdersOrIfMatchDoesNotNeedRedactionKey(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	keyPath, err := configYAMLRedactionKeyPath(configPath)
	if err != nil {
		t.Fatalf("configYAMLRedactionKeyPath: %v", err)
	}
	if err = os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatalf("mkdir redaction state: %v", err)
	}
	if err = os.WriteFile(keyPath, []byte("invalid"), 0o600); err != nil {
		t.Fatalf("write invalid redaction key: %v", err)
	}

	handler := NewHandler(&config.Config{}, configPath, nil, withEnvSecret(""))
	defer func() { _ = handler.Close() }()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", strings.NewReader("port: 8318\n"))
	handler.PutConfigYAML(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
}

func TestLoadOrCreateConfigYAMLRedactionKeyRejectsWritableStateSymlink(t *testing.T) {
	writablePath := t.TempDir()
	target := filepath.Join(t.TempDir(), "state-target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(writablePath, "management-state")); err != nil {
		t.Fatalf("symlink management state: %v", err)
	}
	t.Setenv("WRITABLE_PATH", writablePath)

	if _, err := loadOrCreateConfigYAMLRedactionKey(filepath.Join(t.TempDir(), "config.yaml")); err == nil {
		t.Fatal("loadOrCreateConfigYAMLRedactionKey accepted symlink state directory")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("target mode = %o, want 755", got)
	}
	if _, err = os.Stat(filepath.Join(target, "config-redaction.key")); !os.IsNotExist(err) {
		t.Fatalf("symlink target received redaction key: %v", err)
	}
}

func TestConfigYAMLRedactionKeyRootRemainsAnchoredAfterDirectoryReplacement(t *testing.T) {
	writablePath := t.TempDir()
	t.Setenv("WRITABLE_PATH", writablePath)
	root, keyName, err := openConfigYAMLRedactionKeyRoot(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("openConfigYAMLRedactionKeyRoot() error = %v", err)
	}
	defer func() { _ = root.Close() }()

	stateDir := filepath.Join(writablePath, "management-state")
	movedDir := filepath.Join(writablePath, "management-state-opened")
	if err = os.Rename(stateDir, movedDir); err != nil {
		t.Fatalf("rename management state: %v", err)
	}
	replacementTarget := filepath.Join(t.TempDir(), "replacement")
	if err = os.Mkdir(replacementTarget, 0o700); err != nil {
		t.Fatalf("mkdir replacement target: %v", err)
	}
	if err = os.Symlink(replacementTarget, stateDir); err != nil {
		t.Fatalf("replace management state with symlink: %v", err)
	}
	if err = root.WriteFile(keyName, make([]byte, 32), 0o600); err != nil {
		t.Fatalf("write through opened root: %v", err)
	}

	if _, err = os.Stat(filepath.Join(movedDir, keyName)); err != nil {
		t.Fatalf("opened management state did not receive key: %v", err)
	}
	if _, err = os.Stat(filepath.Join(replacementTarget, keyName)); !os.IsNotExist(err) {
		t.Fatalf("replacement target received key: %v", err)
	}
}

func TestLoadOrCreateConfigYAMLRedactionKeyConcurrent(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	const workers = 8
	keys := make([][32]byte, workers)
	errs := make([]error, workers)
	var group sync.WaitGroup
	for index := range workers {
		group.Go(func() {
			keys[index], errs[index] = loadOrCreateConfigYAMLRedactionKey(configPath)
		})
	}
	group.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", index, err)
		}
		if keys[index] != keys[0] {
			t.Fatalf("worker %d loaded a different key", index)
		}
	}
}

func TestLoadOrCreateConfigYAMLRedactionKeyRejectsSymlinkLock(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	keyPath, err := configYAMLRedactionKeyPath(configPath)
	if err != nil {
		t.Fatalf("configYAMLRedactionKeyPath: %v", err)
	}
	if err = os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatalf("mkdir redaction state: %v", err)
	}
	lockTarget := filepath.Join(filepath.Dir(keyPath), "missing-lock-target")
	if err = os.Symlink(lockTarget, keyPath+".lock"); err != nil {
		t.Fatalf("symlink lock: %v", err)
	}

	if _, err = loadOrCreateConfigYAMLRedactionKey(configPath); err == nil {
		t.Fatal("loadOrCreateConfigYAMLRedactionKey accepted symlink lock")
	}
	if _, err = os.Stat(lockTarget); !os.IsNotExist(err) {
		t.Fatalf("lock target was created: %v", err)
	}
}

func TestPutProxyURLAllowsPrivateAndRejectsInvalidScheme(t *testing.T) {
	cfg := &config.Config{}
	h := newConfigBasicTestHandler(t, cfg)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/config/proxy-url",
		bytes.NewBufferString(`{"value":"http://127.0.0.1:8080"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PutProxyURL(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("private proxy status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if snapshot := h.configSnapshot(); snapshot.ProxyURL != "http://127.0.0.1:8080" {
		t.Fatalf("proxy URL = %q", snapshot.ProxyURL)
	}

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/config/proxy-url",
		bytes.NewBufferString(`{"value":"file:///tmp/socket"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PutProxyURL(c)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("invalid proxy status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	if snapshot := h.configSnapshot(); snapshot.ProxyURL != "http://127.0.0.1:8080" {
		t.Fatalf("proxy URL after invalid update = %q", snapshot.ProxyURL)
	}
}
