package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// RecalculateCosts starts recalculating all historical costs using the current price table.
//
//	POST /v0/management/usage/recalculate-costs
func (h *Handler) RecalculateCosts(c *gin.Context) {
	if h.usagePersister == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage persister not configured"})
		return
	}

	if !h.usagePersister.HasPricing() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no model prices configured"})
		return
	}

	if !h.usagePersister.StartRecalculateCosts() {
		c.JSON(
			http.StatusOK, gin.H{
				"status":          "busy",
				"running":         true,
				"already_running": true,
			},
		)
		return
	}

	RecordMutationEffect(c.Request.Context(), "task.queued", "task")
	CompleteMutation(c.Request.Context())
	c.JSON(
		http.StatusAccepted, gin.H{
			"status":          "started",
			"running":         true,
			"already_running": false,
		},
	)
}

// GetRecalculateCostsStatus returns the latest historical cost recalculation state.
//
//	GET /v0/management/usage/recalculate-costs
func (h *Handler) GetRecalculateCostsStatus(c *gin.Context) {
	if h.usagePersister == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage persister not configured"})
		return
	}
	c.JSON(http.StatusOK, h.usagePersister.RecalculateCostsStatus())
}

// GetUsageStorageUpgrade reports the durable structured storage upgrade state.
func (h *Handler) GetUsageStorageUpgrade(c *gin.Context) {
	if h == nil || h.usagePersister == nil {
		c.JSON(http.StatusOK, gin.H{"status": "idle", "running": false})
		return
	}
	status, err := h.usagePersister.StorageUpgradeStatusContext(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "running": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, status)
}
