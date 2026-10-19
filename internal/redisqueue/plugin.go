package redisqueue

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coresession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/google/uuid"
)

func init() {
	coreusage.RegisterPlugin(newUsagePlugin())
}

// newUsagePlugin creates the Redis usage queue adapter for the compatibility queue.
func newUsagePlugin() coreusage.Plugin { return NewUsagePluginForQueue(defaultQueue) }

// NewUsagePluginForQueue creates an adapter bound to one service's queue.
func NewUsagePluginForQueue(queue *Queue) coreusage.Plugin {
	if queue == nil {
		queue = defaultQueue
	}
	return &usageQueuePlugin{queue: queue}
}

type usageQueuePlugin struct {
	queue *Queue
}

func cloneThinking(src *coreusage.Thinking) *coreusage.Thinking {
	if src == nil {
		return nil
	}
	dst := *src
	if src.Budget != nil {
		dst.Budget = new(int)
		*dst.Budget = *src.Budget
	}
	if dst.Intensity == "" && dst.Mode == "" && dst.Level == "" && dst.Budget == nil {
		return nil
	}
	return &dst
}

func sanitizeResponseHeaders(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		if strings.EqualFold(key, "Set-Cookie") {
			continue
		}
		clean := make([]string, 0, len(values))
		for _, value := range values {
			clean = append(clean, util.MaskSensitiveHeaderValue(key, value))
		}
		if len(clean) > 0 {
			dst[key] = clean
		}
	}
	if len(dst) == 0 {
		return nil
	}
	return dst
}

func (p *usageQueuePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil {
		return
	}
	queue := p.queue
	if queue == nil {
		queue = defaultQueue
	}
	if !queue.Enabled() || !queue.usageStatisticsOn() {
		return
	}

	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	modelName := strings.TrimSpace(record.Model)
	if modelName == "" {
		modelName = "unknown"
	}
	aliasName := strings.TrimSpace(record.Alias)
	if aliasName == "" {
		aliasName = modelName
	}
	provider := strings.TrimSpace(record.Provider)
	if provider == "" {
		provider = "unknown"
	}
	authType := strings.TrimSpace(record.AuthType)
	if authType == "" {
		authType = "unknown"
	}
	apiKey := util.HideAPIKey(strings.TrimSpace(record.APIKey))
	requestID := strings.TrimSpace(internallogging.GetRequestID(ctx))
	traceID := strings.TrimSpace(record.RequestID)
	if traceID == "" {
		traceID = requestID
	}
	executionID := strings.TrimSpace(record.ExecutionID)
	if executionID == "" {
		executionID = coreusage.ExecutionIDFromContext(ctx)
	}
	if executionID == "" {
		executionID = uuid.NewString()
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
	if executorType == "" {
		executorType = "unknown"
	}

	usageDetail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	tokens := tokenStats{
		InputTokens:         usageDetail.InputTokens,
		OutputTokens:        usageDetail.OutputTokens,
		ReasoningTokens:     usageDetail.ReasoningTokens,
		CachedTokens:        usageDetail.CachedTokens,
		CacheReadTokens:     usageDetail.CacheReadTokens,
		CacheCreationTokens: usageDetail.CacheCreationTokens,
		TotalTokens:         usageDetail.TotalTokens,
	}
	clientMetadata := internallogging.GetClientRequestMetadata(ctx)
	sessionID := strings.TrimSpace(record.SessionID)
	parentSessionID := strings.TrimSpace(record.ParentSessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(clientMetadata.SessionID)
		parentSessionID = strings.TrimSpace(clientMetadata.ParentSessionID)
	} else if parentSessionID == "" && sessionID == strings.TrimSpace(clientMetadata.SessionID) {
		parentSessionID = strings.TrimSpace(clientMetadata.ParentSessionID)
	}
	sessionID = coresession.NormalizeToCanonicalUUID(sessionID)
	parentSessionID = coresession.NormalizeToCanonicalUUID(parentSessionID)
	if sessionID == "" || sessionID == parentSessionID {
		parentSessionID = ""
	}
	var nodeKind string
	var isFork, isCompaction bool
	if sessionID != "" && sessionID == coresession.NormalizeToCanonicalUUID(clientMetadata.SessionID) {
		nodeKind = strings.TrimSpace(clientMetadata.NodeKind)
		isFork = clientMetadata.IsFork
		isCompaction = clientMetadata.IsCompaction
	}

	failed := record.Failed
	if !failed {
		failed = !resolveSuccess(ctx)
	}
	fail := resolveFail(ctx, record, failed)

	detail := requestDetail{
		Timestamp:       timestamp,
		LatencyMs:       record.Latency.Milliseconds(),
		TTFTMs:          record.TimeToFirstByteMs,
		Thinking:        cloneThinking(record.Thinking),
		Source:          record.Source,
		AuthIndex:       record.AuthIndex,
		AccessTokenHash: record.AccessTokenSHA256,
		ClientIP:        strings.TrimSpace(clientMetadata.ClientIP),
		XForwardedFor:   strings.TrimSpace(clientMetadata.XForwardedFor),
		UserAgent:       strings.TrimSpace(clientMetadata.UserAgent),
		Tokens:          tokens,
		Failed:          failed,
		Generate:        coreusage.GenerateEnabled(record.Generate),
		Fail:            fail,
		ResponseHeaders: sanitizeResponseHeaders(record.ResponseHeaders),
	}

	payload, err := json.Marshal(
		queuedUsageDetail{
			requestDetail:       detail,
			AccountingVersion:   coreusage.TokenAccountingSchemaVersion,
			TokenBreakdown:      usageDetail.TokenBreakdown,
			Provider:            provider,
			BaseURL:             strings.TrimSpace(record.BaseURL),
			ExecutorType:        executorType,
			Model:               modelName,
			Stream:              record.Stream,
			Alias:               aliasName,
			Endpoint:            resolveEndpoint(ctx),
			AuthType:            authType,
			APIKey:              apiKey,
			RequestID:           requestID,
			ExecutionID:         executionID,
			TraceID:             traceID,
			SessionID:           sessionID,
			ParentSessionID:     parentSessionID,
			NodeKind:            nodeKind,
			IsFork:              isFork,
			IsCompaction:        isCompaction,
			ReasoningEffort:     reasoningEffort,
			ServiceTier:         requestServiceTier,
			RequestServiceTier:  requestServiceTier,
			ResponseServiceTier: responseServiceTier,
			ResponseModel:       strings.TrimSpace(record.ResponseModel),
		},
	)
	if err != nil {
		return
	}
	queue.Enqueue(payload)
}

type queuedUsageDetail struct {
	requestDetail
	AccountingVersion   int                      `json:"accounting_version"`
	TokenBreakdown      coreusage.TokenBreakdown `json:"token_breakdown"`
	Provider            string                   `json:"provider"`
	BaseURL             string                   `json:"base_url,omitempty"`
	ExecutorType        string                   `json:"executor_type"`
	Model               string                   `json:"model"`
	Stream              bool                     `json:"stream"`
	Alias               string                   `json:"alias"`
	Endpoint            string                   `json:"endpoint"`
	AuthType            string                   `json:"auth_type"`
	APIKey              string                   `json:"api_key"`
	RequestID           string                   `json:"request_id"`
	ExecutionID         string                   `json:"execution_id,omitempty"`
	TraceID             string                   `json:"trace_id,omitempty"`
	SessionID           string                   `json:"session_id,omitempty"`
	ParentSessionID     string                   `json:"parent_session_id,omitempty"`
	NodeKind            string                   `json:"node_kind,omitempty"`
	IsFork              bool                     `json:"is_fork,omitempty"`
	IsCompaction        bool                     `json:"is_compaction,omitempty"`
	ReasoningEffort     string                   `json:"reasoning_effort"`
	ServiceTier         string                   `json:"service_tier"`
	RequestServiceTier  string                   `json:"request_service_tier"`
	ResponseServiceTier string                   `json:"response_service_tier,omitempty"`
	ResponseModel       string                   `json:"response_model,omitempty"`
}

type requestDetail struct {
	Timestamp       time.Time           `json:"timestamp"`
	LatencyMs       int64               `json:"latency_ms"`
	TTFTMs          int64               `json:"ttft_ms"`
	Thinking        *coreusage.Thinking `json:"thinking,omitempty"`
	Source          string              `json:"source"`
	AuthIndex       string              `json:"auth_index"`
	AccessTokenHash string              `json:"access_token_sha256,omitempty"`
	ClientIP        string              `json:"client_ip"`
	XForwardedFor   string              `json:"x_forwarded_for"`
	UserAgent       string              `json:"user_agent"`
	Tokens          tokenStats          `json:"tokens"`
	Failed          bool                `json:"failed"`
	Generate        bool                `json:"generate"`
	Fail            failDetail          `json:"fail"`
	ResponseHeaders http.Header         `json:"response_headers,omitempty"`
}

type tokenStats struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

type failDetail struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

func resolveFail(ctx context.Context, record coreusage.Record, failed bool) failDetail {
	fail := failDetail{
		StatusCode: record.Fail.StatusCode,
		Body:       strings.TrimSpace(record.Fail.Body),
	}
	if !failed {
		return failDetail{StatusCode: 200}
	}
	if fail.StatusCode <= 0 {
		fail.StatusCode = internallogging.GetResponseStatus(ctx)
	}
	if fail.StatusCode <= 0 {
		fail.StatusCode = 500
	}
	return fail
}

func resolveSuccess(ctx context.Context) bool {
	status := internallogging.GetResponseStatus(ctx)
	if status == 0 {
		return true
	}
	return status < httpStatusBadRequest
}

func resolveEndpoint(ctx context.Context) string {
	return strings.TrimSpace(internallogging.GetEndpoint(ctx))
}

const httpStatusBadRequest = 400
