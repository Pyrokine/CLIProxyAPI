// Last compiled: 2026-03-30
// Author: pyro

package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// RecalculateCosts recalculates all historical costs using the current price table.
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

	result, err := h.usagePersister.RecalculateCosts()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	status := "ok"
	if result.AlreadyRunning {
		status = "busy"
	}

	c.JSON(
		http.StatusOK, gin.H{
			"status":            status,
			"recalculated_days": result.RecalculatedDays,
			"total_cost":        result.TotalCost,
			"already_running":   result.AlreadyRunning,
		},
	)
}
