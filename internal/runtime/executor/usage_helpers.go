package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
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
	provider                 string
	baseURL                  string
	executorType             string
	model                    string
	stream                   bool
	alias                    string
	clientModel              string
	authID                   string
	authIndex                string
	accessTokenHash          string
	authType                 string
	apiKey                   string
	reservationID            string
	source                   string
	sourceKind               usage.CredentialSourceKind
	sourceRedacted           bool
	reasoningEffort          string
	serviceTier              string
	requestedTier            string
	modelMu                  sync.RWMutex
	responseModel            string
	upstreamModel            string
	upstreamURL              string
	responseURL              string
	responseURLSource        string
	responseURLStatus        string
	upstreamNextHopStatus    string
	billingMetadata          *usage.BillingMetadata
	billingTools             []string
	billingRequestKnown      bool
	billingCodeExecutionFree bool
	billingComplete          bool
	billingMedia             bool
	cacheWrite5mTokens       *int64
	cacheWrite1hTokens       *int64
	responseTier             string
	requestSpeed             string
	responseSpeed            string
	responseProvider         string
	responseProviderSource   string
	observationMu            sync.Mutex
	observationStream        streamUsageBuffer
	modelFinal               bool
	generate                 bool
	thinking                 *usage.Thinking
	requestedAt              time.Time
	once                     sync.Once
}

type usageReporterContextKey struct{}

type bridgeResponseURLContextKey struct{}

func (r *usageReporter) withContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, usageReporterContextKey{}, r)
}

func usageReporterFromContext(ctx context.Context) *usageReporter {
	if ctx == nil {
		return nil
	}
	reporter, _ := ctx.Value(usageReporterContextKey{}).(*usageReporter)
	return reporter
}

func (r *usageReporter) observeUpstreamRequest(info upstreamRequestLog) {
	if r == nil {
		return
	}
	model := ""
	if value := gjson.GetBytes(info.Body, "model"); value.Type == gjson.String &&
		validResponseModel(strings.TrimSpace(value.String())) {
		model = strings.TrimSpace(value.String())
	}
	if model == "" {
		if endpoint, err := url.Parse(info.URL); err == nil {
			if _, suffix, ok := strings.Cut(endpoint.Path, "/models/"); ok {
				name, _, _ := strings.Cut(suffix, ":")
				if validResponseModel(name) {
					model = name
				}
			}
		}
	}
	r.observationMu.Lock()
	r.observationStream = streamUsageBuffer{modelReporter: r}
	r.observationMu.Unlock()
	r.modelMu.Lock()
	defer r.modelMu.Unlock()
	r.upstreamURL = util.SanitizeUpstreamURL(info.URL)
	r.responseURL = ""
	r.responseURLSource = ""
	r.responseURLStatus = "no_response"
	r.upstreamNextHopStatus = "not_reported"
	r.billingMetadata = nil
	r.billingComplete = false
	r.billingTools = nil
	r.billingCodeExecutionFree = false
	r.cacheWrite5mTokens, r.cacheWrite1hTokens = nil, nil
	r.billingRequestKnown = gjson.ValidBytes(info.Body) && gjson.ParseBytes(info.Body).IsObject()
	endpoint, _ := url.Parse(info.URL)
	r.billingMedia = r.provider == "meta" && endpoint != nil && (strings.HasSuffix(
		endpoint.Path, "/images/generations",
	) || strings.HasSuffix(endpoint.Path, "/images/edits"))
	if r.billingRequestKnown && r.provider == "claude" {
		for _, tool := range gjson.GetBytes(info.Body, "tools").Array() {
			kind := tool.Get("type").String()
			for _, prefix := range []string{"web_search_", "web_fetch_"} {
				if version, ok := strings.CutPrefix(kind, prefix); ok && len(version) == 8 {
					if date, err := strconv.Atoi(version); err == nil && date >= 20260209 {
						r.billingCodeExecutionFree = true
					}
				}
			}
		}
	}
	if r.billingRequestKnown {
		for _, tool := range gjson.GetBytes(info.Body, "tools").Array() {
			kind := tool.Get("type").String()
			if kind == "function" || kind == "custom" || (kind == "" && tool.Get("name").Type == gjson.String) || strings.HasPrefix(
				kind, "text_editor_",
			) {
				continue
			}
			if kind != "" {
				r.requireBillingTool(kind)
				continue
			}
			tool.ForEach(
				func(key, value gjson.Result) bool {
					if key.String() != "functionDeclarations" && key.String() != "function_declarations" {
						r.requireBillingTool(key.String())
					}
					return true
				},
			)
		}
	}

	r.upstreamModel = model
	r.responseModel = ""
	r.responseTier = ""
	r.responseSpeed = ""
	r.responseProvider = ""
	r.responseProviderSource = ""
	r.modelFinal = false
	r.requestSpeed = ""
	if speed := gjson.GetBytes(info.Body, "speed"); speed.Type == gjson.String {
		r.requestSpeed = strings.TrimSpace(speed.String())
	}
	r.serviceTier = r.requestedTier
	if tier, ok := explicitServiceTierFromPayload(info.Body); ok {
		r.serviceTier = tier
	}
}

func (r *usageReporter) observeResponseURL(raw, source, missingStatus string) {
	if r == nil {
		return
	}
	endpoint := util.SanitizeUpstreamURL(raw)
	r.modelMu.Lock()
	defer r.modelMu.Unlock()
	r.responseURL = endpoint
	r.responseURLSource = ""
	r.responseURLStatus = missingStatus
	if endpoint != "" {
		r.responseURLSource = source
		r.responseURLStatus = "observed"
		if target, err := url.Parse(endpoint); err == nil {
			region, origin := "", ""
			host := strings.ToLower(target.Hostname())
			locationHost, regional := strings.CutSuffix(host, "-aiplatform.googleapis.com")
			if r.provider == "vertex" && (host == "aiplatform.googleapis.com" || regional && locationHost != "" && !strings.Contains(
				locationHost, ".",
			)) {
				parts := strings.Split(target.Path, "/")
				for index, part := range parts {
					if part == "locations" && index+1 < len(parts) && parts[index+1] != "" {
						region, origin = parts[index+1], "endpoint.locations"
						break
					}
				}
			}
			if r.provider == "xai" && host == "us.api.x.ai" {
				region, origin = "us", "endpoint.host"
			}
			if r.provider == "xai" && host == "api.x.ai" {
				region, origin = "global", "endpoint.host"
			}
			if region != "" {
				if r.billingMetadata == nil {
					r.billingMetadata = &usage.BillingMetadata{SchemaVersion: 1}
				}
				r.billingMetadata.Region, r.billingMetadata.RegionSource = region, origin
			}
		}

	} else if strings.TrimSpace(raw) != "" {
		r.responseURLStatus = "unavailable"
	}
}

func (r *usageReporter) observeHTTPResponseURL(responses []*http.Response, source string) {
	endpoint, missingStatus := "", "unavailable"
	if len(responses) > 0 && responses[0] != nil && responses[0].Request != nil {
		request := responses[0].Request
		if bridge, _ := request.Context().Value(bridgeResponseURLContextKey{}).(bool); bridge {
			source, missingStatus = "bridge_final_url", "not_provided"
		}
		if request.URL != nil {
			endpoint = request.URL.String()
		}
	}
	r.observeResponseURL(endpoint, source, missingStatus)
}

func (r *usageReporter) observeUpstreamHeaders(headers http.Header) {
	if r == nil {
		return
	}
	r.modelMu.Lock()
	defer r.modelMu.Unlock()
	if r.provider == "kimi" {
		for _, field := range []struct {
			name   string
			target **int64
		}{
			{"Msh-Usage-Cache-Write-Tokens-5m", &r.cacheWrite5mTokens},
			{"Msh-Usage-Cache-Write-Tokens-1h", &r.cacheWrite1hTokens},
		} {
			if raw := strings.TrimSpace(helps.HeaderValueCaseInsensitive(headers, field.name)); raw != "" {
				value, err := strconv.ParseInt(raw, 10, 64)
				if err != nil || value < 0 {
					log.Warnf("usage: ignored invalid %s", field.name)
				} else {
					*field.target = &value
				}
			}
		}
	}
	for _, name := range []string{"X-Upstream-Provider", "X-Provider"} {
		if provider := strings.TrimSpace(
			helps.HeaderValueCaseInsensitive(
				headers, name,
			),
		); validResponseModel(provider) {
			r.responseProvider = provider
			r.responseProviderSource = "header." + name
			break
		}
	}
}

func (r *usageReporter) observeResponseChunk(payload []byte) {
	if r == nil || len(payload) == 0 {
		return
	}
	if r.provider == "devin" {
		// Devin logs CPA-generated Interactions; native Connect metrics supply the returned model.
		return
	}
	r.observationMu.Lock()
	defer r.observationMu.Unlock()
	r.observationStream.modelReporter = r
	defer func() {
		if len(r.observationStream.pending) > maxStreamModelEventBytes {
			r.observationStream.pending = nil
			r.observationStream.pendingOverflow = true
		}
	}()
	if len(r.observationStream.pending) > 0 || r.observationStream.pendingOverflow ||
		len(r.observationStream.modelLines) > 0 {
		r.observationStream.observeOpenAIStreamChunk(payload)
		return
	}
	if data := jsonPayload(payload); len(data) > 0 && data[0] == '{' && gjson.ValidBytes(data) {
		r.observeResponseModel(data)
		return
	}
	if data, present, valid := sseDataPayload(payload); present && valid {
		r.observeResponseModel(data)
		return
	}
	r.observationStream.observeOpenAIStreamChunk(payload)
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
	if ctx != nil {
		if delegated, _ := ctx.Value(pluginExecutorUsageDelegationKey{}).(*atomic.Bool); delegated != nil {
			delegated.Store(true)
		}
	}
	provider, executorType := usageReporterProvider(providerOrExecutor)
	rawAPIKey := apiKeyFromContext(ctx)
	source, sourceKind, sourceRedacted := resolveUsageSource(auth, rawAPIKey)
	apiKeyIdentity := usage.APIKeyIdentityFromContext(ctx)
	if apiKeyIdentity == "" {
		apiKeyIdentity = usage.HashAPIKeyIdentity(rawAPIKey)
	}
	alias := usage.RequestedModelAliasFromContext(ctx)
	clientModel := alias
	if alias == "" {
		alias = model
	}
	reporter := &usageReporter{
		provider:        provider,
		baseURL:         selectedAuthBaseURL(auth),
		executorType:    executorType,
		model:           model,
		stream:          usage.StreamFromContext(ctx),
		alias:           strings.TrimSpace(alias),
		clientModel:     strings.TrimSpace(clientModel),
		requestedAt:     time.Now(),
		apiKey:          apiKeyIdentity,
		reservationID:   usage.APIKeyReservationFromContext(ctx),
		source:          source,
		sourceKind:      sourceKind,
		sourceRedacted:  sourceRedacted,
		authType:        resolveUsageAuthType(auth),
		reasoningEffort: usage.ReasoningEffortFromContext(ctx),
		serviceTier:     usage.ServiceTierFromContext(ctx),
		requestedTier:   usage.ServiceTierFromContext(ctx),
		generate:        usage.GenerateFromContext(ctx),
		thinking:        thinkingFromAlias(alias),
	}
	if auth != nil {
		reporter.authID = auth.ID
		reporter.authIndex = auth.EnsureIndex()
		reporter.accessTokenHash = cliproxyauth.AccessTokenSHA256(auth)
	}
	return reporter
}

type pluginExecutorUsageDelegationKey struct{}

// WithPluginExecutorUsageDelegation 在实际执行器创建报告时标记外层计量已委派
func WithPluginExecutorUsageDelegation(ctx context.Context, delegated *atomic.Bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, pluginExecutorUsageDelegationKey{}, delegated)
}

// PluginExecutorUsageReporter publishes usage for the credential selected by a plugin executor.
type PluginExecutorUsageReporter struct {
	reporter *usageReporter
}

func NewPluginExecutorUsageReporter(
	ctx context.Context,
	providerOrExecutor any,
	model string,
	auth *cliproxyauth.Auth,
) *PluginExecutorUsageReporter {
	if auth == nil {
		return nil
	}
	return &PluginExecutorUsageReporter{reporter: newUsageReporter(ctx, providerOrExecutor, model, auth)}
}

func (r *PluginExecutorUsageReporter) SetTranslatedReasoningEffort(payload []byte, format string) {
	if r != nil {
		r.reporter.setTranslatedReasoningEffort(payload, format)
	}
}

func (r *PluginExecutorUsageReporter) WithContext(ctx context.Context) context.Context {
	if r == nil {
		return ctx
	}
	return r.reporter.withContext(ctx)
}

func (r *PluginExecutorUsageReporter) MarkStream() {
	if r != nil {
		r.reporter.markStream()
	}
}

func (r *PluginExecutorUsageReporter) Publish(ctx context.Context, detail usage.Detail) {
	if r != nil {
		r.reporter.publish(ctx, detail)
	}
}

func (r *PluginExecutorUsageReporter) PublishFailure(ctx context.Context, err error) {
	if r != nil {
		r.reporter.publishFailure(ctx, err)
	}
}

func (r *PluginExecutorUsageReporter) PublishFailureWithDetail(ctx context.Context, detail usage.Detail, err error) {
	if r != nil {
		r.reporter.publishFailureWithDetail(ctx, detail, err)
	}
}

func (r *PluginExecutorUsageReporter) EnsurePublished(ctx context.Context) {
	if r != nil {
		r.reporter.ensurePublished(ctx)
	}
}

func selectedAuthBaseURL(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if endpoint := strings.TrimSpace(auth.Attributes["base_url"]); endpoint != "" {
		return endpoint
	}
	endpoint, _ := auth.Metadata["base_url"].(string)
	return strings.TrimSpace(endpoint)
}

func (r *usageReporter) markStream() {
	if r != nil {
		r.stream = true
	}
}

func (r *usageReporter) setTranslatedReasoningEffort(payload []byte, format string) {
	if r == nil {
		return
	}
	r.reasoningEffort = thinking.ExtractTranslatedReasoningEffort(payload, format)
	if serviceTier, ok := explicitServiceTierFromPayload(payload); ok {
		r.serviceTier = serviceTier
	}
	if model := gjson.GetBytes(payload, "model"); model.Type == gjson.String {
		r.setUpstreamModel(model.String())
	}
}

func (r *usageReporter) setUpstreamModel(model string) {
	if r == nil {
		return
	}
	model = strings.TrimSpace(model)
	if !validResponseModel(model) {
		return
	}
	r.modelMu.Lock()
	r.upstreamModel = model
	r.modelMu.Unlock()
}

func (r *usageReporter) observeResponseModel(payload []byte) {
	if r == nil {
		return
	}
	payload = jsonPayload(payload)
	if !gjson.ValidBytes(payload) {
		return
	}
	model, terminal := responseModelEvent(payload, r.provider)
	r.modelMu.Lock()
	defer r.modelMu.Unlock()
	if tier := extractResponseServiceTierFromValidJSON(payload); tier != "" {
		r.responseTier = tier
	}
	for _, path := range []string{"usage.speed", "message.usage.speed", "response.usage.speed"} {
		if speed := gjson.GetBytes(
			payload, path,
		); speed.Type == gjson.String && strings.TrimSpace(speed.String()) != "" {
			r.responseSpeed = strings.TrimSpace(speed.String())
			break
		}
	}
	for _, path := range []string{"response.provider", "message.provider", "provider", "upstream_provider"} {
		if provider := gjson.GetBytes(payload, path); provider.Type == gjson.String &&
			validResponseModel(strings.TrimSpace(provider.String())) {
			r.responseProvider = strings.TrimSpace(provider.String())
			r.responseProviderSource = "body." + path
			break
		}
	}
	r.observeBillingResponse(payload)
	if r.modelFinal {
		return
	}
	if model != "" {
		r.responseModel = model
	}
	r.modelFinal = terminal
}

func (r *usageReporter) requireBillingTool(tool string) {
	if strings.HasPrefix(tool, "code_execution") && r.billingCodeExecutionFree {
		return
	}
	kind := "unreported"
	if strings.HasPrefix(tool, "web_search") {
		kind = "web_search"
	}
	if strings.HasPrefix(tool, "web_fetch") {
		kind = "web_fetch"
	}
	for _, existing := range r.billingTools {
		if existing == kind {
			return
		}
	}
	r.billingTools = append(r.billingTools, kind)
}

func (r *usageReporter) observeBillingResponse(payload []byte) {
	root := gjson.ParseBytes(payload)
	kind := root.Get("type").String()
	if kind == "response.completed" || kind == "response.done" || kind == "message_stop" ||
		(kind == "message" && root.Get("stop_reason").String() != "") || root.Get("choices.0.finish_reason").String() != "" ||
		root.Get("candidates.0.finishReason").String() != "" || root.Get("response.candidates.0.finishReason").String() != "" || root.Get("status").String() == "completed" {
		status := root.Get("response.status").String()
		if status != "incomplete" && status != "failed" && status != "cancelled" {
			r.billingComplete = true
		}
	}
	for _, path := range []string{"candidates", "response.candidates"} {
		for _, candidate := range root.Get(path).Array() {
			if len(candidate.Get("groundingMetadata.webSearchQueries").Array()) > 0 {
				r.requireBillingTool("googleSearch")
			}
		}
	}
	for _, path := range []string{"output", "response.output"} {
		for _, item := range root.Get(path).Array() {
			tool := item.Get("type").String()
			if strings.HasSuffix(tool, "_call") && tool != "function_call" && tool != "custom_tool_call" {
				r.requireBillingTool(tool)
			}
		}
	}
	if r.provider == "claude" || r.provider == "kimi" {
		node := root.Get("usage")
		if kind == "message_start" {
			node = root.Get("message.usage")
		}
		if count := firstExistingUsageNode(
			node, "server_tool_use.code_execution_requests",
		); count.Exists() && count.Int() > 0 {
			r.requireBillingTool("code_execution")
		}
		for _, field := range []struct{ path, kind, unit string }{
			{"web_search_requests", "web_search", "request"}, {"web_fetch_requests", "web_fetch", "request"},
		} {
			if value := firstExistingUsageNode(node, "server_tool_use."+field.path); value.Exists() {
				if r.billingMetadata == nil {
					r.billingMetadata = &usage.BillingMetadata{SchemaVersion: 1}
				}
				quantity := usage.BillingQuantity{Kind: field.kind, Unit: field.unit, Quantity: float64(value.Int())}
				index := -1
				for i, existing := range r.billingMetadata.Charges {
					if existing.Kind == field.kind {
						index = i
						break
					}
				}
				if index < 0 {
					r.billingMetadata.Charges = append(r.billingMetadata.Charges, quantity)
				} else {
					r.billingMetadata.Charges[index] = quantity
				}
			}
		}
	}
	if r.billingMedia && root.Get("data").IsArray() {
		count := 0
		for _, image := range root.Get("data").Array() {
			if image.Get("url").String() != "" || image.Get("b64_json").String() != "" {
				count++
			}
		}
		if count > 0 {
			r.billingComplete = true
			if r.billingMetadata == nil {
				r.billingMetadata = &usage.BillingMetadata{SchemaVersion: 1}
			}
			r.billingMetadata.Charges = []usage.BillingQuantity{
				{
					Kind: "generated_image", Unit: "image", Quantity: float64(count),
				},
			}
		}
	}
}

func (r *usageReporter) setResponseModel(model string) {
	if r == nil {
		return
	}
	model = strings.TrimSpace(model)
	if !validResponseModel(model) {
		return
	}
	r.modelMu.Lock()
	if !r.modelFinal {
		r.responseModel = model
	}
	r.modelMu.Unlock()
}

func (r *usageReporter) responseModelSnapshot() (string, string) {
	if r == nil {
		return "", ""
	}
	r.modelMu.RLock()
	defer r.modelMu.RUnlock()
	return r.responseModel, r.upstreamModel
}

func (r *usageReporter) trackHTTPClient(client *http.Client) *http.Client {
	if r == nil || client == nil {
		return client
	}
	tracked := *client
	transport := tracked.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	tracked.Transport = usageTimingRoundTripper{base: transport}
	return &tracked
}

type usageTimingRoundTripper struct {
	base http.RoundTripper
}

func (t usageTimingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &usageTimingReadCloser{
		ReadCloser: resp.Body,
		mark:       func() { logging.MarkFirstResponseByte(req.Context()) },
	}
	return resp, nil
}

type usageTimingReadCloser struct {
	io.ReadCloser
	once sync.Once
	mark func()
}

func (r *usageTimingReadCloser) Read(p []byte) (int, error) {
	if r == nil || r.ReadCloser == nil {
		return 0, io.ErrClosedPipe
	}
	n, err := r.ReadCloser.Read(p)
	if n > 0 && r.mark != nil {
		r.once.Do(r.mark)
	}
	return n, err
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
	// 原生执行器私有化后，保留已发布的用量标签
	switch value.(type) {
	case *codexExecutor:
		return "CodexExecutor"
	case *codexWebsocketsExecutor:
		return "CodexWebsocketsExecutor"
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
	detail = normalizeUsageDetailTotal(detail, r.provider, r.executorType)
	r.publishRecordImmediate(
		ctx, usage.Record{
			Provider:            r.provider,
			BaseURL:             r.baseURL,
			ExecutorType:        r.executorType,
			Stream:              r.stream,
			Model:               model,
			Alias:               model,
			Source:              r.source,
			SourceKind:          r.sourceKind,
			SourceRedacted:      r.sourceRedacted,
			APIKey:              r.apiKey,
			AuthID:              r.authID,
			AuthIndex:           r.authIndex,
			AccessTokenSHA256:   r.accessTokenHash,
			AuthType:            r.authType,
			ReasoningEffort:     r.reasoningEffort,
			ServiceTier:         r.serviceTier,
			RequestServiceTier:  r.serviceTier,
			ResponseServiceTier: strings.TrimSpace(detail.ResponseServiceTier),
			Generate:            new(r.generate),
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
	r.once.Do(
		func() {
			publishCtx := ctx
			if !failed {
				if errContext := scannerOrContextError(ctx, nil); errContext != nil {
					detail = usage.Detail{}
					failed = true
					fail = failFromErrors(errContext)
					publishCtx = context.WithoutCancel(ctx)
				}
			}
			detail = normalizeUsageDetailTotal(detail, r.provider, r.executorType)
			responseModel, expectedModel := r.responseModelSnapshot()
			if !failed {
				r.warnModelSubstitution(publishCtx, expectedModel, responseModel)
			}
			r.publishRecord(
				publishCtx, usage.Record{
					Provider:            r.provider,
					BaseURL:             r.baseURL,
					ExecutorType:        r.executorType,
					Model:               r.model,
					Stream:              r.stream,
					Alias:               r.alias,
					Source:              r.source,
					SourceKind:          r.sourceKind,
					SourceRedacted:      r.sourceRedacted,
					APIKey:              r.apiKey,
					AuthID:              r.authID,
					AuthIndex:           r.authIndex,
					AccessTokenSHA256:   r.accessTokenHash,
					AuthType:            r.authType,
					ReasoningEffort:     r.reasoningEffort,
					ServiceTier:         r.serviceTier,
					RequestServiceTier:  r.serviceTier,
					ResponseServiceTier: strings.TrimSpace(detail.ResponseServiceTier),
					ResponseModel:       responseModel,
					Generate:            new(r.generate),
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

func normalizeUsageDetailTotal(detail usage.Detail, provider, executorType string) usage.Detail {
	return usage.EnsureTokenBreakdownForProvider(detail, provider, executorType)
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
			failed := false
			fail := usage.Failure{}
			publishCtx := ctx
			if errContext := scannerOrContextError(ctx, nil); errContext != nil {
				failed = true
				fail = failFromErrors(errContext)
				publishCtx = context.WithoutCancel(ctx)
			}
			responseModel, expectedModel := r.responseModelSnapshot()
			if !failed {
				r.warnModelSubstitution(publishCtx, expectedModel, responseModel)
			}
			r.publishRecord(
				publishCtx, usage.Record{
					Provider:           r.provider,
					BaseURL:            r.baseURL,
					ExecutorType:       r.executorType,
					Model:              r.model,
					Stream:             r.stream,
					Alias:              r.alias,
					Source:             r.source,
					SourceKind:         r.sourceKind,
					SourceRedacted:     r.sourceRedacted,
					APIKey:             r.apiKey,
					AuthID:             r.authID,
					AccessTokenSHA256:  r.accessTokenHash,
					AuthIndex:          r.authIndex,
					AuthType:           r.authType,
					ReasoningEffort:    r.reasoningEffort,
					ServiceTier:        r.serviceTier,
					RequestServiceTier: r.serviceTier,
					ResponseModel:      responseModel,
					Generate:           new(r.generate),
					RequestedAt:        r.requestedAt,
					Latency:            r.latency(),
					Thinking:           cloneUsageThinking(r.thinking),
					Failed:             failed,
					Fail:               fail,
					Detail:             usage.Detail{},
				},
			)
		},
	)
}

func (r *usageReporter) publishRecord(ctx context.Context, record usage.Record) {
	r.observationMu.Lock()
	r.observationStream.observeOpenAIStreamChunk([]byte("\n\n"))
	r.observationMu.Unlock()
	r.modelMu.RLock()
	record.ClientModel = r.clientModel
	record.UpstreamURL = r.upstreamURL
	record.ResponseURL = r.responseURL
	record.ResponseURLSource = r.responseURLSource
	record.ResponseURLStatus = r.responseURLStatus
	record.UpstreamNextHopStatus = r.upstreamNextHopStatus
	metadata := usage.CloneBillingMetadata(record.Detail.BillingMetadata)
	if metadata == nil {
		metadata = usage.CloneBillingMetadata(record.BillingMetadata)
	}
	if metadata == nil && (r.billingRequestKnown || r.billingMetadata != nil) {
		metadata = &usage.BillingMetadata{SchemaVersion: 1}
	}
	if metadata != nil {
		if observed := r.billingMetadata; observed != nil {
			if metadata.Region == "" {
				metadata.Region, metadata.RegionSource = observed.Region, observed.RegionSource
			}
			metadata.Charges = append([]usage.BillingQuantity(nil), observed.Charges...)
		}
		metadata.ChargesReported = !record.Failed && r.billingComplete && (r.billingRequestKnown || r.billingMedia)
		for _, tool := range r.billingTools {
			reported := false
			for _, charge := range metadata.Charges {
				if (strings.HasPrefix(tool, "web_search") && charge.Kind == "web_search") || (strings.HasPrefix(
					tool, "web_fetch",
				) && charge.Kind == "web_fetch") {
					reported = true
				}
			}
			if !reported {
				metadata.ChargesReported = false
			}
		}
		record.Detail.BillingMetadata = metadata
		record.BillingMetadata = usage.CloneBillingMetadata(metadata)
	}
	if r.cacheWrite5mTokens != nil || r.cacheWrite1hTokens != nil {
		writes := cacheWriteUsage{ttl: [4]*int64{r.cacheWrite5mTokens, r.cacheWrite1hTokens}, families: [2]bool{true}}
		if record.Detail.CacheWriteReported != nil && *record.Detail.CacheWriteReported {
			writes.aggregate = new(record.Detail.CacheCreationTokens)
		}
		if writes.apply(&record.Detail) && record.Detail.TokenBreakdown.Valid() {
			record.Detail.TokenBreakdown = usage.NewPartialSubsetTokenBreakdown(
				record.Detail.InputTokens, record.Detail.CacheReadTokens, record.Detail.CacheCreationTokens,
				record.Detail.OutputTokens, record.Detail.ReasoningTokens, record.Detail.TotalTokens,
			)
		}
	}

	record.UpstreamModel = r.upstreamModel
	record.RequestSpeed = r.requestSpeed
	record.ResponseSpeed = r.responseSpeed
	record.ResponseProvider = r.responseProvider
	record.ResponseProviderSource = r.responseProviderSource
	if r.responseModel != "" {
		record.ResponseModel = r.responseModel
	}
	if r.responseTier != "" {
		record.ResponseServiceTier = r.responseTier
	}
	r.modelMu.RUnlock()
	record = r.finalizeRecord(ctx, record)
	if !record.Failed && usage.DeferRecord(ctx, record) {
		return
	}
	_ = usage.PublishRecord(ctx, record)
}

func (r *usageReporter) publishRecordImmediate(ctx context.Context, record usage.Record) {
	_ = usage.PublishRecord(ctx, r.finalizeRecord(ctx, record))
}

func (r *usageReporter) finalizeRecord(ctx context.Context, record usage.Record) usage.Record {
	record.CacheReadReported = record.CacheReadReported || record.Detail.CacheReadReported
	record.CacheWriteReported = usage.NormalizeCacheWriteReported(
		record.Detail.CacheWriteReported, record.Detail.CacheCreationTokens,
	)
	if record.BillingMetadata == nil {
		record.BillingMetadata = usage.CloneBillingMetadata(record.Detail.BillingMetadata)
	}
	if record.APIKey == "" {
		record.APIKey = r.apiKey
	}
	if record.ReservationID == "" {
		record.ReservationID = r.reservationID
	}
	if record.RequestID == "" {
		record.RequestID = logging.GetRequestID(ctx)
	}
	if record.ExecutionID == "" {
		record.ExecutionID = usage.ExecutionIDFromContext(ctx)
	}
	if record.TotalDurationMs <= 0 && record.Latency > 0 {
		record.TotalDurationMs = record.Latency.Milliseconds()
	}
	if record.TimeToFirstByteMs <= 0 && !record.RequestedAt.IsZero() {
		firstResponse := logging.GetFirstResponseTokenAt(ctx)
		if firstResponse.IsZero() {
			firstResponse = logging.GetFirstResponseByteAt(ctx)
		}
		if elapsed := firstResponse.Sub(record.RequestedAt); elapsed > 0 {
			record.TimeToFirstByteMs = elapsed.Milliseconds()
		}
	}
	record.Completed = !record.Failed
	record.ResponseHeaders = logging.GetResponseHeaders(ctx)
	metadata := logging.GetClientRequestMetadata(ctx)
	record.ClientIP = metadata.ClientIP
	record.XForwardedFor = metadata.XForwardedFor
	record.UserAgent = metadata.UserAgent
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
		body := strings.TrimSpace(err.Error())
		if upstreamBody, direct := clienterror.ResponseBodyFromError(err); direct {
			body = string(upstreamBody)
		}
		return usage.Failure{
			StatusCode: clienterror.HTTPStatusFromError(err),
			Body:       body,
		}
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

// Cache write totals are known when an aggregate or every TTL in a reported family is valid.
type cacheWriteUsage struct {
	aggregate *int64
	ttl       [4]*int64
	families  [2]bool
}

func (state *cacheWriteUsage) observe(aggregate gjson.Result, ttls [4]gjson.Result) {
	if aggregate.Exists() {
		state.aggregate = nil
		if validUsageTokenNode(aggregate) {
			state.aggregate = new(aggregate.Int())
		}
	}
	for index, value := range ttls {
		if !value.Exists() {
			continue
		}
		if !aggregate.Exists() {
			state.aggregate = nil
		}
		state.families[index/2] = true
		state.ttl[index] = nil
		if validUsageTokenNode(value) {
			state.ttl[index] = new(value.Int())
		}
	}
}

func (state *cacheWriteUsage) apply(detail *usage.Detail) bool {
	quantities := [4]int64{}
	complete := state.families[0] || state.families[1]
	for index, value := range state.ttl {
		if value != nil {
			quantities[index] = *value
		} else if state.families[index/2] {
			complete = false
		}
	}
	total, valid := usage.AddNonNegativeInt64(quantities[:]...)
	if !valid {
		log.Warn("usage: rejected overflowing cache write TTL detail")
		detail.CacheWriteReported = new(false)
		return false
	}
	if state.aggregate != nil {
		total = *state.aggregate
	}
	detail.CacheCreationTokens = total
	detail.CacheWriteReported = new(state.aggregate != nil || complete)
	detail.CacheCreation5mInputTokens, detail.CacheCreation1hInputTokens = quantities[0], quantities[1]
	detail.CacheCreation30mInputTokens, detail.CacheCreation12hInputTokens = quantities[2], quantities[3]
	return true
}

func cacheWriteUsageNode(node gjson.Result, paths ...string) gjson.Result {
	if value := firstExistingUsageNode(node, paths...); value.Exists() {
		return value
	}
	for _, path := range paths {
		if value := node.Get(path); value.Exists() {
			return value
		}
	}
	return gjson.Result{}
}

func (state *cacheWriteUsage) observeOpenAI(node gjson.Result) {
	state.observe(
		cacheWriteUsageNode(
			node,
			"input_tokens_details.cache_creation_tokens", "input_tokens_details.cache_write_tokens",
			"prompt_tokens_details.cache_creation_tokens", "prompt_tokens_details.cache_write_tokens",
		), [4]gjson.Result{
			cacheWriteUsageNode(
				node, "prompt_tokens_details.cache_creation.ephemeral_5m_input_tokens",
				"input_tokens_details.cache_write_tokens_details.ttl_5m_tokens",
			),
			cacheWriteUsageNode(
				node, "prompt_tokens_details.cache_creation.ephemeral_1h_input_tokens",
				"input_tokens_details.cache_write_tokens_details.ttl_1h_tokens",
			),
			node.Get("input_tokens_details.cache_write_tokens_details.ttl_30m_tokens"),
			node.Get("input_tokens_details.cache_write_tokens_details.ttl_12h_tokens"),
		},
	)
}

func (state *cacheWriteUsage) observeClaude(node gjson.Result) {
	previousAggregate := state.aggregate
	aggregate := node.Get("cache_creation_input_tokens")
	ttls := [4]gjson.Result{
		node.Get("cache_creation.ephemeral_5m_input_tokens"),
		node.Get("cache_creation.ephemeral_1h_input_tokens"),
	}
	state.observe(aggregate, ttls)
	if previousAggregate != nil && !aggregate.Exists() && (ttls[0].Exists() || ttls[1].Exists()) {
		var known int64
		for _, value := range state.ttl {
			if value != nil {
				var valid bool
				known, valid = usage.AddNonNegativeInt64(known, *value)
				if !valid {
					return
				}
			}
		}
		// A partial Claude TTL update retains the aggregate's unclassified remainder.
		state.aggregate = new(max(*previousAggregate, known))
	}
}

type streamUsageBuffer struct {
	cacheWriteUsage     cacheWriteUsage
	geminiPromptTokens  int64
	geminiToolUseTokens int64
	geminiModalities    [4]map[string]int64
	detail              usage.Detail
	hasUsage            bool
	hasTier             bool
	pending             []byte
	pendingOverflow     bool
	modelReporter       *usageReporter
	modelLines          [][]byte
	modelBytes          int
	modelOverflow       bool
}

const (
	maxStreamUsageLineBytes  = 50 << 20
	maxStreamModelEventBytes = 64 << 10
	maxStreamModelEventLines = 2048
)

var (
	openAIStreamUsageMarker = []byte(`"usage"`)
	openAIStreamTierMarker  = []byte(`"service_tier"`)
)

func (b *streamUsageBuffer) observe(detail usage.Detail, hasUsage bool, hasTier bool, writes ...cacheWriteUsage) {
	if b == nil || (!hasUsage && !hasTier) {
		return
	}
	if hasUsage {
		if len(writes) > 0 {
			if !writes[0].apply(&detail) {
				return
			}
			b.cacheWriteUsage = writes[0]
			if detail.TokenBreakdown.Valid() && detail.TokenBreakdown.Quality != usage.TokenAccountingQualityInconsistent {
				detail.TokenBreakdown = usage.NewPartialSubsetTokenBreakdown(
					detail.InputTokens, detail.CacheReadTokens, detail.CacheCreationTokens, detail.OutputTokens,
					detail.ReasoningTokens, detail.TotalTokens,
				)
			}
		}
		if !detail.CacheReadReported && b.detail.CacheReadReported && detail.InputTokens >= b.detail.CacheReadTokens {
			detail.CacheReadReported = true
			detail.CacheReadTokens = b.detail.CacheReadTokens
			if detail.CachedTokens == 0 {
				detail.CachedTokens = b.detail.CacheReadTokens
			}
			if detail.TokenBreakdown.Valid() {
				detail.TokenBreakdown = usage.NewPartialSubsetTokenBreakdown(
					detail.InputTokens, detail.CacheReadTokens, detail.CacheCreationTokens,
					detail.OutputTokens, detail.ReasoningTokens, detail.TotalTokens,
				)
			}
		}
		if detail.BillingMetadata == nil {
			detail.BillingMetadata = usage.CloneBillingMetadata(b.detail.BillingMetadata)
		}
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

func (b *streamUsageBuffer) observeGeminiStream(line []byte) {
	if b == nil {
		return
	}
	root := gjson.ParseBytes(jsonPayload(line))
	node := firstValidUsageObject(
		root, geminiUsageTokenFieldsExist,
		"response.usageMetadata", "usageMetadata", "response.usage_metadata", "usage_metadata",
		"response.cpaUsageMetadata", "cpaUsageMetadata",
	)
	if !node.Exists() {
		return
	}
	incoming := parseGeminiFamilyUsageDetail(node)
	if b.hasUsage && node.Get("promptTokenCount").Exists() && node.Get("candidatesTokenCount").Exists() &&
		node.Get("totalTokenCount").Exists() && !hasNonZeroTokenUsage(incoming) {
		if incoming.CacheReadReported && b.detail.CacheReadTokens == 0 {
			b.detail.CacheReadReported = true
		}
		return
	}
	next := b.detail
	promptTokens, toolUseTokens := b.geminiPromptTokens, b.geminiToolUseTokens
	if value := firstExistingUsageNode(node, "promptTokenCount"); value.Exists() {
		promptTokens = value.Int()
	}
	if value := firstExistingUsageNode(node, "toolUsePromptTokenCount"); value.Exists() {
		toolUseTokens = value.Int()
	}
	inputTokens, ok := usage.AddNonNegativeInt64(promptTokens, toolUseTokens)
	toolUseNode := node.Get("toolUsePromptTokenCount")
	if !ok || (toolUseNode.Exists() && !validUsageTokenNode(toolUseNode)) {
		log.Warn("usage: rejected invalid or overflowing Gemini stream input detail")
		if !b.hasUsage {
			b.observe(incoming, true, false)
		}
		return
	}
	next.InputTokens = inputTokens
	next.CacheReadReported = next.CacheReadReported || incoming.CacheReadReported

	for _, field := range []struct {
		path   string
		target *int64
	}{
		{"candidatesTokenCount", &next.OutputTokens},
		{"thoughtsTokenCount", &next.ReasoningTokens},
		{"cachedContentTokenCount", &next.CachedTokens},
		{"cachedContentTokenCount", &next.CacheReadTokens},
	} {
		if value := firstExistingUsageNode(node, field.path); value.Exists() {
			*field.target = value.Int()
		}
	}
	if total := firstExistingUsageNode(node, "totalTokenCount"); total.Exists() {
		next.TotalTokens = total.Int()
	} else {
		var ok bool
		next.TotalTokens, ok = usage.AddNonNegativeInt64(next.InputTokens, next.OutputTokens, next.ReasoningTokens)
		if !ok {
			log.Warn("usage: rejected overflowing Gemini stream token detail")
			return
		}
	}
	for index, field := range []struct {
		count, details string
		previous       int64
	}{
		{"promptTokenCount", "promptTokensDetails", b.geminiPromptTokens},
		{"cachedContentTokenCount", "cacheTokensDetails", b.detail.CacheReadTokens},
		{"candidatesTokenCount", "candidatesTokensDetails", b.detail.OutputTokens},
		{"toolUsePromptTokenCount", "toolUsePromptTokensDetails", b.geminiToolUseTokens},
	} {
		if value := firstExistingUsageNode(
			node, field.count,
		); value.Exists() && value.Int() != field.previous && !node.Get(field.details).IsArray() {
			b.geminiModalities[index] = nil
		}
	}
	billingNode := node
	if next.CacheReadReported && !firstExistingUsageNode(node, "cachedContentTokenCount").Exists() {
		if raw, err := sjson.SetBytes([]byte(node.Raw), "cachedContentTokenCount", next.CacheReadTokens); err == nil {
			billingNode = gjson.ParseBytes(raw)
		}
	}
	next.BillingMetadata = parseGeminiBillingMetadata(billingNode, &b.geminiModalities)
	if next.BillingMetadata.ContextInputTokens == nil && b.detail.BillingMetadata != nil && b.detail.BillingMetadata.ContextInputTokens != nil {
		next.BillingMetadata.ContextInputTokens = new(*b.detail.BillingMetadata.ContextInputTokens)
	}
	b.geminiPromptTokens, b.geminiToolUseTokens = promptTokens, toolUseTokens
	b.observe(next, true, false)
}

func (b *streamUsageBuffer) observeOpenAIStream(line []byte) {
	if b == nil {
		return
	}
	b.observeOpenAIResponseModel(line)
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
	writes := b.cacheWriteUsage
	if hasUsage {
		writes.observeOpenAI(usageNode)
	}
	b.observe(detail, hasUsage, detail.ResponseServiceTier != "", writes)
}

func (b *streamUsageBuffer) observeOpenAIResponseModel(line []byte) {
	if b.modelReporter == nil {
		return
	}
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		b.flushOpenAIResponseModel()
		return
	}
	if b.modelOverflow {
		return
	}
	if len(trimmed) > maxStreamModelEventBytes {
		b.modelOverflow = true
		b.modelLines = nil
		b.modelBytes = 0
		return
	}
	b.modelReporter.observeResponseModel(trimmed)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(trimmed[len("data:"):])
	if b.modelBytes+len(data)+32 > maxStreamModelEventBytes || len(b.modelLines) >= maxStreamModelEventLines {
		b.modelOverflow = true
		b.modelLines = nil
		b.modelBytes = 0
		return
	}
	b.modelLines = append(b.modelLines, bytes.Clone(data))
	b.modelBytes += len(data) + 32
}

func (b *streamUsageBuffer) flushOpenAIResponseModel() {
	if b.modelReporter != nil && !b.modelOverflow && len(b.modelLines) > 1 {
		b.modelReporter.observeResponseModel(bytes.Join(b.modelLines, []byte("\n")))
	}
	b.modelLines = nil
	b.modelBytes = 0
	b.modelOverflow = false
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
	b.modelOverflow = true
	b.modelLines = nil
	b.modelBytes = 0
	log.Warnf("stream usage SSE line exceeded %d bytes and was ignored", maxStreamUsageLineBytes)
}

func (b *streamUsageBuffer) flushOpenAIStream() {
	if b == nil {
		return
	}
	if !b.pendingOverflow && len(b.pending) > 0 {
		b.observeOpenAIStream(b.pending)
	}
	b.flushOpenAIResponseModel()
	b.pending = nil
	b.pendingOverflow = false
}

func (b *streamUsageBuffer) publish(ctx context.Context, reporter *usageReporter) bool {
	if reporter == nil {
		return false
	}
	reporter.markStream()
	if b == nil || (!b.hasUsage && !b.hasTier) {
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

// PluginExecutorStreamUsage collects completed usage events before response translation.
type PluginExecutorStreamUsage struct {
	protocol string
	openAI   streamUsageBuffer
	claude   claudeStreamUsageBuffer
}

func NewPluginExecutorStreamUsage(protocol string) *PluginExecutorStreamUsage {
	return &PluginExecutorStreamUsage{protocol: strings.ToLower(strings.TrimSpace(protocol))}
}

// ObserveLine accepts one complete SSE line or one complete JSON payload.
func (b *PluginExecutorStreamUsage) ObserveLine(line []byte) {
	if b == nil {
		return
	}
	switch b.protocol {
	case "claude":
		b.claude.observe(line)
	case "gemini":
		b.openAI.observeGeminiStream(line)
	case "interactions", "interactions-response":
		if detail, hasUsage, hasTier := parseInteractionsStreamUsageState(line); hasUsage || hasTier {
			writes := b.openAI.cacheWriteUsage
			writes.observe(
				cacheWriteUsageNode(
					interactionsUsageNode(gjson.ParseBytes(jsonPayload(line))), "cache_creation_tokens",
					"cacheCreationTokens", "cache_write_tokens", "cacheWriteTokens",
				), [4]gjson.Result{},
			)
			b.openAI.observe(detail, hasUsage, hasTier, writes)
		}
	case "antigravity":
		b.openAI.observeGeminiStream(line)
	case "codex", "openai-response":
		payload := jsonPayload(line)
		if len(payload) > 0 && gjson.ValidBytes(payload) {
			if detail, ok := parseCodexUsage(payload); ok && hasNonZeroTokenUsage(detail) {
				writes := b.openAI.cacheWriteUsage
				writes.observeOpenAI(gjson.GetBytes(payload, "response.usage"))
				b.openAI.observe(detail, true, detail.ResponseServiceTier != "", writes)
				return
			}
		}
		b.openAI.observeOpenAIStream(line)
	default:
		b.openAI.observeOpenAIStream(line)
	}
}

func (b *PluginExecutorStreamUsage) Detail() (usage.Detail, bool) {
	if b == nil {
		return usage.Detail{}, false
	}
	if b.protocol == "claude" {
		return b.claude.detailValue()
	}
	return b.openAI.detailValue()
}

func hasNonZeroTokenUsage(detail usage.Detail) bool {
	return detail.InputTokens != 0 ||
		detail.OutputTokens != 0 ||
		detail.ReasoningTokens != 0 ||
		detail.CachedTokens != 0 ||
		detail.CacheReadTokens != 0 ||
		detail.CacheCreationTokens != 0 ||
		detail.TotalTokens != 0 ||
		detail.TokenBreakdown.TotalTokens != 0
}

// ParsePluginExecutorResponseUsage reads native plugin response usage before translation.
func ParsePluginExecutorResponseUsage(protocol string, payload []byte) usage.Detail {
	if len(payload) == 0 {
		return usage.Detail{}
	}
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "claude":
		root := gjson.ParseBytes(payload)
		node := root.Get("usage")
		if !node.Exists() {
			node = root.Get("message.usage")
		}
		if !node.Exists() || !gjson.ValidBytes(payload) {
			return usage.Detail{}
		}
		return parseClaudeUsageNode(node)
	case "gemini":
		return parseGeminiUsage(payload)
	case "interactions", "interactions-response":
		return parseInteractionsUsage(payload)
	case "antigravity":
		return parseAntigravityUsage(payload)
	case "codex", "openai-response":
		detail, ok := parseCodexUsage(payload)
		if ok && hasNonZeroTokenUsage(detail) {
			return detail
		}
		openAIDetail := parseOpenAIUsage(payload)
		if hasNonZeroTokenUsage(openAIDetail) {
			if openAIDetail.ResponseServiceTier == "" {
				openAIDetail.ResponseServiceTier = detail.ResponseServiceTier
			}
			return openAIDetail
		}
		if ok {
			return detail
		}
		return openAIDetail
	default:
		return parseOpenAIUsage(payload)
	}
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
	var writes cacheWriteUsage
	writes.observeClaude(usageNode)
	rawOutputTokens := firstExistingUsageNode(usageNode, "output_tokens").Int()
	reasoningTokens := firstExistingUsageNode(
		usageNode,
		"output_tokens_details.thinking_tokens",
		"output_tokens_details.reasoning_tokens",
		"thinking_tokens",
	).Int()
	nonReasoningOutput := rawOutputTokens
	if reasoningTokens <= rawOutputTokens {
		nonReasoningOutput -= reasoningTokens
	} else {
		nonReasoningOutput = 0
	}
	detail := usage.Detail{
		InputTokens:       firstExistingUsageNode(usageNode, "input_tokens").Int(),
		OutputTokens:      nonReasoningOutput,
		ReasoningTokens:   reasoningTokens,
		CachedTokens:      cacheReadTokens,
		CacheReadTokens:   cacheReadTokens,
		CacheReadReported: firstExistingUsageNode(usageNode, "cache_read_input_tokens").Exists(),
		BillingMetadata:   &usage.BillingMetadata{SchemaVersion: 1},

		ResponseServiceTier: usageServiceTier(usageNode),
	}
	if !writes.apply(&detail) {
		return usage.Detail{CacheWriteReported: new(false)}
	}
	if detail.CachedTokens == 0 {
		detail.CachedTokens = detail.CacheCreationTokens
	}
	total, ok := usage.AddNonNegativeInt64(
		detail.InputTokens, rawOutputTokens, detail.CacheReadTokens, detail.CacheCreationTokens,
	)
	if !ok {
		log.Warn("usage: rejected overflowing Claude token detail")
		return usage.Detail{}
	}
	detail.TotalTokens = total
	if firstExistingUsageNode(
		usageNode, "input_tokens",
	).Exists() && detail.CacheReadReported && detail.CacheWriteReported != nil && *detail.CacheWriteReported {
		contextTokens, _ := usage.AddNonNegativeInt64(
			detail.InputTokens, detail.CacheReadTokens, detail.CacheCreationTokens,
		)
		detail.BillingMetadata.ContextInputTokens = &contextTokens
	}
	if region := usageNode.Get("inference_geo"); region.Type == gjson.String && strings.TrimSpace(region.String()) != "" {
		detail.BillingMetadata.Region = strings.TrimSpace(region.String())
		detail.BillingMetadata.RegionSource = "usage.inference_geo"
	}
	detail.TokenBreakdown = usage.NewIndependentTokenBreakdown(
		detail.InputTokens,
		detail.CacheReadTokens,
		detail.CacheCreationTokens,
		detail.OutputTokens,
		detail.ReasoningTokens,
		detail.TotalTokens,
	)
	return detail
}

type claudeStreamUsageBuffer struct {
	cacheWriteUsage cacheWriteUsage
	inputReported   bool
	detail          usage.Detail
	rawOutputTokens int64
	hasUsage        bool
	hasTier         bool
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
		rawOutputTokens := b.rawOutputTokens
		writes := b.cacheWriteUsage
		writes.observeClaude(usageNode)
		if !writes.apply(&candidate) {
			return
		}
		inputReported := b.inputReported || firstExistingUsageNode(usageNode, "input_tokens").Exists()
		mergeClaudeUsageNode(&candidate, &rawOutputTokens, usageNode, inputReported)
		total, ok := usage.AddNonNegativeInt64(
			candidate.InputTokens,
			rawOutputTokens,
			candidate.CacheReadTokens,
			candidate.CacheCreationTokens,
		)
		if ok {
			candidate.TotalTokens = total
			b.rawOutputTokens = rawOutputTokens
			candidate.CachedTokens = candidate.CacheReadTokens
			if candidate.CachedTokens == 0 {
				candidate.CachedTokens = candidate.CacheCreationTokens
			}
			candidate.TokenBreakdown = usage.NewIndependentTokenBreakdown(
				candidate.InputTokens,
				candidate.CacheReadTokens,
				candidate.CacheCreationTokens,
				candidate.OutputTokens,
				candidate.ReasoningTokens,
				candidate.TotalTokens,
			)
			b.inputReported = inputReported
			b.cacheWriteUsage = writes
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
	if reporter == nil {
		return false
	}
	reporter.markStream()
	if b == nil || (!b.hasUsage && !b.hasTier) {
		return false
	}
	reporter.publish(ctx, b.detail)
	return true
}

func (b *claudeStreamUsageBuffer) publishFailure(ctx context.Context, reporter *usageReporter, errs ...error) {
	if reporter == nil {
		return
	}
	reporter.markStream()
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
		"cache_creation.ephemeral_5m_input_tokens",
		"cache_creation.ephemeral_1h_input_tokens",
		"output_tokens_details.thinking_tokens",
		"output_tokens_details.reasoning_tokens",
		"thinking_tokens",
	} {
		if validUsageTokenNode(node.Get(path)) {
			return true
		}
	}
	return false
}

func mergeClaudeUsageNode(detail *usage.Detail, rawOutputTokens *int64, node gjson.Result, inputReported bool) {
	if detail == nil || rawOutputTokens == nil {
		return
	}
	if value := node.Get("input_tokens"); validUsageTokenNode(value) {
		detail.InputTokens = value.Int()
	}
	if value := node.Get("output_tokens"); validUsageTokenNode(value) {
		*rawOutputTokens = value.Int()
	}
	if value := firstExistingUsageNode(
		node,
		"output_tokens_details.thinking_tokens",
		"output_tokens_details.reasoning_tokens",
		"thinking_tokens",
	); value.Exists() {
		detail.ReasoningTokens = value.Int()
	}
	detail.OutputTokens = *rawOutputTokens
	if detail.ReasoningTokens <= *rawOutputTokens {
		detail.OutputTokens -= detail.ReasoningTokens
	} else {
		detail.OutputTokens = 0
	}
	if value := node.Get("cache_read_input_tokens"); validUsageTokenNode(value) {
		detail.CacheReadTokens = value.Int()
		detail.CacheReadReported = true
	}
	metadata := usage.CloneBillingMetadata(detail.BillingMetadata)
	if metadata == nil {
		metadata = &usage.BillingMetadata{SchemaVersion: 1}
	}
	metadata.ContextInputTokens = nil
	if inputReported && detail.CacheReadReported && detail.CacheWriteReported != nil && *detail.CacheWriteReported {
		if value, ok := usage.AddNonNegativeInt64(
			detail.InputTokens, detail.CacheReadTokens, detail.CacheCreationTokens,
		); ok {
			metadata.ContextInputTokens = &value
		}
	}
	if region := node.Get("inference_geo"); region.Type == gjson.String && strings.TrimSpace(region.String()) != "" {
		metadata.Region = strings.TrimSpace(region.String())
		metadata.RegionSource = "usage.inference_geo"
	}
	detail.BillingMetadata = metadata
}

func parseClaudeStreamUsage(line []byte) (usage.Detail, bool) {
	var buffer claudeStreamUsageBuffer
	buffer.observe(line)
	return buffer.detailValue()
}

func parseGeminiBillingMetadata(node gjson.Result, groups *[4]map[string]int64) *usage.BillingMetadata {
	metadata := &usage.BillingMetadata{SchemaVersion: 1}
	if prompt := firstExistingUsageNode(node, "promptTokenCount"); prompt.Exists() {
		value := prompt.Int()
		metadata.ContextInputTokens = &value
	}
	if groups == nil {
		groups = &[4]map[string]int64{}
	}
	for index, path := range []string{
		"promptTokensDetails", "cacheTokensDetails", "candidatesTokensDetails", "toolUsePromptTokensDetails",
	} {
		if !node.Get(path).IsArray() {
			continue
		}
		groups[index] = make(map[string]int64)
		for _, value := range node.Get(path).Array() {
			modality := strings.ToLower(value.Get("modality").String())
			if modality != "text" && modality != "image" && modality != "audio" && modality != "video" {
				continue
			}
			count := firstExistingUsageNode(value, "tokenCount")
			if !count.Exists() {
				continue
			}
			if total, ok := usage.AddNonNegativeInt64(groups[index][modality], count.Int()); ok {
				groups[index][modality] = total
			} else {
				log.Warn("usage: ignored overflowing Gemini modality detail")
			}
		}
	}
	for _, modality := range []string{"text", "image", "audio", "video"} {
		prompt, promptKnown := groups[0][modality]
		cached, cacheKnown := groups[1][modality]
		tool, toolKnown := groups[3][modality]
		ordinary, ordinaryKnown := tool, toolKnown
		cachedTotal := firstExistingUsageNode(node, "cachedContentTokenCount")
		if promptKnown && cached <= prompt && (cacheKnown || cachedTotal.Exists() && cachedTotal.Int() == 0) {
			if count, ok := usage.AddNonNegativeInt64(prompt-cached, tool); ok {
				ordinary, ordinaryKnown = count, true
			}
		}
		if ordinaryKnown {
			metadata.Modalities = append(
				metadata.Modalities,
				usage.ModalityTokenUsage{Category: "uncached_input", Modality: modality, Tokens: ordinary},
			)
		}
		if cacheKnown {
			metadata.Modalities = append(
				metadata.Modalities,
				usage.ModalityTokenUsage{Category: "cache_read", Modality: modality, Tokens: cached},
			)
		}
		if count, known := groups[2][modality]; known {
			metadata.Modalities = append(
				metadata.Modalities, usage.ModalityTokenUsage{Category: "output", Modality: modality, Tokens: count},
			)
		}
	}
	return metadata
}

func parseGeminiFamilyUsageDetail(node gjson.Result) usage.Detail {
	cachedTokens := firstExistingUsageNode(node, "cachedContentTokenCount").Int()
	detail := usage.Detail{
		InputTokens:        firstExistingUsageNode(node, "promptTokenCount").Int(),
		OutputTokens:       firstExistingUsageNode(node, "candidatesTokenCount").Int(),
		ReasoningTokens:    firstExistingUsageNode(node, "thoughtsTokenCount").Int(),
		TotalTokens:        firstExistingUsageNode(node, "totalTokenCount").Int(),
		CachedTokens:       cachedTokens,
		CacheReadTokens:    cachedTokens,
		CacheReadReported:  firstExistingUsageNode(node, "cachedContentTokenCount").Exists(),
		CacheWriteReported: new(false),
		BillingMetadata:    parseGeminiBillingMetadata(node, nil),
	}
	if prompt := firstExistingUsageNode(node, "promptTokenCount"); prompt.Exists() {
		value := prompt.Int()
		detail.BillingMetadata.ContextInputTokens = &value
	}
	toolUseTokens := node.Get("toolUsePromptTokenCount")
	inputTokens, ok := usage.AddNonNegativeInt64(detail.InputTokens, toolUseTokens.Int())
	if !ok || (toolUseTokens.Exists() && !validUsageTokenNode(toolUseTokens)) {
		log.Warn("usage: rejected invalid or overflowing Gemini input detail")
		detail = usage.EnsureTokenBreakdownForProvider(usage.Detail{TotalTokens: detail.TotalTokens}, "gemini", "")
		detail.TokenBreakdown.Quality = usage.TokenAccountingQualityInconsistent
		return detail
	}
	detail.InputTokens = inputTokens
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
	node := firstValidUsageObject(
		root, geminiUsageTokenFieldsExist,
		"response.usageMetadata", "response.usage_metadata", "usageMetadata", "usage_metadata",
	)
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
	detail := parseGeminiFamilyUsageDetail(node)
	if detail.InputTokens == 0 &&
		detail.OutputTokens == 0 &&
		detail.ReasoningTokens == 0 &&
		detail.CachedTokens == 0 &&
		detail.TotalTokens == 0 && !detail.CacheReadReported {
		return usage.Detail{}, false
	}
	return detail, true
}

func parseInteractionsUsageDetail(node gjson.Result) usage.Detail {
	cacheRead := firstExistingUsageNode(node, "cache_read_tokens", "cacheReadTokens")
	cacheWrite := firstExistingUsageNode(
		node, "cache_creation_tokens", "cacheCreationTokens", "cache_write_tokens", "cacheWriteTokens",
	)
	detail := usage.Detail{
		InputTokens: firstExistingUsageNode(node, "input_tokens", "prompt_tokens", "total_input_tokens").Int(),
		OutputTokens: firstExistingUsageNode(
			node, "output_tokens", "completion_tokens", "total_output_tokens",
		).Int(),
		ReasoningTokens: firstExistingUsageNode(
			node, "reasoning_tokens", "thoughtsTokenCount", "total_thought_tokens",
		).Int(),
		TotalTokens: firstExistingUsageNode(node, "total_tokens", "totalTokenCount").Int(),
		CachedTokens: firstExistingUsageNode(
			node, "cached_tokens", "cachedContentTokenCount", "total_cached_tokens",
		).Int(),
		CacheReadTokens:     cacheRead.Int(),
		CacheReadReported:   cacheRead.Exists() || firstExistingUsageNode(node, "cachedContentTokenCount").Exists(),
		CacheWriteReported:  new(cacheWrite.Exists()),
		CacheCreationTokens: cacheWrite.Int(),
	}
	if !cacheRead.Exists() && detail.CachedTokens > 0 {
		detail.CacheReadTokens = detail.CachedTokens
	}
	var toolUseTokens gjson.Result
	for _, field := range []string{"total_tool_use_tokens", "tool_use_tokens", "toolUsePromptTokenCount"} {
		if value := node.Get(field); value.Exists() {
			toolUseTokens = value
			break
		}
	}
	inputTokens, ok := usage.AddNonNegativeInt64(detail.InputTokens, toolUseTokens.Int())
	if !ok || (toolUseTokens.Exists() && !validUsageTokenNode(toolUseTokens)) {
		log.Warn("usage: rejected invalid or overflowing Interactions input detail")
		detail = usage.EnsureTokenBreakdownForProvider(
			usage.Detail{TotalTokens: detail.TotalTokens}, "interactions", "",
		)
		detail.TokenBreakdown.Quality = usage.TokenAccountingQualityInconsistent
		return detail
	}
	detail.InputTokens = inputTokens
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
		"toolUsePromptTokenCount",
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
		"total_tool_use_tokens", "tool_use_tokens", "toolUsePromptTokenCount",
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
	node := firstValidUsageObject(
		root, geminiUsageTokenFieldsExist,
		"response.usageMetadata", "response.usage_metadata", "usageMetadata", "usage_metadata",
	)
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
