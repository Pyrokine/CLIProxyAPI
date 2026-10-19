package xai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

func TestSaveTokenToFilePublishesConcurrentWritesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth", "xai.json")
	storages := []*TokenStorage{
		{AccessToken: "first-access", RefreshToken: "first-refresh"},
		{AccessToken: "second-access", RefreshToken: "second-refresh"},
	}
	start := make(chan struct{})
	errs := make(chan error, len(storages))
	var group sync.WaitGroup
	for _, storage := range storages {
		group.Go(func() {
			<-start
			errs <- storage.SaveTokenToFile(path)
		})
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("SaveTokenToFile() error = %v", err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	var saved TokenStorage
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("unmarshal token file: %v; data=%q", err, data)
	}
	valid := (saved.AccessToken == "first-access" && saved.RefreshToken == "first-refresh") ||
		(saved.AccessToken == "second-access" && saved.RefreshToken == "second-refresh")
	if !valid {
		t.Fatalf("saved token = %#v", saved)
	}
}

func TestSaveTokenToFileRejectsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	path := filepath.Join(dir, "xai.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink token file: %v", err)
	}
	storage := &TokenStorage{AccessToken: "access", RefreshToken: "refresh"}
	if err := storage.SaveTokenToFile(path); err == nil {
		t.Fatal("SaveTokenToFile accepted symlink target")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "original\n" {
		t.Fatalf("target data = %q", data)
	}
}

func TestSaveTokenToFileRejectsSymlinkDirectory(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "auth-target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatalf("chmod target: %v", err)
	}
	authDir := filepath.Join(base, "auth")
	if err := os.Symlink(target, authDir); err != nil {
		t.Fatalf("symlink auth directory: %v", err)
	}

	storage := &TokenStorage{AccessToken: "access", RefreshToken: "refresh"}
	if err := storage.SaveTokenToFile(filepath.Join(authDir, "xai.json")); err == nil {
		t.Fatal("SaveTokenToFile accepted symlink directory")
	}
	if _, err := os.Stat(filepath.Join(target, "xai.json")); !os.IsNotExist(err) {
		t.Fatalf("symlink target received token file: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("target mode = %o, want 755", got)
	}
}

func TestSaveTokenPayloadRemainsAnchoredAfterDirectoryReplacement(t *testing.T) {
	base := t.TempDir()
	authDir := filepath.Join(base, "auth")
	root, err := util.OpenOrCreateDirectoryRoot(authDir, 0o700)
	if err != nil {
		t.Fatalf("OpenOrCreateDirectoryRoot() error = %v", err)
	}
	defer func() { _ = root.Close() }()

	movedDir := filepath.Join(base, "auth-opened")
	if err = os.Rename(authDir, movedDir); err != nil {
		t.Fatalf("rename auth directory: %v", err)
	}
	replacementTarget := filepath.Join(t.TempDir(), "replacement")
	if err = os.Mkdir(replacementTarget, 0o700); err != nil {
		t.Fatalf("mkdir replacement target: %v", err)
	}
	if err = os.Symlink(replacementTarget, authDir); err != nil {
		t.Fatalf("replace auth directory with symlink: %v", err)
	}
	if err = saveTokenPayload(root, "xai.json", []byte("{\"type\":\"xai\"}\n")); err != nil {
		t.Fatalf("saveTokenPayload() error = %v", err)
	}

	if _, err = os.Stat(filepath.Join(movedDir, "xai.json")); err != nil {
		t.Fatalf("opened auth directory did not receive token: %v", err)
	}
	if _, err = os.Stat(filepath.Join(replacementTarget, "xai.json")); !os.IsNotExist(err) {
		t.Fatalf("replacement target received token: %v", err)
	}
}

func TestSaveTokenToFileRejectsDirectoryPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auth")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir auth directory: %v", err)
	}
	storage := &TokenStorage{AccessToken: "access", RefreshToken: "refresh"}
	if err := storage.SaveTokenToFile(dir + string(os.PathSeparator)); err == nil {
		t.Fatal("SaveTokenToFile accepted directory path")
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.Base(dir))); !os.IsNotExist(err) {
		t.Fatalf("directory path created nested token file: %v", err)
	}
}

func TestSaveTokenToFileSupportsRelativeAuthPath(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	testDir := t.TempDir()
	if err = os.Chdir(testDir); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(workingDirectory) })
	relativePath := filepath.Join("auth", "xai.json")
	absolutePath := filepath.Join(testDir, relativePath)
	storage := &TokenStorage{AccessToken: "access", RefreshToken: "refresh"}
	if err = storage.SaveTokenToFile(relativePath); err != nil {
		t.Fatalf("SaveTokenToFile() error = %v", err)
	}
	if _, err = os.Stat(absolutePath); err != nil {
		t.Fatalf("stat token file: %v", err)
	}
}

func TestSaveTokenToFileSupportsLongBasename(t *testing.T) {
	name := strings.Repeat("x", 240)
	path := filepath.Join(t.TempDir(), "auth", name)
	storage := &TokenStorage{AccessToken: "access", RefreshToken: "refresh"}
	if err := storage.SaveTokenToFile(path); err != nil {
		t.Fatalf("SaveTokenToFile() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat token file: %v", err)
	}
}

func TestSaveTokenToFileKeepsExistingFileWhenMarshalFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth", "xai.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
		t.Fatalf("write original: %v", err)
	}
	storage := &TokenStorage{
		AccessToken: "access",
		Metadata:    map[string]any{"invalid": func() {}},
	}
	if err := storage.SaveTokenToFile(path); err == nil {
		t.Fatal("SaveTokenToFile succeeded with invalid metadata")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if string(data) != "original\n" {
		t.Fatalf("original data = %q", data)
	}
}

func TestSaveTokenToFileSecuresExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth", "xai.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write existing auth file: %v", err)
	}

	storage := &TokenStorage{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
	}
	if err := storage.SaveTokenToFile(path); err != nil {
		t.Fatalf("SaveTokenToFile() error = %v", err)
	}

	if os.PathSeparator == '/' {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat auth file: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("auth file mode = %o, want 600", got)
		}
	}
}
