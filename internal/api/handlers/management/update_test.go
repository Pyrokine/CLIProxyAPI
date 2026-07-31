package management

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
)

func TestFindReleaseAssets(t *testing.T) {
	assets := []githubReleaseAsset{
		{
			Name:               "CLIProxyAPI_6.10.9-augmented_linux_amd64.tar.gz",
			BrowserDownloadURL: "https://github.com/example/archive",
		},
		{Name: "checksums.txt", BrowserDownloadURL: "https://github.com/example/checksums"},
	}

	archiveAsset, checksumAsset := findReleaseAssets(
		assets, []string{"CLIProxyAPI_6.10.9-augmented_linux_amd64.tar.gz", "CLIProxyAPI_linux_amd64.tar.gz"},
	)
	if archiveAsset == nil || archiveAsset.Name != "CLIProxyAPI_6.10.9-augmented_linux_amd64.tar.gz" {
		t.Fatalf("archive asset = %#v", archiveAsset)
	}
	if checksumAsset == nil || checksumAsset.Name != "checksums.txt" {
		t.Fatalf("checksum asset = %#v", checksumAsset)
	}
}

func TestExpectedArchiveAssetNames(t *testing.T) {
	names := expectedArchiveAssetNames("v6.10.9-augmented")
	if len(names) != 2 {
		t.Fatalf("len(names) = %d, want 2", len(names))
	}
	if names[0] != "CLIProxyAPI_6.10.9-augmented_linux_amd64.tar.gz" {
		t.Fatalf("names[0] = %q", names[0])
	}
	if names[1] != "CLIProxyAPI_linux_amd64.tar.gz" {
		t.Fatalf("names[1] = %q", names[1])
	}
}

func TestCompareUpdateVersions(t *testing.T) {
	tests := []struct {
		name           string
		left           string
		right          string
		wantOrder      int
		wantComparable bool
	}{
		{name: "plain older", left: "v7.2.49", right: "v7.2.50", wantOrder: -1, wantComparable: true},
		{name: "plain equal", left: "v7.2.50", right: "v7.2.50", wantComparable: true},
		{name: "augmented after plain", left: "v7.2.50-aug.1", right: "v7.2.50", wantOrder: 1, wantComparable: true},
		{name: "augmentation zero after plain", left: "v7.2.50-aug.0", right: "v7.2.50", wantOrder: 1, wantComparable: true},
		{name: "augmentation revision", left: "v7.2.50-aug.1", right: "v7.2.50-aug.2", wantOrder: -1, wantComparable: true},
		{name: "new base after augmentation", left: "v7.2.51", right: "v7.2.50-aug.9", wantOrder: 1, wantComparable: true},
		{name: "unprefixed versions", left: "7.2.51", right: "7.2.50", wantOrder: 1, wantComparable: true},
		{name: "development version", left: "v7.2.51", right: "dev"},
		{name: "unsupported suffix", left: "v7.2.51-rc.1", right: "v7.2.50"},
		{
			name:  "overflowing component",
			left:  "v18446744073709551616.0.0",
			right: "v7.2.50",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotOrder, gotComparable := compareUpdateVersions(test.left, test.right)
			if gotComparable != test.wantComparable {
				t.Fatalf("comparable = %t, want %t", gotComparable, test.wantComparable)
			}
			if gotOrder != test.wantOrder {
				t.Fatalf("order = %d, want %d", gotOrder, test.wantOrder)
			}
		})
	}
}

func TestBuildUpdateCompatibility_RejectsComparableDowngrade(t *testing.T) {
	oldVersion := buildinfo.Version
	t.Cleanup(func() {
		buildinfo.Version = oldVersion
	})
	buildinfo.Version = "v7.2.50-aug.1"

	var h *Handler
	compat := h.buildUpdateCompatibility(context.Background(), "v7.2.50")
	if compat.Compatible {
		t.Fatalf("downgrade compatibility unexpectedly true: %#v", compat)
	}
	if len(compat.Warnings) != 1 {
		t.Fatalf("warnings = %#v, want one downgrade warning", compat.Warnings)
	}
}

func TestBuildUpdateCompatibility_AllowsNewerAndUncomparableTargets(t *testing.T) {
	oldVersion := buildinfo.Version
	t.Cleanup(func() {
		buildinfo.Version = oldVersion
	})
	buildinfo.Version = "v7.2.50-aug.1"

	var h *Handler
	for _, target := range []string{"v7.2.51", "dev"} {
		compat := h.buildUpdateCompatibility(context.Background(), target)
		if !compat.Compatible {
			t.Fatalf("target %q compatibility unexpectedly false: %#v", target, compat)
		}
		if len(compat.Warnings) != 0 {
			t.Fatalf("target %q warnings = %#v, want none", target, compat.Warnings)
		}
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
	if compat.Usage.SchemaVersion != usage.CurrentSchemaVersion {
		t.Fatalf(
			"schema_version = %q, want %s",
			compat.Usage.SchemaVersion,
			usage.CurrentSchemaVersion,
		)
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

func TestUpdateHTTPClientRejectsNonHTTPSRedirect(t *testing.T) {
	client := newUpdateHTTPClient("")
	if client.CheckRedirect == nil {
		t.Fatal("CheckRedirect is nil")
	}
	req, err := http.NewRequest(http.MethodGet, "http://github.com/Pyrokine/CLIProxyAPI/releases", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if err := client.CheckRedirect(req, nil); err == nil {
		t.Fatal("expected non-HTTPS redirect to be rejected")
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
