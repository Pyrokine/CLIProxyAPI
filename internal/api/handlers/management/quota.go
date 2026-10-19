package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const codexRateLimitResetCreditsConsumeDefaultURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"

var codexRateLimitResetCreditsConsumeURL = codexRateLimitResetCreditsConsumeDefaultURL

// Quota exceeded toggles

func (h *Handler) GetSwitchProject(c *gin.Context) {
	cfg := h.configSnapshot()
	// configSnapshot may be nil for a handler created without runtime configuration.
	// noinspection GoDfaConstantCondition
	c.JSON(200, gin.H{"switch-project": cfg != nil && cfg.QuotaExceeded.SwitchProject})
}
func (h *Handler) PutSwitchProject(c *gin.Context) {
	h.updateBoolField(c, func(cfg *config.Config, v bool) { cfg.QuotaExceeded.SwitchProject = v })
}

func (h *Handler) GetSwitchPreviewModel(c *gin.Context) {
	cfg := h.configSnapshot()
	// configSnapshot may be nil for a handler created without runtime configuration.
	// noinspection GoDfaConstantCondition
	c.JSON(200, gin.H{"switch-preview-model": cfg != nil && cfg.QuotaExceeded.SwitchPreviewModel})
}
func (h *Handler) PutSwitchPreviewModel(c *gin.Context) {
	h.updateBoolField(c, func(cfg *config.Config, v bool) { cfg.QuotaExceeded.SwitchPreviewModel = v })
}

// ResetQuota consumes a Codex reset credit before clearing local quota and cooldown state.
func (h *Handler) ResetQuota(c *gin.Context) {
	h.resetQuota(c, true)
}

// ResetCooldown clears only local quota and cooldown routing state.
func (h *Handler) ResetCooldown(c *gin.Context) {
	h.resetQuota(c, false)
}

func (h *Handler) resetQuota(c *gin.Context, consumeCodexCredit bool) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		AuthIndex string `json:"auth_index"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	authIndex := strings.TrimSpace(req.AuthIndex)
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}

	if consumeCodexCredit && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		if err := h.consumeCodexResetCredit(c.Request.Context(), h.configSnapshot(), auth); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to consume Codex reset credit: %v", err)})
			return
		}
	}

	reset := h.authManager.ResetCooldown
	if consumeCodexCredit {
		reset = h.authManager.ResetQuota
	}
	updated, models, err := reset(c.Request.Context(), auth.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to clear local routing state: %v", err)})
		return
	}
	if updated == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	updated.EnsureIndex()

	c.JSON(
		http.StatusOK, gin.H{
			"status":     "ok",
			"auth_index": updated.Index,
			"models":     models,
		},
	)
}

func (h *Handler) consumeCodexResetCredit(
	ctx context.Context,
	cfg *config.Config,
	auth *coreauth.Auth,
) error {
	token, err := h.resolveTokenForAuth(ctx, cfg, auth)
	if err != nil {
		return err
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("auth token not found")
	}

	body, err := json.Marshal(
		map[string]string{
			"redeem_request_id": uuid.NewString(),
		},
	)
	if err != nil {
		return fmt.Errorf("encode reset request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, codexRateLimitResetCreditsConsumeURL, bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("build reset request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)")

	client := &http.Client{Timeout: defaultAPICallTimeout, Transport: h.apiCallTransport(cfg, auth, "")}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request reset credit: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}
	var confirmation struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&confirmation); err != nil {
		return fmt.Errorf("decode reset credit confirmation: %w", err)
	}
	if confirmation.Code != "reset" && confirmation.Code != "already_redeemed" {
		return fmt.Errorf("upstream did not confirm reset credit consumption")
	}
	return nil
}
