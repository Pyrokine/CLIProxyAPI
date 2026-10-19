package management

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	metaauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/meta"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	authFileRefreshTimeout        = 30 * time.Second
	authFileRefreshBatchTimeout   = 4*time.Minute + 30*time.Second
	defaultAuthFileRefreshWorkers = 16
)

type authFileRefreshResponse struct {
	coreauth.ForceRefreshResult
	Error string `json:"error,omitempty"`
}

type authFileRefreshBatchItem struct {
	coreauth.ForceRefreshResult
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

type authFileRefreshBatchResponse struct {
	Results []authFileRefreshBatchItem `json:"results"`
}

// RefreshAuthFile refreshes one exact credential identity without returning credential data.
func (h *Handler) RefreshAuthFile(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core_auth_manager_unavailable"})
		return
	}

	var req struct {
		Name      string `json:"name"`
		AuthIndex string `json:"auth_index"`
		All       bool   `json:"all"`
	}
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request_body"})
		return
	}
	if req.All {
		h.refreshAllAuthFiles(c)
		return
	}
	name := strings.TrimSpace(req.Name)
	authIndex := strings.TrimSpace(req.AuthIndex)
	if name == "" || authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name_and_auth_index_required"})
		return
	}

	targetAuth, ok := h.lookupAuthFile(name, authIndex)
	if !ok || targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth_file_not_found"})
		return
	}
	if isMetaAuthFile(targetAuth) && !metaManualRefreshAvailable(targetAuth) {
		c.JSON(http.StatusConflict, gin.H{"error": "credential_refresh_unavailable"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), authFileRefreshTimeout)
	defer cancel()
	result, errRefresh := h.authManager.ForceRefreshAuth(ctx, targetAuth.ID)
	if errRefresh == nil {
		c.JSON(http.StatusOK, result)
		return
	}

	safeError := strings.TrimSpace(string(util.MaskSensitivePayload([]byte(errRefresh.Error()))))
	log.WithFields(log.Fields{
		"provider":   result.Provider,
		"filename":   result.FileName,
		"auth_index": result.AuthIndex,
		"error":      safeError,
	}).Warn("explicit credential refresh failed")

	status := http.StatusBadGateway
	errorCode := "credential_refresh_failed"
	if errors.Is(errRefresh, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
		errorCode = "credential_refresh_timeout"
	}
	c.JSON(status, authFileRefreshResponse{
		ForceRefreshResult: result,
		Error:              errorCode,
	})
}

func (h *Handler) refreshAllAuthFiles(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), authFileRefreshBatchTimeout)
	defer cancel()

	targets := refreshableAuthFiles(h.authManager.List())
	results := make([]authFileRefreshBatchItem, len(targets))
	if len(targets) == 0 {
		c.JSON(http.StatusOK, authFileRefreshBatchResponse{Results: results})
		return
	}

	workers := defaultAuthFileRefreshWorkers
	if cfg := h.configSnapshot(); cfg != nil && cfg.AuthAutoRefreshWorkers > 0 {
		workers = cfg.AuthAutoRefreshWorkers
	}
	if workers > len(targets) {
		workers = len(targets)
	}
	if workers < 1 {
		workers = 1
	}

	type refreshJob struct {
		index int
		auth  *coreauth.Auth
	}
	jobs := make(chan refreshJob, len(targets))
	for index, auth := range targets {
		jobs <- refreshJob{index: index, auth: auth}
	}
	close(jobs)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				results[job.index] = h.refreshAuthFileJob(ctx, job.auth)
			}
		}()
	}
	wg.Wait()
	c.JSON(http.StatusOK, authFileRefreshBatchResponse{Results: results})
}

func isMetaAuthFile(auth *coreauth.Auth) bool {
	return auth != nil && strings.EqualFold(auth.Provider, "meta")
}

func metaManualRefreshAvailable(auth *coreauth.Auth) bool {
	if auth == nil || coreauth.IsConfigAPIKeyAuth(auth) {
		return false
	}
	if strings.TrimSpace(auth.Attributes["dca_token"]) != "" ||
		strings.HasPrefix(strings.TrimSpace(auth.Attributes["access_token"]), "dca:") {
		return true
	}
	if token, ok := auth.Metadata["dca_token"].(string); ok && strings.TrimSpace(token) != "" {
		return true
	}
	if token, ok := auth.Metadata["access_token"].(string); ok && strings.HasPrefix(strings.TrimSpace(token), "dca:") {
		return true
	}
	if storage, ok := auth.Storage.(*metaauth.MetaTokenStorage); ok && storage != nil {
		return strings.TrimSpace(storage.DCAToken) != "" || strings.HasPrefix(strings.TrimSpace(storage.AccessToken), "dca:")
	}
	return false
}

func refreshableAuthFiles(auths []*coreauth.Auth) []*coreauth.Auth {
	targets := make([]*coreauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		if isMetaAuthFile(auth) {
			if !metaManualRefreshAvailable(auth) {
				continue
			}
		} else if auth.Runtime == nil && authFileRefreshCredential(auth) == "" {
			continue
		}
		targets = append(targets, auth)
	}
	sort.Slice(targets, func(i, j int) bool {
		return strings.TrimSpace(targets[i].ID) < strings.TrimSpace(targets[j].ID)
	})
	return targets
}

func authFileRefreshCredential(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	for _, key := range []string{"refresh_token", "refreshToken"} {
		value, _ := auth.Metadata[key].(string)
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func refreshResultForAuth(auth *coreauth.Auth) coreauth.ForceRefreshResult {
	if auth == nil {
		return coreauth.ForceRefreshResult{}
	}
	auth = auth.Clone()
	auth.EnsureIndex()
	fileName := strings.TrimSpace(auth.FileName)
	if fileName == "" {
		fileName = strings.TrimSpace(auth.ID)
	}
	if fileName != "" {
		fileName = filepath.Base(fileName)
	}
	return coreauth.ForceRefreshResult{
		FileName:    fileName,
		Provider:    strings.TrimSpace(auth.Provider),
		AuthIndex:   strings.TrimSpace(auth.Index),
		Status:      auth.Status,
		RefreshedAt: auth.LastRefreshedAt,
	}
}

func (h *Handler) refreshAuthFileJob(ctx context.Context, auth *coreauth.Auth) authFileRefreshBatchItem {
	if errCtx := ctx.Err(); errCtx != nil {
		return authFileRefreshBatchItem{
			ForceRefreshResult: refreshResultForAuth(auth),
			Error:              refreshErrorCode(ctx, errCtx),
		}
	}
	jobCtx, cancel := context.WithTimeout(ctx, authFileRefreshTimeout)
	defer cancel()
	result, errRefresh := h.authManager.ForceRefreshAuth(jobCtx, auth.ID)
	if errRefresh == nil {
		return authFileRefreshBatchItem{ForceRefreshResult: result, Success: true}
	}
	log.WithFields(log.Fields{
		"provider":   result.Provider,
		"filename":   result.FileName,
		"auth_index": result.AuthIndex,
		"error": strings.TrimSpace(string(util.MaskSensitivePayload(
			[]byte(errRefresh.Error()),
		))),
	}).Warn("bulk credential refresh failed")
	return authFileRefreshBatchItem{
		ForceRefreshResult: result,
		Error:              refreshErrorCode(jobCtx, errRefresh),
	}
}

func refreshErrorCode(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "credential_refresh_timeout"
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return "credential_refresh_cancelled"
	default:
		return "credential_refresh_failed"
	}
}
