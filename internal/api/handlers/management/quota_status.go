package management

import (
	"net/http"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/gin-gonic/gin"
)

// GetQuotaStatus returns the cached quota status for all credentials.
func (h *Handler) GetQuotaStatus(c *gin.Context) {
	if h.quotaScheduler == nil {
		c.JSON(
			http.StatusOK, quota.StatusResponse{
				Credentials: make(map[string]*quota.Entry),
			},
		)
		return
	}
	c.JSON(http.StatusOK, h.quotaScheduler.GetStatus())
}

// PostQuotaRefresh triggers an immediate quota refresh asynchronously.
func (h *Handler) PostQuotaRefresh(c *gin.Context) {
	if h.quotaScheduler == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "quota scheduler not configured"})
		return
	}

	var body struct {
		Credentials []string `json:"credentials"`
	}
	_ = c.ShouldBindJSON(&body) // optional body

	go h.quotaScheduler.RefreshNow(body.Credentials)
	c.JSON(http.StatusAccepted, gin.H{"status": "refresh_started"})
}

// GetQuotaConfig returns the current quota scheduler configuration.
func (h *Handler) GetQuotaConfig(c *gin.Context) {
	if h.quotaScheduler == nil {
		c.JSON(http.StatusOK, quota.DefaultConfig())
		return
	}
	c.JSON(http.StatusOK, h.quotaScheduler.GetConfig())
}

// PutQuotaConfig updates the quota scheduler configuration.
// Uses merge semantics: unspecified fields retain their current values.
func (h *Handler) PutQuotaConfig(c *gin.Context) {
	if h.quotaScheduler == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "quota scheduler not configured"})
		return
	}

	// Start from current config for merge semantics
	cfg := h.quotaScheduler.GetConfig()
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}

	// Validate interval bounds
	if cfg.Enabled {
		if cfg.Interval < 60 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "interval must be at least 60 seconds"})
			return
		}
		if cfg.MaxInterval < 300 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "max-interval must be at least 300 seconds"})
			return
		}
	}
	if cfg.MaxInterval > 0 && cfg.MaxInterval < cfg.Interval {
		c.JSON(http.StatusBadRequest, gin.H{"error": "max-interval must be greater than or equal to interval"})
		return
	}

	h.quotaScheduler.UpdateConfig(cfg)
	c.JSON(http.StatusOK, h.quotaScheduler.GetConfig())
}
