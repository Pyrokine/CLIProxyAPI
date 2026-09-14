package management

import (
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// summaryTokens holds a token breakdown.
type summaryTokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Cached     int64 `json:"cached"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Reasoning  int64 `json:"reasoning"`
	Total      int64 `json:"total"`
}

type summaryCostCoverageCount struct {
	Requests int64 `json:"requests"`
	Tokens   int64 `json:"tokens"`
}

type summaryCostCoverage struct {
	Complete summaryCostCoverageCount `json:"complete"`
	Partial  summaryCostCoverageCount `json:"partial"`
	Unpriced summaryCostCoverageCount `json:"unpriced"`
}

// summaryTotals holds the global aggregation.
type summaryTotals struct {
	Requests           int64               `json:"requests"`
	Success            int64               `json:"success"`
	Failure            int64               `json:"failure"`
	Tokens             summaryTokens       `json:"tokens"`
	Cost               float64             `json:"cost"`
	CostCoverage       summaryCostCoverage `json:"cost_coverage"`
	AverageLatencyMs   *float64            `json:"average_latency_ms,omitempty"`
	TotalLatencyMs     *int64              `json:"total_latency_ms,omitempty"`
	LatencySampleCount int64               `json:"latency_sample_count,omitempty"`
}

// summaryModelStats holds per-model aggregation.
type summaryModelStats struct {
	Requests           int64         `json:"requests"`
	Success            int64         `json:"success"`
	Failure            int64         `json:"failure"`
	Tokens             summaryTokens `json:"tokens"`
	Cost               float64       `json:"cost"`
	AverageLatencyMs   *float64      `json:"average_latency_ms,omitempty"`
	TotalLatencyMs     *int64        `json:"total_latency_ms,omitempty"`
	LatencySampleCount int64         `json:"latency_sample_count,omitempty"`
}

// summaryCredentialStats holds per-credential aggregation.
// Provider/Source are surfaced so the frontend can render dropdowns and
// stat cards without parsing the map key. FilterKey is a stable, type-aware
// query value; map keys add a readable source-kind suffix for chart labels.
type summaryCredentialStats struct {
	Success    int64  `json:"success"`
	Failure    int64  `json:"failure"`
	Provider   string `json:"provider,omitempty"`
	Source     string `json:"source,omitempty"`
	SourceKind string `json:"source_kind,omitempty"`
	FilterKey  string `json:"filter_key,omitempty"`
}

type summaryProviderStats struct {
	Provider  string  `json:"provider"`
	Requests  int64   `json:"requests"`
	Success   int64   `json:"success"`
	Failure   int64   `json:"failure"`
	ErrorRate float64 `json:"error_rate"`
}

// summaryTimePoint represents a single time bucket.
type summaryTimePoint struct {
	Time     string        `json:"time"`
	Requests int64         `json:"requests"`
	Success  int64         `json:"success"`
	Failure  int64         `json:"failure"`
	Tokens   summaryTokens `json:"tokens"`
	Cost     float64       `json:"cost"`
	HasCost  bool          `json:"has_cost"`
}

// summaryAPIKeyStats holds per-API-key aggregation.
type summaryAPIKeyStats struct {
	Requests           int64         `json:"requests"`
	Success            int64         `json:"success"`
	Failure            int64         `json:"failure"`
	Tokens             summaryTokens `json:"tokens"`
	Cost               float64       `json:"cost"`
	AverageLatencyMs   *float64      `json:"average_latency_ms,omitempty"`
	TotalLatencyMs     *int64        `json:"total_latency_ms,omitempty"`
	LatencySampleCount int64         `json:"latency_sample_count,omitempty"`
}

type summaryCredentialAccumulator struct {
	Success    int64
	Failure    int64
	Provider   string
	Source     string
	SourceKind string
	FilterKey  string
}

type summaryAPIKeyAccumulator struct {
	Requests       int64
	Success        int64
	Failure        int64
	Tokens         summaryTokens
	Cost           float64
	TotalLatencyMs int64
	LatencySamples int64
}

type summaryTimePointAccumulator struct {
	Requests int64
	Success  int64
	Failure  int64
	Tokens   summaryTokens
	Cost     float64
}

// summaryResponse is the full response for GET /usage/summary.
type summaryResponse struct {
	Period                 summaryPeriod                      `json:"period"`
	Totals                 summaryTotals                      `json:"totals"`
	ByModel                map[string]*summaryModelStats      `json:"by_model"`
	ByCredential           map[string]*summaryCredentialStats `json:"by_credential"`
	ByProvider             map[string]*summaryProviderStats   `json:"by_provider"`
	ByAPIKey               map[string]*summaryAPIKeyStats     `json:"by_api_key"`
	ByAuthIndex            map[string]*summaryAPIKeyStats     `json:"by_auth_index,omitempty"`
	TimeSeries             []summaryTimePoint                 `json:"time_series"`
	TimeSeriesByModel      map[string][]summaryTimePoint      `json:"time_series_by_model"`
	TimeSeriesByCredential map[string][]summaryTimePoint      `json:"time_series_by_credential"`
	TimeSeriesByAPIKey     map[string][]summaryTimePoint      `json:"time_series_by_api_key"`
	TimeSeriesByAuthIndex  map[string][]summaryTimePoint      `json:"time_series_by_auth_index,omitempty"`
}

type summaryPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// GetUsageSummary returns aggregated usage data for dashboard rendering.
//
//	GET /v0/management/usage/summary?from=...&to=...&granularity=hourly|daily
func (h *Handler) GetUsageSummary(c *gin.Context) {
	start := time.Now()
	now := time.Now().UTC()

	from, errFrom := parseTimeParam(c.Query("from"), now.AddDate(0, 0, -30))
	to, errTo := parseTimeParam(c.Query("to"), now)
	if errFrom != nil || errTo != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from/to parameter"})
		return
	}
	if from.After(to) {
		from, to = to, from
	}

	granularity := strings.ToLower(strings.TrimSpace(c.DefaultQuery("granularity", "hourly")))
	if granularity != "hourly" && granularity != "daily" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid granularity parameter"})
		return
	}
	authIndexGranularity := strings.ToLower(strings.TrimSpace(c.Query("auth_index_granularity")))
	if authIndexGranularity != "" && authIndexGranularity != "10m" &&
		authIndexGranularity != "hourly" && authIndexGranularity != "daily" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid auth_index_granularity parameter"})
		return
	}

	modelFilter := c.Query("model")
	apiKeyFilter := c.Query("api_key")
	apiKeyFilters := c.QueryArray("api_key")
	credentialFilter := c.Query("credential")
	includeGroups := c.DefaultQuery("groups", "all") != "none"
	includeAuthIndex := strings.EqualFold(strings.TrimSpace(c.Query("auth_index_groups")), "true")

	if h.usagePersister == nil {
		c.JSON(http.StatusOK, emptySummaryResponse(from, to))
		return
	}

	apiKeyPresenter := newUsageAPIKeyPresenter(h.configSnapshot())
	apiKeyFilter, apiKeyFilters = apiKeyPresenter.expandFilters(apiKeyFilter, apiKeyFilters)
	filters := usage.EventFilters{
		Model:   modelFilter,
		Source:  credentialFilter,
		APIKey:  apiKeyFilter,
		APIKeys: apiKeyFilters,
	}
	var qr usage.QueryResult
	var err error
	if includeAuthIndex {
		qr, err = h.usagePersister.QueryWithAuthIndexGranularity(
			from,
			to,
			filters,
			granularity,
			authIndexGranularity,
			includeGroups,
		)
	} else {
		qr, err = h.usagePersister.Query(from, to, filters, granularity, includeGroups)
	}
	if err != nil {
		switch {
		case errors.Is(err, usage.ErrSummaryQueryTooLarge):
			c.JSON(
				http.StatusBadRequest,
				gin.H{"error": "summary range or dimension count exceeds the supported resource limit"},
			)
			return
		case errors.Is(err, usage.ErrTimestampOutOfRange):
			c.JSON(http.StatusBadRequest, gin.H{"error": "from/to parameter is outside the supported time range"})
			return
		}
		log.Errorf("usage: GetUsageSummary query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query summary"})
		return
	}

	resp := buildSummaryResponse(
		from,
		to,
		qr,
		newUsageCredentialMasker(h.configSnapshot()),
		apiKeyPresenter,
	)

	log.Debugf(
		"usage: GetUsageSummary took %v (from=%s, to=%s)",
		time.Since(start), from.Format(time.RFC3339), to.Format(time.RFC3339),
	)
	c.JSON(http.StatusOK, resp)
}

func emptySummaryResponse(from, to time.Time) summaryResponse {
	return summaryResponse{
		Period: summaryPeriod{
			From: from.Format(time.RFC3339),
			To:   to.Format(time.RFC3339),
		},
		ByModel:                make(map[string]*summaryModelStats),
		ByCredential:           make(map[string]*summaryCredentialStats),
		ByProvider:             make(map[string]*summaryProviderStats),
		ByAPIKey:               make(map[string]*summaryAPIKeyStats),
		ByAuthIndex:            make(map[string]*summaryAPIKeyStats),
		TimeSeriesByModel:      make(map[string][]summaryTimePoint),
		TimeSeriesByCredential: make(map[string][]summaryTimePoint),
		TimeSeriesByAPIKey:     make(map[string][]summaryTimePoint),
		TimeSeriesByAuthIndex:  make(map[string][]summaryTimePoint),
	}
}

func buildSummaryResponse(
	from,
	to time.Time,
	qr usage.QueryResult,
	credentialMasker usageCredentialMasker,
	apiKeyPresenter usageAPIKeyPresenter,
) summaryResponse {
	totalAverageLatency, totalLatency, totalLatencySamples := latencyResponseFields(
		qr.Totals.Latency.TotalMs,
		qr.Totals.Latency.SampleCount,
	)
	resp := summaryResponse{
		Period: summaryPeriod{
			From: from.Format(time.RFC3339),
			To:   to.Format(time.RFC3339),
		},
		Totals: summaryTotals{
			Requests: qr.Totals.Requests,
			Success:  qr.Totals.Success,
			Failure:  qr.Totals.Failure,
			Tokens: summaryTokens{
				Input:      qr.Totals.Tokens.InputTokens,
				Output:     qr.Totals.Tokens.OutputTokens,
				Cached:     qr.Totals.Tokens.CachedTokens,
				CacheRead:  qr.Totals.Tokens.CacheReadTokens,
				CacheWrite: qr.Totals.Tokens.CacheWriteTokens,
				Reasoning:  qr.Totals.Tokens.ReasoningTokens,
				Total:      qr.Totals.Tokens.TotalTokens,
			},
			Cost: roundCost(qr.Totals.Cost),
			CostCoverage: summaryCostCoverage{
				Complete: summaryCostCoverageCount{
					Requests: qr.Totals.CostCoverage.Complete.Requests,
					Tokens:   qr.Totals.CostCoverage.Complete.Tokens,
				},
				Partial: summaryCostCoverageCount{
					Requests: qr.Totals.CostCoverage.Partial.Requests,
					Tokens:   qr.Totals.CostCoverage.Partial.Tokens,
				},
				Unpriced: summaryCostCoverageCount{
					Requests: qr.Totals.CostCoverage.Unpriced.Requests,
					Tokens:   qr.Totals.CostCoverage.Unpriced.Tokens,
				},
			},
			AverageLatencyMs:   totalAverageLatency,
			TotalLatencyMs:     totalLatency,
			LatencySampleCount: totalLatencySamples,
		},
		ByModel:                make(map[string]*summaryModelStats, len(qr.ByModel)),
		ByCredential:           make(map[string]*summaryCredentialStats, len(qr.ByCredential)),
		ByProvider:             make(map[string]*summaryProviderStats),
		ByAPIKey:               make(map[string]*summaryAPIKeyStats, len(qr.ByAPIKey)),
		ByAuthIndex:            make(map[string]*summaryAPIKeyStats, len(qr.ByAuthIndex)),
		TimeSeriesByModel:      make(map[string][]summaryTimePoint, len(qr.TimeSeriesByModel)),
		TimeSeriesByCredential: make(map[string][]summaryTimePoint, len(qr.TimeSeriesByCredential)),
		TimeSeriesByAPIKey:     make(map[string][]summaryTimePoint, len(qr.TimeSeriesByAPIKey)),
		TimeSeriesByAuthIndex:  make(map[string][]summaryTimePoint, len(qr.TimeSeriesByAuthIndex)),
	}

	for model, ms := range qr.ByModel {
		averageLatency, totalLatency, latencySamples := latencyResponseFields(
			ms.Latency.TotalMs,
			ms.Latency.SampleCount,
		)
		resp.ByModel[model] = &summaryModelStats{
			Requests: ms.Requests,
			Success:  ms.Success,
			Failure:  ms.Failure,
			Tokens: summaryTokens{
				Input:      ms.Tokens.InputTokens,
				Output:     ms.Tokens.OutputTokens,
				Cached:     ms.Tokens.CachedTokens,
				CacheRead:  ms.Tokens.CacheReadTokens,
				CacheWrite: ms.Tokens.CacheWriteTokens,
				Reasoning:  ms.Tokens.ReasoningTokens,
				Total:      ms.Tokens.TotalTokens,
			},
			Cost:               roundCost(ms.Cost),
			AverageLatencyMs:   averageLatency,
			TotalLatencyMs:     totalLatency,
			LatencySampleCount: latencySamples,
		}
	}

	credentialDisplayKeys := make(map[string]string, len(qr.ByCredential))
	credentialAccumulators := make(
		map[string]*summaryCredentialAccumulator,
		len(qr.ByCredential),
	)
	for cred, cs := range qr.ByCredential {
		displaySource, kind := credentialMasker.describe(cs.Source)
		displayKey := summaryCredentialMapKey(
			cs.Provider,
			usage.DisambiguateCredentialSource(displaySource, kind),
		)
		filterKey := summaryCredentialMapKey(
			cs.Provider,
			usage.EncodeCredentialFilter(displaySource, kind),
		)
		credentialDisplayKeys[cred] = displayKey
		accumulator := credentialAccumulators[displayKey]
		if accumulator == nil {
			accumulator = &summaryCredentialAccumulator{
				Provider:   cs.Provider,
				Source:     displaySource,
				SourceKind: string(kind),
				FilterKey:  filterKey,
			}
			credentialAccumulators[displayKey] = accumulator
		}
		accumulator.Success += cs.Success
		accumulator.Failure += cs.Failure

		provider := strings.ToLower(strings.TrimSpace(cs.Provider))
		if provider == "" {
			provider = "unknown"
		}
		providerStats := resp.ByProvider[provider]
		if providerStats == nil {
			providerStats = &summaryProviderStats{Provider: provider}
			resp.ByProvider[provider] = providerStats
		}
		providerStats.Success += cs.Success
		providerStats.Failure += cs.Failure
		providerStats.Requests += cs.Success + cs.Failure
		if providerStats.Requests > 0 {
			providerStats.ErrorRate = float64(providerStats.Failure) / float64(providerStats.Requests)
		}
	}
	for key, accumulator := range credentialAccumulators {
		resp.ByCredential[key] = &summaryCredentialStats{
			Success:    accumulator.Success,
			Failure:    accumulator.Failure,
			Provider:   accumulator.Provider,
			Source:     accumulator.Source,
			SourceKind: accumulator.SourceKind,
			FilterKey:  accumulator.FilterKey,
		}
	}

	apiKeyAccumulators := make(map[string]*summaryAPIKeyAccumulator, len(qr.ByAPIKey))
	for key, aks := range qr.ByAPIKey {
		maskedKey := apiKeyPresenter.present(key).display
		accumulator := apiKeyAccumulators[maskedKey]
		if accumulator == nil {
			accumulator = &summaryAPIKeyAccumulator{}
			apiKeyAccumulators[maskedKey] = accumulator
		}
		accumulator.Requests += aks.Requests
		accumulator.Success += aks.Success
		accumulator.Failure += aks.Failure
		accumulator.Tokens.Input += aks.Tokens.InputTokens
		accumulator.Tokens.Output += aks.Tokens.OutputTokens
		accumulator.Tokens.Cached += aks.Tokens.CachedTokens
		accumulator.Tokens.CacheRead += aks.Tokens.CacheReadTokens
		accumulator.Tokens.CacheWrite += aks.Tokens.CacheWriteTokens
		accumulator.Tokens.Reasoning += aks.Tokens.ReasoningTokens
		accumulator.Tokens.Total += aks.Tokens.TotalTokens
		accumulator.Cost += aks.Cost
		accumulator.TotalLatencyMs += aks.Latency.TotalMs
		accumulator.LatencySamples += aks.Latency.SampleCount
	}
	for key, accumulator := range apiKeyAccumulators {
		averageLatency, totalLatency, latencySamples := latencyResponseFields(
			accumulator.TotalLatencyMs,
			accumulator.LatencySamples,
		)
		resp.ByAPIKey[key] = &summaryAPIKeyStats{
			Requests:           accumulator.Requests,
			Success:            accumulator.Success,
			Failure:            accumulator.Failure,
			Tokens:             accumulator.Tokens,
			Cost:               roundCost(accumulator.Cost),
			AverageLatencyMs:   averageLatency,
			TotalLatencyMs:     totalLatency,
			LatencySampleCount: latencySamples,
		}
	}

	for authIndex, stats := range qr.ByAuthIndex {
		averageLatency, totalLatency, latencySamples := latencyResponseFields(
			stats.Latency.TotalMs,
			stats.Latency.SampleCount,
		)
		resp.ByAuthIndex[authIndex] = &summaryAPIKeyStats{
			Requests: stats.Requests,
			Success:  stats.Success,
			Failure:  stats.Failure,
			Tokens: summaryTokens{
				Input:      stats.Tokens.InputTokens,
				Output:     stats.Tokens.OutputTokens,
				Cached:     stats.Tokens.CachedTokens,
				CacheRead:  stats.Tokens.CacheReadTokens,
				CacheWrite: stats.Tokens.CacheWriteTokens,
				Reasoning:  stats.Tokens.ReasoningTokens,
				Total:      stats.Tokens.TotalTokens,
			},
			Cost:               roundCost(stats.Cost),
			AverageLatencyMs:   averageLatency,
			TotalLatencyMs:     totalLatency,
			LatencySampleCount: latencySamples,
		}
	}

	resp.TimeSeries = make([]summaryTimePoint, len(qr.TimeSeries))
	for i, tp := range qr.TimeSeries {
		resp.TimeSeries[i] = summaryTimePoint{
			Time:     tp.Time,
			Requests: tp.Requests,
			Success:  tp.Success,
			Failure:  tp.Failure,
			Tokens: summaryTokens{
				Input:      tp.Tokens.InputTokens,
				Output:     tp.Tokens.OutputTokens,
				Cached:     tp.Tokens.CachedTokens,
				CacheRead:  tp.Tokens.CacheReadTokens,
				CacheWrite: tp.Tokens.CacheWriteTokens,
				Reasoning:  tp.Tokens.ReasoningTokens,
				Total:      tp.Tokens.TotalTokens,
			},
			Cost:    roundCost(tp.Cost),
			HasCost: tp.Cost > 0,
		}
	}

	for model, points := range qr.TimeSeriesByModel {
		converted := make([]summaryTimePoint, len(points))
		for i, tp := range points {
			converted[i] = summaryTimePoint{
				Time:     tp.Time,
				Requests: tp.Requests,
				Success:  tp.Success,
				Failure:  tp.Failure,
				Tokens: summaryTokens{
					Input:      tp.Tokens.InputTokens,
					Output:     tp.Tokens.OutputTokens,
					Cached:     tp.Tokens.CachedTokens,
					CacheRead:  tp.Tokens.CacheReadTokens,
					CacheWrite: tp.Tokens.CacheWriteTokens,
					Reasoning:  tp.Tokens.ReasoningTokens,
					Total:      tp.Tokens.TotalTokens,
				},
				Cost:    roundCost(tp.Cost),
				HasCost: tp.Cost > 0,
			}
		}
		resp.TimeSeriesByModel[model] = converted
	}

	credentialTimeSeriesAccumulators := make(
		map[string]map[string]*summaryTimePointAccumulator,
		len(qr.TimeSeriesByCredential),
	)
	for cred, points := range qr.TimeSeriesByCredential {
		displayKey := credentialDisplayKeys[cred]
		if displayKey == "" {
			displayKey = maskSummaryCredentialMapKey(cred, credentialMasker)
		}
		byTime := credentialTimeSeriesAccumulators[displayKey]
		if byTime == nil {
			byTime = make(map[string]*summaryTimePointAccumulator, len(points))
			credentialTimeSeriesAccumulators[displayKey] = byTime
		}
		for _, tp := range points {
			accumulator := byTime[tp.Time]
			if accumulator == nil {
				accumulator = &summaryTimePointAccumulator{}
				byTime[tp.Time] = accumulator
			}
			accumulator.Requests += tp.Requests
			accumulator.Success += tp.Success
			accumulator.Failure += tp.Failure
			accumulator.Tokens.Input += tp.Tokens.InputTokens
			accumulator.Tokens.Output += tp.Tokens.OutputTokens
			accumulator.Tokens.Cached += tp.Tokens.CachedTokens
			accumulator.Tokens.CacheRead += tp.Tokens.CacheReadTokens
			accumulator.Tokens.CacheWrite += tp.Tokens.CacheWriteTokens
			accumulator.Tokens.Reasoning += tp.Tokens.ReasoningTokens
			accumulator.Tokens.Total += tp.Tokens.TotalTokens
			accumulator.Cost += tp.Cost
		}
	}
	for key, byTime := range credentialTimeSeriesAccumulators {
		times := make([]string, 0, len(byTime))
		for pointTime := range byTime {
			times = append(times, pointTime)
		}
		sort.Strings(times)
		converted := make([]summaryTimePoint, 0, len(times))
		for _, pointTime := range times {
			accumulator := byTime[pointTime]
			converted = append(
				converted, summaryTimePoint{
					Time:     pointTime,
					Requests: accumulator.Requests,
					Success:  accumulator.Success,
					Failure:  accumulator.Failure,
					Tokens:   accumulator.Tokens,
					Cost:     roundCost(accumulator.Cost),
					HasCost:  accumulator.Cost > 0,
				},
			)
		}
		resp.TimeSeriesByCredential[key] = converted
	}

	timeSeriesAccumulators := make(
		map[string]map[string]*summaryTimePointAccumulator,
		len(qr.TimeSeriesByAPIKey),
	)
	for key, points := range qr.TimeSeriesByAPIKey {
		maskedKey := apiKeyPresenter.present(key).display
		byTime := timeSeriesAccumulators[maskedKey]
		if byTime == nil {
			byTime = make(map[string]*summaryTimePointAccumulator, len(points))
			timeSeriesAccumulators[maskedKey] = byTime
		}
		for _, tp := range points {
			accumulator := byTime[tp.Time]
			if accumulator == nil {
				accumulator = &summaryTimePointAccumulator{}
				byTime[tp.Time] = accumulator
			}
			accumulator.Requests += tp.Requests
			accumulator.Success += tp.Success
			accumulator.Failure += tp.Failure
			accumulator.Tokens.Input += tp.Tokens.InputTokens
			accumulator.Tokens.Output += tp.Tokens.OutputTokens
			accumulator.Tokens.Cached += tp.Tokens.CachedTokens
			accumulator.Tokens.CacheRead += tp.Tokens.CacheReadTokens
			accumulator.Tokens.CacheWrite += tp.Tokens.CacheWriteTokens
			accumulator.Tokens.Reasoning += tp.Tokens.ReasoningTokens
			accumulator.Tokens.Total += tp.Tokens.TotalTokens
			accumulator.Cost += tp.Cost
		}
	}
	for key, byTime := range timeSeriesAccumulators {
		times := make([]string, 0, len(byTime))
		for pointTime := range byTime {
			times = append(times, pointTime)
		}
		sort.Strings(times)
		converted := make([]summaryTimePoint, 0, len(times))
		for _, pointTime := range times {
			accumulator := byTime[pointTime]
			converted = append(
				converted, summaryTimePoint{
					Time:     pointTime,
					Requests: accumulator.Requests,
					Success:  accumulator.Success,
					Failure:  accumulator.Failure,
					Tokens:   accumulator.Tokens,
					Cost:     roundCost(accumulator.Cost),
					HasCost:  accumulator.Cost > 0,
				},
			)
		}
		resp.TimeSeriesByAPIKey[key] = converted
	}

	for authIndex, points := range qr.TimeSeriesByAuthIndex {
		converted := make([]summaryTimePoint, len(points))
		for i, point := range points {
			converted[i] = summaryTimePoint{
				Time:     point.Time,
				Requests: point.Requests,
				Success:  point.Success,
				Failure:  point.Failure,
				Tokens: summaryTokens{
					Input:      point.Tokens.InputTokens,
					Output:     point.Tokens.OutputTokens,
					Cached:     point.Tokens.CachedTokens,
					CacheRead:  point.Tokens.CacheReadTokens,
					CacheWrite: point.Tokens.CacheWriteTokens,
					Reasoning:  point.Tokens.ReasoningTokens,
					Total:      point.Tokens.TotalTokens,
				},
				Cost:    roundCost(point.Cost),
				HasCost: point.Cost > 0,
			}
		}
		resp.TimeSeriesByAuthIndex[authIndex] = converted
	}

	return resp
}

func summaryCredentialMapKey(provider, source string) string {
	if strings.TrimSpace(provider) == "" {
		return source
	}
	return provider + ":" + source
}

func maskSummaryCredentialMapKey(key string, masker usageCredentialMasker) string {
	provider, source := splitSummaryCredentialMapKey(key)
	return summaryCredentialMapKey(provider, masker.mask(source))
}

func splitSummaryCredentialMapKey(key string) (string, string) {
	separator := strings.Index(key, ":")
	if separator <= 0 || separator >= len(key)-1 {
		return "", key
	}
	return key[:separator], key[separator+1:]
}

func latencyResponseFields(totalMs, sampleCount int64) (*float64, *int64, int64) {
	if sampleCount <= 0 {
		return nil, nil, 0
	}
	average := new(float64)
	*average = float64(totalMs) / float64(sampleCount)
	total := new(int64)
	*total = totalMs
	return average, total, sampleCount
}

func parseTimeParam(value string, fallback time.Time) (time.Time, error) {
	if value == "" {
		return fallback, nil
	}
	// RFC3339: 2026-03-10T12:00:00Z
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		// datetime-local format from HTML input: 2026-03-10T12:00
		t, err = time.Parse("2006-01-02T15:04", value)
		if err != nil {
			// Date-only: 2026-03-10
			t, err = time.Parse("2006-01-02", value)
			if err != nil {
				return time.Time{}, err
			}
		}
	}
	return t.UTC(), nil
}

func roundCost(v float64) float64 {
	return math.Round(v*100) / 100
}
