package watcher

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestConfigReloadRejectsInvalidPortBeforePublication(t *testing.T) {
	for _, entry := range []string{"direct", "file event"} {
		for _, invalid := range []string{"port: -1\n", "port: 65536\n", "port: 70000\n", "port: [\n"} {
			t.Run(
				entry+"/"+strings.TrimSpace(invalid), func(t *testing.T) {
					dir := t.TempDir()
					authDir := filepath.Join(dir, "auth")
					if err := os.Mkdir(authDir, 0o700); err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(dir, "config.yaml")
					good := []byte(fmt.Sprintf("port: 8317\nauth-dir: %q\n", authDir))
					backup := config.LastGoodPath(path)
					if err := os.WriteFile(backup, good, 0o600); err != nil {
						t.Fatal(err)
					}
					bad := []byte(fmt.Sprintf("%sauth-dir: %q\n", invalid, authDir))
					if err := os.WriteFile(path, bad, 0o600); err != nil {
						t.Fatal(err)
					}
					previous, err := config.ParseConfigBytes(good)
					if err != nil {
						t.Fatal(err)
					}
					calls := 0
					w := &Watcher{
						configPath: path, authDir: authDir,
						lastAuthHashes: make(map[string]string),
						reloadCallback: func(*config.Config) bool { calls++; return true },
					}
					t.Cleanup(w.stopConfigReloadTimer)
					w.SetConfig(previous)
					w.lastConfigHash = "last-accepted-config"
					previousYAML := bytes.Clone(w.oldConfigYaml)
					if entry == "direct" {
						if w.reloadConfig() {
							t.Fatal("invalid configuration was accepted")
						}
					} else {
						w.reloadConfigIfChanged()
					}
					if calls != 0 || w.config != previous || w.authDir != authDir ||
						w.lastConfigHash != "last-accepted-config" || !bytes.Equal(w.oldConfigYaml, previousYAML) {
						t.Fatalf("rejected configuration was published: calls=%d config=%+v", calls, w.config)
					}
					if data, err := os.ReadFile(backup); err != nil || !bytes.Equal(data, good) {
						t.Fatalf("last-good backup changed: data=%q error=%v", data, err)
					}
					if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, bad) {
						t.Fatalf("invalid source file was overwritten: data=%q error=%v", data, err)
					}
					valid := []byte(fmt.Sprintf("port: 8345\nauth-dir: %q\n", authDir))
					if err := os.WriteFile(path, valid, 0o600); err != nil {
						t.Fatal(err)
					}
					if entry == "direct" {
						if !w.reloadConfig() {
							t.Fatal("corrected configuration was rejected")
						}
					} else {
						w.reloadConfigIfChanged()
					}
					if calls != 1 || w.config == nil || w.config.Port != 8345 {
						t.Fatalf("corrected configuration was not published: calls=%d config=%+v", calls, w.config)
					}
				},
			)
		}
	}
}
