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
