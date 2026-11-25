package management

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	usageBackupKeyHeader       = "X-Usage-Backup-Key"
	usageBackupKeyBase64Header = "X-Usage-Backup-Key-Base64"
)

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
	c.Header("X-Usage-Queue-Dropped", strconv.FormatUint(queue.DroppedCount(), 10))
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
	if count > redisqueue.MaxPopCount {
		return 0, fmt.Errorf("count must not exceed %d", redisqueue.MaxPopCount)
	}
	return count, nil
}

// GetUsageStatistics returns the legacy request statistics snapshot. Callers
// that do not need per-request details can pass details=false for a bounded
// aggregate response.
func (h *Handler) GetUsageStatistics(c *gin.Context) {
	h.getUsageStatistics(c, usage.MaxLegacySnapshotEvents)
}

func (h *Handler) getUsageStatistics(c *gin.Context, maxEvents int64) {
	ctx := c.Request.Context()
	if ctx.Err() != nil {
		return
	}
	includeDetails := !strings.EqualFold(c.Query("details"), "false")
	detailsTruncated := false
	var snapshot usage.StatisticsSnapshot
	if h.usagePersister != nil {
		var err error
		if includeDetails {
			snapshot, err = h.usagePersister.QuerySnapshotLimitedContext(ctx, maxEvents)
			if errors.Is(err, usage.ErrLegacySnapshotTooLarge) {
				snapshot, err = h.usagePersister.QueryAggregateSnapshotContext(ctx)
				detailsTruncated = err == nil
			}
		} else {
			snapshot, err = h.usagePersister.QueryAggregateSnapshotContext(ctx)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query usage statistics"})
			return
		}
	} else if h.usageStats != nil {
		var err error
		if includeDetails {
			snapshot, err = h.usageStats.SnapshotLimitedContext(
				ctx,
				maxEvents,
				usage.MaxLegacySnapshotDetailBytes,
			)
			if errors.Is(err, usage.ErrLegacySnapshotTooLarge) {
				snapshot, err = h.usageStats.AggregateSnapshotContext(ctx)
				detailsTruncated = err == nil
			}
		} else {
			snapshot, err = h.usageStats.AggregateSnapshotContext(ctx)
		}
		if includeDetails && h.usageStats.DetailsTruncated() {
			detailsTruncated = true
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query usage statistics"})
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	payload := gin.H{
		"usage":           snapshot,
		"failed_requests": snapshot.FailureCount,
	}
	if detailsTruncated {
		payload["details_truncated"] = true
		payload["warning"] = "legacy usage details exceed the compatibility memory budget; " +
			"use /usage/summary and /usage/events for large datasets"
	}
	writeRedactedUsageJSON(
		c,
		http.StatusOK,
		payload,
		newUsageCredentialMasker(h.configSnapshot()),
	)
}

// ExportUsageStatistics returns a versioned, reversible backup of canonical
// SQLite events. Irreversible API key identities are encrypted with the authenticated
// management credential, or with X-Usage-Backup-Key when the caller supplies a portable
// backup key. Optional filters scope the exported event set.
func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	if h == nil || h.usagePersister == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage persister unavailable"})
		return
	}
	apiKeyIdentity := strings.TrimSpace(c.Query("api_key_identity"))
	if apiKeyIdentity != "" && apiKeyIdentity != "canonical" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid api_key_identity parameter"})
		return
	}
	canonicalAPIKeys := apiKeyIdentity == "canonical"

	speed, errSpeed := usage.ParseRequestSpeedFilter(c.Query("speed"))
	if errSpeed != nil || len(c.QueryArray("speed")) > 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid speed parameter"})
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
	apiKeyFilter, apiKeyFilters := newUsageAPIKeyPresenter(h.configSnapshot()).expandFilters(
		c.Query("api_key"),
		c.QueryArray("api_key"),
		canonicalAPIKeys,
	)
	filters := usage.EventFilters{
		Model:            c.Query("model"),
		APIKey:           apiKeyFilter,
		APIKeys:          apiKeyFilters,
		CanonicalAPIKeys: canonicalAPIKeys,
		Source:           c.Query("credential"),
		Speed:            speed,
	}
	events, err := h.usagePersister.QueryBackupEventsContext(c.Request.Context(), from, to, filters)
	if err != nil {
		if errors.Is(err, usage.ErrPortableBackupTooLarge) {
			writeUsageBackupTooLarge(c)
			return
		}
		if errors.Is(err, usage.ErrTimestampOutOfRange) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "from/to parameter is outside the supported time range"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query usage export"})
		return
	}
	secret, ok := usageBackupSecret(c, "usage backup encryption key unavailable")
	if !ok {
		return
	}
	exportedAt := time.Now().UTC()
	payload, err := usage.MarshalUsageBackup(events, secret, exportedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt usage export"})
		return
	}
	if speed != "" {
		filter, err := json.Marshal(gin.H{"speed_filter": speed})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode usage export"})
			return
		}
		// The backup is a JSON object; append the response marker without decoding its ciphertext.
		payload = append(payload[:len(payload)-1], ',')
		payload = append(payload, filter[1:]...)
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
		gin.H{
			"error": "usage backup exceeds the 100 MiB portable limit; narrow the export wit" +
				"h from, to, model, api_key, or credential filters",
		},
	)
}

func usageBackupSecret(c *gin.Context, missingError string) (string, bool) {
	if secret := strings.TrimSpace(c.GetHeader(usageBackupKeyHeader)); secret != "" {
		return secret, true
	}
	if encoded := strings.TrimSpace(c.GetHeader(usageBackupKeyBase64Header)); encoded != "" {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		secret := strings.TrimSpace(string(decoded))
		if err != nil || !utf8.Valid(decoded) || secret == "" {
			rejectMutation(c.Request.Context())
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid usage backup password encoding"})
			return "", false
		}
		return secret, true
	}
	secret := strings.TrimSpace(c.GetHeader("Authorization"))
	if secret != "" {
		if prefix, value, ok := strings.Cut(secret, " "); ok && strings.EqualFold(prefix, "Bearer") {
			secret = strings.TrimSpace(value)
		}
	} else {
		secret = strings.TrimSpace(c.GetHeader("X-Management-Key"))
	}
	if secret == "" && missingError != "" {
		rejectMutation(c.Request.Context())
		c.JSON(http.StatusUnauthorized, gin.H{"error": missingError, "code": "usage_backup_password_required"})
		return "", false
	}
	return secret, true
}

// ImportUsageStatistics merges a usage backup or supported legacy payload into
// SQLite. Encrypted backups require the password or key that created them.
func (h *Handler) ImportUsageStatistics(c *gin.Context) {
	if h == nil || h.usagePersister == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage persister unavailable"})
		return
	}
	ctx := c.Request.Context()
	if ctx.Err() != nil {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, usage.MaxPortableBackupBytes)
	data, err := handlers.ReadRequestBodyWithLimit(c, usage.MaxPortableBackupBytes)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		message := "failed to read request body"
		if strings.Contains(err.Error(), "request body too large") || strings.Contains(
			err.Error(), "http: request body too large",
		) {
			message = "import file too large: request body exceeds 100MB limit"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": message})
		return
	}
	if ctx.Err() != nil {
		return
	}

	missingError := ""
	if usage.IsUsageBackup(data) {
		missingError = "usage backup decryption key unavailable"
	}
	secret, ok := usageBackupSecret(c, missingError)
	if !ok {
		return
	}
	result, err := h.usagePersister.ImportContextWithSecret(ctx, data, secret)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		status := http.StatusInternalServerError
		if errors.Is(err, usage.ErrInvalidImport) {
			rejectMutation(ctx)
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	if result.Added > 0 {
		RecordMutationEffect(ctx, "usage.persisted", "usage")
	}
	if result.Warning != "" {
		failMutation(ctx)
	} else {
		CompleteMutation(ctx)
	}
	totalRequests, failureCount, err := h.usagePersister.QueryRequestCountsContext(ctx)
	if ctx.Err() != nil {
		return
	}
	writeUsageImportResult(c, result, totalRequests, failureCount, err)
}

func writeUsageImportResult(
	c *gin.Context,
	result usage.ImportResult,
	totalRequests int64,
	failureCount int64,
	queryErr error,
) {
	if c == nil {
		return
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		return
	}
	payload := gin.H{
		"added":   result.Added,
		"skipped": result.Skipped,
		"format":  result.Format,
	}
	warnings := make([]string, 0, 2)
	if result.Warning != "" {
		warnings = append(warnings, result.Warning)
	}
	if queryErr != nil {
		warnings = append(warnings, "usage imported, but totals could not be queried")
	} else {
		payload["total_requests"] = totalRequests
		payload["failed_requests"] = failureCount
	}
	if len(warnings) > 0 {
		payload["warning"] = strings.Join(warnings, "; ")
	}
	c.JSON(http.StatusOK, payload)
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
	sizeBytes, maxBytes, warningThresholdPct, warning, capped, err := h.usagePersister.DBSizeStatusWithError()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read usage database size"})
		return
	}
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
	if body.MaxDBSizeMB != nil && *body.MaxDBSizeMB > config.MaxStorageSizeMB {
		c.JSON(http.StatusBadRequest, gin.H{"error": "max_db_size_mb exceeds the supported range"})
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
	committed, ok := h.mutateConfig(
		c, func(next *config.Config) error {
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
		},
	)
	if !ok {
		return
	}
	retention := committed.UsageRetention
	h.usagePersister.SetRetention(retention)
	RecordMutationEffect(c.Request.Context(), "usage.runtime_applied", "usage")
	CompleteMutation(c.Request.Context())

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
	deleted, err := h.usagePersister.TrimWithCount()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if deleted > 0 {
		RecordMutationEffect(c.Request.Context(), "usage.deleted", "usage")
	}
	CompleteMutation(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"status": "ok", "deleted": deleted})
}

// TrimPreviewUsageStatistics returns a preview of what would be cleaned by Trim.
func (h *Handler) TrimPreviewUsageStatistics(c *gin.Context) {
	if h == nil || h.usagePersister == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage persister unavailable"})
		return
	}
	ctx := c.Request.Context()
	preview, err := h.usagePersister.TrimPreview(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, usage.ErrTrimPreviewUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		log.WithError(err).Warn("failed to read usage trim preview")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query usage trim preview"})
		return
	}
	c.JSON(http.StatusOK, preview)
}
