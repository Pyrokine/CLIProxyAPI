// Last compiled: 2026-03-10
// Author: pyro

package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
)

type usageExportPayload struct {
	Version    int                      `json:"version"`
	ExportedAt time.Time                `json:"exported_at"`
	Usage      usage.StatisticsSnapshot `json:"usage"`
}

type usageImportPayload struct {
	Version int                      `json:"version"`
	Usage   usage.StatisticsSnapshot `json:"usage"`
}

// GetUsageStatistics returns the in-memory request statistics snapshot.
// With v2 storage, this returns only today's data via the persister.
func (h *Handler) GetUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h.usagePersister != nil {
		snapshot = h.usagePersister.Snapshot()
	} else if h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	c.JSON(
		http.StatusOK, gin.H{
			"usage":           snapshot,
			"failed_requests": snapshot.FailureCount,
		},
	)
}

// ExportUsageStatistics returns a complete usage snapshot for backup/migration.
func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h.usagePersister != nil {
		snapshot = h.usagePersister.Snapshot()
	} else if h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	c.JSON(
		http.StatusOK, usageExportPayload{
			Version:    1,
			ExportedAt: time.Now().UTC(),
			Usage:      snapshot,
		},
	)
}

// ImportUsageStatistics merges a previously exported usage snapshot into memory.
func (h *Handler) ImportUsageStatistics(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 10<<20)
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	var payload usageImportPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if payload.Version != 0 && payload.Version != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported version"})
		return
	}

	result := h.usageStats.MergeSnapshot(payload.Usage)
	snapshot := h.usageStats.Snapshot()
	c.JSON(
		http.StatusOK, gin.H{
			"added":           result.Added,
			"skipped":         result.Skipped,
			"total_requests":  snapshot.TotalRequests,
			"failed_requests": snapshot.FailureCount,
		},
	)
}

// GetUsageRetention returns the current retention configuration.
func (h *Handler) GetUsageRetention(c *gin.Context) {
	if h == nil || h.usagePersister == nil {
		c.JSON(
			http.StatusOK, gin.H{
				"days":             0,
				"max_file_size_mb": 0,
				"archive_months":   0,
			},
		)
		return
	}
	r := h.usagePersister.Retention()
	c.JSON(
		http.StatusOK, gin.H{
			"days":             r.Days,
			"max_file_size_mb": r.MaxFileSizeMB,
			"archive_months":   r.ArchiveMonths,
		},
	)
}

// PutUsageRetention updates the retention configuration.
func (h *Handler) PutUsageRetention(c *gin.Context) {
	if h == nil || h.usagePersister == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage persister unavailable"})
		return
	}

	var body struct {
		Days          *int `json:"days"`
		MaxFileSizeMB *int `json:"max_file_size_mb"`
		ArchiveMonths *int `json:"archive_months"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}

	// Validate: only -1 (disable) or positive integers
	validate := func(v *int, name string) bool {
		if v == nil {
			return true
		}
		if *v == 0 || *v < -1 {
			c.JSON(
				http.StatusBadRequest, gin.H{
					"error": name + " must be -1 (disable) or a positive integer",
				},
			)
			return false
		}
		return true
	}
	if !validate(body.Days, "days") ||
		!validate(body.MaxFileSizeMB, "max_file_size_mb") ||
		!validate(body.ArchiveMonths, "archive_months") {
		return
	}

	r := h.usagePersister.Retention()
	if body.Days != nil {
		r.Days = *body.Days
	}
	if body.MaxFileSizeMB != nil {
		r.MaxFileSizeMB = *body.MaxFileSizeMB
	}
	if body.ArchiveMonths != nil {
		r.ArchiveMonths = *body.ArchiveMonths
	}
	h.usagePersister.SetRetention(r)

	if h.cfg != nil {
		h.cfg.UsageRetention = r
	}

	// Persist config to disk so retention survives restarts
	if h.configFilePath != "" {
		h.mu.Lock()
		if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); err != nil {
			h.mu.Unlock()
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", err)})
			return
		}
		h.mu.Unlock()
	}

	c.JSON(
		http.StatusOK, gin.H{
			"status":           "ok",
			"days":             r.Days,
			"max_file_size_mb": r.MaxFileSizeMB,
			"archive_months":   r.ArchiveMonths,
		},
	)
}

// TrimUsageStatistics triggers an immediate cleanup of expired details.
func (h *Handler) TrimUsageStatistics(c *gin.Context) {
	if h == nil || h.usagePersister == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage persister unavailable"})
		return
	}
	h.usagePersister.Trim()
	archives := h.usagePersister.ListArchives()
	c.JSON(
		http.StatusOK, gin.H{
			"status":   "ok",
			"archives": archives,
		},
	)
}

// TrimPreviewUsageStatistics returns a preview of what would be cleaned by Trim.
func (h *Handler) TrimPreviewUsageStatistics(c *gin.Context) {
	if h == nil || h.usagePersister == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage persister unavailable"})
		return
	}
	preview := h.usagePersister.TrimPreview()
	c.JSON(http.StatusOK, preview)
}
