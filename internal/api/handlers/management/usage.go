// Last compiled: 2026-05-07
// Author: pyro

package management

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

type usageExportPayload struct {
	Version    int                      `json:"version"`
	ExportedAt time.Time                `json:"exported_at"`
	Usage      usage.StatisticsSnapshot `json:"usage"`
}

// GetUsageStatistics returns the in-memory request statistics snapshot.
// With SQLite storage, the snapshot is rebuilt from disk on every call.
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

// ExportUsageStatistics returns a usage snapshot for backup/migration. The
// response shape (version 1 + nested StatisticsSnapshot) is preserved so
// previously exported files stay re-importable across the SQLite swap.
//
// Supports optional query parameters from / to / model / api_key / credential
// to scope the export to the user's current dashboard view. Without any
// parameters the full snapshot is returned (legacy behaviour).
func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h.usagePersister != nil {
		from, errFrom := parseTimeParam(c.Query("from"), time.Time{})
		to, errTo := parseTimeParam(c.Query("to"), time.Time{})
		if errFrom != nil || errTo != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from/to parameter"})
			return
		}
		if !from.IsZero() && !to.IsZero() && from.After(to) {
			from, to = to, from
		}
		filters := usage.EventFilters{
			Model:  c.Query("model"),
			APIKey: c.Query("api_key"),
			Source: c.Query("credential"),
		}
		snapshot = h.usagePersister.SnapshotFiltered(from, to, filters)
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

// ImportUsageStatistics merges a previously exported payload into the SQLite
// store. Auto-detects the format (v1 nested, v2 detail, today.json, raw
// FlatDetail array) so old export files remain importable.
func (h *Handler) ImportUsageStatistics(c *gin.Context) {
	if h == nil || h.usagePersister == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage persister unavailable"})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 100<<20)
	data, err := c.GetRawData()
	if err != nil {
		message := "failed to read request body"
		if strings.Contains(err.Error(), "request body too large") || strings.Contains(
			err.Error(), "http: request body too large",
		) {
			message = "import file too large: request body exceeds 100MB limit"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": message})
		return
	}

	result, err := h.usagePersister.Import(data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	snapshot := h.usagePersister.Snapshot()
	c.JSON(
		http.StatusOK, gin.H{
			"added":           result.Added,
			"skipped":         result.Skipped,
			"format":          result.Format,
			"total_requests":  snapshot.TotalRequests,
			"failed_requests": snapshot.FailureCount,
		},
	)
}

// GetUsageRetention returns the current retention configuration.
func (h *Handler) GetUsageRetention(c *gin.Context) {
	if h == nil || h.usagePersister == nil {
		c.JSON(http.StatusOK, gin.H{"days": 0, "max_db_size_mb": 0, "warning_threshold_pct": 80})
		return
	}
	r := h.usagePersister.Retention()
	warningThreshold := r.WarningThresholdPct
	if warningThreshold == 0 {
		warningThreshold = 80
	}
	c.JSON(
		http.StatusOK, gin.H{
			"days":                  r.Days,
			"max_db_size_mb":        r.MaxDBSizeMB,
			"warning_threshold_pct": warningThreshold,
		},
	)
}

// GetUsageDBSize returns the on-disk size of the SQLite database (events.db +
// WAL + SHM). Used by the retention card to surface real disk usage.
func (h *Handler) GetUsageDBSize(c *gin.Context) {
	if h == nil || h.usagePersister == nil {
		c.JSON(
			http.StatusOK,
			gin.H{"size_bytes": 0, "max_size_bytes": 0, "warning": false, "capped": false, "warning_threshold_pct": 80},
		)
		return
	}
	sizeBytes, maxBytes, warningThresholdPct, warning, capped := h.usagePersister.DBSizeStatus()
	c.JSON(
		http.StatusOK, gin.H{
			"size_bytes":            sizeBytes,
			"max_size_bytes":        maxBytes,
			"warning_threshold_pct": warningThresholdPct,
			"warning":               warning,
			"capped":                capped,
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
		Days                *int `json:"days"`
		MaxDBSizeMB         *int `json:"max_db_size_mb"`
		WarningThresholdPct *int `json:"warning_threshold_pct"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}

	if body.Days != nil && (*body.Days == 0 || *body.Days < -1) {
		c.JSON(
			http.StatusBadRequest, gin.H{
				"error": "days must be -1 (disable) or a positive integer",
			},
		)
		return
	}
	if body.MaxDBSizeMB != nil && *body.MaxDBSizeMB < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "max_db_size_mb must be 0 (disable) or a positive integer"})
		return
	}
	if body.WarningThresholdPct != nil && (*body.WarningThresholdPct < 1 || *body.WarningThresholdPct > 100) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "warning_threshold_pct must be between 1 and 100"})
		return
	}

	r := h.usagePersister.Retention()
	if body.Days != nil {
		r.Days = *body.Days
	}
	if body.MaxDBSizeMB != nil {
		r.MaxDBSizeMB = *body.MaxDBSizeMB
	}
	if body.WarningThresholdPct != nil {
		r.WarningThresholdPct = *body.WarningThresholdPct
	}
	if r.WarningThresholdPct == 0 {
		r.WarningThresholdPct = 80
	}
	h.usagePersister.SetRetention(r)

	if h.cfg != nil {
		h.cfg.UsageRetention = r
	}

	// Persist config to disk so retention survives restarts
	if h.configFilePath != "" {
		h.mu.Lock()
		_ = config.SaveLastGood(h.configFilePath)
		if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); err != nil {
			h.rollbackLastGood()
			h.mu.Unlock()
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", err)})
			return
		}
		h.mu.Unlock()
	}

	warningThreshold := r.WarningThresholdPct
	if warningThreshold == 0 {
		warningThreshold = 80
	}
	c.JSON(
		http.StatusOK, gin.H{
			"status":                "ok",
			"days":                  r.Days,
			"max_db_size_mb":        r.MaxDBSizeMB,
			"warning_threshold_pct": warningThreshold,
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
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
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
