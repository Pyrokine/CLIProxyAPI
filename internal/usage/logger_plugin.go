// Package usage provides usage tracking and logging functionality for the CLI Proxy API server.
// It includes plugins for monitoring API usage, token consumption, and other metrics
// to help with observability and billing purposes.
package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

// globalPersister is retained for callers using the process-wide default usage
// manager. Service instances bind their own persister directly to loggerPlugin.
var (
	globalPersisterMu sync.RWMutex
	globalPersister   *Persister
)

// SetGlobalPersister configures the compatibility logger on the default manager.
func SetGlobalPersister(p *Persister) {
	globalPersisterMu.Lock()
	defer globalPersisterMu.Unlock()
	globalPersister = p
}

// getGlobalPersister returns the active Persister, if any.
func getGlobalPersister() *Persister {
	globalPersisterMu.RLock()
	defer globalPersisterMu.RUnlock()
	return globalPersister
}

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
	useGlobal bool
}

// newLoggerPlugin constructs the compatibility plugin used by the default manager.
func newLoggerPlugin() *loggerPlugin {
	return &loggerPlugin{stats: defaultRequestStatistics, useGlobal: true}
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
	tokens := normaliseDetail(record.Detail)
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

	// Prefer the service-owned persister. The compatibility plugin resolves the
	// process-global pointer for callers that still use the default manager.
	persister := p.persister
	if p.useGlobal {
		persister = getGlobalPersister()
	}
	if persister != nil {
		detail := FlatDetail{
			Timestamp:           timestamp,
			Model:               modelName,
			Source:              source,
			AuthIndex:           authIndex,
			APIKey:              record.APIKey,
			Provider:            record.Provider,
			LatencyMs:           latencyMilliseconds(record.Latency),
			RequestID:           strings.TrimSpace(record.RequestID),
			TimeToFirstByteMs:   record.TimeToFirstByteMs,
			TotalDurationMs:     record.TotalDurationMs,
			Completed:           record.Completed,
			ReasoningEffort:     reasoningEffort,
			ServiceTier:         requestServiceTier,
			RequestServiceTier:  requestServiceTier,
			ResponseServiceTier: responseServiceTier,
			ExecutorType:        executorType,
			Thinking:            thinkingFromCore(record.Thinking),
			ResponseHeaders:     sanitizeResponseHeaders(record.ResponseHeaders),
			Tokens:              tokens,
			Failed:              failed,
		}
		persister.Record(detail)
		return
	}

	// Fallback: legacy RequestStatistics (used when no Persister is configured)
	if p == nil || p.stats == nil {
		return
	}
	p.stats.Record(ctx, record)
}

// SetStatisticsEnabled toggles whether in-memory statistics are recorded.
func SetStatisticsEnabled(enabled bool) { statisticsEnabled.Store(enabled) }

// RequestStatistics maintains aggregated request metrics in memory.
// This is the legacy storage used when no Persister is configured.
type RequestStatistics struct {
	mu sync.RWMutex

	totalRequests int64
	successCount  int64
	failureCount  int64
	totalTokens   int64

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
	Timestamp           time.Time   `json:"timestamp"`
	Source              string      `json:"source"`
	AuthIndex           string      `json:"auth_index"`
	LatencyMs           int64       `json:"latency_ms,omitempty"`
	RequestID           string      `json:"request_id,omitempty"`
	TimeToFirstByteMs   int64       `json:"time_to_first_byte_ms,omitempty"`
	TotalDurationMs     int64       `json:"total_duration_ms,omitempty"`
	Completed           bool        `json:"completed"`
	ReasoningEffort     string      `json:"reasoning_effort,omitempty"`
	ServiceTier         string      `json:"service_tier,omitempty"`
	RequestServiceTier  string      `json:"request_service_tier,omitempty"`
	ResponseServiceTier string      `json:"response_service_tier,omitempty"`
	ExecutorType        string      `json:"executor_type,omitempty"`
	Thinking            *Thinking   `json:"thinking,omitempty"`
	ResponseHeaders     http.Header `json:"response_headers,omitempty"`
	Tokens              tokenStats  `json:"tokens"`
	Failed              bool        `json:"failed"`
}

// tokenStats captures the token usage breakdown for a request.
type tokenStats struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
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

// Record ingests a new usage record and updates the aggregates (legacy path).
func (s *RequestStatistics) Record(ctx context.Context, record coreusage.Record) {
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
	detail := normaliseDetail(record.Detail)
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
			ReasoningEffort:     reasoningEffort,
			ServiceTier:         requestServiceTier,
			RequestServiceTier:  requestServiceTier,
			ResponseServiceTier: responseServiceTier,
			ExecutorType:        executorType,
			Thinking:            thinkingFromCore(record.Thinking),
			ResponseHeaders:     sanitizeResponseHeaders(record.ResponseHeaders),
			Tokens:              detail,
			Failed:              failed,
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
	modelStatsValue.Details = append(modelStatsValue.Details, detail)
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

// Snapshot returns a copy of the aggregated metrics for external consumption.
func (s *RequestStatistics) Snapshot() StatisticsSnapshot {
	result := StatisticsSnapshot{}
	if s == nil {
		return result
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	result.TotalRequests = s.totalRequests
	result.SuccessCount = s.successCount
	result.FailureCount = s.failureCount
	result.TotalTokens = s.totalTokens

	result.APIs = make(map[string]aPISnapshot, len(s.apis))
	for apiName, stats := range s.apis {
		apiSnapshot := aPISnapshot{
			TotalRequests: stats.TotalRequests,
			TotalTokens:   stats.TotalTokens,
			Models:        make(map[string]modelSnapshot, len(stats.Models)),
		}
		for modelName, modelStatsValue := range stats.Models {
			requestDetails := make([]requestDetail, len(modelStatsValue.Details))
			for i, detail := range modelStatsValue.Details {
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

	result.RequestsByDay = make(map[string]int64, len(s.requestsByDay))
	maps.Copy(result.RequestsByDay, s.requestsByDay)

	result.RequestsByHour = make(map[string]int64, len(s.requestsByHour))
	for hour, v := range s.requestsByHour {
		key := formatHour(hour)
		result.RequestsByHour[key] = v
	}

	result.TokensByDay = make(map[string]int64, len(s.tokensByDay))
	maps.Copy(result.TokensByDay, s.tokensByDay)

	result.TokensByHour = make(map[string]int64, len(s.tokensByHour))
	for hour, v := range s.tokensByHour {
		key := formatHour(hour)
		result.TokensByHour[key] = v
	}

	return result
}

type MergeResult struct {
	Added   int64 `json:"added"`
	Skipped int64 `json:"skipped"`
}

// maxImportFieldLen limits apiName/modelName length in imports to prevent memory abuse.
const maxImportFieldLen = 256

// ImportHook is called for each successfully imported detail with the resolved fields.
type ImportHook func(detail FlatDetail)

// MergeSnapshot merges an exported statistics snapshot into the current store.
// Existing data is preserved and duplicate request details are skipped.
// An optional hook is called for each newly added detail (after dedup, before return).
func (s *RequestStatistics) MergeSnapshot(snapshot StatisticsSnapshot, hooks ...ImportHook) MergeResult {
	result := MergeResult{}
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
							ReasoningEffort:     strings.TrimSpace(detail.ReasoningEffort),
							ServiceTier:         requestServiceTier,
							RequestServiceTier:  requestServiceTier,
							ResponseServiceTier: strings.TrimSpace(detail.ResponseServiceTier),
							ExecutorType:        strings.TrimSpace(detail.ExecutorType),
							Thinking:            cloneThinking(detail.Thinking),
							ResponseHeaders:     cloneHTTPHeader(detail.ResponseHeaders),
							Tokens:              detail.Tokens,
							Failed:              detail.Failed,
						},
					)
				}
			}
		}
	}

	return result
}

func (s *RequestStatistics) recordImported(_, modelName string, stats *apiStats, detail requestDetail) {
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
	if detail.CacheReadTokens < 0 || detail.CacheCreationTokens < 0 {
		log.Warn("usage: rejected negative token detail")
		return tokenStats{}
	}
	return normaliseTokenStats(
		tokenStats{
			InputTokens:     detail.InputTokens,
			OutputTokens:    detail.OutputTokens,
			ReasoningTokens: detail.ReasoningTokens,
			CachedTokens:    detail.CachedTokens,
			TotalTokens:     detail.TotalTokens,
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
		{name: "total_tokens", value: tokens.TotalTokens},
	} {
		if field.value < 0 {
			return tokenStats{}, fmt.Errorf("usage: %s must be non-negative", field.name)
		}
	}
	if tokens.TotalTokens != 0 {
		return tokens, nil
	}
	total, ok := coreusage.AddNonNegativeInt64(
		tokens.InputTokens,
		tokens.OutputTokens,
		tokens.ReasoningTokens,
		tokens.CachedTokens,
	)
	if !ok {
		return tokenStats{}, errors.New("usage: token detail total overflows int64")
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

// ArchivedDetail pairs a trimmed requestDetail with its API and model names for archival.
type ArchivedDetail struct {
	APIName   string
	ModelName string
	Detail    requestDetail
}

// TrimDetails removes all details older than the cutoff time from memory.
// Aggregate counters are preserved. Trimmed records are returned for archival.
func (s *RequestStatistics) TrimDetails(cutoff time.Time) []ArchivedDetail {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var archived []ArchivedDetail
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
					archived = append(
						archived, ArchivedDetail{
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
	return archived
}
