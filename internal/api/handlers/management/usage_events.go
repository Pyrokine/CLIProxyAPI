package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const defaultEventsPageSize = 50

type usageEventResponse struct {
	usage.FlatDetail
	APIKeyAlias string `json:"api_key_alias"`
	RawAPIKey   string `json:"raw_api_key,omitempty"`
}

// noinspection GoMixedReceiverTypes
func (r usageEventResponse) MarshalJSON() ([]byte, error) {
	data, err := r.FlatDetail.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	alias, err := json.Marshal(r.APIKeyAlias)
	if err != nil {
		return nil, err
	}
	fields["api_key_alias"] = alias
	if r.RawAPIKey != "" {
		raw, err := json.Marshal(r.RawAPIKey)
		if err != nil {
			return nil, err
		}
		fields["raw_api_key"] = raw
	}
	return json.Marshal(fields)
}

// noinspection GoMixedReceiverTypes
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

type eventsPagination struct {
	Total           int    `json:"total"`
	Page            int    `json:"page"`
	PageSize        int    `json:"page_size"`
	TotalPages      int    `json:"total_pages"`
	AccessiblePages int    `json:"accessible_pages"`
	RequestIDFilter string `json:"request_id_filter,omitempty"`
	SpeedFilter     string `json:"speed_filter,omitempty"`
}

// eventsResponse is the paginated response for GET /usage/events.
type eventsResponse struct {
	Events []usageEventResponse `json:"events"`
	eventsPagination
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
	Timestamp              time.Time                  `json:"timestamp"`
	Model                  string                     `json:"model"`
	Source                 string                     `json:"source"`
	AuthIndex              string                     `json:"auth_index"`
	APIKey                 string                     `json:"api_key"`
	APIKeyAlias            string                     `json:"api_key_alias"`
	Provider               string                     `json:"provider"`
	LatencyMs              *int64                     `json:"latency_ms,omitempty"`
	RequestID              string                     `json:"request_id,omitempty"`
	RequestLogState        string                     `json:"request_log_state,omitempty"`
	RequestLogHomeIP       string                     `json:"request_log_home_ip,omitempty"`
	TimeToFirstByteMs      int64                      `json:"time_to_first_byte_ms,omitempty"`
	TotalDurationMs        int64                      `json:"total_duration_ms,omitempty"`
	Completed              bool                       `json:"completed"`
	MetadataRecorded       bool                       `json:"metadata_recorded,omitempty"`
	ReasoningEffort        string                     `json:"reasoning_effort,omitempty"`
	ServiceTier            string                     `json:"service_tier,omitempty"`
	RequestServiceTier     string                     `json:"request_service_tier,omitempty"`
	ResponseServiceTier    string                     `json:"response_service_tier,omitempty"`
	RequestSpeed           string                     `json:"request_speed,omitempty"`
	ResponseSpeed          string                     `json:"response_speed,omitempty"`
	CacheReadReported      bool                       `json:"cache_read_reported,omitempty"`
	CacheWriteReported     *bool                      `json:"cache_write_reported,omitempty"`
	BillingMetadata        *coreusage.BillingMetadata `json:"billing_metadata,omitempty"`
	Speed                  string                     `json:"speed"`
	ExecutorType           string                     `json:"executor_type,omitempty"`
	ClientModel            string                     `json:"client_model,omitempty"`
	UpstreamURL            string                     `json:"upstream_url,omitempty"`
	ResponseURL            string                     `json:"response_url,omitempty"`
	ResponseURLSource      string                     `json:"response_url_source,omitempty"`
	ResponseURLStatus      string                     `json:"response_url_status,omitempty"`
	UpstreamNextHopStatus  string                     `json:"upstream_next_hop_status,omitempty"`
	UpstreamModel          string                     `json:"upstream_model,omitempty"`
	ResponseModel          string                     `json:"response_model,omitempty"`
	ResponseProvider       string                     `json:"response_provider,omitempty"`
	ResponseProviderSource string                     `json:"response_provider_source,omitempty"`
	Thinking               *usage.Thinking            `json:"thinking,omitempty"`
	Tokens                 compactUsageEventTokens    `json:"tokens"`
	Cost                   float64                    `json:"cost"`
	CostStatus             usage.CostStatus           `json:"cost_status"`
	CostReason             string                     `json:"cost_reason,omitempty"`
	PricingReceipt         *usage.PricingReceipt      `json:"pricing_receipt,omitempty"`
	Failed                 bool                       `json:"failed"`
}

type compactEventsResponse struct {
	Events []compactUsageEventResponse `json:"events"`
	eventsPagination
}

// GetUsageEvents returns paginated usage event details.
//
//	GET /v0/management/usage/events?from=...&to=...&page=1&page_size=50
//	    &model=...&source=...&speed=...&search=...&request_id=...&sort=timestamp&order=desc
//
// export=true includes sealed canonical data for re-import using the same
// authenticated management credential or an explicit X-Usage-Backup-Key.
func (h *Handler) GetUsageEvents(c *gin.Context) {
	view := strings.TrimSpace(c.Query("view"))
	if view != "" && view != "compact" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid usage events view"})
		return
	}
	apiKeyIdentity := strings.TrimSpace(c.Query("api_key_identity"))
	if apiKeyIdentity != "" && apiKeyIdentity != "canonical" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid api_key_identity parameter"})
		return
	}
	canonicalAPIKeys := apiKeyIdentity == "canonical"

	compact := view == "compact"
	export := c.Query("export") == "true"
	if compact && export {
		c.JSON(http.StatusBadRequest, gin.H{"error": "event export requires the full view"})
		return
	}
	if export && usageBackupSecret(c) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "usage event export key unavailable"})
		return
	}

	speed, errSpeed := usage.ParseRequestSpeedFilter(c.Query("speed"))
	if errSpeed != nil || len(c.QueryArray("speed")) > 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid speed parameter"})
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
		canonicalAPIKeys,
	)
	filters := usage.EventFilters{
		Model:            c.Query("model"),
		Source:           c.Query("source"),
		APIKey:           apiKeyFilter,
		APIKeys:          apiKeyFilters,
		CanonicalAPIKeys: canonicalAPIKeys,
		Status:           c.Query("status"),
		Speed:            speed,
		Search:           c.Query("search"),
		RequestID:        c.Query("request_id"),
	}

	queryEvents := h.usageStats.QueryEventsContext
	if h.usagePersister != nil {
		queryEvents = h.usagePersister.QueryEventsContext
		if compact {
			queryEvents = h.usagePersister.QueryCompactEventsContext
		}
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
		case errors.Is(err, usage.ErrSummaryPrecisionUnavailable):
			c.JSON(
				http.StatusConflict, gin.H{
					"error":   "summary_precision_unavailable",
					"message": "usage details are incomplete; enable usage persistence for complete history",
				},
			)
			return
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
	pagination := eventsPagination{
		Total:           total,
		Page:            page,
		PageSize:        pageSize,
		TotalPages:      totalPages,
		AccessiblePages: min(totalPages, usage.MaxEventsPage(pageSize)),
		RequestIDFilter: filters.RequestID,
		SpeedFilter:     speed,
	}
	credentialMasker := newUsageCredentialMasker(h.configSnapshot())
	if compact {
		response := compactEventsResponse{
			Events:           compactUsageEventResponses(events, apiKeyPresenter, credentialMasker),
			eventsPagination: pagination,
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

	if export {
		if err := usage.PrepareUsageEventExport(events, usageBackupSecret(c)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to seal usage event export"})
			return
		}
	}
	c.JSON(
		http.StatusOK, eventsResponse{
			Events:           maskUsageEventsWithPresenter(events, apiKeyPresenter, credentialMasker),
			eventsPagination: pagination,
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
		speed, _ := usage.ObservedRequestSpeed(event)
		out[i] = compactUsageEventResponse{
			Timestamp:              event.Timestamp,
			Model:                  event.Model,
			Source:                 credentialMasker.mask(event.Source),
			AuthIndex:              event.AuthIndex,
			APIKey:                 apiKey,
			APIKeyAlias:            apiKeyAlias,
			Provider:               event.Provider,
			LatencyMs:              event.RecordedLatencyMs(),
			RequestID:              event.RequestID,
			RequestLogState:        string(event.RequestLogState),
			RequestLogHomeIP:       event.RequestLogHomeIP,
			TimeToFirstByteMs:      event.TimeToFirstByteMs,
			TotalDurationMs:        event.TotalDurationMs,
			Completed:              event.Completed,
			MetadataRecorded:       event.MetadataRecorded,
			ReasoningEffort:        event.ReasoningEffort,
			ClientModel:            event.ClientModel,
			UpstreamURL:            event.UpstreamURL,
			ResponseURL:            event.ResponseURL,
			ResponseURLSource:      event.ResponseURLSource,
			ResponseURLStatus:      event.ResponseURLStatus,
			UpstreamNextHopStatus:  event.UpstreamNextHopStatus,
			UpstreamModel:          event.UpstreamModel,
			ResponseModel:          event.ResponseModel,
			ResponseProvider:       event.ResponseProvider,
			ResponseProviderSource: event.ResponseProviderSource,
			RequestSpeed:           event.RequestSpeed,
			ResponseSpeed:          event.ResponseSpeed,
			CacheReadReported:      event.CacheReadReported,
			CacheWriteReported:     coreusage.NormalizeCacheWriteReported(event.CacheWriteReported, 0),
			BillingMetadata:        coreusage.CloneBillingMetadata(event.BillingMetadata),
			Speed:                  speed,
			ExecutorType:           event.ExecutorType,
			ServiceTier:            event.ServiceTier,
			RequestServiceTier:     event.RequestServiceTier,
			ResponseServiceTier:    event.ResponseServiceTier,
			Thinking:               event.Thinking,
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
		if events[i].ImportData != "" {
			out[i].RawAPIKey = events[i].APIKey
		}
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
