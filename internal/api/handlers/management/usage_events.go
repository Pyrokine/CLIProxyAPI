package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const defaultEventsPageSize = 50

type usageEventResponse struct {
	usage.FlatDetail
	APIKeyAlias string `json:"api_key_alias"`
}

func (r *usageEventResponse) UnmarshalJSON(data []byte) error {
	if r == nil {
		return nil
	}
	var detail usage.FlatDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		return err
	}
	var envelope struct {
		APIKeyAlias string `json:"api_key_alias"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	r.FlatDetail = detail
	r.APIKeyAlias = envelope.APIKeyAlias
	return nil
}

// eventsResponse is the paginated response for GET /usage/events.
type eventsResponse struct {
	Events     []usageEventResponse `json:"events"`
	Total      int                  `json:"total"`
	Page       int                  `json:"page"`
	PageSize   int                  `json:"page_size"`
	TotalPages int                  `json:"total_pages"`
}

type compactUsageEventTokens struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_tokens"`
	Cached     int64 `json:"cached_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
	Total      int64 `json:"total_tokens"`
}

type compactUsageEventResponse struct {
	Timestamp         time.Time               `json:"timestamp"`
	Model             string                  `json:"model"`
	Source            string                  `json:"source"`
	AuthIndex         string                  `json:"auth_index"`
	APIKey            string                  `json:"api_key"`
	APIKeyAlias       string                  `json:"api_key_alias"`
	Provider          string                  `json:"provider"`
	LatencyMs         int64                   `json:"latency_ms,omitempty"`
	RequestID         string                  `json:"request_id,omitempty"`
	RequestLogState   string                  `json:"request_log_state,omitempty"`
	RequestLogHomeIP  string                  `json:"request_log_home_ip,omitempty"`
	TimeToFirstByteMs int64                   `json:"time_to_first_byte_ms,omitempty"`
	TotalDurationMs   int64                   `json:"total_duration_ms,omitempty"`
	Completed         bool                    `json:"completed"`
	MetadataRecorded  bool                    `json:"metadata_recorded,omitempty"`
	Thinking          *usage.Thinking         `json:"thinking,omitempty"`
	Tokens            compactUsageEventTokens `json:"tokens"`
	Cost              float64                 `json:"cost"`
	CostStatus        usage.CostStatus        `json:"cost_status"`
	CostReason        string                  `json:"cost_reason,omitempty"`
	PricingReceipt    *usage.PricingReceipt   `json:"pricing_receipt,omitempty"`
	Failed            bool                    `json:"failed"`
}

type compactEventsResponse struct {
	Events     []compactUsageEventResponse `json:"events"`
	Total      int                         `json:"total"`
	Page       int                         `json:"page"`
	PageSize   int                         `json:"page_size"`
	TotalPages int                         `json:"total_pages"`
}

// GetUsageEvents returns paginated usage event details.
//
//	GET /v0/management/usage/events?from=...&to=...&page=1&page_size=50
//	    &model=...&source=...&search=...&sort=timestamp&order=desc
func (h *Handler) GetUsageEvents(c *gin.Context) {
	view := strings.TrimSpace(c.Query("view"))
	if view != "" && view != "compact" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid usage events view"})
		return
	}
	compact := view == "compact"

	if h.usagePersister == nil {
		if compact {
			c.JSON(
				http.StatusOK, compactEventsResponse{
					Events:   []compactUsageEventResponse{},
					Page:     1,
					PageSize: defaultEventsPageSize,
				},
			)
			return
		}
		c.JSON(
			http.StatusOK, eventsResponse{
				Events:   []usageEventResponse{},
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
	if pageSize > usage.MaxEventsPageSize {
		pageSize = usage.MaxEventsPageSize
	}

	sortField := c.DefaultQuery("sort", "timestamp")
	sortOrder := c.DefaultQuery("order", "desc")
	if sortOrder != "asc" && sortOrder != "desc" {
		sortOrder = "desc"
	}

	apiKeyPresenter := newUsageAPIKeyPresenter(h.configSnapshot())
	apiKeyFilter, apiKeyFilters := apiKeyPresenter.expandFilters(
		c.Query("api_key"),
		c.QueryArray("api_key"),
	)
	filters := usage.EventFilters{
		Model:   c.Query("model"),
		Source:  c.Query("source"),
		APIKey:  apiKeyFilter,
		APIKeys: apiKeyFilters,
		Status:  c.Query("status"),
		Search:  c.Query("search"),
	}

	queryEvents := h.usagePersister.QueryEventsContext
	if compact {
		queryEvents = h.usagePersister.QueryCompactEventsContext
	}
	events, total, err := queryEvents(
		c.Request.Context(),
		from,
		to,
		filters,
		page,
		pageSize,
		sortField,
		sortOrder == "desc",
	)
	if err != nil {
		switch {
		case errors.Is(err, usage.ErrEventsPageTooDeep):
			c.JSON(
				http.StatusBadRequest,
				gin.H{"error": "page exceeds the supported usage event offset limit"},
			)
			return
		case errors.Is(err, usage.ErrEventsPageTooLarge):
			c.JSON(
				http.StatusBadRequest,
				gin.H{"error": "usage event page exceeds the metadata byte limit; reduce page_size"},
			)
			return
		case errors.Is(err, usage.ErrTimestampOutOfRange):
			c.JSON(http.StatusBadRequest, gin.H{"error": "from/to parameter is outside the supported time range"})
			return
		}
		log.Errorf("usage: QueryEvents failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query events"})
		return
	}
	if events == nil {
		events = []usage.FlatDetail{}
	}
	totalPages := (total + pageSize - 1) / pageSize
	credentialMasker := newUsageCredentialMasker(h.configSnapshot())
	if compact {
		response := compactEventsResponse{
			Events:     compactUsageEventResponses(events, apiKeyPresenter, credentialMasker),
			Total:      total,
			Page:       page,
			PageSize:   pageSize,
			TotalPages: totalPages,
		}
		startedAt := time.Now()
		payload, errMarshal := json.Marshal(response)
		if errMarshal != nil {
			log.Errorf("usage: marshal compact events: %v", errMarshal)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode events"})
			return
		}
		log.Debugf("usage: compact events JSON marshal took %v", time.Since(startedAt))
		c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
		return
	}

	c.JSON(
		http.StatusOK, eventsResponse{
			Events:     maskUsageEventsWithPresenter(events, apiKeyPresenter, credentialMasker),
			Total:      total,
			Page:       page,
			PageSize:   pageSize,
			TotalPages: totalPages,
		},
	)
}

func compactUsageEventResponses(
	events []usage.FlatDetail,
	apiKeyPresenter usageAPIKeyPresenter,
	credentialMasker usageCredentialMasker,
) []compactUsageEventResponse {
	out := make([]compactUsageEventResponse, len(events))
	for i := range events {
		event := &events[i]
		presentation := apiKeyPresenter.present(event.APIKey)
		apiKey := presentation.display
		apiKeyAlias := presentation.alias
		out[i] = compactUsageEventResponse{
			Timestamp:         event.Timestamp,
			Model:             event.Model,
			Source:            credentialMasker.mask(event.Source),
			AuthIndex:         event.AuthIndex,
			APIKey:            apiKey,
			APIKeyAlias:       apiKeyAlias,
			Provider:          event.Provider,
			LatencyMs:         event.LatencyMs,
			RequestID:         event.RequestID,
			RequestLogState:   string(event.RequestLogState),
			RequestLogHomeIP:  event.RequestLogHomeIP,
			TimeToFirstByteMs: event.TimeToFirstByteMs,
			TotalDurationMs:   event.TotalDurationMs,
			Completed:         event.Completed,
			MetadataRecorded:  event.MetadataRecorded,
			Thinking:          event.Thinking,
			Tokens: compactUsageEventTokens{
				Input:      event.Tokens.InputTokens,
				Output:     event.Tokens.OutputTokens,
				Reasoning:  event.Tokens.ReasoningTokens,
				Cached:     event.Tokens.CachedTokens,
				CacheRead:  event.Tokens.CacheReadTokens,
				CacheWrite: event.Tokens.CacheWriteTokens,
				Total:      event.Tokens.TotalTokens,
			},
			Cost:           event.Cost,
			CostStatus:     event.CostStatus,
			CostReason:     event.CostReason,
			PricingReceipt: event.PricingReceipt,
			Failed:         event.Failed,
		}
	}
	return out
}

func maskUsageEvents(
	events []usage.FlatDetail,
	aliases map[string]string,
	credentialMasker usageCredentialMasker,
) []usageEventResponse {
	return maskUsageEventsWithPresenter(
		events,
		newUsageAPIKeyPresenterFromAliases(aliases),
		credentialMasker,
	)
}

func maskUsageEventsWithPresenter(
	events []usage.FlatDetail,
	apiKeyPresenter usageAPIKeyPresenter,
	credentialMasker usageCredentialMasker,
) []usageEventResponse {
	out := make([]usageEventResponse, len(events))
	for i := range events {
		out[i].FlatDetail = events[i]
		out[i].Source = credentialMasker.mask(out[i].Source)
		presentation := apiKeyPresenter.present(out[i].APIKey)
		out[i].APIKey = presentation.display
		out[i].APIKeyAlias = presentation.alias
	}
	return out
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
