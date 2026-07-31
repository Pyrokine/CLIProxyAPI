//go:build windows

package management

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigYAMLRedactionKeyPathDistinguishesCaseSensitiveFilesOnWindows(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	testDir, err := os.MkdirTemp(".", ".config-case-sensitive-test-")
	if err != nil {
		t.Fatalf("create test directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(testDir) })
	upperPath := filepath.Join(testDir, "Config.yaml")
	lowerPath := filepath.Join(testDir, "config.yaml")
	if err = os.WriteFile(upperPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write upper config: %v", err)
	}
	if err = os.WriteFile(lowerPath, []byte("port: 8318\n"), 0o600); err != nil {
		t.Fatalf("write lower config: %v", err)
	}
	upperInfo, err := os.Stat(upperPath)
	if err != nil {
		t.Fatalf("stat upper config: %v", err)
	}
	lowerInfo, err := os.Stat(lowerPath)
	if err != nil {
		t.Fatalf("stat lower config: %v", err)
	}
	if os.SameFile(upperInfo, lowerInfo) {
		t.Skip("test directory is case-insensitive")
	}
	upperKeyPath, err := configYAMLRedactionKeyPath(upperPath)
	if err != nil {
		t.Fatalf("upper configYAMLRedactionKeyPath() error = %v", err)
	}
	lowerKeyPath, err := configYAMLRedactionKeyPath(lowerPath)
	if err != nil {
		t.Fatalf("lower configYAMLRedactionKeyPath() error = %v", err)
	}
	if filepath.Dir(upperKeyPath) == filepath.Dir(lowerKeyPath) {
		t.Fatalf("case-sensitive files share state directory %q", filepath.Dir(upperKeyPath))
	}
}

func TestConfigYAMLRedactionKeyPathIsCaseInsensitiveOnWindows(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	configPath := filepath.Join(t.TempDir(), "Config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	firstKeyPath, err := configYAMLRedactionKeyPath(configPath)
	if err != nil {
		t.Fatalf("first configYAMLRedactionKeyPath() error = %v", err)
	}
	secondKeyPath, err := configYAMLRedactionKeyPath(strings.ToLower(configPath))
	if err != nil {
		t.Fatalf("second configYAMLRedactionKeyPath() error = %v", err)
	}
	if !strings.EqualFold(firstKeyPath, secondKeyPath) {
		t.Fatalf("case-equivalent paths produced %q and %q", firstKeyPath, secondKeyPath)
	}
}
