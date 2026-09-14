package management

import (
	"bufio"
	"bytes"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/gin-gonic/gin"
)

func (h *Handler) resolveRequestLogByID(c *gin.Context, dir, requestID string) {
	if h != nil && h.usagePersister != nil {
		artifact, found, errArtifact := h.usagePersister.QueryRequestLogArtifactContext(
			c.Request.Context(),
			requestID,
		)
		if errArtifact != nil {
			c.JSON(
				http.StatusInternalServerError,
				gin.H{"error": fmt.Sprintf("failed to query request log state: %v", errArtifact)},
			)
			return
		}
		if found {
			h.serveRequestLogArtifact(c, dir, artifact)
			return
		}
	}

	matches, errMatches := findLegacyRequestLogFiles(dir, requestID)
	if errMatches != nil {
		if os.IsNotExist(errMatches) {
			h.writeUnknownRequestLogState(c, requestID)
			return
		}
		c.JSON(
			http.StatusInternalServerError,
			gin.H{"error": fmt.Sprintf("failed to list request logs: %v", errMatches)},
		)
		return
	}
	if len(matches) > 1 {
		writeRequestLogState(
			c,
			http.StatusConflict,
			logging.RequestLogArtifact{
				RequestID: requestID,
				State:     logging.RequestLogLegacyUnknown,
				Reason:    "multiple legacy request logs match this request ID",
			},
			"multiple legacy request logs match the given request ID",
		)
		return
	}
	if len(matches) == 1 {
		artifact := logging.PrepareRequestLogArtifact(
			logging.RequestLogArtifact{
				RequestID: requestID,
				State:     logging.RequestLogAvailable,
				Location:  logging.RequestLogLocationLocal,
				FileName:  matches[0],
			},
		)
		if h != nil && h.usagePersister != nil {
			h.usagePersister.SubmitRequestLogArtifact(artifact)
		}
		h.serveRequestLogArtifact(c, dir, artifact)
		return
	}
	h.writeUnknownRequestLogState(c, requestID)
}

func (h *Handler) writeUnknownRequestLogState(c *gin.Context, requestID string) {
	if h != nil && h.usagePersister != nil {
		exists, errExists := h.usagePersister.HasEventRequestIDContext(c.Request.Context(), requestID)
		if errExists != nil {
			c.JSON(
				http.StatusInternalServerError,
				gin.H{"error": fmt.Sprintf("failed to query request ID: %v", errExists)},
			)
			return
		}
		if exists {
			writeRequestLogState(
				c,
				http.StatusConflict,
				logging.RequestLogArtifact{
					RequestID: requestID,
					State:     logging.RequestLogLegacyUnknown,
					Reason:    "legacy event has no request log lifecycle record",
				},
				"request log state is unavailable for this legacy event",
			)
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "unknown request ID"})
}

func (h *Handler) serveRequestLogArtifact(
	c *gin.Context,
	dir string,
	artifact logging.RequestLogArtifact,
) {
	switch artifact.State {
	case logging.RequestLogPending:
		c.Header("Retry-After", "1")
		writeRequestLogState(c, http.StatusAccepted, artifact, "request log is still being published")
	case logging.RequestLogAvailable:
		switch artifact.Location {
		case logging.RequestLogLocationLocal:
			h.serveLocalRequestLogArtifact(c, dir, artifact)
		case logging.RequestLogLocationHome:
			h.serveHomeRequestLogArtifact(c, artifact)
		default:
			writeRequestLogState(
				c,
				http.StatusInternalServerError,
				artifact,
				"request log has an invalid storage location",
			)
		}
	case logging.RequestLogExpired:
		writeRequestLogState(c, http.StatusGone, artifact, "request log has expired")
	case logging.RequestLogDisabled:
		writeRequestLogState(c, http.StatusConflict, artifact, "request logging was disabled")
	case logging.RequestLogNotCaptured:
		writeRequestLogState(c, http.StatusConflict, artifact, "request log was not captured")
	case logging.RequestLogPublishFailed:
		writeRequestLogState(c, http.StatusInternalServerError, artifact, "request log could not be published")
	case logging.RequestLogLegacyUnknown:
		writeRequestLogState(c, http.StatusConflict, artifact, "request log state is unavailable for this legacy event")
	default:
		writeRequestLogState(c, http.StatusInternalServerError, artifact, "request log has an invalid lifecycle state")
	}
}

func (h *Handler) serveLocalRequestLogArtifact(
	c *gin.Context,
	dir string,
	artifact logging.RequestLogArtifact,
) {
	fileName := strings.TrimSpace(artifact.FileName)
	if fileName == "" || filepath.Base(fileName) != fileName {
		writeRequestLogState(c, http.StatusInternalServerError, artifact, "request log has an invalid file name")
		return
	}
	dirAbs, errAbs := filepath.Abs(dir)
	if errAbs != nil {
		c.JSON(
			http.StatusInternalServerError,
			gin.H{"error": fmt.Sprintf("failed to resolve log directory: %v", errAbs)},
		)
		return
	}
	fullPath := filepath.Join(dirAbs, fileName)
	file, errOpen := openLogFileNoFollow(fullPath)
	if errOpen != nil {
		if os.IsNotExist(errOpen) {
			h.expireRequestLogArtifact(c, artifact, "request log file no longer exists")
			return
		}
		if isLogSymlinkError(errOpen) {
			writeRequestLogState(c, http.StatusBadRequest, artifact, "request log file is invalid")
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to open log file: %v", errOpen)})
		return
	}
	_ = file.Close()

	if linesParam := c.Query("lines"); linesParam != "" {
		h.previewLogFile(c, dirAbs, fileName, requestLogPreviewLines(linesParam))
		return
	}
	h.serveLogFile(c, dirAbs, fileName)
}

func (h *Handler) serveHomeRequestLogArtifact(c *gin.Context, artifact logging.RequestLogArtifact) {
	requestedHomeIP := strings.TrimSpace(c.Query("home_ip"))
	if requestedHomeIP != "" && artifact.HomeIP != "" && requestedHomeIP != artifact.HomeIP {
		writeRequestLogState(c, http.StatusConflict, artifact, "request log belongs to a different Home service")
		return
	}
	client := h.currentHomeClient()
	if client == nil {
		writeRequestLogState(c, http.StatusServiceUnavailable, artifact, "Home request log storage is unavailable")
		return
	}
	content, found, errLookup := client.FindRequestLog(c.Request.Context(), artifact.RequestID)
	if errLookup != nil {
		writeRequestLogState(
			c,
			http.StatusBadGateway,
			artifact,
			fmt.Sprintf("failed to query Home request log storage: %v", errLookup),
		)
		return
	}
	if !found {
		h.expireRequestLogArtifact(c, artifact, "Home request log entry no longer exists")
		return
	}
	if linesParam := c.Query("lines"); linesParam != "" {
		previewRequestLogContent(c, content, requestLogPreviewLines(linesParam))
		return
	}
	fileName := "request-" + artifact.RequestID + ".log"
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": fileName}))
	http.ServeContent(c.Writer, c.Request, fileName, artifact.UpdatedAt, bytes.NewReader([]byte(content)))
}

func (h *Handler) expireRequestLogArtifact(
	c *gin.Context,
	artifact logging.RequestLogArtifact,
	reason string,
) {
	expired := logging.PrepareRequestLogArtifact(
		logging.RequestLogArtifact{
			RequestID: artifact.RequestID,
			State:     logging.RequestLogExpired,
			Reason:    reason,
		},
	)
	if h != nil && h.usagePersister != nil {
		h.usagePersister.SubmitRequestLogArtifact(expired)
	}
	writeRequestLogState(c, http.StatusGone, expired, "request log has expired")
}

func writeRequestLogState(
	c *gin.Context,
	status int,
	artifact logging.RequestLogArtifact,
	message string,
) {
	response := gin.H{
		"error":             message,
		"request_id":        artifact.RequestID,
		"request_log_state": artifact.State,
	}
	if artifact.Location != "" {
		response["request_log_location"] = artifact.Location
	}
	if artifact.HomeIP != "" {
		response["request_log_home_ip"] = artifact.HomeIP
	}
	if artifact.Reason != "" {
		response["reason"] = artifact.Reason
	}
	if artifact.Revision > 0 {
		response["request_log_revision"] = artifact.Revision
	}
	if !artifact.UpdatedAt.IsZero() {
		response["request_log_updated_at"] = artifact.UpdatedAt
	}
	c.JSON(status, response)
}

func findLegacyRequestLogFiles(dir, requestID string) ([]string, error) {
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		return nil, errRead
	}
	suffix := "-" + requestID + ".log"
	matches := make([]string, 0, 1)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), suffix) {
			matches = append(matches, entry.Name())
		}
	}
	sort.Strings(matches)
	return matches, nil
}

func requestLogPreviewLines(raw string) int {
	maxLines, err := strconv.Atoi(raw)
	if err != nil || maxLines < 1 {
		return 100
	}
	return min(maxLines, maxLogLinesLimit)
}

func previewRequestLogContent(c *gin.Context, content string, maxLines int) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 0, logScannerInitialBuffer), logScannerMaxBuffer)
	ring := make([]string, maxLines)
	ringIndex := 0
	totalLines := 0
	for scanner.Scan() {
		ring[ringIndex%maxLines] = scanner.Text()
		ringIndex++
		totalLines++
	}
	if errScan := scanner.Err(); errScan != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to read request log: %v", errScan)})
		return
	}
	count := min(totalLines, maxLines)
	lines := make([]string, count)
	start := ringIndex - count
	for i := range count {
		lines[i] = ring[(start+i)%maxLines]
	}
	c.JSON(
		http.StatusOK,
		gin.H{
			"content":     strings.Join(lines, "\n"),
			"truncated":   totalLines > maxLines,
			"total_lines": totalLines,
		},
	)
}
