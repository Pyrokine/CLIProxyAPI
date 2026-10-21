package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	codexauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
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

	resetContext := c.Request.Context()
	var expected *coreauth.Auth
	if consumeCodexCredit && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		if _, busy := h.quotaResetInFlight.LoadOrStore(auth.ID, struct{}{}); busy {
			c.JSON(http.StatusConflict, gin.H{"error": "Codex reset is already in progress"})
			return
		}
		defer h.quotaResetInFlight.Delete(auth.ID)
		expected = auth.Clone()
		cfg := h.configSnapshot()
		ctx, cancel := context.WithTimeout(resetContext, defaultAPICallTimeout)
		defer cancel()
		resetContext = ctx
		probe, errProbe := h.probeCodexResetCredits(ctx, cfg, expected)
		if errProbe != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to check Codex reset credits: %v", errProbe)})
			return
		}
		if probe.Availability != "available" {
			c.JSON(http.StatusConflict, gin.H{"error": "Codex reset credits are " + probe.Availability})
			return
		}
		if err := h.consumeCodexResetCredit(ctx, cfg, expected, probe.Data); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to consume Codex reset credit: %v", err)})
			return
		}
		if !h.codexResetBindingCurrent(expected, cfg) {
			c.JSON(http.StatusConflict, gin.H{"error": "credential or proxy changed after Codex reset; local cooldown was retained"})
			return
		}
	}

	var updated *coreauth.Auth
	var models []string
	var err error
	if consumeCodexCredit {
		if expected != nil {
			resetContext = coreauth.WithQuotaResetCredential(resetContext, expected)
		}
		updated, models, err = h.authManager.ResetQuota(resetContext, auth.ID)
	} else {
		updated, models, err = h.authManager.ResetCooldown(resetContext, auth.ID)
	}
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
	creditData json.RawMessage,
) error {
	token, err := h.resolveTokenForAuth(ctx, cfg, auth)
	if err != nil {
		return err
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("auth token not found")
	}
	if errContext := ctx.Err(); errContext != nil {
		return errContext
	}
	if !h.codexResetBindingCurrent(auth, cfg) {
		return fmt.Errorf("credential or proxy changed before Codex reset")
	}
	availability, err := quota.CodexResetCreditAvailability(creditData, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("check Codex reset before submission: %w", err)
	}
	if availability != "available" {
		return fmt.Errorf("codex reset eligibility changed before submission: %s", availability)
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
	if accountID := codexResetAccountID(auth); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
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
	confirmationBody, err := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if err != nil {
		return fmt.Errorf("read reset credit confirmation: %w", err)
	}
	if len(confirmationBody) > 16<<10 {
		return fmt.Errorf("reset credit confirmation is too large")
	}
	if err = json.Unmarshal(confirmationBody, &confirmation); err != nil {
		return fmt.Errorf("decode reset credit confirmation: %w", err)
	}
	if confirmation.Code != "reset" && confirmation.Code != "already_redeemed" {
		return fmt.Errorf("upstream did not confirm reset credit consumption")
	}
	return nil
}

type codexResetCreditProbe struct {
	Purpose      string          `json:"purpose"`
	AuthIndex    string          `json:"auth_index"`
	Generation   uint64          `json:"generation"`
	CheckedAt    time.Time       `json:"checked_at"`
	Availability string          `json:"availability"`
	Data         json.RawMessage `json:"data"`
}

func (h *Handler) probeCodexResetCredits(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (codexResetCreditProbe, error) {
	if h.quotaScheduler == nil {
		return codexResetCreditProbe{}, fmt.Errorf("native quota probe is unavailable")
	}
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") ||
		auth.Disabled || auth.Status == coreauth.StatusDisabled || !h.codexResetBindingCurrent(auth, cfg) {
		return codexResetCreditProbe{}, fmt.Errorf("codex credential is unavailable or changed")
	}
	accountID := codexResetAccountID(auth)
	if len(accountID) > 256 || strings.IndexFunc(accountID, unicode.IsControl) >= 0 {
		return codexResetCreditProbe{}, fmt.Errorf("invalid Codex account identity")
	}
	targets, _ := h.quotaScheduler.ResolveRefreshTargets(nil)
	var target quota.RefreshTarget
	matched := false
	for _, candidate := range targets {
		if candidate.Type != quota.TypeCodex || candidate.AuthIndex != auth.Index {
			continue
		}
		if matched || candidate.Disabled {
			return codexResetCreditProbe{}, fmt.Errorf("codex quota target is ambiguous or disabled")
		}
		target, matched = candidate, true
	}
	if !matched {
		return codexResetCreditProbe{}, fmt.Errorf("codex quota target is unavailable")
	}
	probe := h.quotaScheduler.ProbeTarget(quota.WithCodexResetCreditProbe(ctx), target)
	if probe.Outcome != quota.ProbeOutcomePassed {
		return codexResetCreditProbe{}, fmt.Errorf("codex reset credit query did not complete: %s", probe.Error)
	}
	if !h.codexResetBindingCurrent(auth, cfg) {
		return codexResetCreditProbe{}, fmt.Errorf("credential or proxy changed during Codex reset credit query")
	}
	checkedAt := time.Now().UTC()
	availability, err := quota.CodexResetCreditAvailability(probe.Data, checkedAt)
	if err != nil {
		return codexResetCreditProbe{}, err
	}
	var payload map[string]json.RawMessage
	if err = json.Unmarshal(probe.Data, &payload); err != nil {
		return codexResetCreditProbe{}, err
	}
	data := make(map[string]json.RawMessage)
	for _, field := range []string{"rate_limit_reset_credits", "rateLimitResetCredits", "rate_limit_reset_credits_details", "rateLimitResetCreditsDetails"} {
		if value, exists := payload[field]; exists {
			data[field] = value
		}
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return codexResetCreditProbe{}, err
	}
	return codexResetCreditProbe{
		Purpose: quota.CodexResetCreditProbePurpose, AuthIndex: auth.Index,
		Generation: target.Generation, CheckedAt: checkedAt, Availability: availability, Data: encoded,
	}, nil
}

func (h *Handler) codexResetBindingCurrent(expected *coreauth.Auth, cfg *config.Config) bool {
	if h.authManager == nil || expected == nil {
		return false
	}
	current, exists := h.authManager.GetByID(expected.ID)
	if !exists || current == nil {
		return false
	}
	current.EnsureIndex()
	if current.Generation != expected.Generation || current.Index != expected.Index ||
		current.Provider != expected.Provider || current.FileName != expected.FileName ||
		current.Disabled != expected.Disabled || current.Status == coreauth.StatusDisabled ||
		current.ProxyURL != expected.ProxyURL {
		return false
	}
	expectedRevision, _, expectedErr := coreauth.QuotaCredentialRevisions(expected)
	currentRevision, _, currentErr := coreauth.QuotaCredentialRevisions(current)
	if expectedErr != nil || currentErr != nil || expectedRevision != currentRevision {
		return false
	}
	expectedProxy, currentProxy := "", ""
	if cfg != nil {
		expectedProxy = strings.TrimSpace(cfg.ProxyURL)
	}
	if currentConfig := h.configSnapshot(); currentConfig != nil {
		currentProxy = strings.TrimSpace(currentConfig.ProxyURL)
	}
	return expectedProxy == currentProxy
}

func codexResetAccountID(auth *coreauth.Auth) string {
	if storage, ok := auth.Storage.(*codexauth.TokenStorage); ok {
		if accountID := strings.TrimSpace(storage.AccountID); accountID != "" {
			return accountID
		}
	}
	accountID, _ := auth.Metadata["account_id"].(string)
	return strings.TrimSpace(accountID)
}
