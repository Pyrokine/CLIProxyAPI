// Package usage provides usage tracking and logging functionality for the CLI Proxy API server.
// It includes plugins for monitoring API usage, token consumption, and other metrics
// to help with observability and billing purposes.
package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

var statisticsEnabled atomic.Bool

func init() {
	statisticsEnabled.Store(true)
	coreusage.RegisterPlugin(newLoggerPlugin())
}

// loggerPlugin collects in-memory request statistics for usage analysis.
// It implements coreusage.Plugin to receive usage records emitted by the runtime.
type loggerPlugin struct {
	stats     *RequestStatistics
	persister *Persister
	enabled   func() bool
}

// newLoggerPlugin constructs the compatibility plugin used by the default manager.
func newLoggerPlugin() *loggerPlugin {
	return &loggerPlugin{stats: defaultRequestStatistics}
}

// NewLoggerPlugin constructs a logger bound to one service's storage and enabled state.
func NewLoggerPlugin(
	persister *Persister,
	stats *RequestStatistics,
	enabled func() bool,
) coreusage.Plugin {
	if stats == nil {
		stats = NewRequestStatistics()
	}
	return &loggerPlugin{stats: stats, persister: persister, enabled: enabled}
}

func isAPIKeyUsageAuthType(authType string) bool {
	normalized := strings.ToLower(strings.TrimSpace(authType))
	normalized = strings.NewReplacer("_", "", "-", "").Replace(normalized)
	return normalized == "apikey"
}

func latencyMilliseconds(latency time.Duration) int64 {
	if latency <= 0 {
		return 0
	}
	return latency.Milliseconds()
}

func thinkingFromCore(src *coreusage.Thinking) *Thinking {
	if src == nil {
		return nil
	}
	var budget *int
	if src.Budget != nil {
		budget = new(int)
		*budget = *src.Budget
	}
	return cloneThinking(
		&Thinking{
			Intensity: strings.TrimSpace(src.Intensity),
			Mode:      strings.TrimSpace(src.Mode),
			Level:     strings.TrimSpace(src.Level),
			Budget:    budget,
		},
	)
}

// HandleUsage implements coreusage.Plugin.
// When a global Persister is registered, records are routed through it (v2 path).
// Otherwise, the legacy RequestStatistics is used as fallback.
func (p *loggerPlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil {
		return
	}
	record.APIKey = coreusage.EnsureAPIKeyIdentity(record.APIKey)
	if p.enabled != nil {
		if !p.enabled() {
			return
		}
	} else if !statisticsEnabled.Load() {
		return
	}

	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	usageDetail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	tokens := normaliseDetail(usageDetail)
	modelName := record.Model
	if modelName == "" {
		modelName = "unknown"
	}
	failed := record.Failed
	if !failed {
		failed = !resolveSuccess(ctx)
	}

	source := strings.TrimSpace(record.Source)
	if source != "" {
		switch record.SourceKind {
		case coreusage.CredentialSourceKindAPIKey:
			source = TagAPIKeyCredentialSource(source, record.SourceRedacted)
		case coreusage.CredentialSourceKindIdentity:
			source = TagIdentityCredentialSource(source)
		default:
			if record.SourceRedacted || isAPIKeyUsageAuthType(record.AuthType) {
				source = TagAPIKeyCredentialSource(source, record.SourceRedacted)
			} else {
				source = TagIdentityCredentialSource(source)
			}
		}
	}
	record.Source = source
	authIndex := record.AuthIndex
	reasoningEffort := strings.TrimSpace(record.ReasoningEffort)
	if reasoningEffort == "" {
		reasoningEffort = coreusage.ReasoningEffortFromContext(ctx)
	}
	requestServiceTier := strings.TrimSpace(record.RequestServiceTier)
	if requestServiceTier == "" {
		requestServiceTier = strings.TrimSpace(record.ServiceTier)
	}
	if requestServiceTier == "" {
		requestServiceTier = coreusage.ServiceTierFromContext(ctx)
	}
	responseServiceTier := strings.TrimSpace(record.ResponseServiceTier)
	if responseServiceTier == "" {
		responseServiceTier = strings.TrimSpace(record.Detail.ResponseServiceTier)
	}
	executorType := strings.TrimSpace(record.ExecutorType)

	persister := p.persister
	if persister != nil {
		detail := FlatDetail{
			Timestamp:           timestamp,
			Model:               modelName,
			Source:              source,
			AuthIndex:           authIndex,
			APIKey:              record.APIKey,
			ReservationID:       record.ReservationID,
			Provider:            record.Provider,
			LatencyMs:           latencyMilliseconds(record.Latency),
			RequestID:           strings.TrimSpace(record.RequestID),
			TimeToFirstByteMs:   record.TimeToFirstByteMs,
			TotalDurationMs:     record.TotalDurationMs,
			Completed:           record.Completed,
			Generate:            coreusage.GenerateEnabled(record.Generate),
			ClientIP:            strings.TrimSpace(record.ClientIP),
			XForwardedFor:       strings.TrimSpace(record.XForwardedFor),
			UserAgent:           strings.TrimSpace(record.UserAgent),
			AccountingVersion:   coreusage.TokenAccountingSchemaVersion,
			TokenBreakdown:      usageDetail.TokenBreakdown,
			cacheWrite5mTokens:  usageDetail.CacheCreation5mInputTokens,
			cacheWrite1hTokens:  usageDetail.CacheCreation1hInputTokens,
			ReasoningEffort:     reasoningEffort,
			ServiceTier:         requestServiceTier,
			RequestServiceTier:  requestServiceTier,
			ResponseServiceTier: responseServiceTier,
			ExecutorType:        executorType,
			Thinking:            thinkingFromCore(record.Thinking),
			ResponseHeaders:     sanitizeResponseHeaders(record.ResponseHeaders),
			Tokens:              tokens,
			Failed:              failed,
			generateRecorded:    true,
		}
		if !persister.Record(detail) {
			log.Warn("usage: SQLite persister did not accept live usage record")
		}
		return
	}

	// Fallback: legacy RequestStatistics (used when no Persister is configured)
	if p.stats == nil {
		return
	}
	p.stats.record(ctx, record)
}

// SetStatisticsEnabled toggles whether in-memory statistics are recorded.
func SetStatisticsEnabled(enabled bool) { statisticsEnabled.Store(enabled) }

// RequestStatistics maintains aggregated request metrics in memory.
// This is the legacy storage used when no Persister is configured.
type RequestStatistics struct {
	mu sync.RWMutex

	totalRequests  int64
	successCount   int64
	failureCount   int64
	totalTokens    int64
	detailsCount   int64
	detailsBytes   int64
	detailsDropped int64

	apis map[string]*apiStats

	requestsByDay  map[string]int64
	requestsByHour map[int]int64
	tokensByDay    map[string]int64
	tokensByHour   map[int]int64
}

// apiStats holds aggregated metrics for a single API key.
type apiStats struct {
	TotalRequests int64
	TotalTokens   int64
	Models        map[string]*modelStats
}

// modelStats holds aggregated metrics for a specific model within an API.
type modelStats struct {
	TotalRequests int64
	TotalTokens   int64
	Details       []requestDetail
}

// requestDetail stores the timestamp and token usage for a single request.
type requestDetail struct {
	Timestamp           time.Time                `json:"timestamp"`
	Source              string                   `json:"source"`
	AuthIndex           string                   `json:"auth_index"`
	LatencyMs           int64                    `json:"latency_ms,omitempty"`
	RequestID           string                   `json:"request_id,omitempty"`
	TimeToFirstByteMs   int64                    `json:"time_to_first_byte_ms,omitempty"`
	TotalDurationMs     int64                    `json:"total_duration_ms,omitempty"`
	Completed           bool                     `json:"completed"`
	Generate            bool                     `json:"generate"`
	ClientIP            string                   `json:"client_ip,omitempty"`
	XForwardedFor       string                   `json:"x_forwarded_for,omitempty"`
	UserAgent           string                   `json:"user_agent,omitempty"`
	AccountingVersion   int                      `json:"accounting_version,omitempty"`
	TokenBreakdown      coreusage.TokenBreakdown `json:"token_breakdown,omitempty"`
	ReasoningEffort     string                   `json:"reasoning_effort,omitempty"`
	ServiceTier         string                   `json:"service_tier,omitempty"`
	RequestServiceTier  string                   `json:"request_service_tier,omitempty"`
	ResponseServiceTier string                   `json:"response_service_tier,omitempty"`
	ExecutorType        string                   `json:"executor_type,omitempty"`
	Thinking            *Thinking                `json:"thinking,omitempty"`
	ResponseHeaders     http.Header              `json:"response_headers,omitempty"`
	Tokens              tokenStats               `json:"tokens"`
	Failed              bool                     `json:"failed"`
	generateRecorded    bool
}

// UnmarshalJSON preserves generation for snapshots written before the field existed.
func (d *requestDetail) UnmarshalJSON(data []byte) error {
	type requestDetailAlias requestDetail
	*d = requestDetail{}
	decoded := struct {
		Generate *bool `json:"generate"`
		*requestDetailAlias
	}{requestDetailAlias: (*requestDetailAlias)(d)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	d.Generate = decoded.Generate == nil || *decoded.Generate
	d.generateRecorded = decoded.Generate != nil
	return nil
}

// tokenStats captures the token usage breakdown for a request.
type tokenStats struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// StatisticsSnapshot represents an immutable view of the aggregated metrics.
type StatisticsSnapshot struct {
	TotalRequests int64 `json:"total_requests"`
	SuccessCount  int64 `json:"success_count"`
	FailureCount  int64 `json:"failure_count"`
	TotalTokens   int64 `json:"total_tokens"`

	APIs map[string]aPISnapshot `json:"apis"`

	RequestsByDay  map[string]int64 `json:"requests_by_day"`
	RequestsByHour map[string]int64 `json:"requests_by_hour"`
	TokensByDay    map[string]int64 `json:"tokens_by_day"`
	TokensByHour   map[string]int64 `json:"tokens_by_hour"`
}

// aPISnapshot summarises metrics for a single API key.
type aPISnapshot struct {
	TotalRequests int64                    `json:"total_requests"`
	TotalTokens   int64                    `json:"total_tokens"`
	Models        map[string]modelSnapshot `json:"models"`
}

// modelSnapshot summarises metrics for a specific model.
type modelSnapshot struct {
	TotalRequests int64           `json:"total_requests"`
	TotalTokens   int64           `json:"total_tokens"`
	Details       []requestDetail `json:"details"`
}

var defaultRequestStatistics = NewRequestStatistics()

// GetRequestStatistics returns the shared statistics store.
func GetRequestStatistics() *RequestStatistics { return defaultRequestStatistics }

// NewRequestStatistics constructs an empty statistics store.
func NewRequestStatistics() *RequestStatistics {
	return &RequestStatistics{
		apis:           make(map[string]*apiStats),
		requestsByDay:  make(map[string]int64),
		requestsByHour: make(map[int]int64),
		tokensByDay:    make(map[string]int64),
		tokensByHour:   make(map[int]int64),
	}
}

func newRequestStatistics() *RequestStatistics { return NewRequestStatistics() }

// record ingests a new usage record and updates the aggregates (legacy path).
func (s *RequestStatistics) record(ctx context.Context, record coreusage.Record) {
	if s == nil {
		return
	}
	if !statisticsEnabled.Load() {
		return
	}
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	timestamp = timestamp.UTC()
	usageDetail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	detail := normaliseDetail(usageDetail)
	totalTokens := detail.TotalTokens
	statsKey := record.APIKey
	if statsKey == "" {
		statsKey = resolveAPIIdentifier(ctx, record)
	}
	failed := record.Failed
	if !failed {
		failed = !resolveSuccess(ctx)
	}
	success := !failed
	modelName := record.Model
	if modelName == "" {
		modelName = "unknown"
	}
	reasoningEffort := strings.TrimSpace(record.ReasoningEffort)
	if reasoningEffort == "" {
		reasoningEffort = coreusage.ReasoningEffortFromContext(ctx)
	}
	requestServiceTier := strings.TrimSpace(record.RequestServiceTier)
	if requestServiceTier == "" {
		requestServiceTier = strings.TrimSpace(record.ServiceTier)
	}
	if requestServiceTier == "" {
		requestServiceTier = coreusage.ServiceTierFromContext(ctx)
	}
	responseServiceTier := strings.TrimSpace(record.ResponseServiceTier)
	if responseServiceTier == "" {
		responseServiceTier = strings.TrimSpace(record.Detail.ResponseServiceTier)
	}
	executorType := strings.TrimSpace(record.ExecutorType)
	dayKey := timestamp.Format("2006-01-02")
	hourKey := timestamp.Hour()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.totalRequests++
	if success {
		s.successCount++
	} else {
		s.failureCount++
	}

	stats, ok := s.apis[statsKey]
	if !ok {
		stats = &apiStats{Models: make(map[string]*modelStats)}
		s.apis[statsKey] = stats
	}
	modelStatsValue := s.updateAPIStats(
		stats, modelName, requestDetail{
			Timestamp:           timestamp,
			Source:              record.Source,
			AuthIndex:           record.AuthIndex,
			LatencyMs:           latencyMilliseconds(record.Latency),
			RequestID:           strings.TrimSpace(record.RequestID),
			TimeToFirstByteMs:   record.TimeToFirstByteMs,
			TotalDurationMs:     record.TotalDurationMs,
			Completed:           record.Completed,
			Generate:            coreusage.GenerateEnabled(record.Generate),
			ClientIP:            strings.TrimSpace(record.ClientIP),
			XForwardedFor:       strings.TrimSpace(record.XForwardedFor),
			UserAgent:           strings.TrimSpace(record.UserAgent),
			AccountingVersion:   coreusage.TokenAccountingSchemaVersion,
			TokenBreakdown:      usageDetail.TokenBreakdown,
			ReasoningEffort:     reasoningEffort,
			ServiceTier:         requestServiceTier,
			RequestServiceTier:  requestServiceTier,
			ResponseServiceTier: responseServiceTier,
			ExecutorType:        executorType,
			Thinking:            thinkingFromCore(record.Thinking),
			ResponseHeaders:     sanitizeResponseHeaders(record.ResponseHeaders),
			Tokens:              detail,
			Failed:              failed,
			generateRecorded:    true,
		},
	)

	s.requestsByDay[dayKey]++
	s.requestsByHour[hourKey]++
	s.updateTokenAggregates(stats, modelStatsValue, dayKey, hourKey, totalTokens)
}

func (s *RequestStatistics) updateAPIStats(stats *apiStats, model string, detail requestDetail) *modelStats {
	stats.TotalRequests++
	modelStatsValue, ok := stats.Models[model]
	if !ok {
		modelStatsValue = &modelStats{}
		stats.Models[model] = modelStatsValue
	}
	modelStatsValue.TotalRequests++
	detailBytes := estimateRequestDetailBytes(detail)
	if s.detailsCount < MaxLegacySnapshotEvents &&
		detailBytes <= MaxLegacySnapshotDetailBytes-s.detailsBytes {
		modelStatsValue.Details = append(modelStatsValue.Details, detail)
		s.detailsCount++
		s.detailsBytes += detailBytes
	} else {
		s.detailsDropped++
	}
	return modelStatsValue
}

func (s *RequestStatistics) updateTokenAggregates(
	stats *apiStats,
	modelStatsValue *modelStats,
	dayKey string,
	hourKey int,
	totalTokens int64,
) {
	globalTotal, globalOK := coreusage.AddNonNegativeInt64(s.totalTokens, totalTokens)
	apiTotal, apiOK := coreusage.AddNonNegativeInt64(stats.TotalTokens, totalTokens)
	modelTotal, modelOK := coreusage.AddNonNegativeInt64(modelStatsValue.TotalTokens, totalTokens)
	dayTotal, dayOK := coreusage.AddNonNegativeInt64(s.tokensByDay[dayKey], totalTokens)
	hourTotal, hourOK := coreusage.AddNonNegativeInt64(s.tokensByHour[hourKey], totalTokens)
	if !globalOK || !apiOK || !modelOK || !dayOK || !hourOK {
		log.Warn("usage: rejected overflowing token aggregate update")
		return
	}
	s.totalTokens = globalTotal
	stats.TotalTokens = apiTotal
	modelStatsValue.TotalTokens = modelTotal
	s.tokensByDay[dayKey] = dayTotal
	s.tokensByHour[hourKey] = hourTotal
}

// snapshot returns a copy of the aggregated metrics and request details.
func (s *RequestStatistics) snapshot() StatisticsSnapshot {
	snapshot, _ := s.snapshotContext(context.Background(), true)
	return snapshot
}

// snapshotLimited returns the full snapshot only when its request count and
// estimated detail size fit the compatibility limits.
func (s *RequestStatistics) snapshotLimited(
	maxEvents int64,
	maxDetailBytes int64,
) (StatisticsSnapshot, error) {
	return s.SnapshotLimitedContext(
		context.Background(),
		maxEvents,
		maxDetailBytes,
	)
}

// SnapshotLimitedContext applies the compatibility limits and stops estimating
// or copying details when the caller cancels the request.
func (s *RequestStatistics) SnapshotLimitedContext(
	ctx context.Context,
	maxEvents int64,
	maxDetailBytes int64,
) (StatisticsSnapshot, error) {
	if s == nil {
		return StatisticsSnapshot{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return StatisticsSnapshot{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return StatisticsSnapshot{}, err
	}
	if maxEvents > 0 && s.totalRequests > maxEvents {
		return StatisticsSnapshot{}, fmt.Errorf(
			"%w: more than %d events",
			ErrLegacySnapshotTooLarge,
			maxEvents,
		)
	}
	if err := s.validateLegacySnapshotSizeLocked(ctx, maxDetailBytes); err != nil {
		return StatisticsSnapshot{}, err
	}
	return s.snapshotLockedContext(ctx, true)
}

func (s *RequestStatistics) validateLegacySnapshotSizeLocked(
	ctx context.Context,
	maxDetailBytes int64,
) error {
	if maxDetailBytes <= 0 {
		return nil
	}
	var estimatedBytes int64
	for apiName, stats := range s.apis {
		if err := ctx.Err(); err != nil {
			return err
		}
		if stats == nil {
			continue
		}
		estimatedBytes += int64(len(apiName))*legacySnapshotJSONStringExpansion + 256
		if estimatedBytes > maxDetailBytes {
			return legacySnapshotSizeError(maxDetailBytes)
		}
		for modelName, modelStatsValue := range stats.Models {
			if err := ctx.Err(); err != nil {
				return err
			}
			if modelStatsValue == nil {
				continue
			}
			estimatedBytes += int64(len(modelName))*legacySnapshotJSONStringExpansion + 256
			if estimatedBytes > maxDetailBytes {
				return legacySnapshotSizeError(maxDetailBytes)
			}
			for _, detail := range modelStatsValue.Details {
				if err := ctx.Err(); err != nil {
					return err
				}
				estimatedBytes += estimateRequestDetailBytes(detail)
				if estimatedBytes > maxDetailBytes {
					return legacySnapshotSizeError(maxDetailBytes)
				}
			}
		}
	}
	return nil
}

func legacySnapshotSizeError(maxDetailBytes int64) error {
	return fmt.Errorf(
		"%w: estimated snapshot uses more than %d bytes",
		ErrLegacySnapshotTooLarge,
		maxDetailBytes,
	)
}

func estimateRequestDetailBytes(detail requestDetail) int64 {
	estimatedBytes := legacySnapshotEventOverhead +
		int64(
			len(detail.Source)+
				len(detail.AuthIndex)+
				len(detail.RequestID)+
				len(detail.ClientIP)+
				len(detail.XForwardedFor)+
				len(detail.UserAgent)+
				len(detail.ReasoningEffort)+
				len(detail.ServiceTier)+
				len(detail.RequestServiceTier)+
				len(detail.ResponseServiceTier)+
				len(detail.ExecutorType),
		)*legacySnapshotJSONStringExpansion
	if detail.Thinking != nil {
		estimatedBytes += int64(
			len(detail.Thinking.Intensity)+
				len(detail.Thinking.Mode)+
				len(detail.Thinking.Level),
		)*legacySnapshotJSONStringExpansion + 64
	}
	for name, values := range detail.ResponseHeaders {
		estimatedBytes += int64(len(name))*legacySnapshotJSONStringExpansion +
			legacySnapshotHeaderNameOverhead
		for _, value := range values {
			estimatedBytes += int64(len(value))*legacySnapshotJSONStringExpansion +
				legacySnapshotHeaderValueOverhead
		}
	}
	return estimatedBytes
}

// aggregateSnapshot returns the legacy aggregate shape without copying request
// details. It is used by bounded management and TUI views.
func (s *RequestStatistics) aggregateSnapshot() StatisticsSnapshot {
	snapshot, _ := s.AggregateSnapshotContext(context.Background())
	return snapshot
}

// AggregateSnapshotContext returns the bounded aggregate shape and stops map
// copying when the caller cancels the request.
func (s *RequestStatistics) AggregateSnapshotContext(
	ctx context.Context,
) (StatisticsSnapshot, error) {
	return s.snapshotContext(ctx, false)
}

func (s *RequestStatistics) snapshotContext(
	ctx context.Context,
	includeDetails bool,
) (StatisticsSnapshot, error) {
	if s == nil {
		return StatisticsSnapshot{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return StatisticsSnapshot{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLockedContext(ctx, includeDetails)
}

func (s *RequestStatistics) snapshotLockedContext(
	ctx context.Context,
	includeDetails bool,
) (StatisticsSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return StatisticsSnapshot{}, err
	}
	result := StatisticsSnapshot{
		TotalRequests: s.totalRequests,
		SuccessCount:  s.successCount,
		FailureCount:  s.failureCount,
		TotalTokens:   s.totalTokens,
		APIs:          make(map[string]aPISnapshot),
	}
	if includeDetails {
		result.APIs = make(map[string]aPISnapshot, len(s.apis))
		for apiName, stats := range s.apis {
			if err := ctx.Err(); err != nil {
				return StatisticsSnapshot{}, err
			}
			if stats == nil {
				continue
			}
			apiSnapshot := aPISnapshot{
				TotalRequests: stats.TotalRequests,
				TotalTokens:   stats.TotalTokens,
				Models:        make(map[string]modelSnapshot, len(stats.Models)),
			}
			for modelName, modelStatsValue := range stats.Models {
				if err := ctx.Err(); err != nil {
					return StatisticsSnapshot{}, err
				}
				if modelStatsValue == nil {
					continue
				}
				requestDetails := make([]requestDetail, len(modelStatsValue.Details))
				for i, detail := range modelStatsValue.Details {
					if err := ctx.Err(); err != nil {
						return StatisticsSnapshot{}, err
					}
					requestDetails[i] = detail
					requestDetails[i].Thinking = cloneThinking(detail.Thinking)
					requestDetails[i].ResponseHeaders = cloneHTTPHeader(detail.ResponseHeaders)
				}
				apiSnapshot.Models[modelName] = modelSnapshot{
					TotalRequests: modelStatsValue.TotalRequests,
					TotalTokens:   modelStatsValue.TotalTokens,
					Details:       requestDetails,
				}
			}
			result.APIs[apiName] = apiSnapshot
		}
	}

	result.RequestsByDay = make(map[string]int64, len(s.requestsByDay))
	for day, requests := range s.requestsByDay {
		if err := ctx.Err(); err != nil {
			return StatisticsSnapshot{}, err
		}
		result.RequestsByDay[day] = requests
	}
	result.RequestsByHour = make(map[string]int64, len(s.requestsByHour))
	for hour, requests := range s.requestsByHour {
		if err := ctx.Err(); err != nil {
			return StatisticsSnapshot{}, err
		}
		result.RequestsByHour[formatHour(hour)] = requests
	}
	result.TokensByDay = make(map[string]int64, len(s.tokensByDay))
	for day, tokens := range s.tokensByDay {
		if err := ctx.Err(); err != nil {
			return StatisticsSnapshot{}, err
		}
		result.TokensByDay[day] = tokens
	}
	result.TokensByHour = make(map[string]int64, len(s.tokensByHour))
	for hour, tokens := range s.tokensByHour {
		if err := ctx.Err(); err != nil {
			return StatisticsSnapshot{}, err
		}
		result.TokensByHour[formatHour(hour)] = tokens
	}

	return result, nil
}

type mergeResult struct {
	Added   int64 `json:"added"`
	Skipped int64 `json:"skipped"`
}

// maxImportFieldLen limits apiName/modelName length in imports to prevent memory abuse.
const maxImportFieldLen = 256

// importHook is called for each successfully imported detail with the resolved fields.
type importHook func(detail FlatDetail)

// mergeSnapshot merges an exported statistics snapshot into the current store.
// Existing data is preserved and duplicate request details are skipped.
// An optional hook is called for each newly added detail (after dedup, before return).
func (s *RequestStatistics) mergeSnapshot(snapshot StatisticsSnapshot, hooks ...importHook) mergeResult {
	result := mergeResult{}
	if s == nil {
		return result
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[string]struct{})
	for apiName, stats := range s.apis {
		if stats == nil {
			continue
		}
		for modelName, modelStatsValue := range stats.Models {
			if modelStatsValue == nil {
				continue
			}
			for _, detail := range modelStatsValue.Details {
				seen[dedupKey(apiName, modelName, detail)] = struct{}{}
			}
		}
	}

	for apiName, apiSnapshot := range snapshot.APIs {
		apiName = strings.TrimSpace(apiName)
		if apiName == "" {
			continue
		}
		if len(apiName) > maxImportFieldLen {
			apiName = apiName[:maxImportFieldLen]
		}
		stats, ok := s.apis[apiName]
		if !ok || stats == nil {
			stats = &apiStats{Models: make(map[string]*modelStats)}
			s.apis[apiName] = stats
		} else if stats.Models == nil {
			stats.Models = make(map[string]*modelStats)
		}
		for modelName, modelSnapshot := range apiSnapshot.Models {
			modelName = strings.TrimSpace(modelName)
			if modelName == "" {
				modelName = "unknown"
			}
			if len(modelName) > maxImportFieldLen {
				modelName = modelName[:maxImportFieldLen]
			}
			for _, detail := range modelSnapshot.Details {
				detail.Tokens = normaliseTokenStats(detail.Tokens)
				if detail.Timestamp.IsZero() {
					detail.Timestamp = time.Now()
				}
				if !detail.generateRecorded {
					detail.Generate = true
				}
				key := dedupKey(apiName, modelName, detail)
				if _, exists := seen[key]; exists {
					result.Skipped++
					continue
				}
				seen[key] = struct{}{}
				s.recordImported(apiName, modelName, stats, detail)
				result.Added++
				if len(hooks) > 0 && hooks[0] != nil {
					requestServiceTier := strings.TrimSpace(detail.RequestServiceTier)
					if requestServiceTier == "" {
						requestServiceTier = strings.TrimSpace(detail.ServiceTier)
					}
					hooks[0](
						FlatDetail{
							Timestamp:           detail.Timestamp,
							Model:               modelName,
							Source:              detail.Source,
							AuthIndex:           detail.AuthIndex,
							APIKey:              apiName,
							LatencyMs:           detail.LatencyMs,
							RequestID:           strings.TrimSpace(detail.RequestID),
							TimeToFirstByteMs:   detail.TimeToFirstByteMs,
							TotalDurationMs:     detail.TotalDurationMs,
							Completed:           detail.Completed,
							Generate:            detail.Generate,
							ClientIP:            strings.TrimSpace(detail.ClientIP),
							XForwardedFor:       strings.TrimSpace(detail.XForwardedFor),
							UserAgent:           strings.TrimSpace(detail.UserAgent),
							AccountingVersion:   detail.AccountingVersion,
							TokenBreakdown:      detail.TokenBreakdown,
							ReasoningEffort:     strings.TrimSpace(detail.ReasoningEffort),
							ServiceTier:         requestServiceTier,
							RequestServiceTier:  requestServiceTier,
							ResponseServiceTier: strings.TrimSpace(detail.ResponseServiceTier),
							ExecutorType:        strings.TrimSpace(detail.ExecutorType),
							Thinking:            cloneThinking(detail.Thinking),
							ResponseHeaders:     cloneHTTPHeader(detail.ResponseHeaders),
							Tokens:              detail.Tokens,
							Failed:              detail.Failed,
							generateRecorded:    true,
						},
					)
				}
			}
		}
	}

	return result
}

func (s *RequestStatistics) recordImported(_, modelName string, stats *apiStats, detail requestDetail) {
	detail.Timestamp = detail.Timestamp.UTC()
	totalTokens := detail.Tokens.TotalTokens

	s.totalRequests++
	if detail.Failed {
		s.failureCount++
	} else {
		s.successCount++
	}

	modelStatsValue := s.updateAPIStats(stats, modelName, detail)

	dayKey := detail.Timestamp.Format("2006-01-02")
	hourKey := detail.Timestamp.Hour()

	s.requestsByDay[dayKey]++
	s.requestsByHour[hourKey]++
	s.updateTokenAggregates(stats, modelStatsValue, dayKey, hourKey, totalTokens)
}

func dedupKey(apiName, modelName string, detail requestDetail) string {
	timestamp := detail.Timestamp.UTC().Format(time.RFC3339Nano)
	tokens := normaliseTokenStats(detail.Tokens)
	return fmt.Sprintf(
		"%s|%s|%s|%s|%s|%t|%d|%d|%d|%d|%d",
		apiName,
		modelName,
		timestamp,
		detail.Source,
		detail.AuthIndex,
		detail.Failed,
		tokens.InputTokens,
		tokens.OutputTokens,
		tokens.ReasoningTokens,
		tokens.CachedTokens,
		tokens.TotalTokens,
	)
}

func resolveAPIIdentifier(ctx context.Context, record coreusage.Record) string {
	if ctx != nil {
		if ginCtx, ok := util.GinContextValue(ctx).(*gin.Context); ok && ginCtx != nil {
			path := ginCtx.FullPath()
			if path == "" && ginCtx.Request != nil {
				path = ginCtx.Request.URL.Path
			}
			method := ""
			if ginCtx.Request != nil {
				method = ginCtx.Request.Method
			}
			if path != "" {
				if method != "" {
					return method + " " + path
				}
				return path
			}
		}
	}
	if record.Provider != "" {
		return record.Provider
	}
	return "unknown"
}

func resolveSuccess(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	ginCtx, ok := util.GinContextValue(ctx).(*gin.Context)
	if !ok || ginCtx == nil {
		return true
	}
	status := ginCtx.Writer.Status()
	if status == 0 {
		return true
	}
	return status < httpStatusBadRequest
}

const httpStatusBadRequest = 400

func normaliseDetail(detail coreusage.Detail) tokenStats {
	breakdown := detail.TokenBreakdown
	if !breakdown.Valid() {
		log.Warn("usage: rejected invalid token breakdown")
		return tokenStats{}
	}
	cachedTokens, ok := coreusage.AddNonNegativeInt64(
		breakdown.Input.CacheReadTokens,
		breakdown.Input.CacheWriteTokens,
	)
	if !ok {
		log.Warn("usage: cached token total overflows int64")
		return tokenStats{}
	}
	return normaliseTokenStats(
		tokenStats{
			InputTokens:      breakdown.Input.TotalTokens,
			OutputTokens:     breakdown.Output.TotalTokens,
			ReasoningTokens:  breakdown.Output.ReasoningTokens,
			CachedTokens:     cachedTokens,
			CacheReadTokens:  breakdown.Input.CacheReadTokens,
			CacheWriteTokens: breakdown.Input.CacheWriteTokens,
			TotalTokens:      breakdown.TotalTokens,
		},
	)
}

// UnmarshalJSON supports both legacy int64 (→ TotalTokens) and full object formats.
func (t *tokenStats) UnmarshalJSON(data []byte) error {
	// Try int64 first (legacy format)
	var n int64
	if json.Unmarshal(data, &n) == nil {
		*t = tokenStats{TotalTokens: n}
		return nil
	}
	// Full object
	type alias tokenStats
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*t = tokenStats(a)
	return nil
}

func validatedTokenStats(tokens tokenStats) (tokenStats, error) {
	for _, field := range []struct {
		name  string
		value int64
	}{
		{name: "input_tokens", value: tokens.InputTokens},
		{name: "output_tokens", value: tokens.OutputTokens},
		{name: "reasoning_tokens", value: tokens.ReasoningTokens},
		{name: "cached_tokens", value: tokens.CachedTokens},
		{name: "cache_read_tokens", value: tokens.CacheReadTokens},
		{name: "cache_write_tokens", value: tokens.CacheWriteTokens},
		{name: "total_tokens", value: tokens.TotalTokens},
	} {
		if field.value < 0 {
			return tokenStats{}, fmt.Errorf("usage: %s must be non-negative", field.name)
		}
	}
	cacheTotal, okCache := coreusage.AddNonNegativeInt64(tokens.CacheReadTokens, tokens.CacheWriteTokens)
	if !okCache {
		return tokenStats{}, errors.New("usage: cached token detail total overflows int64")
	}
	if cacheTotal > 0 {
		tokens.CachedTokens = cacheTotal
	}
	if tokens.TotalTokens != 0 {
		return tokens, nil
	}
	total, ok := coreusage.AddNonNegativeInt64(tokens.InputTokens, tokens.OutputTokens)
	if !ok {
		return tokenStats{}, errors.New("usage: token detail total overflows int64")
	}
	if total == 0 {
		cacheTotal, okCache := coreusage.AddNonNegativeInt64(tokens.CacheReadTokens, tokens.CacheWriteTokens)
		if !okCache {
			return tokenStats{}, errors.New("usage: cached token detail total overflows int64")
		}
		total = max(tokens.CachedTokens, cacheTotal, tokens.ReasoningTokens)
	}
	tokens.TotalTokens = total
	return tokens, nil
}

func normaliseTokenStats(tokens tokenStats) tokenStats {
	normalised, err := validatedTokenStats(tokens)
	if err != nil {
		log.Warn(err)
		return tokenStats{}
	}
	return normalised
}

func formatHour(hour int) string {
	if hour < 0 {
		hour = 0
	}
	hour = hour % 24
	return fmt.Sprintf("%02d", hour)
}

// archivedDetail pairs a trimmed requestDetail with its API and model names for archival.
type archivedDetail struct {
	APIName   string
	ModelName string
	Detail    requestDetail
}

// DetailsTruncated reports whether the bounded in-memory fallback discarded request details.
func (s *RequestStatistics) DetailsTruncated() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.detailsDropped > 0
}

// trimDetails removes all details older than the cutoff time from memory.
// Aggregate counters are preserved. Trimmed records are returned for archival.
func (s *RequestStatistics) trimDetails(cutoff time.Time) []archivedDetail {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var archived []archivedDetail
	var removedCount int64
	var removedBytes int64
	for apiName, stats := range s.apis {
		if stats == nil {
			continue
		}
		for modelName, ms := range stats.Models {
			if ms == nil || len(ms.Details) == 0 {
				continue
			}
			var kept []requestDetail
			for _, d := range ms.Details {
				d.ResponseHeaders = cloneHTTPHeader(d.ResponseHeaders)
				if d.Timestamp.Before(cutoff) {
					removedCount++
					removedBytes += estimateRequestDetailBytes(d)
					archived = append(
						archived, archivedDetail{
							APIName:   apiName,
							ModelName: modelName,
							Detail:    d,
						},
					)
				} else {
					kept = append(kept, d)
				}
			}
			ms.Details = kept
		}
	}
	s.detailsCount -= removedCount
	if s.detailsCount < 0 {
		s.detailsCount = 0
	}
	s.detailsBytes -= removedBytes
	if s.detailsBytes < 0 {
		s.detailsBytes = 0
	}
	return archived
}
