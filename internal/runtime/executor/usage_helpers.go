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
	log "github.com/sirupsen/logrus"
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
	sourceKind      usage.CredentialSourceKind
	sourceRedacted  bool
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
	source, sourceKind, sourceRedacted := resolveUsageSource(auth, apiKey)
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
		source:          source,
		sourceKind:      sourceKind,
		sourceRedacted:  sourceRedacted,
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
	if serviceTier, ok := explicitServiceTierFromPayload(payload); ok {
		r.serviceTier = serviceTier
	}
}

func extractServiceTierFromPayload(payload []byte) string {
	if serviceTier, ok := explicitServiceTierFromPayload(payload); ok {
		return serviceTier
	}
	return usage.DefaultServiceTier
}

func explicitServiceTierFromPayload(payload []byte) (string, bool) {
	if len(payload) == 0 {
		return "", false
	}
	for _, path := range []string{"service_tier", "request.service_tier", "response.service_tier"} {
		value := gjson.GetBytes(payload, path)
		if value.Type != gjson.String {
			continue
		}
		if tier := strings.TrimSpace(value.String()); tier != "" {
			return tier, true
		}
	}
	return "", false
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
	detail = normalizeUsageDetailTotal(detail)
	r.publishRecordImmediate(
		ctx, usage.Record{
			Provider:            r.provider,
			ExecutorType:        r.executorType,
			Model:               model,
			Alias:               model,
			Source:              r.source,
			SourceKind:          r.sourceKind,
			SourceRedacted:      r.sourceRedacted,
			APIKey:              r.apiKey,
			AuthID:              r.authID,
			AuthIndex:           r.authIndex,
			AuthType:            r.authType,
			ReasoningEffort:     r.reasoningEffort,
			ServiceTier:         r.serviceTier,
			RequestServiceTier:  r.serviceTier,
			ResponseServiceTier: strings.TrimSpace(detail.ResponseServiceTier),
			RequestedAt:         r.requestedAt,
			Latency:             r.latency(),
			Thinking:            cloneUsageThinking(r.thinking),
			Failed:              false,
			Detail:              detail,
		},
	)
}

func (r *usageReporter) publishFailure(ctx context.Context, errs ...error) {
	r.publishFailureWithDetail(ctx, usage.Detail{}, errs...)
}

func (r *usageReporter) publishFailureWithDetail(ctx context.Context, detail usage.Detail, errs ...error) {
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	r.publishWithOutcome(ctx, detail, true, failFromErrors(errs...))
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
	detail = normalizeUsageDetailTotal(detail)
	r.once.Do(
		func() {
			r.publishRecord(
				ctx, usage.Record{
					Provider:            r.provider,
					ExecutorType:        r.executorType,
					Model:               r.model,
					Alias:               r.alias,
					Source:              r.source,
					SourceKind:          r.sourceKind,
					SourceRedacted:      r.sourceRedacted,
					APIKey:              r.apiKey,
					AuthID:              r.authID,
					AuthIndex:           r.authIndex,
					AuthType:            r.authType,
					ReasoningEffort:     r.reasoningEffort,
					ServiceTier:         r.serviceTier,
					RequestServiceTier:  r.serviceTier,
					ResponseServiceTier: strings.TrimSpace(detail.ResponseServiceTier),
					RequestedAt:         r.requestedAt,
					Latency:             r.latency(),
					Thinking:            cloneUsageThinking(r.thinking),
					Failed:              failed,
					Fail:                fail,
					Detail:              detail,
				},
			)
		},
	)
}

func normalizeUsageDetailTotal(detail usage.Detail) usage.Detail {
	for _, value := range []int64{
		detail.InputTokens,
		detail.OutputTokens,
		detail.ReasoningTokens,
		detail.CachedTokens,
		detail.CacheReadTokens,
		detail.CacheCreationTokens,
		detail.TotalTokens,
	} {
		if value < 0 {
			log.Warn("usage: rejected negative token detail")
			return usage.Detail{ResponseServiceTier: detail.ResponseServiceTier}
		}
	}
	if detail.TotalTokens != 0 {
		return detail
	}
	total, ok := usage.AddNonNegativeInt64(detail.InputTokens, detail.OutputTokens, detail.ReasoningTokens)
	if !ok {
		log.Warn("usage: rejected overflowing token detail")
		return usage.Detail{ResponseServiceTier: detail.ResponseServiceTier}
	}
	detail.TotalTokens = total
	return detail
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
					Provider:           r.provider,
					ExecutorType:       r.executorType,
					Model:              r.model,
					Alias:              r.alias,
					Source:             r.source,
					SourceKind:         r.sourceKind,
					SourceRedacted:     r.sourceRedacted,
					APIKey:             r.apiKey,
					AuthID:             r.authID,
					AuthIndex:          r.authIndex,
					AuthType:           r.authType,
					ReasoningEffort:    r.reasoningEffort,
					ServiceTier:        r.serviceTier,
					RequestServiceTier: r.serviceTier,
					RequestedAt:        r.requestedAt,
					Latency:            r.latency(),
					Thinking:           cloneUsageThinking(r.thinking),
					Failed:             false,
					Detail:             usage.Detail{},
				},
			)
		},
	)
}

func (r *usageReporter) publishRecord(ctx context.Context, record usage.Record) {
	record = r.finalizeRecord(ctx, record)
	if !record.Failed && usage.DeferRecord(ctx, record) {
		return
	}
	usage.PublishRecord(ctx, record)
}

func (r *usageReporter) publishRecordImmediate(ctx context.Context, record usage.Record) {
	usage.PublishRecord(ctx, r.finalizeRecord(ctx, record))
}

func (r *usageReporter) finalizeRecord(ctx context.Context, record usage.Record) usage.Record {
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
	return record
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

func resolveUsageSource(
	auth *cliproxyauth.Auth,
	ctxAPIKey string,
) (string, usage.CredentialSourceKind, bool) {
	if auth != nil {
		provider := strings.TrimSpace(auth.Provider)
		if strings.EqualFold(provider, "gemini-cli") {
			if id := strings.TrimSpace(auth.ID); id != "" {
				return id, usage.CredentialSourceKindIdentity, false
			}
		}
		if strings.EqualFold(provider, "vertex") {
			if auth.Metadata != nil {
				if projectID, ok := auth.Metadata["project_id"].(string); ok {
					if trimmed := strings.TrimSpace(projectID); trimmed != "" {
						return trimmed, usage.CredentialSourceKindIdentity, false
					}
				}
				if project, ok := auth.Metadata["project"].(string); ok {
					if trimmed := strings.TrimSpace(project); trimmed != "" {
						return trimmed, usage.CredentialSourceKindIdentity, false
					}
				}
			}
		}
		if kind, value := auth.AccountInfo(); strings.TrimSpace(value) != "" {
			trimmed := strings.TrimSpace(value)
			if strings.EqualFold(strings.TrimSpace(kind), "api_key") {
				return util.HideAPIKey(trimmed), usage.CredentialSourceKindAPIKey, true
			}
			return trimmed, usage.CredentialSourceKindIdentity, false
		}
		if auth.Metadata != nil {
			if email, ok := auth.Metadata["email"].(string); ok {
				if trimmed := strings.TrimSpace(email); trimmed != "" {
					return trimmed, usage.CredentialSourceKindIdentity, false
				}
			}
		}
		if auth.Attributes != nil {
			if key := strings.TrimSpace(auth.Attributes["api_key"]); key != "" {
				return util.HideAPIKey(key), usage.CredentialSourceKindAPIKey, true
			}
		}
	}
	if trimmed := strings.TrimSpace(ctxAPIKey); trimmed != "" {
		return util.HideAPIKey(trimmed), usage.CredentialSourceKindAPIKey, true
	}
	return "", "", false
}

type streamUsageBuffer struct {
	detail          usage.Detail
	hasUsage        bool
	hasTier         bool
	pending         []byte
	pendingOverflow bool
}

const maxStreamUsageLineBytes = 50 << 20

var (
	openAIStreamUsageMarker = []byte(`"usage"`)
	openAIStreamTierMarker  = []byte(`"service_tier"`)
)

func (b *streamUsageBuffer) observe(detail usage.Detail, hasUsage bool, hasTier bool) {
	if b == nil || (!hasUsage && !hasTier) {
		return
	}
	if hasUsage {
		preservedTier := b.detail.ResponseServiceTier
		b.detail = detail
		if !hasTier {
			b.detail.ResponseServiceTier = preservedTier
		}
		b.hasUsage = true
	}
	if hasTier {
		b.detail.ResponseServiceTier = strings.TrimSpace(detail.ResponseServiceTier)
		b.hasTier = true
	}
}

func (b *streamUsageBuffer) observeOpenAIStream(line []byte) {
	if b == nil {
		return
	}
	payload := jsonPayload(line)
	if len(payload) == 0 {
		return
	}
	hasDirectMarker := bytes.Contains(payload, openAIStreamUsageMarker) ||
		bytes.Contains(payload, openAIStreamTierMarker)
	if !hasDirectMarker && !bytes.Contains(payload, []byte(`\u`)) {
		return
	}
	if !gjson.ValidBytes(payload) {
		return
	}

	var detail usage.Detail
	usageNode := gjson.GetBytes(payload, "usage")
	hasUsage := openAIStyleUsageTokenFieldsExist(usageNode)
	if hasUsage {
		detail = parseOpenAIStyleUsageNode(usageNode)
	}
	detail.ResponseServiceTier = extractResponseServiceTierFromValidJSON(payload)
	b.observe(detail, hasUsage, detail.ResponseServiceTier != "")
}

func (b *streamUsageBuffer) observeOpenAIStreamChunk(chunk []byte) {
	if b == nil || len(chunk) == 0 {
		return
	}
	for len(chunk) > 0 {
		lineEnd := bytes.IndexByte(chunk, '\n')
		if b.pendingOverflow {
			if lineEnd < 0 {
				return
			}
			b.pendingOverflow = false
			chunk = chunk[lineEnd+1:]
			continue
		}

		if lineEnd < 0 {
			if len(b.pending)+len(chunk) > maxStreamUsageLineBytes {
				b.discardOversizedOpenAIStreamLine()
				return
			}
			b.pending = append(b.pending, chunk...)
			return
		}

		line := chunk[:lineEnd]
		if len(b.pending)+len(line) > maxStreamUsageLineBytes {
			b.discardOversizedOpenAIStreamLine()
			b.pendingOverflow = false
		} else {
			b.pending = append(b.pending, line...)
			b.observeOpenAIStream(b.pending)
			b.pending = b.pending[:0]
		}
		chunk = chunk[lineEnd+1:]
	}
}

func (b *streamUsageBuffer) discardOversizedOpenAIStreamLine() {
	b.pending = nil
	b.pendingOverflow = true
	log.Warnf("stream usage SSE line exceeded %d bytes and was ignored", maxStreamUsageLineBytes)
}

func (b *streamUsageBuffer) flushOpenAIStream() {
	if b == nil {
		return
	}
	if !b.pendingOverflow && len(b.pending) > 0 {
		b.observeOpenAIStream(b.pending)
	}
	b.pending = nil
	b.pendingOverflow = false
}

func (b *streamUsageBuffer) publish(ctx context.Context, reporter *usageReporter) bool {
	if b == nil || reporter == nil || (!b.hasUsage && !b.hasTier) {
		return false
	}
	reporter.publish(ctx, b.detail)
	return true
}

func (b *streamUsageBuffer) detailValue() (usage.Detail, bool) {
	if b == nil || (!b.hasUsage && !b.hasTier) {
		return usage.Detail{}, false
	}
	return b.detail, true
}

func parseCodexUsage(data []byte) (usage.Detail, bool) {
	responseServiceTier := extractResponseServiceTier(data)
	usageNode := gjson.ParseBytes(data).Get("response.usage")
	if !openAIStyleUsageTokenFieldsExist(usageNode) {
		if responseServiceTier == "" {
			return usage.Detail{}, false
		}
		return usage.Detail{ResponseServiceTier: responseServiceTier}, true
	}
	detail := parseOpenAIStyleUsageNode(usageNode)
	detail.ResponseServiceTier = responseServiceTier
	return detail, true
}

func parseOpenAIUsage(data []byte) usage.Detail {
	responseServiceTier := extractResponseServiceTier(data)
	usageNode := gjson.ParseBytes(data).Get("usage")
	if !openAIStyleUsageTokenFieldsExist(usageNode) {
		return usage.Detail{ResponseServiceTier: responseServiceTier}
	}
	detail := parseOpenAIStyleUsageNode(usageNode)
	detail.ResponseServiceTier = responseServiceTier
	return detail
}

func parseOpenAIStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	responseServiceTier := extractResponseServiceTierFromValidJSON(payload)
	usageNode := gjson.GetBytes(payload, "usage")
	if !openAIStyleUsageTokenFieldsExist(usageNode) {
		if responseServiceTier == "" {
			return usage.Detail{}, false
		}
		return usage.Detail{ResponseServiceTier: responseServiceTier}, true
	}
	detail := parseOpenAIStyleUsageNode(usageNode)
	detail.ResponseServiceTier = responseServiceTier
	return detail, true
}

func parseClaudeUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data).Get("usage")
	if !usageNode.Exists() {
		return usage.Detail{}
	}
	return parseClaudeUsageNode(usageNode)
}

func parseClaudeUsageNode(usageNode gjson.Result) usage.Detail {
	cacheReadTokens := firstExistingUsageNode(usageNode, "cache_read_input_tokens").Int()
	cacheCreationTokens := firstExistingUsageNode(usageNode, "cache_creation_input_tokens").Int()
	detail := usage.Detail{
		InputTokens:         firstExistingUsageNode(usageNode, "input_tokens").Int(),
		OutputTokens:        firstExistingUsageNode(usageNode, "output_tokens").Int(),
		CachedTokens:        cacheReadTokens,
		CacheReadTokens:     cacheReadTokens,
		CacheCreationTokens: cacheCreationTokens,
		ResponseServiceTier: usageServiceTier(usageNode),
	}
	if detail.CachedTokens == 0 {
		detail.CachedTokens = detail.CacheCreationTokens
	}
	total, ok := usage.AddNonNegativeInt64(
		detail.InputTokens, detail.OutputTokens, detail.CacheReadTokens, detail.CacheCreationTokens,
	)
	if !ok {
		log.Warn("usage: rejected overflowing Claude token detail")
		return usage.Detail{}
	}
	detail.TotalTokens = total
	return detail
}

type claudeStreamUsageBuffer struct {
	detail   usage.Detail
	hasUsage bool
	hasTier  bool
}

func (b *claudeStreamUsageBuffer) observe(line []byte) {
	if b == nil {
		return
	}
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return
	}
	root := gjson.ParseBytes(payload)
	usageNode := root.Get("usage")
	if strings.TrimSpace(root.Get("type").String()) == "message_start" {
		usageNode = root.Get("message.usage")
	}
	if claudeUsageTokenFieldsExist(usageNode) {
		candidate := b.detail
		mergeClaudeUsageNode(&candidate, usageNode)
		total, ok := usage.AddNonNegativeInt64(
			candidate.InputTokens,
			candidate.OutputTokens,
			candidate.CacheReadTokens,
			candidate.CacheCreationTokens,
		)
		if ok {
			candidate.TotalTokens = total
			candidate.CachedTokens = candidate.CacheReadTokens
			if candidate.CachedTokens == 0 {
				candidate.CachedTokens = candidate.CacheCreationTokens
			}
			b.detail = candidate
			b.hasUsage = true
		} else {
			log.Warn("usage: rejected overflowing Claude stream token detail")
		}
	}
	if tier := extractResponseServiceTierFromValidJSON(payload); tier != "" {
		b.detail.ResponseServiceTier = tier
		b.hasTier = true
	}
}

func (b *claudeStreamUsageBuffer) publish(ctx context.Context, reporter *usageReporter) bool {
	if b == nil || reporter == nil || (!b.hasUsage && !b.hasTier) {
		return false
	}
	reporter.publish(ctx, b.detail)
	return true
}

func (b *claudeStreamUsageBuffer) publishFailure(ctx context.Context, reporter *usageReporter, errs ...error) {
	if reporter == nil {
		return
	}
	reporter.publishFailureWithDetail(ctx, b.detail, errs...)
}

func (b *claudeStreamUsageBuffer) detailValue() (usage.Detail, bool) {
	if b == nil || (!b.hasUsage && !b.hasTier) {
		return usage.Detail{}, false
	}
	return b.detail, true
}

func claudeUsageTokenFieldsExist(node gjson.Result) bool {
	if !node.Exists() || !node.IsObject() {
		return false
	}
	for _, path := range []string{
		"input_tokens",
		"output_tokens",
		"cache_read_input_tokens",
		"cache_creation_input_tokens",
	} {
		if validUsageTokenNode(node.Get(path)) {
			return true
		}
	}
	return false
}

func mergeClaudeUsageNode(detail *usage.Detail, node gjson.Result) {
	if detail == nil {
		return
	}
	if value := node.Get("input_tokens"); validUsageTokenNode(value) {
		detail.InputTokens = value.Int()
	}
	if value := node.Get("output_tokens"); validUsageTokenNode(value) {
		detail.OutputTokens = value.Int()
	}
	if value := node.Get("cache_read_input_tokens"); validUsageTokenNode(value) {
		detail.CacheReadTokens = value.Int()
	}
	if value := node.Get("cache_creation_input_tokens"); validUsageTokenNode(value) {
		detail.CacheCreationTokens = value.Int()
	}
}

func parseClaudeStreamUsage(line []byte) (usage.Detail, bool) {
	var buffer claudeStreamUsageBuffer
	buffer.observe(line)
	return buffer.detailValue()
}

func parseGeminiFamilyUsageDetail(node gjson.Result) usage.Detail {
	cachedTokens := firstExistingUsageNode(node, "cachedContentTokenCount").Int()
	detail := usage.Detail{
		InputTokens:     firstExistingUsageNode(node, "promptTokenCount").Int(),
		OutputTokens:    firstExistingUsageNode(node, "candidatesTokenCount").Int(),
		ReasoningTokens: firstExistingUsageNode(node, "thoughtsTokenCount").Int(),
		TotalTokens:     firstExistingUsageNode(node, "totalTokenCount").Int(),
		CachedTokens:    cachedTokens,
		CacheReadTokens: cachedTokens,
	}
	if detail.TotalTokens == 0 {
		total, ok := usage.AddNonNegativeInt64(detail.InputTokens, detail.OutputTokens, detail.ReasoningTokens)
		if !ok {
			log.Warn("usage: rejected overflowing Gemini token detail")
			return usage.Detail{}
		}
		detail.TotalTokens = total
	}
	return detail
}

func parseGeminiCLIUsage(data []byte) usage.Detail {
	root := gjson.ParseBytes(data)
	node := firstValidUsageObject(root, geminiUsageTokenFieldsExist, "response.usageMetadata", "response.usage_metadata")
	if !node.Exists() {
		return usage.Detail{}
	}
	return parseGeminiFamilyUsageDetail(node)
}

func parseGeminiUsage(data []byte) usage.Detail {
	root := gjson.ParseBytes(data)
	node := firstValidUsageObject(root, geminiUsageTokenFieldsExist, "usageMetadata", "usage_metadata")
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
	root := gjson.ParseBytes(payload)
	node := firstValidUsageObject(root, geminiUsageTokenFieldsExist, "usageMetadata", "usage_metadata")
	if !node.Exists() {
		return usage.Detail{}, false
	}
	return parseGeminiFamilyUsageDetail(node), true
}

func parseInteractionsUsageDetail(node gjson.Result) usage.Detail {
	cacheRead := firstExistingUsageNode(node, "cache_read_tokens", "cacheReadTokens")
	detail := usage.Detail{
		InputTokens:         firstExistingUsageNode(node, "input_tokens", "prompt_tokens", "total_input_tokens").Int(),
		OutputTokens:        firstExistingUsageNode(node, "output_tokens", "completion_tokens", "total_output_tokens").Int(),
		ReasoningTokens:     firstExistingUsageNode(node, "reasoning_tokens", "thoughtsTokenCount", "total_thought_tokens").Int(),
		TotalTokens:         firstExistingUsageNode(node, "total_tokens", "totalTokenCount").Int(),
		CachedTokens:        firstExistingUsageNode(node, "cached_tokens", "cachedContentTokenCount", "total_cached_tokens").Int(),
		CacheReadTokens:     cacheRead.Int(),
		CacheCreationTokens: firstExistingUsageNode(node, "cache_creation_tokens", "cacheCreationTokens", "cache_write_tokens", "cacheWriteTokens").Int(),
	}
	if !cacheRead.Exists() && detail.CachedTokens > 0 {
		detail.CacheReadTokens = detail.CachedTokens
	}
	if detail.TotalTokens == 0 {
		total, ok := usage.AddNonNegativeInt64(detail.InputTokens, detail.OutputTokens, detail.ReasoningTokens)
		if !ok {
			log.Warn("usage: rejected overflowing Interactions token detail")
			return usage.Detail{}
		}
		detail.TotalTokens = total
	}
	return detail
}

func firstValidUsageObject(
	root gjson.Result,
	valid func(gjson.Result) bool,
	paths ...string,
) gjson.Result {
	for _, path := range paths {
		value := root.Get(path)
		if value.IsObject() && valid(value) {
			return value
		}
	}
	return gjson.Result{}
}

func firstExistingUsageNode(root gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		value := root.Get(path)
		if validUsageTokenNode(value) {
			return value
		}
	}
	return gjson.Result{}
}

func validUsageTokenNode(value gjson.Result) bool {
	if value.Type != gjson.Number {
		return false
	}
	raw := strings.TrimSpace(value.Raw)
	if raw == "" {
		return false
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	return err == nil && parsed >= 0
}

func hasUsageDetail(detail usage.Detail) bool {
	return detail.InputTokens != 0 || detail.OutputTokens != 0 || detail.ReasoningTokens != 0 || detail.TotalTokens != 0 ||
		detail.CachedTokens != 0 || detail.CacheReadTokens != 0 || detail.CacheCreationTokens != 0
}

func interactionsUsageNode(root gjson.Result) gjson.Result {
	return firstValidUsageObject(
		root,
		interactionsUsageTokenFieldsExist,
		"interaction.usage",
		"usage",
		"total_usage",
		"metadata.total_usage",
		"metadata.usage",
		"usageMetadata",
		"usage_metadata",
		"interaction.total_usage",
		"interaction.metadata.total_usage",
		"interaction.metadata.usage",
	)
}

func geminiUsageTokenFieldsExist(node gjson.Result) bool {
	if !node.Exists() || !node.IsObject() {
		return false
	}
	for _, path := range []string{
		"promptTokenCount",
		"candidatesTokenCount",
		"thoughtsTokenCount",
		"totalTokenCount",
		"cachedContentTokenCount",
	} {
		if validUsageTokenNode(node.Get(path)) {
			return true
		}
	}
	return false
}

func interactionsUsageTokenFieldsExist(node gjson.Result) bool {
	if !node.Exists() || node.Type == gjson.Null {
		return false
	}
	for _, path := range []string{
		"input_tokens", "prompt_tokens", "total_input_tokens",
		"output_tokens", "completion_tokens", "total_output_tokens",
		"reasoning_tokens", "total_thought_tokens", "thoughtsTokenCount",
		"total_tokens", "totalTokenCount", "promptTokenCount", "candidatesTokenCount",
		"cached_tokens", "cachedContentTokenCount", "total_cached_tokens",
		"cache_read_tokens", "cacheReadTokens", "cache_creation_tokens", "cacheCreationTokens",
		"cache_write_tokens", "cacheWriteTokens",
	} {
		if validUsageTokenNode(node.Get(path)) {
			return true
		}
	}
	return false
}

func parseInteractionsUsage(data []byte) usage.Detail {
	responseServiceTier := extractResponseServiceTier(data)
	root := gjson.ParseBytes(data)
	node := interactionsUsageNode(root)
	if !node.Exists() {
		return usage.Detail{ResponseServiceTier: responseServiceTier}
	}
	var detail usage.Detail
	if firstExistingUsageNode(node, "promptTokenCount", "candidatesTokenCount").Exists() {
		detail = parseGeminiFamilyUsageDetail(node)
	} else {
		detail = parseInteractionsUsageDetail(node)
	}
	detail.ResponseServiceTier = responseServiceTier
	return detail
}

func extractResponseServiceTier(payload []byte) string {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return ""
	}
	return extractResponseServiceTierFromValidJSON(payload)
}

func usageServiceTier(node gjson.Result) string {
	value := node.Get("service_tier")
	if value.Type != gjson.String {
		return ""
	}
	return strings.TrimSpace(value.String())
}

func extractResponseServiceTierFromValidJSON(payload []byte) string {
	for _, path := range []string{
		"response.service_tier",
		"message.usage.service_tier",
		"usage.service_tier",
		"service_tier",
		"interaction.service_tier",
	} {
		value := gjson.GetBytes(payload, path)
		if value.Type != gjson.String {
			continue
		}
		if tier := strings.TrimSpace(value.String()); tier != "" {
			return tier
		}
	}
	return ""
}

func parseInteractionsStreamUsageState(line []byte) (usage.Detail, bool, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 {
		payload = line
	}
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false, false
	}
	root := gjson.ParseBytes(payload)
	hasUsage := interactionsUsageTokenFieldsExist(interactionsUsageNode(root))
	detail := parseInteractionsUsage(payload)
	hasTier := strings.TrimSpace(detail.ResponseServiceTier) != ""
	if !hasUsage && !hasTier {
		return usage.Detail{}, false, false
	}
	return detail, hasUsage, hasTier
}

func parseInteractionsStreamUsage(line []byte) (usage.Detail, bool) {
	detail, hasUsage, hasTier := parseInteractionsStreamUsageState(line)
	return detail, hasUsage || hasTier
}

func parseGeminiCLIStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	root := gjson.ParseBytes(payload)
	node := firstValidUsageObject(root, geminiUsageTokenFieldsExist, "response.usageMetadata", "response.usage_metadata")
	if !node.Exists() {
		return usage.Detail{}, false
	}
	return parseGeminiFamilyUsageDetail(node), true
}

func parseAntigravityUsage(data []byte) usage.Detail {
	root := gjson.ParseBytes(data)
	node := firstValidUsageObject(
		root, geminiUsageTokenFieldsExist, "response.usageMetadata", "usageMetadata", "usage_metadata",
	)
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
	root := gjson.ParseBytes(payload)
	node := firstValidUsageObject(
		root, geminiUsageTokenFieldsExist, "response.usageMetadata", "usageMetadata", "usage_metadata",
	)
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
