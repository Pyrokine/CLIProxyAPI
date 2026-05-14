// Last compiled: 2026-04-27
// Author: pyro

package management

import (
	"net/http"
	"strconv"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	defaultEventsPageSize = 50
	maxEventsPageSize     = 500
)

// eventsResponse is the paginated response for GET /usage/events.
type eventsResponse struct {
	Events     []usage.FlatDetail `json:"events"`
	Total      int                `json:"total"`
	Page       int                `json:"page"`
	PageSize   int                `json:"page_size"`
	TotalPages int                `json:"total_pages"`
}

// GetUsageEvents returns paginated usage event details.
//
//	GET /v0/management/usage/events?from=...&to=...&page=1&page_size=50
//	    &model=...&source=...&search=...&sort=timestamp&order=desc
func (h *Handler) GetUsageEvents(c *gin.Context) {
	if h.usagePersister == nil {
		c.JSON(
			http.StatusOK, eventsResponse{
				Events:   []usage.FlatDetail{},
				Page:     1,
				PageSize: defaultEventsPageSize,
			},
		)
		return
	}

	now := time.Now().UTC()
	from, errFrom := parseTimeParam(c.Query("from"), now.Truncate(24*time.Hour))
	to, errTo := parseTimeParam(c.Query("to"), now)
	if errFrom != nil || errTo != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from/to parameter"})
		return
	}
	if from.After(to) {
		from, to = to, from
	}

	page := max(intParam(c, "page", 1), 1)
	pageSize := intParam(c, "page_size", defaultEventsPageSize)
	if pageSize < 1 {
		pageSize = defaultEventsPageSize
	}
	if pageSize > maxEventsPageSize {
		pageSize = maxEventsPageSize
	}

	sortField := c.DefaultQuery("sort", "timestamp")
	sortOrder := c.DefaultQuery("order", "desc")
	if sortOrder != "asc" && sortOrder != "desc" {
		sortOrder = "desc"
	}

	filters := usage.EventFilters{
		Model:  c.Query("model"),
		Source: c.Query("source"),
		APIKey: c.Query("api_key"),
		Status: c.Query("status"),
		Search: c.Query("search"),
	}

	events, total, err := h.usagePersister.QueryEvents(
		from, to, filters, page, pageSize, sortField, sortOrder == "desc",
	)
	if err != nil {
		log.Errorf("usage: QueryEvents failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query events"})
		return
	}
	if events == nil {
		events = []usage.FlatDetail{}
	}

	totalPages := 0
	if pageSize > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}

	c.JSON(
		http.StatusOK, eventsResponse{
			Events:     events,
			Total:      total,
			Page:       page,
			PageSize:   pageSize,
			TotalPages: totalPages,
		},
	)
}

func intParam(c *gin.Context, name string, fallback int) int {
	v := c.Query(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
