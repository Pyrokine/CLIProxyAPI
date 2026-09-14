package management

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/artifact"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

const managementTestPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/p9sAAAAASUVORK5CYII="

func managementTestPNGBytes(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(managementTestPNGBase64)
	if err != nil {
		t.Fatalf("DecodeString test PNG: %v", err)
	}
	return data
}

func largeManagementTestPNGBytes(t *testing.T, fill byte) []byte {
	t.Helper()
	data := append([]byte(nil), managementTestPNGBytes(t)...)
	data = append(data, bytes.Repeat([]byte{fill}, 700*1024)...)
	return data
}

func newArtifactCacheTestContext(method string, path string, body string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, path, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

func TestGetImageArtifactCacheUsesConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.ImageArtifactCache.RetentionDays = 3
	cfg.ImageArtifactCache.MaxTotalSizeMB = 128
	store, err := artifact.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	store.ApplyCacheConfig(artifact.CacheConfigFromLimits(9, 256))
	h := NewHandler(cfg, "", nil, withEnvSecret(""), WithImageArtifacts(store))

	c, rec := newArtifactCacheTestContext(http.MethodGet, "/image-artifact-cache", "")
	h.GetImageArtifactCache(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		RetentionDays  int `json:"retention_days"`
		MaxTotalSizeMB int `json:"max_total_size_mb"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if body.RetentionDays != 3 || body.MaxTotalSizeMB != 128 {
		t.Fatalf("body = %#v, want config values", body)
	}
}

func TestPutImageArtifactCacheUpdatesConfigStoreAndCleans(t *testing.T) {
	root := t.TempDir()
	store, err := artifact.NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	record1, err := store.PutBytes(context.Background(), largeManagementTestPNGBytes(t, 1), "image/png")
	if err != nil {
		t.Fatalf("PutBytes record1: %v", err)
	}
	record2, err := store.PutBytes(context.Background(), largeManagementTestPNGBytes(t, 2), "image/png")
	if err != nil {
		t.Fatalf("PutBytes record2: %v", err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	newTime := time.Now()
	if err = os.Chtimes(filepath.Join(root, record1.ID), oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes record1: %v", err)
	}
	if err = os.Chtimes(filepath.Join(root, record2.ID), newTime, newTime); err != nil {
		t.Fatalf("Chtimes record2: %v", err)
	}

	cfg := &config.Config{}
	cfg.ImageArtifactCache.RetentionDays = 7
	cfg.ImageArtifactCache.MaxTotalSizeMB = 10240
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	h := NewHandler(cfg, configPath, nil, withEnvSecret(""), WithImageArtifacts(store))

	c, rec := newArtifactCacheTestContext(
		http.MethodPut,
		"/image-artifact-cache",
		`{"retention_days":5,"max_total_size_mb":1}`,
	)
	ctx, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(ctx)
	h.PutImageArtifactCache(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	committed := h.configSnapshot()
	if committed == nil || committed.ImageArtifactCache.RetentionDays != 5 || committed.ImageArtifactCache.MaxTotalSizeMB != 1 {
		t.Fatalf("committed config cache = %#v", committed)
	}
	if cfg.ImageArtifactCache.RetentionDays != 7 || cfg.ImageArtifactCache.MaxTotalSizeMB != 10240 {
		t.Fatalf("original config changed: %#v", cfg.ImageArtifactCache)
	}
	cacheCfg := store.CacheConfig()
	if cacheCfg.Retention != 5*24*time.Hour || cacheCfg.MaxTotalBytes != 1024*1024 {
		t.Fatalf("store cache config = %#v", cacheCfg)
	}
	if _, err = os.Stat(filepath.Join(root, record1.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record1 stat err = %v, want os.ErrNotExist", err)
	}
	if _, err = os.Stat(filepath.Join(root, record2.ID)); err != nil {
		t.Fatalf("record2 should remain: %v", err)
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.FileCount != 1 {
		t.Fatalf("stats = %#v, want one artifact after cleanup", stats)
	}
}

func TestPutImageArtifactCacheDelegatesRuntimeUpdateToCommitCallback(t *testing.T) {
	store, err := artifact.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}

	callbackCount := 0
	callbackStoreConfig := artifact.CacheConfigFromLimits(11, 512)
	h := NewHandler(
		&config.Config{},
		configPath,
		nil,
		withEnvSecret(""),
		WithImageArtifacts(store),
		WithConfigCommitted(
			func(context.Context, ConfigCommit) error {
				callbackCount++
				store.ApplyCacheConfig(callbackStoreConfig)
				return nil
			},
		),
	)

	c, rec := newArtifactCacheTestContext(
		http.MethodPut,
		"/image-artifact-cache",
		`{"retention_days":5,"max_total_size_mb":1}`,
	)
	h.PutImageArtifactCache(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if callbackCount != 1 {
		t.Fatalf("config commit callback count = %d, want 1", callbackCount)
	}
	if got := store.CacheConfig(); got != callbackStoreConfig {
		t.Fatalf("store cache config = %#v, want callback config %#v", got, callbackStoreConfig)
	}
}

func TestPutImageArtifactCacheDoesNotCleanWhenValuesAreUnchanged(t *testing.T) {
	root := filepath.Join(t.TempDir(), "images")
	store, err := artifact.NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err = os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile root blocker: %v", err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(
		configPath, []byte("image-artifact-cache:\n  retention-days: 5\n  max-total-size-mb: 1\n"), 0o600,
	); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	h := NewHandler(
		&config.Config{ImageArtifactCache: config.ImageArtifactCache{RetentionDays: 5, MaxTotalSizeMB: 1}},
		configPath,
		nil,
		withEnvSecret(""),
		WithImageArtifacts(store),
	)

	c, rec := newArtifactCacheTestContext(
		http.MethodPut,
		"/image-artifact-cache",
		`{"retention_days":5,"max_total_size_mb":1}`,
	)
	h.PutImageArtifactCache(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "cleanup_error") {
		t.Fatalf("unchanged cache config attempted cleanup: %s", rec.Body.String())
	}
}

func TestPutImageArtifactCacheSaveFailureKeepsStoreConfig(t *testing.T) {
	root := t.TempDir()
	store, err := artifact.NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	previousStoreConfig := artifact.CacheConfigFromLimits(7, 1024)
	store.ApplyCacheConfig(previousStoreConfig)

	previousConfig := config.ImageArtifactCache{RetentionDays: 7, MaxTotalSizeMB: 1024}
	cfg := &config.Config{ImageArtifactCache: previousConfig}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(configPath, []byte("image-artifact-cache: [\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	h := NewHandler(cfg, configPath, nil, withEnvSecret(""), WithImageArtifacts(store))

	c, rec := newArtifactCacheTestContext(
		http.MethodPut,
		"/image-artifact-cache",
		`{"retention_days":5,"max_total_size_mb":1}`,
	)
	h.PutImageArtifactCache(c)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", rec.Code, rec.Body.String())
	}
	if cfg.ImageArtifactCache != previousConfig {
		t.Fatalf("original config changed: %#v", cfg.ImageArtifactCache)
	}
	committed := h.configSnapshot()
	if committed == nil || committed.ImageArtifactCache != previousConfig {
		t.Fatalf("handler config = %#v, want %#v", committed, previousConfig)
	}
	if got := store.CacheConfig(); got != previousStoreConfig {
		t.Fatalf("store cache config = %#v, want %#v", got, previousStoreConfig)
	}
}

func TestPutImageArtifactCacheRejectsNegativeValues(t *testing.T) {
	cfg := &config.Config{}
	cfg.ImageArtifactCache.RetentionDays = 7
	cfg.ImageArtifactCache.MaxTotalSizeMB = 10240
	h := NewHandler(cfg, "", nil, withEnvSecret(""))

	c, rec := newArtifactCacheTestContext(
		http.MethodPut,
		"/image-artifact-cache",
		`{"retention_days":-1,"max_total_size_mb":1}`,
	)
	h.PutImageArtifactCache(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if cfg.ImageArtifactCache.RetentionDays != 7 || cfg.ImageArtifactCache.MaxTotalSizeMB != 10240 {
		t.Fatalf("config changed after invalid request: %#v", cfg.ImageArtifactCache)
	}
}

func TestGetImageArtifactCacheSize(t *testing.T) {
	nilHandler := NewHandler(&config.Config{}, "", nil, withEnvSecret(""))
	c, rec := newArtifactCacheTestContext(http.MethodGet, "/image-artifact-cache/size", "")
	nilHandler.GetImageArtifactCacheSize(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("nil store status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var nilBody struct {
		TotalBytes int64 `json:"total_bytes"`
		FileCount  int   `json:"file_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &nilBody); err != nil {
		t.Fatalf("Unmarshal nil store body: %v", err)
	}
	if nilBody.TotalBytes != 0 || nilBody.FileCount != 0 {
		t.Fatalf("nil store body = %#v, want zero size", nilBody)
	}

	store, err := artifact.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	record, err := store.PutBytes(context.Background(), managementTestPNGBytes(t), "image/png")
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	h := NewHandler(&config.Config{}, "", nil, withEnvSecret(""), WithImageArtifacts(store))
	c, rec = newArtifactCacheTestContext(http.MethodGet, "/image-artifact-cache/size", "")
	h.GetImageArtifactCacheSize(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		TotalBytes int64 `json:"total_bytes"`
		FileCount  int   `json:"file_count"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if body.TotalBytes != record.Size || body.FileCount != 1 {
		t.Fatalf("body = %#v, want size %d and one file", body, record.Size)
	}
}
