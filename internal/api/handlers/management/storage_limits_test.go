package management

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/artifact"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
)

func TestStorageLimitRequestsPreservePublicationAndReadBackSavedLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte(("port: 8317\nlogs-max-total-size-mb: 64\nusage-retention:\n  days: 30\n" +
		"  max-db-size-mb: 256\nimage-artifact-cache:\n  retention-days: 7\n  m" +
		"ax-total-size-mb: 128\n"))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.ApplyCacheConfig(artifact.CacheConfigFromLimits(7, 128))
	if _, err := store.PutBytes(context.Background(), managementTestPNGBytes(t), "image/png"); err != nil {
		t.Fatal(err)
	}
	initialStats, err := store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	p := usage.NewPersister(t.TempDir(), cfg.UsageRetention)
	initialRetention := p.Retention()
	h := NewHandler(cfg, path, nil, withEnvSecret(""), WithImageArtifacts(store))
	h.usagePersister = p
	etag := func() string {
		c, rec := newArtifactCacheTestContext(http.MethodGet, "/config.yaml", "")
		h.GetConfigYAML(c)
		if rec.Code != 200 || rec.Header().Get("ETag") == "" {
			t.Fatalf("yaml GET=%d %s", rec.Code, rec.Body.String())
		}
		return rec.Header().Get("ETag")
	}
	initialETag := etag()
	for _, item := range []struct{ name, body string }{
		{"usage", fmt.Sprintf(`{"max_db_size_mb":%d}`, config.MaxStorageSizeMB+1)},
		{"logs", fmt.Sprintf(`{"value":%d}`, config.MaxStorageSizeMB+1)},
		{"image", fmt.Sprintf(`{"max_total_size_mb":%d}`, config.MaxStorageSizeMB+1)},
		{"image", fmt.Sprintf(`{"retention_days":%d}`, config.MaxImageArtifactRetentionDays+1)},
	} {
		c, rec := newArtifactCacheTestContext(http.MethodPut, "/settings", item.body)
		switch item.name {
		case "usage":
			h.PutUsageRetention(c)
		case "logs":
			h.PutLogsMaxTotalSizeMB(c)
		default:
			h.PutImageArtifactCache(c)
		}
		if rec.Code != 400 {
			t.Fatalf("%s status=%d body=%s", item.name, rec.Code, rec.Body.String())
		}
		saved, err := os.ReadFile(path)
		if err != nil || string(saved) != string(raw) || etag() != initialETag {
			t.Fatalf("%s rejected update changed saved config or ETag", item.name)
		}
		if h.configSnapshot().ImageArtifactCache != cfg.ImageArtifactCache ||
			h.configSnapshot().LogsMaxTotalSizeMB != cfg.LogsMaxTotalSizeMB ||
			h.configSnapshot().UsageRetention != cfg.UsageRetention ||
			p.Retention() != initialRetention ||
			store.CacheConfig() != artifact.CacheConfigFromLimits(
				7, 128,
			) {
			t.Fatalf("%s rejected update published runtime state", item.name)
		}
		stats, err := store.Stats()
		if err != nil || stats != initialStats {
			t.Fatalf("%s rejected update changed artifact stats", item.name)
		}
	}
	c, rec := newArtifactCacheTestContext(
		http.MethodPut, "/image-artifact-cache", fmt.Sprintf(
			`{"retention_days":%d,"max_total_size_mb":%d}`, config.MaxImageArtifactRetentionDays,
			config.MaxImageArtifactCacheSizeMB,
		),
	)
	h.PutImageArtifactCache(c)
	if rec.Code != 200 {
		t.Fatalf("image max boundary=%d %s", rec.Code, rec.Body.String())
	}
	loaded, err := config.LoadConfig(path)
	if err != nil || loaded.ImageArtifactCache.RetentionDays != config.MaxImageArtifactRetentionDays ||
		loaded.ImageArtifactCache.MaxTotalSizeMB != config.MaxStorageSizeMB {
		t.Fatalf("saved image readback err=%v", err)
	}
	if etag() == initialETag || store.CacheConfig() != artifact.CacheConfigFromLimits(
		config.MaxImageArtifactRetentionDays, config.MaxStorageSizeMB,
	) {
		t.Fatal("valid image update was not published")
	}
	stats, err := store.Stats()
	if err != nil || stats != initialStats {
		t.Fatalf("valid cache update changed retained stats: %v", err)
	}
	c, rec = newArtifactCacheTestContext(http.MethodGet, "/image-artifact-cache/size", "")
	h.GetImageArtifactCacheSize(c)
	if rec.Code != 200 {
		t.Fatalf("stats GET=%d", rec.Code)
	}
	for _, size := range []int{0, config.MaxStorageSizeMB} {
		c, rec = newArtifactCacheTestContext(
			http.MethodPut, "/usage/settings", fmt.Sprintf(`{"max_db_size_mb":%d}`, size),
		)
		h.PutUsageRetention(c)
		if rec.Code != 200 {
			t.Fatalf("usage boundary=%d %s", rec.Code, rec.Body.String())
		}
		c, rec = newArtifactCacheTestContext(
			http.MethodPut, "/logs-max-total-size-mb", fmt.Sprintf(`{"value":%d}`, size),
		)
		h.PutLogsMaxTotalSizeMB(c)
		if rec.Code != 200 {
			t.Fatalf("logs boundary=%d %s", rec.Code, rec.Body.String())
		}
		loaded, err = config.LoadConfig(path)
		if err != nil || loaded.UsageRetention.MaxDBSizeMB != size || loaded.LogsMaxTotalSizeMB != size ||
			p.Retention().MaxDBSizeMB != size {
			t.Fatalf("boundary readback=%d err=%v", size, err)
		}
	}
	c, rec = newArtifactCacheTestContext(http.MethodPut, "/logs-max-total-size-mb", `{"value":-1}`)
	h.PutLogsMaxTotalSizeMB(c)
	if rec.Code != 200 || h.configSnapshot().LogsMaxTotalSizeMB != 0 {
		t.Fatalf("negative logs legacy=%d", rec.Code)
	}
}
