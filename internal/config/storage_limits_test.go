package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestStorageSizeLimitsRejectOverflowBeforeConfigPublication(t *testing.T) {
	for _, key := range []string{
		"logs-max-total-size-mb", "usage-retention:\n  max-db-size-mb", "image-artifact-cache:\n  max-total-size-mb",
	} {
		t.Run(
			key, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				original := []byte("port: 8317\n")
				if err := os.WriteFile(path, original, 0o600); err != nil {
					t.Fatal(err)
				}
				oversized := []byte(fmt.Sprintf("port: 8317\n%s: %d\n", key, MaxStorageSizeMB+1))
				if _, err := ParseConfigBytes(oversized); err == nil {
					t.Fatal("ParseConfigBytes accepted overflow")
				}
				if err := os.WriteFile(path, oversized, 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := LoadConfig(path); err == nil {
					t.Fatal("LoadConfig accepted overflow")
				}
				if err := os.WriteFile(path, original, 0o600); err != nil {
					t.Fatal(err)
				}
				cfg := &Config{Port: 8317}
				switch key {
				case "logs-max-total-size-mb":
					cfg.LogsMaxTotalSizeMB = MaxStorageSizeMB + 1
				case "usage-retention:\n  max-db-size-mb":
					cfg.UsageRetention.MaxDBSizeMB = MaxStorageSizeMB + 1
				default:
					cfg.ImageArtifactCache.MaxTotalSizeMB = MaxStorageSizeMB + 1
				}
				if err := SaveConfigPreserveComments(path, cfg); err == nil {
					t.Fatal("save accepted overflow")
				}
				saved, err := os.ReadFile(path)
				if err != nil || string(saved) != string(original) {
					t.Fatalf("rejected save changed config: %v", err)
				}
			},
		)
	}
	for _, size := range []int{-1, 0, MaxStorageSizeMB} {
		raw := []byte(fmt.Sprintf(
			"port: 8317\nlogs-max-total-size-mb: %d\nusage-retention:\n  max-db-size-mb: %d\n", size, size,
		))
		cfg, err := ParseConfigBytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.LogsMaxTotalSizeMB != max(size, 0) || cfg.UsageRetention.MaxDBSizeMB != size {
			t.Fatalf(
				"normalized size=%d logs=%d usage=%d", size, cfg.LogsMaxTotalSizeMB, cfg.UsageRetention.MaxDBSizeMB,
			)
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := SaveConfigPreserveComments(path, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfig(path)
		if err != nil || loaded.LogsMaxTotalSizeMB != cfg.LogsMaxTotalSizeMB ||
			loaded.UsageRetention.MaxDBSizeMB != size {
			t.Fatalf("readback err=%v", err)
		}
	}
}
