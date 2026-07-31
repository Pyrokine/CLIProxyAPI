package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenOrCreateDirectoryRootRejectsFilesystemRoot(t *testing.T) {
	rootPath := string(os.PathSeparator)
	if volume := filepath.VolumeName(t.TempDir()); volume != "" {
		rootPath = volume + string(os.PathSeparator)
	}
	if _, err := OpenOrCreateDirectoryRoot(rootPath, 0o700); err == nil {
		t.Fatal("OpenOrCreateDirectoryRoot accepted filesystem root")
	}
}

func TestSecurePrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open credential: %v", err)
	}
	if err = SecurePrivateFile(file, false); err != nil {
		_ = file.Close()
		t.Fatalf("SecurePrivateFile() error = %v", err)
	}
	if err = file.Close(); err != nil {
		t.Fatalf("close credential: %v", err)
	}
	if os.PathSeparator == '/' {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat credential: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("credential mode = %o, want 600", got)
		}
	}
}

func TestOpenOrCreateDirectoryRootCreatesPrivateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "one", "two")
	root, err := OpenOrCreateDirectoryRoot(dir, 0o700)
	if err != nil {
		t.Fatalf("OpenOrCreateDirectoryRoot() error = %v", err)
	}
	defer func() { _ = root.Close() }()

	if err = root.WriteFile("value", []byte("ok"), 0o600); err != nil {
		t.Fatalf("write through root: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "value"))
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}
	if string(data) != "ok" {
		t.Fatalf("created file = %q", data)
	}
	if os.PathSeparator == '/' {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat directory: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("directory mode = %o, want 700", got)
		}
	}
}

func TestOpenOrCreateDirectoryRootRejectsSymlinkComponent(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(base, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	if _, err := OpenOrCreateDirectoryRoot(filepath.Join(link, "nested"), 0o700); err == nil {
		t.Fatal("OpenOrCreateDirectoryRoot accepted symlink component")
	}
	if _, err := os.Stat(filepath.Join(target, "nested")); !os.IsNotExist(err) {
		t.Fatalf("symlink target was modified: %v", err)
	}
}

func TestOpenOrCreateDirectoryRootPreservesUnixBackslash(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("backslash is a path separator on this platform")
	}
	base := t.TempDir()
	dir := filepath.Join(base, `literal\name`)
	root, err := OpenOrCreateDirectoryRoot(dir, 0o700)
	if err != nil {
		t.Fatalf("OpenOrCreateDirectoryRoot() error = %v", err)
	}
	defer func() { _ = root.Close() }()
	if err = root.WriteFile("value", []byte("ok"), 0o600); err != nil {
		t.Fatalf("write through root: %v", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "value")); err != nil {
		t.Fatalf("literal backslash directory did not receive file: %v", err)
	}
	if _, err = os.Stat(filepath.Join(base, "literal", "name", "value")); !os.IsNotExist(err) {
		t.Fatalf("backslash was treated as a separator: %v", err)
	}
}

func TestOpenOrCreateDirectoryRootRemainsAnchoredAfterRename(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "state")
	root, err := OpenOrCreateDirectoryRoot(dir, 0o700)
	if err != nil {
		t.Fatalf("OpenOrCreateDirectoryRoot() error = %v", err)
	}
	defer func() { _ = root.Close() }()

	moved := filepath.Join(base, "state-opened")
	if err = os.Rename(dir, moved); err != nil {
		t.Fatalf("rename opened directory: %v", err)
	}
	if err = os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create replacement directory: %v", err)
	}
	if err = root.WriteFile("value", []byte("opened"), 0o600); err != nil {
		t.Fatalf("write through renamed root: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(moved, "value"))
	if err != nil {
		t.Fatalf("read file from opened directory: %v", err)
	}
	if string(data) != "opened" {
		t.Fatalf("opened directory file = %q", data)
	}
	if _, err = os.Stat(filepath.Join(dir, "value")); !os.IsNotExist(err) {
		t.Fatalf("replacement directory received file: %v", err)
	}
}
