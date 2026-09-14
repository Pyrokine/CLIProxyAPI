package management

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
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

// PostQuotaRefresh creates a persistent quota refresh task.
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
	task, err := h.quotaRefreshService().createTask(append([]string(nil), body.Credentials...))
	if err != nil {
		writeQuotaRefreshTaskError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, task)
}

func (h *Handler) finalizeQuotaRefresh(attempt quota.RefreshAttempt) error {
	return h.finalizeQuotaRefreshContext(context.Background(), attempt)
}

func (h *Handler) finalizeQuotaRefreshContext(ctx context.Context, attempt quota.RefreshAttempt) error {
	if attempt.Type != quota.TypeCodex {
		return nil
	}
	auth := h.authByIndex(attempt.AuthIndex)
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return nil
	}
	root, parsed := parseCodexQuotaPayload(attempt.Data)
	available, known := false, false
	if parsed {
		available, known = inspectCodexRateLimit(firstGJSON(root, "rate_limit", "rateLimit"))
	}
	if !known || !available {
		if authHasQuotaState(auth) && !known {
			log.WithField("auth_index", attempt.AuthIndex).Warn(
				"Codex usage refresh did not include a conclusive availability signal; routing cooldown was left unchanged",
			)
		}
		return nil
	}

	families := coreauth.QuotaFamilyRecoveryCandidates(auth)
	recoverableFamilies := make([]coreauth.QuotaFamilyRecoveryCandidate, 0, len(families))
	for _, candidate := range families {
		familyAvailable, familyKnown := inspectCodexQuotaFamilyAvailability(root, candidate)
		if familyKnown && familyAvailable {
			recoverableFamilies = append(recoverableFamilies, candidate)
			continue
		}
		if !familyKnown {
			log.WithFields(
				log.Fields{
					"auth_index":   attempt.AuthIndex,
					"quota_family": candidate.Family,
				},
			).Warn("Codex usage refresh did not include a conclusive family availability signal; family cooldown was left unchanged")
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	finalizeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	updated, err := h.authManager.RecoverQuotaAfterRefresh(
		finalizeCtx,
		auth.ID,
		attempt.StartedAt,
		recoverableFamilies,
	)
	if err != nil {
		return err
	}
	if updated == nil {
		return fmt.Errorf("codex credential %q was removed during quota recovery", attempt.AuthIndex)
	}
	return nil
}

func authHasQuotaState(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Quota.Exceeded || auth.LastError != nil && auth.LastError.HTTPStatus == http.StatusTooManyRequests {
		return true
	}
	for _, state := range auth.ModelStates {
		if state != nil && (state.Quota.Exceeded ||
			state.LastError != nil && state.LastError.HTTPStatus == http.StatusTooManyRequests) {
			return true
		}
	}
	return false
}

func codexCredentialQuotaAvailability(data []byte) (bool, bool) {
	root, ok := parseCodexQuotaPayload(data)
	if !ok {
		return false, false
	}
	return inspectCodexRateLimit(firstGJSON(root, "rate_limit", "rateLimit"))
}

func inspectCodexQuotaFamilyAvailability(
	root gjson.Result,
	candidate coreauth.QuotaFamilyRecoveryCandidate,
) (available bool, known bool) {
	rootAvailable, rootKnown := inspectCodexRateLimit(firstGJSON(root, "rate_limit", "rateLimit"))
	if !rootKnown || !rootAvailable {
		return rootAvailable, rootKnown
	}
	additional := firstGJSON(root, "additional_rate_limits", "additionalRateLimits")
	if !additional.IsArray() {
		return true, true
	}

	matched := false
	unknown := false
	for _, entry := range additional.Array() {
		if !codexQuotaMeterMatchesFamily(entry, candidate) {
			continue
		}
		matched = true
		meterAvailable, meterKnown := inspectCodexRateLimit(firstGJSON(entry, "rate_limit", "rateLimit"))
		if !meterKnown {
			unknown = true
			continue
		}
		if !meterAvailable {
			return false, true
		}
	}
	if unknown {
		return false, false
	}
	if matched {
		return true, true
	}
	return rootAvailable, rootKnown
}

func codexQuotaMeterMatchesFamily(
	entry gjson.Result,
	candidate coreauth.QuotaFamilyRecoveryCandidate,
) bool {
	scopes := []gjson.Result{
		firstGJSON(entry, "limit_name", "limitName"),
		firstGJSON(entry, "metered_feature", "meteredFeature"),
	}
	for _, scope := range scopes {
		name := strings.TrimSpace(scope.String())
		if name == "" {
			continue
		}
		if quota.ModelScopeMatches(name, candidate.Family) {
			return true
		}
		for _, model := range candidate.Models {
			if quota.ModelScopeMatches(name, model) {
				return true
			}
		}
	}
	return false
}

func codexQuotaAvailability(data []byte) (bool, bool) {
	root, ok := parseCodexQuotaPayload(data)
	if !ok {
		return false, false
	}
	limits := make([]gjson.Result, 0, 4)
	appendCodexRateLimit(&limits, firstGJSON(root, "rate_limit", "rateLimit"))
	appendCodexRateLimit(&limits, firstGJSON(root, "code_review_rate_limit", "codeReviewRateLimit"))
	additional := firstGJSON(root, "additional_rate_limits", "additionalRateLimits")
	if additional.IsArray() {
		for _, entry := range additional.Array() {
			appendCodexRateLimit(&limits, firstGJSON(entry, "rate_limit", "rateLimit"))
		}
	}

	sawLimit := false
	exhausted := false
	available := false
	for _, limit := range limits {
		limitAvailable, limitKnown := inspectCodexRateLimit(limit)
		if !limitKnown {
			continue
		}
		sawLimit = true
		if limitAvailable {
			available = true
			continue
		}
		exhausted = true
	}
	if !sawLimit {
		return false, false
	}
	if exhausted {
		return false, true
	}
	if available {
		return true, true
	}
	return false, false
}

func parseCodexQuotaPayload(data []byte) (gjson.Result, bool) {
	if !gjson.ValidBytes(data) {
		return gjson.Result{}, false
	}
	root := gjson.ParseBytes(data)
	if root.Type != gjson.String {
		return root, true
	}
	unquoted := strings.TrimSpace(root.String())
	if !gjson.Valid(unquoted) {
		return gjson.Result{}, false
	}
	return gjson.Parse(unquoted), true
}

func appendCodexRateLimit(dst *[]gjson.Result, limit gjson.Result) {
	if dst == nil || !limit.IsObject() {
		return
	}
	*dst = append(*dst, limit)
}

func inspectCodexRateLimit(limit gjson.Result) (available bool, known bool) {
	if !limit.IsObject() {
		return false, false
	}
	allowed, allowedKnown := gjsonBool(firstGJSON(limit, "allowed"))
	limitReached, limitReachedKnown := gjsonBool(firstGJSON(limit, "limit_reached", "limitReached"))
	if (allowedKnown && !allowed) || (limitReachedKnown && limitReached) {
		return false, true
	}

	windowAvailable := false
	windowKnown := false
	for _, name := range [][]string{
		{"primary_window", "primaryWindow"},
		{"secondary_window", "secondaryWindow"},
	} {
		window := firstGJSON(limit, name...)
		if !window.IsObject() {
			continue
		}
		currentAvailable, currentKnown := inspectCodexRateLimitWindow(
			window,
			allowed,
			allowedKnown,
			limitReached,
			limitReachedKnown,
		)
		if !currentKnown {
			continue
		}
		windowKnown = true
		if currentAvailable {
			windowAvailable = true
			continue
		}
		return false, true
	}
	if allowedKnown && allowed && (!limitReachedKnown || !limitReached) {
		if windowKnown && !windowAvailable {
			return false, true
		}
		return true, true
	}
	if windowKnown {
		return windowAvailable, true
	}
	return false, false
}

func inspectCodexRateLimitWindow(
	window gjson.Result,
	parentAllowed bool,
	parentAllowedKnown bool,
	parentLimitReached bool,
	parentLimitReachedKnown bool,
) (available bool, known bool) {
	allowed, allowedKnown := gjsonBool(firstGJSON(window, "allowed"))
	if !allowedKnown {
		allowed, allowedKnown = parentAllowed, parentAllowedKnown
	}
	limitReached, limitReachedKnown := gjsonBool(firstGJSON(window, "limit_reached", "limitReached"))
	if !limitReachedKnown {
		limitReached, limitReachedKnown = parentLimitReached, parentLimitReachedKnown
	}
	if (allowedKnown && !allowed) || (limitReachedKnown && limitReached) {
		return false, true
	}
	usedPercent, usedKnown := gjsonPercent(firstGJSON(window, "used_percent", "usedPercent"))
	if usedKnown {
		if usedPercent >= 100 {
			return false, true
		}
		return true, true
	}
	if allowedKnown && (!limitReachedKnown || !limitReached) {
		return true, true
	}
	return false, false
}

func firstGJSON(parent gjson.Result, names ...string) gjson.Result {
	for _, name := range names {
		value := parent.Get(name)
		if value.Exists() {
			return value
		}
	}
	return gjson.Result{}
}

func gjsonBool(value gjson.Result) (bool, bool) {
	if !value.Exists() || value.Type == gjson.Null {
		return false, false
	}
	switch value.Type {
	case gjson.True:
		return true, true
	case gjson.False:
		return false, true
	case gjson.String:
		parsed, err := strconv.ParseBool(strings.TrimSpace(value.String()))
		if err != nil {
			return false, false
		}
		return parsed, true
	default:
		return false, false
	}
}

func gjsonPercent(value gjson.Result) (float64, bool) {
	if !value.Exists() || value.Type == gjson.Null {
		return 0, false
	}
	switch value.Type {
	case gjson.Number:
		return value.Float(), true
	case gjson.String:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value.String()), 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
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
	if _, ok := h.mutateConfig(
		c, func(next *config.Config) error {
			next.QuotaRefresh = quotaRefresh
			return nil
		},
	); !ok {
		return
	}

	h.quotaScheduler.UpdateConfig(cfg)
	c.JSON(http.StatusOK, h.quotaScheduler.GetConfig())
}
