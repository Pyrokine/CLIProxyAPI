package management

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/gin-gonic/gin"
)

const usageBackupKeyHeader = "X-Usage-Backup-Key"

type usageQueueRecord []byte

func (r usageQueueRecord) MarshalJSON() ([]byte, error) {
	if json.Valid(r) {
		return append([]byte(nil), r...), nil
	}
	return json.Marshal(string(r))
}

// GetUsageQueue pops queued usage records from the usage queue.
func (h *Handler) GetUsageQueue(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	count, errCount := parseUsageQueueCount(c.Query("count"))
	if errCount != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errCount.Error()})
		return
	}

	queue := h.usageQueue
	if queue == nil {
		queue = redisqueue.DefaultQueue()
	}
	items := queue.PopOldest(count)
	records := make([]usageQueueRecord, 0, len(items))
	for _, item := range items {
		records = append(records, usageQueueRecord(append([]byte(nil), item...)))
	}

	c.JSON(http.StatusOK, records)
}

func parseUsageQueueCount(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 1, nil
	}
	count, errCount := strconv.Atoi(value)
	if errCount != nil || count <= 0 {
		return 0, errors.New("count must be a positive integer")
	}
	return count, nil
}

// GetUsageStatistics returns the in-memory request statistics snapshot.
// With SQLite storage, the snapshot is rebuilt from disk on every call.
func (h *Handler) GetUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h.usagePersister != nil {
		var err error
		snapshot, err = h.usagePersister.QuerySnapshot()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query usage statistics"})
			return
		}
	} else if h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	writeRedactedUsageJSON(
		c,
		http.StatusOK,
		gin.H{
			"usage":           snapshot,
			"failed_requests": snapshot.FailureCount,
		},
		newUsageCredentialMasker(h.configSnapshot()),
	)
}

// ExportUsageStatistics returns a versioned, reversible backup of canonical
// SQLite events. Raw API keys are encrypted with the authenticated management
// credential, or with X-Usage-Backup-Key when the caller supplies a portable
// backup key. Optional filters scope the exported event set.
func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	if h == nil || h.usagePersister == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage persister unavailable"})
		return
	}
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
		Model:   c.Query("model"),
		APIKey:  c.Query("api_key"),
		APIKeys: c.QueryArray("api_key"),
		Source:  c.Query("credential"),
	}
	events, err := h.usagePersister.QueryBackupEventsContext(c.Request.Context(), from, to, filters)
	if err != nil {
		if errors.Is(err, usage.ErrPortableBackupTooLarge) {
			writeUsageBackupTooLarge(c)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query usage export"})
		return
	}
	secret := usageBackupSecret(c)
	if secret == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "usage backup encryption key unavailable"})
		return
	}
	exportedAt := time.Now().UTC()
	payload, err := usage.MarshalUsageBackup(events, secret, exportedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt usage export"})
		return
	}
	if int64(len(payload)) > usage.MaxPortableBackupBytes {
		writeUsageBackupTooLarge(c)
		return
	}
	c.Header("Content-Disposition", "attachment; filename=usage-backup-"+exportedAt.Format("20060102T150405Z")+".json")
	c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
}

func writeUsageBackupTooLarge(c *gin.Context) {
	c.JSON(
		http.StatusRequestEntityTooLarge,
		gin.H{"error": "usage backup exceeds the 100 MiB portable limit; narrow the export with from, to, model, api_key, or credential filters"},
	)
}

func usageBackupSecret(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	if secret := strings.TrimSpace(c.GetHeader(usageBackupKeyHeader)); secret != "" {
		return secret
	}
	provided := strings.TrimSpace(c.GetHeader("Authorization"))
	if provided != "" {
		parts := strings.SplitN(provided, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
		return provided
	}
	return strings.TrimSpace(c.GetHeader("X-Management-Key"))
}

// ImportUsageStatistics merges an encrypted v3 backup or any supported legacy
// payload into SQLite. Encrypted backups require the key that created them.
func (h *Handler) ImportUsageStatistics(c *gin.Context) {
	if h == nil || h.usagePersister == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage persister unavailable"})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, usage.MaxPortableBackupBytes)
	data, err := handlers.ReadRequestBodyWithLimit(c, usage.MaxPortableBackupBytes)
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

	secret := ""
	if usage.IsUsageBackup(data) {
		secret = usageBackupSecret(c)
		if secret == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "usage backup decryption key unavailable"})
			return
		}
	}
	result, err := h.usagePersister.ImportContextWithSecret(c.Request.Context(), data, secret)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, usage.ErrInvalidImport) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	snapshot, err := h.usagePersister.QuerySnapshot()
	if err != nil {
		c.JSON(
			http.StatusOK, gin.H{
				"added":   result.Added,
				"skipped": result.Skipped,
				"format":  result.Format,
				"warning": "usage imported, but totals could not be queried",
			},
		)
		return
	}
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
	decoder := json.NewDecoder(c.Request.Body)
	if err := decoder.Decode(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
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

	runtimeRetention := h.usagePersister.Retention()
	committed, ok := h.mutateConfig(c, func(next *config.Config) error {
		retention := next.UsageRetention
		if retention == (config.UsageRetention{}) {
			retention = runtimeRetention
		}
		if body.Days != nil {
			retention.Days = *body.Days
		}
		if body.MaxDBSizeMB != nil {
			retention.MaxDBSizeMB = *body.MaxDBSizeMB
		}
		if body.WarningThresholdPct != nil {
			retention.WarningThresholdPct = *body.WarningThresholdPct
		}
		if retention.WarningThresholdPct == 0 {
			retention.WarningThresholdPct = 80
		}
		next.UsageRetention = retention
		return nil
	})
	if !ok {
		return
	}
	retention := committed.UsageRetention
	h.usagePersister.SetRetention(retention)

	warningThreshold := retention.WarningThresholdPct
	if warningThreshold == 0 {
		warningThreshold = 80
	}
	c.JSON(
		http.StatusOK, gin.H{
			"status":                "ok",
			"days":                  retention.Days,
			"max_db_size_mb":        retention.MaxDBSizeMB,
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
