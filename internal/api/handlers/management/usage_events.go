// Last compiled: 2026-03-23
// Author: pyro

package management

import (
	"net/http"
	"strconv"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
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
		Search: c.Query("search"),
	}

	// Collect today's events (in memory, already filtered and sorted).
	var todayFiltered []usage.FlatDetail
	if todayStore := h.usagePersister.TodayStore(); todayStore != nil {
		todayFiltered, _ = todayStore.Query(from, to, 0, 0, filters, sortField, sortOrder == "desc")
	}

	// Collect historical events from DetailStore (on disk, already filtered and sorted).
	var historicalFiltered []usage.FlatDetail
	if detailStore := h.usagePersister.DetailStore(); detailStore != nil {
		historicalFiltered, _, _ = detailStore.QueryRange(from, to, 0, 0, filters, sortField, sortOrder == "desc")
	}

	// Merge the two pre-sorted slices and paginate.
	merged := usage.MergeSorted(todayFiltered, historicalFiltered, sortField, sortOrder == "desc")
	total := len(merged)

	start := (page - 1) * pageSize
	if start >= total {
		c.JSON(
			http.StatusOK, eventsResponse{
				Events:     []usage.FlatDetail{},
				Total:      total,
				Page:       page,
				PageSize:   pageSize,
				TotalPages: (total + pageSize - 1) / pageSize,
			},
		)
		return
	}
	end := min(start+pageSize, total)

	c.JSON(
		http.StatusOK, eventsResponse{
			Events:     merged[start:end],
			Total:      total,
			Page:       page,
			PageSize:   pageSize,
			TotalPages: (total + pageSize - 1) / pageSize,
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
