package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestPortValidationUsesBothConfigEntrypoints(t *testing.T) {
	for _, port := range []int{-1, 0, 1, 8317, 65535, 65536, 70000} {
		t.Run(
			strconv.Itoa(port), func(t *testing.T) {
				data := []byte(fmt.Sprintf("port: %d\n", port))
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				for _, entry := range []struct {
					name string
					load func() (*Config, error)
				}{
					{"bytes", func() (*Config, error) { return ParseConfigBytes(data) }},
					{"file", func() (*Config, error) { return LoadConfig(path) }},
				} {
					t.Run(
						entry.name, func(t *testing.T) {
							cfg, err := entry.load()
							if port < 0 || port > 65535 {
								if err == nil || cfg != nil || !strings.Contains(err.Error(), "out of range") {
									t.Fatalf("invalid port accepted: cfg=%+v error=%v", cfg, err)
								}
								return
							}
							if err != nil || cfg == nil || cfg.Port != port {
								t.Fatalf("valid port changed: cfg=%+v error=%v", cfg, err)
							}
						},
					)
				}
			},
		)
	}
}

func TestLoadWithFallbackReportsBackupSaveFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := LastGoodPath(path)
	if err := os.Mkdir(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(backup, "sentinel")
	if err := os.WriteFile(sentinel, []byte("retained backup state"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previous := log.StandardLogger().Out
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	cfg, actualPath, fallback, err := LoadWithFallback(path)
	if err != nil || fallback || actualPath != path || cfg == nil || cfg.Port != 8317 {
		t.Fatalf(
			"valid primary config was rejected: cfg=%+v path=%q fallback=%v error=%v", cfg, actualPath, fallback, err,
		)
	}
	if !strings.Contains(output.String(), "failed to save last-good config") {
		t.Fatalf("backup failure was not reported: %s", output.String())
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "retained backup state" {
		t.Fatalf("existing backup state changed: data=%q error=%v", data, err)
	}
}
