// Last compiled: 2026-05-07
// Author: pyro

package management

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
)

func TestFindReleaseAssets(t *testing.T) {
	assets := []githubReleaseAsset{
		{Name: "CLIProxyAPI_linux_amd64.tar.gz", BrowserDownloadURL: "https://github.com/example/archive"},
		{Name: "checksums.txt", BrowserDownloadURL: "https://github.com/example/checksums"},
	}

	archiveAsset, checksumAsset := findReleaseAssets(assets, "CLIProxyAPI_linux_amd64.tar.gz")
	if archiveAsset == nil || archiveAsset.Name != "CLIProxyAPI_linux_amd64.tar.gz" {
		t.Fatalf("archive asset = %#v", archiveAsset)
	}
	if checksumAsset == nil || checksumAsset.Name != "checksums.txt" {
		t.Fatalf("checksum asset = %#v", checksumAsset)
	}
}

func TestExtractBinaryFromTarGz(t *testing.T) {
	var archive bytes.Buffer
	gzWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzWriter)
	payload := []byte("binary-data")
	if err := tarWriter.WriteHeader(
		&tar.Header{
			Name: "CLIProxyAPI_linux_amd64/cli-proxy-api", Mode: 0o755, Size: int64(len(payload)),
		},
	); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tarWriter.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzWriter.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	got, err := extractBinaryFromArchive("CLIProxyAPI_linux_amd64.tar.gz", archive.Bytes())
	if err != nil {
		t.Fatalf("extractBinaryFromArchive: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %q want %q", got, payload)
	}
}

func TestBuildUpdateCompatibility_WithReadySQLitePersister(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{UsageDataDir: dir}
	persister := usage.NewPersister(dir, config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	defer persister.Stop()

	h := NewHandler(cfg, filepath.Join(dir, "config.yaml"), nil, WithUsagePersister(persister))
	compat := h.buildUpdateCompatibility(context.Background(), "v9.9.9")

	if !compat.Compatible {
		t.Fatalf("compatibility unexpectedly false: %#v", compat)
	}
	if !compat.Usage.PersisterReady {
		t.Fatal("expected persister_ready=true")
	}
	if compat.Usage.SchemaVersion != "2" {
		t.Fatalf("schema_version = %q, want 2", compat.Usage.SchemaVersion)
	}
	if compat.Usage.DBPath == "" {
		t.Fatal("expected db_path to be populated")
	}
}

func TestBuildUpdateCompatibility_DBExistsWithoutPersister(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "events.db")
	if err := os.WriteFile(dbPath, []byte("stub"), 0o600); err != nil {
		t.Fatalf("write db stub: %v", err)
	}

	h := NewHandler(&config.Config{UsageDataDir: dir}, filepath.Join(dir, "config.yaml"), nil)
	compat := h.buildUpdateCompatibility(context.Background(), "v9.9.9")

	if compat.Compatible {
		t.Fatalf("expected incompatible when db exists without persister: %#v", compat)
	}
	if !compat.Usage.DBExists {
		t.Fatal("expected db_exists=true")
	}
}

func TestValidateGitHubDownloadURL(t *testing.T) {
	if err := validateGitHubDownloadURL(
		"archive", "https://github.com/Pyrokine/CLIProxyAPI/releases/download/v1/a.tar.gz",
	); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if err := validateGitHubDownloadURL("archive", "https://example.com/a.tar.gz"); err == nil {
		t.Fatal("expected non-GitHub host to fail validation")
	}
}

func TestBuildUpdateCompatibility_ReportsMigrationMarker(t *testing.T) {
	dir := t.TempDir()
	persister := usage.NewPersister(dir, config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	defer persister.Stop()

	h := NewHandler(
		&config.Config{UsageDataDir: dir}, filepath.Join(dir, "config.yaml"), nil, WithUsagePersister(persister),
	)
	compat := h.buildUpdateCompatibility(context.Background(), "v9.9.9")
	if compat.Usage.MigratedFrom != "v1+v2-json" {
		t.Fatalf("migrated_from = %q, want v1+v2-json", compat.Usage.MigratedFrom)
	}
	if compat.Usage.MigratedAt == nil {
		t.Fatal("expected migrated_at to be populated")
	}
}
