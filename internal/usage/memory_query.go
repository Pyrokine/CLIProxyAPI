package usage

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func (s *RequestStatistics) queryDetails(ctx context.Context, from, to time.Time, filters EventFilters) (
	[]FlatDetail,
	error,
) {
	speedFilter, err := ParseRequestSpeedFilter(filters.Speed)
	if err != nil {
		return nil, err
	}
	if _, err := unixNanoUTC(from); err != nil {
		return nil, err
	}
	if _, err := unixNanoUTC(to); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, nil
	}
	s.mu.RLock()
	if s.detailsDropped > 0 {
		s.mu.RUnlock()
		return nil, ErrSummaryPrecisionUnavailable
	}
	snapshot, err := s.snapshotLockedContext(ctx, true)
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	details := flattenSnapshot(snapshot)
	models := strings.Split(filters.Model, ",")
	for _, detail := range details {
		if detail.Model == strings.TrimSpace(filters.Model) {
			models = []string{filters.Model}
			break
		}
	}
	apiKeys := filters.APIKeys
	if len(apiKeys) == 0 && strings.TrimSpace(filters.APIKey) != "" {
		apiKeys = strings.Split(filters.APIKey, ",")
	}
	matches := details[:0]
	for _, detail := range details {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if detail.Timestamp.Before(from) || detail.Timestamp.After(to) {
			continue
		}
		if filters.RequestID != "" && detail.RequestID != filters.RequestID {
			continue
		}
		if strings.TrimSpace(filters.Model) != "" && !slices.ContainsFunc(
			models, func(value string) bool { return strings.TrimSpace(value) == detail.Model },
		) {
			continue
		}
		if len(apiKeys) > 0 && !slices.ContainsFunc(
			apiKeys, func(value string) bool {
				value = strings.TrimSpace(value)
				if filters.CanonicalAPIKeys && value == noneSentinel {
					return detail.APIKey == ""
				}
				return value == detail.APIKey || coreusage.EnsureAPIKeyIdentity(value) == coreusage.EnsureAPIKeyIdentity(detail.APIKey) ||
					util.HideAPIKey(detail.APIKey) == value ||
					(value == noneSentinel && detail.APIKey == "")
			},
		) {
			continue
		}
		if filters.Source != "" && !memoryCredentialMatches(detail, filters.Source) {
			continue
		}
		if filters.Status == "success" && detail.Failed || filters.Status == "failure" && !detail.Failed {
			continue
		}
		speed, _ := ObservedRequestSpeed(&detail)
		if speedFilter != "" && speed != speedFilter {
			continue
		}
		if search := strings.TrimSpace(filters.Search); search != "" && !strings.Contains(
			strings.ToLower(
				strings.Join(
					[]string{
						detail.Model, detail.Source, detail.AuthIndex, detail.APIKey, detail.RequestID, detail.Provider,
						detail.ServiceTier, detail.RequestServiceTier, detail.ResponseServiceTier,
						detail.RequestSpeed, detail.ResponseSpeed, speed,
					}, "\n",
				),
			), strings.ToLower(search),
		) {
			continue
		}
		detail.CostStatus = CostStatusUnpriced
		detail.CostReason = "usage persistence disabled"
		detail.MetadataRecorded = true
		matches = append(matches, detail)
	}
	return matches, nil
}

func memoryCredentialMatches(detail FlatDetail, raw string) bool {
	credential := detail.Source
	if credential == "" {
		credential = detail.AuthIndex
	}
	display, kind, tagged := DecodeCredentialSource(credential)
	for token := range strings.SplitSeq(raw, ",") {
		provider, name := splitCredentialToken(strings.TrimSpace(token))
		if provider != "" && provider != detail.Provider {
			continue
		}
		name, filterKind, typed := decodeCredentialFilter(name)
		if typed && (tagged && filterKind != kind || !tagged && filterKind != coreusage.CredentialSourceKindAPIKey) {
			continue
		}
		if credential == name || display == name ||
			kind == coreusage.CredentialSourceKindAPIKey && display == util.HideAPIKey(name) ||
			!tagged && util.HideAPIKey(credential) == name {
			return true
		}
	}
	return false
}

func (s *RequestStatistics) QueryEventsContext(
	ctx context.Context,
	from, to time.Time,
	filters EventFilters,
	page, pageSize int,
	sortField string,
	descending bool,
) ([]FlatDetail, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	pageSize = min(pageSize, MaxEventsPageSize)
	if page > MaxEventsPage(pageSize) {
		return nil, 0, ErrEventsPageTooDeep
	}
	details, err := s.queryDetails(ctx, from, to, filters)
	if err != nil {
		return nil, 0, err
	}
	sort.SliceStable(
		details, func(i, j int) bool {
			a, b := details[i], details[j]
			comparison := a.Timestamp.Compare(b.Timestamp)
			switch sortField {
			case "model":
				comparison = strings.Compare(a.Model, b.Model)
			case "tokens":
				comparison = 0
				if a.Tokens.TotalTokens < b.Tokens.TotalTokens {
					comparison = -1
				} else if a.Tokens.TotalTokens > b.Tokens.TotalTokens {
					comparison = 1
				}
			case "latency_ms":
				comparison = 0
				if a.LatencyMs < b.LatencyMs {
					comparison = -1
				} else if a.LatencyMs > b.LatencyMs {
					comparison = 1
				}
			}
			if comparison == 0 {
				comparison = strings.Compare(
					a.APIKey+"\x00"+a.Model+"\x00"+a.Source+"\x00"+a.RequestID,
					b.APIKey+"\x00"+b.Model+"\x00"+b.Source+"\x00"+b.RequestID,
				)
			}
			if descending {
				return comparison > 0
			}
			return comparison < 0
		},
	)
	total := len(details)
	start := min((page-1)*pageSize, total)
	return details[start:min(start+pageSize, total)], total, nil
}

func addSummaryDetail(stats *summaryAPIKeyStats, detail FlatDetail) error {
	stats.Requests++
	if detail.Failed {
		stats.Failure++
	} else {
		stats.Success++
	}
	for _, token := range []struct {
		target *int64
		value  int64
	}{
		{&stats.Tokens.InputTokens, detail.Tokens.InputTokens},
		{&stats.Tokens.OutputTokens, detail.Tokens.OutputTokens},
		{&stats.Tokens.ReasoningTokens, detail.Tokens.ReasoningTokens},
		{&stats.Tokens.CachedTokens, detail.Tokens.CachedTokens},
		{&stats.Tokens.CacheReadTokens, detail.Tokens.CacheReadTokens},
		{&stats.Tokens.CacheWriteTokens, detail.Tokens.CacheWriteTokens},
		{&stats.Tokens.TotalTokens, detail.Tokens.TotalTokens},
	} {
		var ok bool
		if *token.target, ok = coreusage.AddNonNegativeInt64(*token.target, token.value); !ok {
			return fmt.Errorf("usage: token aggregate overflows int64")
		}
	}
	if _, samples := latencyBucketValues(detail.LatencyMs, detail.latencyRecorded); samples > 0 {
		var ok bool
		if stats.Latency.TotalMs, ok = coreusage.AddNonNegativeInt64(stats.Latency.TotalMs, detail.LatencyMs); !ok {
			return fmt.Errorf("usage: latency aggregate overflows int64")
		}
		stats.Latency.SampleCount++
	}
	return nil
}

func (s *RequestStatistics) QuerySummary(
	ctx context.Context,
	from, to time.Time,
	filters EventFilters,
	granularity, authIndexGranularity string,
	includeGroups, includeAuthIndex bool,
) (QueryResult, error) {
	result := emptyQueryResult()
	result.Granularity = effectiveSummaryGranularity(from, to, granularity)
	if !from.Before(to) {
		return result, nil
	}
	if _, err := unixNanoUTC(from); err != nil {
		return result, err
	}
	if _, err := unixNanoUTC(to); err != nil {
		return result, err
	}
	step := nsPerHour
	if result.Granularity == "daily" {
		step = nsPerDay
	}
	authStep := step
	if includeAuthIndex {
		switch authIndexGranularity {
		case "10m":
			if to.Sub(from) > maxAuthIndexTenMinuteRange {
				return result, ErrSummaryQueryTooLarge
			}
			authStep = nsPerTenMinute
		case "hourly":
			authStep = nsPerHour
		case "daily":
			authStep = nsPerDay
		}
	}
	if _, err := summarySeriesLimit(from.UnixNano(), to.UnixNano(), step); err != nil {
		return result, err
	}
	details, err := s.queryDetails(ctx, from, to, filters)
	if err != nil {
		return result, err
	}
	models := make(map[string]*summaryAPIKeyStats)
	var totals summaryAPIKeyStats
	type seriesKey struct {
		dimension, name string
		bucket          int64
	}
	series := make(map[seriesKey]*summaryAPIKeyStats)
	for _, detail := range details {
		credential := detail.Source
		if credential == "" {
			credential = detail.AuthIndex
		}
		credentialKey := credentialMapKey(detail.Provider, credential)
		apiKey := detail.APIKey
		if apiKey == "" {
			apiKey = noneSentinel
		}
		if models[detail.Model] == nil {
			models[detail.Model] = &summaryAPIKeyStats{}
		}
		if result.ByAPIKey[apiKey] == nil {
			result.ByAPIKey[apiKey] = &summaryAPIKeyStats{}
		}
		for _, stats := range []*summaryAPIKeyStats{&totals, models[detail.Model], result.ByAPIKey[apiKey]} {
			if err := addSummaryDetail(stats, detail); err != nil {
				return result, err
			}
		}
		if credential != "" {
			if result.ByCredential[credentialKey] == nil {
				result.ByCredential[credentialKey] = &summaryCredentialStats{
					Provider: detail.Provider, Source: credential,
				}
			}
			if detail.Failed {
				result.ByCredential[credentialKey].Failure++
			} else {
				result.ByCredential[credentialKey].Success++
			}
		}
		bucket, err := floorUnixNano(detail.Timestamp.UnixNano(), step)
		if err != nil {
			return result, err
		}
		keys := []seriesKey{{dimension: "total", bucket: bucket}}
		if includeGroups {
			keys = append(keys, seriesKey{"model", detail.Model, bucket}, seriesKey{"api_key", apiKey, bucket})
			if credential != "" {
				keys = append(keys, seriesKey{"credential", credentialKey, bucket})
			}
		}
		if includeAuthIndex && detail.AuthIndex != "" {
			if result.ByAuthIndex[detail.AuthIndex] == nil {
				result.ByAuthIndex[detail.AuthIndex] = &summaryAPIKeyStats{}
			}
			if err := addSummaryDetail(result.ByAuthIndex[detail.AuthIndex], detail); err != nil {
				return result, err
			}
			authBucket, err := floorUnixNano(detail.Timestamp.UnixNano(), authStep)
			if err != nil {
				return result, err
			}
			keys = append(keys, seriesKey{"auth_index", detail.AuthIndex, authBucket})
		}
		for _, key := range keys {
			if series[key] == nil {
				series[key] = &summaryAPIKeyStats{}
			}
			if err := addSummaryDetail(series[key], detail); err != nil {
				return result, err
			}
		}
	}
	result.Totals = summaryTotals{
		Requests: totals.Requests, Success: totals.Success, Failure: totals.Failure, Tokens: totals.Tokens,
		Latency: totals.Latency, CostCoverage: costCoverage{
			Unpriced: costCoverageCount{
				Requests: totals.Requests, Tokens: totals.Tokens.TotalTokens,
			},
		},
	}
	for model, stats := range models {
		value := summaryModelStats(*stats)
		result.ByModel[model] = &value
	}
	if summaryDimensionCount(&result) > maxSummaryDimensionCount {
		return result, ErrSummaryQueryTooLarge
	}
	for key, stats := range series {
		point := timePoint{
			Time: time.Unix(0, key.bucket).UTC().Format(time.RFC3339), Requests: stats.Requests, Success: stats.Success,
			Failure: stats.Failure, Tokens: stats.Tokens,
		}
		switch key.dimension {
		case "total":
			result.TimeSeries = append(result.TimeSeries, point)
		case "model":
			result.TimeSeriesByModel[key.name] = append(result.TimeSeriesByModel[key.name], point)
		case "credential":
			result.TimeSeriesByCredential[key.name] = append(result.TimeSeriesByCredential[key.name], point)
		case "api_key":
			result.TimeSeriesByAPIKey[key.name] = append(result.TimeSeriesByAPIKey[key.name], point)
		case "auth_index":
			result.TimeSeriesByAuthIndex[key.name] = append(result.TimeSeriesByAuthIndex[key.name], point)
		}
	}
	if includeAuthIndex {
		if err := validateSummaryPointBudgetWithAuthIndex(
			&result, from.UnixNano(), to.UnixNano(), step, from.UnixNano(), to.UnixNano(), authStep,
		); err != nil {
			return result, err
		}
		authSeries := result.TimeSeriesByAuthIndex
		result.TimeSeriesByAuthIndex = make(map[string][]timePoint)
		if err := padTimeSeries(&result, from.UnixNano(), to.UnixNano(), step); err != nil {
			return result, err
		}
		if err := padNamedTimeSeries(authSeries, from.UnixNano(), to.UnixNano(), authStep); err != nil {
			return result, err
		}
		result.TimeSeriesByAuthIndex = authSeries
	} else {
		if err := validateSummaryPointBudget(&result, from.UnixNano(), to.UnixNano(), step); err != nil {
			return result, err
		}
		if err := padTimeSeries(&result, from.UnixNano(), to.UnixNano(), step); err != nil {
			return result, err
		}
	}
	return result, nil
}
