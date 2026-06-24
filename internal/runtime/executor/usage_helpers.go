package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type usageReporter struct {
	provider        string
	executorType    string
	model           string
	alias           string
	authID          string
	authIndex       string
	authType        string
	apiKey          string
	source          string
	reasoningEffort string
	serviceTier     string
	thinking        *usage.Thinking
	requestedAt     time.Time
	once            sync.Once
}

type usageExecutor interface {
	Identifier() string
}

func newUsageReporter(
	ctx context.Context,
	providerOrExecutor any,
	model string,
	auth *cliproxyauth.Auth,
) *usageReporter {
	provider, executorType := usageReporterProvider(providerOrExecutor)
	apiKey := apiKeyFromContext(ctx)
	alias := usage.RequestedModelAliasFromContext(ctx)
	if alias == "" {
		alias = model
	}
	reporter := &usageReporter{
		provider:        provider,
		executorType:    executorType,
		model:           model,
		alias:           strings.TrimSpace(alias),
		requestedAt:     time.Now(),
		apiKey:          apiKey,
		source:          resolveUsageSource(auth, apiKey),
		authType:        resolveUsageAuthType(auth),
		reasoningEffort: usage.ReasoningEffortFromContext(ctx),
		serviceTier:     usage.ServiceTierFromContext(ctx),
		thinking:        thinkingFromAlias(alias),
	}
	if auth != nil {
		reporter.authID = auth.ID
		reporter.authIndex = auth.EnsureIndex()
	}
	return reporter
}

func (r *usageReporter) SetTranslatedReasoningEffort(payload []byte, format string) {
	if r == nil {
		return
	}
	r.reasoningEffort = thinking.ExtractTranslatedReasoningEffort(payload, format)
	r.serviceTier = extractServiceTierFromPayload(payload)
}

func extractServiceTierFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return usage.DefaultServiceTier
	}
	for _, path := range []string{"service_tier", "request.service_tier", "response.service_tier"} {
		value := strings.TrimSpace(gjson.GetBytes(payload, path).String())
		if value != "" {
			return value
		}
	}
	return usage.DefaultServiceTier
}

func usageReporterProvider(value any) (string, string) {
	switch v := value.(type) {
	case nil:
		return "", ""
	case string:
		trimmed := strings.TrimSpace(v)
		return trimmed, trimmed
	case usageExecutor:
		provider := strings.TrimSpace(v.Identifier())
		executorType := executorTypeName(v)
		if executorType == "" {
			executorType = provider
		}
		return provider, executorType
	default:
		name := executorTypeName(v)
		return strings.TrimSpace(fmt.Sprint(v)), name
	}
}

func executorTypeName(value any) string {
	if value == nil {
		return ""
	}
	t := reflect.TypeOf(value)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return strings.TrimSpace(t.Name())
}

func cloneUsageThinking(src *usage.Thinking) *usage.Thinking {
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

func thinkingFromAlias(alias string) *usage.Thinking {
	parsed := thinking.ParseSuffix(alias)
	if !parsed.HasSuffix {
		return nil
	}
	raw := strings.TrimSpace(parsed.RawSuffix)
	if raw == "" {
		return nil
	}
	lower := strings.ToLower(raw)
	if lower == "none" {
		return &usage.Thinking{Intensity: "none", Mode: "none", Level: "none", Budget: new(int)}
	}
	if lower == "auto" || raw == "-1" {
		budget := new(int)
		*budget = -1
		return &usage.Thinking{Intensity: "auto", Mode: "auto", Budget: budget}
	}
	if budget, err := strconv.Atoi(raw); err == nil && budget >= 0 {
		if budget == 0 {
			return &usage.Thinking{Intensity: "none", Mode: "none", Level: "none", Budget: &budget}
		}
		return &usage.Thinking{Mode: "budget", Budget: &budget}
	}
	switch lower {
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return &usage.Thinking{Intensity: lower, Mode: "level", Level: lower}
	default:
		return nil
	}
}

func (r *usageReporter) publish(ctx context.Context, detail usage.Detail) {
	r.publishWithOutcome(ctx, detail, false, usage.Failure{})
}

func (r *usageReporter) publishAdditionalModel(ctx context.Context, model string, detail usage.Detail) {
	if r == nil {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	if detail.TotalTokens == 0 {
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens + usageCachedTokens(detail)
	}
	if detail.InputTokens == 0 && detail.OutputTokens == 0 && detail.ReasoningTokens == 0 && detail.CachedTokens == 0 &&
		detail.CacheReadTokens == 0 && detail.CacheCreationTokens == 0 && detail.TotalTokens == 0 {
		return
	}
	r.publishRecord(
		ctx, usage.Record{
			Provider:        r.provider,
			ExecutorType:    r.executorType,
			Model:           model,
			Alias:           model,
			Source:          r.source,
			APIKey:          r.apiKey,
			AuthID:          r.authID,
			AuthIndex:       r.authIndex,
			AuthType:        r.authType,
			ReasoningEffort: r.reasoningEffort,
			ServiceTier:     r.serviceTier,
			RequestedAt:     r.requestedAt,
			Latency:         r.latency(),
			Thinking:        cloneUsageThinking(r.thinking),
			Failed:          false,
			Detail:          detail,
		},
	)
}

func (r *usageReporter) publishFailure(ctx context.Context, errs ...error) {
	r.publishWithOutcome(ctx, usage.Detail{}, true, failFromErrors(errs...))
}

func (r *usageReporter) trackFailure(ctx context.Context, errPtr *error) {
	if r == nil || errPtr == nil {
		return
	}
	if *errPtr != nil {
		r.publishFailure(ctx, *errPtr)
	}
}

func (r *usageReporter) publishWithOutcome(ctx context.Context, detail usage.Detail, failed bool, fail usage.Failure) {
	if r == nil {
		return
	}
	if detail.TotalTokens == 0 {
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens + usageCachedTokens(detail)
	}
	r.once.Do(
		func() {
			r.publishRecord(
				ctx, usage.Record{
					Provider:        r.provider,
					ExecutorType:    r.executorType,
					Model:           r.model,
					Alias:           r.alias,
					Source:          r.source,
					APIKey:          r.apiKey,
					AuthID:          r.authID,
					AuthIndex:       r.authIndex,
					AuthType:        r.authType,
					ReasoningEffort: r.reasoningEffort,
					ServiceTier:     r.serviceTier,
					RequestedAt:     r.requestedAt,
					Latency:         r.latency(),
					Thinking:        cloneUsageThinking(r.thinking),
					Failed:          failed,
					Fail:            fail,
					Detail:          detail,
				},
			)
		},
	)
}

func usageCachedTokens(detail usage.Detail) int64 {
	if detail.CachedTokens != 0 {
		return detail.CachedTokens
	}
	return detail.CacheReadTokens + detail.CacheCreationTokens
}

// ensurePublished guarantees that a usage record is emitted exactly once.
// It is safe to call multiple times; only the first call wins due to once.Do.
// This is used to ensure request counting even when upstream responses do not
// include any usage fields (tokens), especially for streaming paths.
func (r *usageReporter) ensurePublished(ctx context.Context) {
	if r == nil {
		return
	}
	r.once.Do(
		func() {
			r.publishRecord(
				ctx, usage.Record{
					Provider:        r.provider,
					ExecutorType:    r.executorType,
					Model:           r.model,
					Alias:           r.alias,
					Source:          r.source,
					APIKey:          r.apiKey,
					AuthID:          r.authID,
					AuthIndex:       r.authIndex,
					AuthType:        r.authType,
					ReasoningEffort: r.reasoningEffort,
					ServiceTier:     r.serviceTier,
					RequestedAt:     r.requestedAt,
					Latency:         r.latency(),
					Thinking:        cloneUsageThinking(r.thinking),
					Failed:          false,
					Detail:          usage.Detail{},
				},
			)
		},
	)
}

func (r *usageReporter) publishRecord(ctx context.Context, record usage.Record) {
	if record.RequestID == "" {
		record.RequestID = logging.GetRequestID(ctx)
	}
	if record.TotalDurationMs <= 0 && record.Latency > 0 {
		record.TotalDurationMs = record.Latency.Milliseconds()
	}
	if record.TimeToFirstByteMs <= 0 && !record.RequestedAt.IsZero() {
		if firstByte := logging.GetFirstResponseByteAt(ctx); !firstByte.IsZero() {
			elapsed := firstByte.Sub(record.RequestedAt)
			if elapsed > 0 {
				record.TimeToFirstByteMs = elapsed.Milliseconds()
			}
		}
	}
	record.Completed = !record.Failed
	record.ResponseHeaders = logging.GetResponseHeaders(ctx)
	usage.PublishRecord(ctx, record)
}

func (r *usageReporter) latency() time.Duration {
	if r == nil || r.requestedAt.IsZero() {
		return 0
	}
	latency := time.Since(r.requestedAt)
	if latency < 0 {
		return 0
	}
	return latency
}

func failFromErrors(errs ...error) usage.Failure {
	for _, err := range errs {
		if err == nil {
			continue
		}
		fail := usage.Failure{
			Body: strings.TrimSpace(err.Error()),
		}
		var se interface{ StatusCode() int }
		if errors.As(err, &se) && se != nil {
			fail.StatusCode = se.StatusCode()
		}
		return fail
	}
	return usage.Failure{}
}

func apiKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, ok := util.GinContextValue(ctx).(*gin.Context)
	if !ok || ginCtx == nil {
		return ""
	}
	if v, exists := ginCtx.Get("apiKey"); exists {
		switch value := v.(type) {
		case string:
			return value
		case fmt.Stringer:
			return value.String()
		default:
			return fmt.Sprintf("%v", value)
		}
	}
	return ""
}

func resolveUsageAuthType(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	kind, _ := auth.AccountInfo()
	kind = strings.TrimSpace(kind)
	if kind == "api_key" {
		return "apikey"
	}
	return kind
}

func resolveUsageSource(auth *cliproxyauth.Auth, ctxAPIKey string) string {
	if auth != nil {
		provider := strings.TrimSpace(auth.Provider)
		if strings.EqualFold(provider, "gemini-cli") {
			if id := strings.TrimSpace(auth.ID); id != "" {
				return id
			}
		}
		if strings.EqualFold(provider, "vertex") {
			if auth.Metadata != nil {
				if projectID, ok := auth.Metadata["project_id"].(string); ok {
					if trimmed := strings.TrimSpace(projectID); trimmed != "" {
						return trimmed
					}
				}
				if project, ok := auth.Metadata["project"].(string); ok {
					if trimmed := strings.TrimSpace(project); trimmed != "" {
						return trimmed
					}
				}
			}
		}
		if _, value := auth.AccountInfo(); value != "" {
			return strings.TrimSpace(value)
		}
		if auth.Metadata != nil {
			if email, ok := auth.Metadata["email"].(string); ok {
				if trimmed := strings.TrimSpace(email); trimmed != "" {
					return trimmed
				}
			}
		}
		if auth.Attributes != nil {
			if key := strings.TrimSpace(auth.Attributes["api_key"]); key != "" {
				return maskAPIKey(key)
			}
		}
	}
	if trimmed := strings.TrimSpace(ctxAPIKey); trimmed != "" {
		return maskAPIKey(trimmed)
	}
	return ""
}

// maskAPIKey redacts an API key to prefix + last 4 characters (e.g. "sk-pyro-...d878").
func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	dashIdx := strings.LastIndex(key, "-")
	if dashIdx == -1 || dashIdx >= len(key)-4 {
		return key[:4] + "..." + key[len(key)-4:]
	}
	prefix := key[:dashIdx+1]
	suffix := key[len(key)-4:]
	return prefix + "..." + suffix
}

func parseCodexUsage(data []byte) (usage.Detail, bool) {
	usageNode := gjson.ParseBytes(data).Get("response.usage")
	if !openAIStyleUsageTokenFieldsExist(usageNode) {
		return usage.Detail{}, false
	}
	return parseOpenAIStyleUsageNode(usageNode), true
}

func parseOpenAIUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data).Get("usage")
	if !openAIStyleUsageTokenFieldsExist(usageNode) {
		return usage.Detail{}
	}
	return parseOpenAIStyleUsageNode(usageNode)
}

func parseOpenAIStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	usageNode := gjson.GetBytes(payload, "usage")
	if !openAIStyleUsageTokenFieldsExist(usageNode) {
		return usage.Detail{}, false
	}
	return parseOpenAIStyleUsageNode(usageNode), true
}

func parseClaudeUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data).Get("usage")
	if !usageNode.Exists() {
		return usage.Detail{}
	}
	return parseClaudeUsageNode(usageNode)
}

func parseClaudeUsageNode(usageNode gjson.Result) usage.Detail {
	cacheReadTokens := usageNode.Get("cache_read_input_tokens").Int()
	cacheCreationTokens := usageNode.Get("cache_creation_input_tokens").Int()
	detail := usage.Detail{
		InputTokens:         usageNode.Get("input_tokens").Int(),
		OutputTokens:        usageNode.Get("output_tokens").Int(),
		CachedTokens:        cacheReadTokens,
		CacheReadTokens:     cacheReadTokens,
		CacheCreationTokens: cacheCreationTokens,
	}
	if detail.CachedTokens == 0 {
		detail.CachedTokens = detail.CacheCreationTokens
	}
	detail.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.CacheReadTokens + detail.CacheCreationTokens
	return detail
}

func parseClaudeStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	usageNode := gjson.GetBytes(payload, "usage")
	if !usageNode.Exists() {
		return usage.Detail{}, false
	}
	return parseClaudeUsageNode(usageNode), true
}

func parseGeminiFamilyUsageDetail(node gjson.Result) usage.Detail {
	detail := usage.Detail{
		InputTokens:     node.Get("promptTokenCount").Int(),
		OutputTokens:    node.Get("candidatesTokenCount").Int(),
		ReasoningTokens: node.Get("thoughtsTokenCount").Int(),
		TotalTokens:     node.Get("totalTokenCount").Int(),
		CachedTokens:    node.Get("cachedContentTokenCount").Int(),
	}
	if detail.TotalTokens == 0 {
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	return detail
}

func parseGeminiCLIUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data)
	node := usageNode.Get("response.usageMetadata")
	if !node.Exists() {
		node = usageNode.Get("response.usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}
	}
	return parseGeminiFamilyUsageDetail(node)
}

func parseGeminiUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data)
	node := usageNode.Get("usageMetadata")
	if !node.Exists() {
		node = usageNode.Get("usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}
	}
	return parseGeminiFamilyUsageDetail(node)
}

func parseGeminiStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	node := gjson.GetBytes(payload, "usageMetadata")
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}, false
	}
	return parseGeminiFamilyUsageDetail(node), true
}

func parseGeminiCLIStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	node := gjson.GetBytes(payload, "response.usageMetadata")
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}, false
	}
	return parseGeminiFamilyUsageDetail(node), true
}

func parseAntigravityUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data)
	node := usageNode.Get("response.usageMetadata")
	if !node.Exists() {
		node = usageNode.Get("usageMetadata")
	}
	if !node.Exists() {
		node = usageNode.Get("usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}
	}
	return parseGeminiFamilyUsageDetail(node)
}

func parseAntigravityStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	node := gjson.GetBytes(payload, "response.usageMetadata")
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usageMetadata")
	}
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}, false
	}
	return parseGeminiFamilyUsageDetail(node), true
}

var stopChunkWithoutUsage sync.Map

func rememberStopWithoutUsage(traceID string) {
	stopChunkWithoutUsage.Store(traceID, struct{}{})
	time.AfterFunc(10*time.Minute, func() { stopChunkWithoutUsage.Delete(traceID) })
}

// filterSSEUsageMetadata removes usageMetadata from SSE events that are not
// terminal (finishReason != "stop"). Stop chunks are left untouched. This
// function is shared between aistudio and antigravity executors.
func filterSSEUsageMetadata(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}

	lines := bytes.Split(payload, []byte("\n"))
	modified := false
	foundData := false
	for idx, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		foundData = true
		before, after, ok := bytes.Cut(line, []byte("data:"))
		if !ok {
			continue
		}
		rawJSON := bytes.TrimSpace(after)
		traceID := gjson.GetBytes(rawJSON, "traceId").String()
		if isStopChunkWithoutUsage(rawJSON) && traceID != "" {
			rememberStopWithoutUsage(traceID)
			continue
		}
		if traceID != "" {
			if _, ok := stopChunkWithoutUsage.Load(traceID); ok && hasUsageMetadata(rawJSON) {
				stopChunkWithoutUsage.Delete(traceID)
				continue
			}
		}

		cleaned, changed := stripUsageMetadataFromJSON(rawJSON)
		if !changed {
			continue
		}
		var rebuilt []byte
		rebuilt = append(rebuilt, before...)
		rebuilt = append(rebuilt, []byte("data:")...)
		if len(cleaned) > 0 {
			rebuilt = append(rebuilt, ' ')
			rebuilt = append(rebuilt, cleaned...)
		}
		lines[idx] = rebuilt
		modified = true
	}
	if !modified {
		if !foundData {
			// Handle payloads that are raw JSON without SSE data: prefix.
			trimmed := bytes.TrimSpace(payload)
			cleaned, changed := stripUsageMetadataFromJSON(trimmed)
			if !changed {
				return payload
			}
			return cleaned
		}
		return payload
	}
	return bytes.Join(lines, []byte("\n"))
}

// stripUsageMetadataFromJSON drops usageMetadata unless finishReason is present (terminal).
// It handles both formats:
// - Aistudio: candidates.0.finishReason
// - Antigravity: response.candidates.0.finishReason
func stripUsageMetadataFromJSON(rawJSON []byte) ([]byte, bool) {
	jsonBytes := bytes.TrimSpace(rawJSON)
	if len(jsonBytes) == 0 || !gjson.ValidBytes(jsonBytes) {
		return rawJSON, false
	}

	// Check for finishReason in both aistudio and antigravity formats
	finishReason := gjson.GetBytes(jsonBytes, "candidates.0.finishReason")
	if !finishReason.Exists() {
		finishReason = gjson.GetBytes(jsonBytes, "response.candidates.0.finishReason")
	}
	terminalReason := finishReason.Exists() && strings.TrimSpace(finishReason.String()) != ""

	usageMetadata := gjson.GetBytes(jsonBytes, "usageMetadata")
	if !usageMetadata.Exists() {
		usageMetadata = gjson.GetBytes(jsonBytes, "response.usageMetadata")
	}

	// Terminal chunk: keep as-is.
	if terminalReason {
		return rawJSON, false
	}

	// Nothing to strip
	if !usageMetadata.Exists() {
		return rawJSON, false
	}

	// Remove usageMetadata from both possible locations
	cleaned := jsonBytes
	var changed bool

	if usageMetadata = gjson.GetBytes(cleaned, "usageMetadata"); usageMetadata.Exists() {
		// Rename usageMetadata to cpaUsageMetadata in the message_start event of Claude
		cleaned, _ = sjson.SetRawBytes(cleaned, "cpaUsageMetadata", []byte(usageMetadata.Raw))
		cleaned, _ = sjson.DeleteBytes(cleaned, "usageMetadata")
		changed = true
	}

	if usageMetadata = gjson.GetBytes(cleaned, "response.usageMetadata"); usageMetadata.Exists() {
		// Rename usageMetadata to cpaUsageMetadata in the message_start event of Claude
		cleaned, _ = sjson.SetRawBytes(cleaned, "response.cpaUsageMetadata", []byte(usageMetadata.Raw))
		cleaned, _ = sjson.DeleteBytes(cleaned, "response.usageMetadata")
		changed = true
	}

	return cleaned, changed
}

func hasUsageMetadata(jsonBytes []byte) bool {
	if len(jsonBytes) == 0 || !gjson.ValidBytes(jsonBytes) {
		return false
	}
	if gjson.GetBytes(jsonBytes, "usageMetadata").Exists() {
		return true
	}
	if gjson.GetBytes(jsonBytes, "response.usageMetadata").Exists() {
		return true
	}
	return false
}

func isStopChunkWithoutUsage(jsonBytes []byte) bool {
	if len(jsonBytes) == 0 || !gjson.ValidBytes(jsonBytes) {
		return false
	}
	finishReason := gjson.GetBytes(jsonBytes, "candidates.0.finishReason")
	if !finishReason.Exists() {
		finishReason = gjson.GetBytes(jsonBytes, "response.candidates.0.finishReason")
	}
	trimmed := strings.TrimSpace(finishReason.String())
	if !finishReason.Exists() || trimmed == "" {
		return false
	}
	return !hasUsageMetadata(jsonBytes)
}

func jsonPayload(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("event:")) {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	return trimmed
}
