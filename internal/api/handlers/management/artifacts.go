package management

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/artifact"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func normalizeImageArtifactCache(raw config.ImageArtifactCache) config.ImageArtifactCache {
	if raw.RetentionDays < 0 {
		raw.RetentionDays = 0
	}
	if raw.MaxTotalSizeMB < 0 {
		raw.MaxTotalSizeMB = 0
	}
	return raw
}

func artifactCacheConfigFromStore(store *artifact.Store, cfg *config.Config) config.ImageArtifactCache {
	if cfg != nil {
		return normalizeImageArtifactCache(cfg.ImageArtifactCache)
	}
	if store == nil {
		return config.ImageArtifactCache{}
	}
	cacheCfg := store.CacheConfig()
	return config.ImageArtifactCache{
		RetentionDays:  int(cacheCfg.Retention.Hours() / 24),
		MaxTotalSizeMB: int(cacheCfg.MaxTotalBytes / (1024 * 1024)),
	}
}

// GetImageArtifactCache returns the image artifact cache cleanup configuration.
func (h *Handler) GetImageArtifactCache(c *gin.Context) {
	cacheConfig := artifactCacheConfigFromStore(h.imageArtifacts, h.configSnapshot())
	c.JSON(
		http.StatusOK, gin.H{
			"retention_days":    cacheConfig.RetentionDays,
			"max_total_size_mb": cacheConfig.MaxTotalSizeMB,
		},
	)
}

// PutImageArtifactCache updates the image artifact cache cleanup configuration.
func (h *Handler) PutImageArtifactCache(c *gin.Context) {
	warnings := beginConfigCommitWarnings(c)
	var body struct {
		RetentionDays  *int `json:"retention_days"`
		MaxTotalSizeMB *int `json:"max_total_size_mb"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if body.RetentionDays != nil && *body.RetentionDays < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "retention_days must be 0 (disable) or a positive integer"})
		return
	}
	if body.MaxTotalSizeMB != nil && *body.MaxTotalSizeMB < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "max_total_size_mb must be 0 (disable) or a positive integer"})
		return
	}

	cacheConfigChanged := false
	committed, ok := h.mutateConfig(
		c, func(next *config.Config) error {
			cacheConfig := normalizeImageArtifactCache(next.ImageArtifactCache)
			previous := cacheConfig
			if body.RetentionDays != nil {
				cacheConfig.RetentionDays = *body.RetentionDays
			}
			if body.MaxTotalSizeMB != nil {
				cacheConfig.MaxTotalSizeMB = *body.MaxTotalSizeMB
			}
			next.ImageArtifactCache = cacheConfig
			cacheConfigChanged = cacheConfig != previous
			return nil
		},
	)
	if !ok {
		return
	}
	cacheConfig := committed.ImageArtifactCache
	if cacheConfigChanged && h.configCommitted == nil && h.imageArtifacts != nil {
		h.imageArtifacts.ApplyCacheConfig(
			artifact.CacheConfigFromLimits(cacheConfig.RetentionDays, cacheConfig.MaxTotalSizeMB),
		)
		if _, err := h.imageArtifacts.Cleanup(context.WithoutCancel(c.Request.Context())); err != nil {
			log.WithError(err).Warn("image artifact cache cleanup failed after config update")
			ReportConfigCommitCleanupError(c.Request.Context(), err)
		}
		h.imageArtifacts.StartCleaner()
	}

	response := gin.H{
		"status":            "ok",
		"retention_days":    cacheConfig.RetentionDays,
		"max_total_size_mb": cacheConfig.MaxTotalSizeMB,
	}
	if cleanupError := warnings.cleanupErrorMessage(); cleanupError != "" {
		response["cleanup_error"] = cleanupError
	}
	c.JSON(http.StatusOK, response)
}

// GetImageArtifactCacheSize returns the current image artifact cache disk usage.
func (h *Handler) GetImageArtifactCacheSize(c *gin.Context) {
	if h == nil || h.imageArtifacts == nil {
		c.JSON(http.StatusOK, gin.H{"total_bytes": 0, "file_count": 0})
		return
	}
	stats, err := h.imageArtifacts.Stats()
	if err != nil {
		c.JSON(
			http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to read image artifact cache: %v", err)},
		)
		return
	}
	c.JSON(http.StatusOK, gin.H{"total_bytes": stats.TotalBytes, "file_count": stats.FileCount})
}
