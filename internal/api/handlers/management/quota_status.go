package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

// GetQuotaStatus returns the cached quota status for all credentials.
func (h *Handler) GetQuotaStatus(c *gin.Context) {
	if h.quotaScheduler == nil {
		c.JSON(
			http.StatusOK, quota.StatusResponse{
				Credentials: make(map[string]*quota.Entry),
				ByProvider:  make(map[string]quota.ProviderStatusSummary),
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
	if err := c.ShouldBindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	credentials := append([]string(nil), body.Credentials...)
	go h.quotaScheduler.RefreshNowWithFinalizer(credentials, h.finalizeManualQuotaRefresh)
	c.JSON(http.StatusAccepted, gin.H{"status": "refresh_started"})
}

func (h *Handler) finalizeManualQuotaRefresh(attempt quota.RefreshAttempt) error {
	if attempt.Type != quota.TypeCodex {
		return nil
	}
	available, known := codexQuotaAvailability(attempt.Data)
	if !known || !available {
		return nil
	}
	auth := h.authByIndex(attempt.AuthIndex)
	if auth == nil {
		return fmt.Errorf("auth index %q is not available", attempt.AuthIndex)
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return fmt.Errorf("auth index %q does not reference a Codex credential", attempt.AuthIndex)
	}
	if !authHasQuotaState(auth) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	updated, _, err := h.authManager.ResetQuota(ctx, auth.ID)
	if err != nil {
		return err
	}
	if updated == nil {
		return fmt.Errorf("Codex credential %q was removed during quota recovery", attempt.AuthIndex)
	}
	return nil
}

func authHasQuotaState(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Quota.Exceeded {
		return true
	}
	for _, state := range auth.ModelStates {
		if state != nil && state.Quota.Exceeded {
			return true
		}
	}
	return false
}

func codexQuotaAvailability(data []byte) (bool, bool) {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return false, false
	}
	rawRateLimit := payload["rate_limit"]
	if rawRateLimit == nil {
		rawRateLimit = payload["rateLimit"]
	}
	rateLimit, ok := rawRateLimit.(map[string]any)
	if !ok {
		return false, false
	}
	allowed, allowedKnown := rateLimit["allowed"].(bool)
	rawLimitReached := rateLimit["limit_reached"]
	if rawLimitReached == nil {
		rawLimitReached = rateLimit["limitReached"]
	}
	limitReached, limitKnown := rawLimitReached.(bool)
	if (allowedKnown && !allowed) || (limitKnown && limitReached) {
		return false, true
	}
	if allowedKnown && allowed && (!limitKnown || !limitReached) {
		return true, true
	}
	return false, false
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

	quotaRefresh := config.QuotaRefresh{
		Enabled:     cfg.Enabled,
		Interval:    cfg.Interval,
		MaxInterval: cfg.MaxInterval,
	}
	if _, ok := h.mutateConfig(c, func(next *config.Config) error {
		next.QuotaRefresh = quotaRefresh
		return nil
	}); !ok {
		return
	}

	h.quotaScheduler.UpdateConfig(cfg)
	c.JSON(http.StatusOK, h.quotaScheduler.GetConfig())
}
