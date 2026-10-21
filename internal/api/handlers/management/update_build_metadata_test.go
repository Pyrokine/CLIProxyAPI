package management

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
)

func TestUpdateGuardSessionBuildMetadata(t *testing.T) {
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	executable := filepath.Join(dir, "cli-proxy-api")
	host := updateGuardHost{
		ExecutablePath: executable,
		StateDBPath:    filepath.Join(dir, "management-state.db"),
		ServiceUnit:    defaultUpdateServiceUnit,
		HealthBaseURL:  "http://127.0.0.1:8317",
	}
	for _, test := range []struct {
		previous string
		target   string
		valid    bool
	}{
		{"v7.2.67-aug.3", "v7.2.127-aug.1", true},
		{"v7.2.67-aug.3+dirty.8f1a76e34d62", "v7.2.127-aug.1", true},
		{"v7.2.67+linux-amd64.123", "v7.2.127-aug.1", true},
		{"dev", "v7.2.127-aug.1", true},
		{"v7.2.67+", "v7.2.127-aug.1", false},
		{"v7.2.67+dirty..hash", "v7.2.127-aug.1", false},
		{"v7.2.67+dirty/hash", "v7.2.127-aug.1", false},
		{"v7.2.67+dirty_hash", "v7.2.127-aug.1", false},
		{"v7.2.67+dirty+hash", "v7.2.127-aug.1", false},
		{"v7.2.67+dirty\n", "v7.2.127-aug.1", false},
		{"v7.2.67", "v7.2.127-aug.1+dirty.123", false},
	} {
		t.Run(test.previous+"->"+test.target, func(t *testing.T) {
			session := newUpdateGuardSession(host, "0123456789abcdef0123456789abcdef", test.target, false, now)
			session.PreviousVersion = test.previous
			err := validateUpdateGuardSession(session, host.StateDBPath, executable, false, now)
			if (err == nil) != test.valid {
				t.Fatalf("validate session: %v, want valid=%t", err, test.valid)
			}
		})
	}
}

func TestBeginPersistentUpdatePreservesBuildMetadata(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd host validation requires Linux")
	}
	binDir := t.TempDir()
	for _, name := range []string{"systemctl", "systemd-run"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 91\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	t.Setenv("INVOCATION_ID", "test-update-invocation")
	previous := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = previous })
	buildinfo.Version = "v7.2.67-aug.3+dirty.8f1a76e34d62"
	store := openUpdateGuardTestStore(t)
	h := &Handler{stateStore: store, inferenceAdmission: executionregistry.NewAdmission()}
	session, err := h.beginPersistentUpdate(context.Background(), &config.Config{Port: 8317}, "v7.2.127-aug.1", false, "")
	if err != nil {
		t.Fatalf("begin persistent update: %v", err)
	}
	stored, err := store.loadUpdateSession(context.Background())
	if err != nil || stored == nil {
		t.Fatalf("load session: %#v, %v", stored, err)
	}
	if session.PreviousVersion != buildinfo.Version || stored.PreviousVersion != buildinfo.Version {
		t.Fatalf("rollback identity changed: created=%q stored=%q", session.PreviousVersion, stored.PreviousVersion)
	}
	if stored.Phase != updatePhaseDownloading || stored.TargetVersion != "v7.2.127-aug.1" {
		t.Fatalf("stored session: %#v", stored)
	}
}

func TestBuildMetadataDoesNotBypassDowngradeProtection(t *testing.T) {
	previous := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = previous })
	buildinfo.Version = "v7.2.67-aug.3+dirty.8f1a76e34d62"
	var h *Handler
	compat := h.buildUpdateCompatibility(context.Background(), "v7.2.67-aug.2")
	if compat.Compatible || len(compat.Warnings) == 0 {
		t.Fatalf("dirty-build downgrade accepted: %#v", compat)
	}
}

func TestCompareUpdateVersionsBuildMetadata(t *testing.T) {
	for _, test := range []struct {
		left       string
		right      string
		order      int
		comparable bool
	}{
		{"v7.2.127-aug.1", "v7.2.67-aug.3+dirty.8f1a76e34d62", 1, true},
		{"v7.2.67-aug.2", "v7.2.67-aug.3+dirty.8f1a76e34d62", -1, true},
		{"v7.2.127-aug.1+build.20260915", "v7.2.127-aug.1+dirty.123", 0, true},
		{"v7.2.127-aug.1", "v7.2.127-aug.1+linux-amd64.123", 0, true},
		{"v7.2.127", "v7.2.127+", 0, false},
		{"v7.2.127", "v7.2.127+dirty..hash", 0, false},
		{"v7.2.127", "v7.2.127+dirty/hash", 0, false},
	} {
		t.Run(test.left+"/"+test.right, func(t *testing.T) {
			order, comparable := compareUpdateVersions(test.left, test.right)
			if order != test.order || comparable != test.comparable {
				t.Fatalf("comparison=(%d,%t), want (%d,%t)", order, comparable, test.order, test.comparable)
			}
		})
	}
}
