package management

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

type bulkAuthFilesRequest struct {
	Names []string `json:"names"`
}

type bulkAuthFilesResult struct {
	Updated []string          `json:"updated"`
	Failed  map[string]string `json:"failed,omitempty"`
}

// PatchAuthFilesBulkEnable enables (sets disabled=false) every auth file named in the body.
// Paired with PatchAuthFilesBulkDisable to let the panel flip many credentials at once
// without N round-trips to PatchAuthFileStatus.
func (h *Handler) PatchAuthFilesBulkEnable(c *gin.Context) {
	h.bulkSetDisabled(c, false)
}

// PatchAuthFilesBulkDisable disables every auth file named in the body.
func (h *Handler) PatchAuthFilesBulkDisable(c *gin.Context) {
	h.bulkSetDisabled(c, true)
}

func (h *Handler) bulkSetDisabled(c *gin.Context, disabled bool) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req bulkAuthFilesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	names := make([]string, 0, len(req.Names))
	for _, n := range req.Names {
		trimmed := strings.TrimSpace(n)
		if trimmed != "" {
			names = append(names, trimmed)
		}
	}
	if len(names) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "names must be a non-empty array"})
		return
	}

	ctx := c.Request.Context()
	result := bulkAuthFilesResult{
		Updated: make([]string, 0, len(names)),
		Failed:  make(map[string]string),
	}

	for _, name := range names {
		if err := h.setAuthFileDisabled(ctx, name, disabled); err != nil {
			result.Failed[name] = err.Error()
			continue
		}
		result.Updated = append(result.Updated, name)
	}

	status := http.StatusOK
	if len(result.Updated) == 0 {
		status = http.StatusUnprocessableEntity
	}
	if len(result.Failed) == 0 {
		result.Failed = nil
	}
	c.JSON(status, result)
}

// setAuthFileDisabled mirrors PatchAuthFileStatus's update semantics for a single name.
// Keeping the logic in one place means bulk ops and the single-name endpoint stay aligned
// on status transitions, quota scheduler sync, and StatusMessage wording.
func (h *Handler) setAuthFileDisabled(ctx context.Context, name string, disabled bool) error {
	var target *coreauth.Auth
	if auth, ok := h.authManager.GetByID(name); ok {
		target = auth
	} else {
		for _, auth := range h.authManager.List() {
			if auth != nil && auth.FileName == name {
				target = auth
				break
			}
		}
	}
	if target == nil {
		return fmt.Errorf("auth file not found")
	}

	target.Disabled = disabled
	if disabled {
		target.Status = coreauth.StatusDisabled
		target.StatusMessage = "disabled via management API"
	} else {
		target.Status = coreauth.StatusActive
		target.StatusMessage = ""
	}
	target.UpdatedAt = time.Now()

	if _, err := h.authManager.Update(ctx, target); err != nil {
		return fmt.Errorf("failed to update auth: %w", err)
	}

	h.syncQuotaSchedulerForAuth(target)
	return nil
}
