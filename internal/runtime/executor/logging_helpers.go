package executor

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	apiAttemptsKey          = "API_UPSTREAM_ATTEMPTS"
	apiRequestKey           = "API_REQUEST"
	apiResponseKey          = "API_RESPONSE"
	apiWebsocketTimelineKey = "API_WEBSOCKET_TIMELINE"
)

// upstreamRequestLog captures the outbound upstream request details for logging.
type upstreamRequestLog struct {
	URL       string
	Method    string
	Headers   http.Header
	Body      []byte
	Provider  string
	AuthID    string
	AuthLabel string
	AuthType  string
	AuthValue string
}

type upstreamAttempt struct {
	index                int
	request              string
	response             *strings.Builder
	responseSource       *logging.FileBodySource
	responseIntroWritten bool
	statusWritten        bool
	headersWritten       bool
	bodyStarted          bool
	bodyHasContent       bool
	errorWritten         bool
}

// recordUpstreamRequest records an upstream API request with auth info extracted from the given auth.
// This is a convenience wrapper that combines auth info extraction with recordAPIRequest,
// eliminating the repeated auth variable extraction pattern across all executors.
func recordUpstreamRequest(
	ctx context.Context,
	cfg *config.Config,
	url string,
	headers http.Header,
	body []byte,
	provider string,
	auth *cliproxyauth.Auth,
) {
	RecordAPIRequest(ctx, cfg, url, http.MethodPost, headers, body, provider, auth)
}

// RecordAPIRequest stores outbound upstream request metadata in Gin context for request logging.
func RecordAPIRequest(
	ctx context.Context,
	cfg *config.Config,
	url string,
	method string,
	headers http.Header,
	body []byte,
	provider string,
	auth *cliproxyauth.Auth,
) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(
		ctx, cfg, upstreamRequestLog{
			URL:       url,
			Method:    method,
			Headers:   headers,
			Body:      body,
			Provider:  provider,
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		},
	)
}

// RecordAPIResponseMetadata captures upstream response status/header information for request logging.
func RecordAPIResponseMetadata(ctx context.Context, cfg *config.Config, status int, headers http.Header) {
	recordAPIResponseMetadata(ctx, cfg, status, headers)
}

// RecordAPIResponseError adds an upstream response error entry for request logging.
func RecordAPIResponseError(ctx context.Context, cfg *config.Config, err error) {
	recordAPIResponseError(ctx, cfg, err)
}

// AppendAPIResponseChunk appends an upstream response body chunk for request logging.
func AppendAPIResponseChunk(ctx context.Context, cfg *config.Config, chunk []byte) {
	appendAPIResponseChunk(ctx, cfg, chunk)
}

// recordAPIRequest stores the upstream request metadata in Gin context for request logging.
func requestLogCaptureEnabled(cfg *config.Config) bool {
	return cfg != nil && cfg.RequestLog && !cfg.CommercialMode
}

func recordAPIRequest(ctx context.Context, cfg *config.Config, info upstreamRequestLog) {
	if !requestLogCaptureEnabled(cfg) {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}

	attempts := getAttempts(ginCtx)
	index := len(attempts) + 1
	addRequestTimelineEvent(ginCtx, "upstream.request.start", upstreamRequestTimelineDetail(index, info))

	builder := &strings.Builder{}
	builder.WriteString(fmt.Sprintf("=== API REQUEST %d ===\n", index))
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	if info.URL != "" {
		builder.WriteString(fmt.Sprintf("Upstream URL: %s\n", info.URL))
	} else {
		builder.WriteString("Upstream URL: <unknown>\n")
	}
	if info.Method != "" {
		builder.WriteString(fmt.Sprintf("HTTP Method: %s\n", info.Method))
	}
	if auth := formatAuthInfo(info); auth != "" {
		builder.WriteString(fmt.Sprintf("Auth: %s\n", auth))
	}
	builder.WriteString("\nHeaders:\n")
	writeHeaders(builder, info.Headers)
	builder.WriteString("\nBody:\n")
	if len(info.Body) > 0 {
		builder.WriteString(string(info.Body))
	} else {
		builder.WriteString("<empty>")
	}
	builder.WriteString("\n\n")

	requestText := builder.String()
	if source, ok := apiRequestSource(ginCtx); ok {
		if errWrite := source.AppendBytes([]byte(requestText)); errWrite == nil {
			requestText = ""
		} else {
			log.WithError(errWrite).Warn("failed to append api request log part")
			requestText = drainFileBodySourceToString(source) + requestText
			disableFileBodySource(ginCtx, logging.APIRequestSourceContextKey, source)
		}
	}

	attempt := &upstreamAttempt{
		index:          index,
		request:        requestText,
		response:       &strings.Builder{},
		responseSource: apiResponseSourceOrNil(ginCtx),
	}
	attempts = append(attempts, attempt)
	ginCtx.Set(apiAttemptsKey, attempts)
	if requestText != "" {
		updateAggregatedRequest(ginCtx, attempts)
	}
}

// recordAPIResponseMetadata captures upstream response status/header information for the latest attempt.
func recordAPIResponseMetadata(ctx context.Context, cfg *config.Config, status int, headers http.Header) {
	logging.SetResponseHeaders(ctx, headers)
	if !requestLogCaptureEnabled(cfg) {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	attempts, attempt := ensureAttempt(ginCtx)
	ensureResponseIntro(ginCtx, attempt)
	addRequestTimelineEvent(
		ginCtx, "upstream.response.headers", fmt.Sprintf("attempt=%d status=%d", attempt.index, status),
	)

	if status > 0 && !attempt.statusWritten {
		writeAttemptResponse(ginCtx, attempt, []byte(fmt.Sprintf("Status: %d\n", status)))
		attempt.statusWritten = true
	}
	if !attempt.headersWritten {
		builder := &strings.Builder{}
		builder.WriteString("Headers:\n")
		writeHeaders(builder, headers)
		writeAttemptResponse(ginCtx, attempt, []byte(builder.String()))
		attempt.headersWritten = true
		writeAttemptResponse(ginCtx, attempt, []byte("\n"))
	}

	updateAggregatedResponseIfMemoryBacked(ginCtx, attempts)
}

// recordAPIResponseError adds an error entry for the latest attempt when no HTTP response is available.
func recordAPIResponseError(ctx context.Context, cfg *config.Config, err error) {
	if !requestLogCaptureEnabled(cfg) || err == nil {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	attempts, attempt := ensureAttempt(ginCtx)
	ensureResponseIntro(ginCtx, attempt)
	addRequestTimelineEvent(ginCtx, "upstream.response.error", fmt.Sprintf("attempt=%d", attempt.index))

	if attempt.bodyStarted && !attempt.bodyHasContent {
		// Ensure body does not stay empty marker if error arrives first.
		attempt.bodyStarted = false
	}
	if attempt.errorWritten {
		writeAttemptResponse(ginCtx, attempt, []byte("\n"))
	}
	writeAttemptResponse(ginCtx, attempt, []byte(fmt.Sprintf("Error: %s\n", err.Error())))
	attempt.errorWritten = true

	updateAggregatedResponseIfMemoryBacked(ginCtx, attempts)
}

// appendAPIResponseChunk appends an upstream response chunk to Gin context for request logging.
func appendAPIResponseChunk(ctx context.Context, cfg *config.Config, chunk []byte) {
	logging.MarkFirstResponseByte(ctx)
	if !requestLogCaptureEnabled(cfg) {
		return
	}
	data := bytes.TrimSpace(chunk)
	if len(data) == 0 {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	attempts, attempt := ensureAttempt(ginCtx)
	ensureResponseIntro(ginCtx, attempt)
	addRequestTimelineEventOnce(ginCtx, "upstream.response.first_body", fmt.Sprintf("attempt=%d", attempt.index))

	if !attempt.headersWritten {
		builder := &strings.Builder{}
		builder.WriteString("Headers:\n")
		writeHeaders(builder, nil)
		writeAttemptResponse(ginCtx, attempt, []byte(builder.String()))
		attempt.headersWritten = true
		writeAttemptResponse(ginCtx, attempt, []byte("\n"))
	}
	if !attempt.bodyStarted {
		writeAttemptResponse(ginCtx, attempt, []byte("Body:\n"))
		attempt.bodyStarted = true
	}
	if attempt.bodyHasContent {
		writeAttemptResponse(ginCtx, attempt, []byte("\n\n"))
	}
	writeAttemptResponse(ginCtx, attempt, data)
	attempt.bodyHasContent = true

	updateAggregatedResponseIfMemoryBacked(ginCtx, attempts)
}

func recordAPIWebsocketRequest(ctx context.Context, cfg *config.Config, info upstreamRequestLog) {
	if !requestLogCaptureEnabled(cfg) {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}

	builder := &strings.Builder{}
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	builder.WriteString("Event: api.websocket.request\n")
	if info.URL != "" {
		builder.WriteString(fmt.Sprintf("Upstream URL: %s\n", info.URL))
	}
	if auth := formatAuthInfo(info); auth != "" {
		builder.WriteString(fmt.Sprintf("Auth: %s\n", auth))
	}
	builder.WriteString("Headers:\n")
	writeHeaders(builder, info.Headers)
	builder.WriteString("\nBody:\n")
	if len(info.Body) > 0 {
		builder.Write(info.Body)
	} else {
		builder.WriteString("<empty>")
	}
	builder.WriteString("\n")

	addRequestTimelineEvent(ginCtx, "upstream.websocket.request", upstreamRequestTimelineDetail(1, info))
	appendAPIWebsocketTimeline(ginCtx, []byte(builder.String()))
}

func recordAPIWebsocketHandshake(ctx context.Context, cfg *config.Config, status int, headers http.Header) {
	logging.SetResponseHeaders(ctx, headers)
	if !requestLogCaptureEnabled(cfg) {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}

	builder := &strings.Builder{}
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	builder.WriteString("Event: api.websocket.handshake\n")
	if status > 0 {
		builder.WriteString(fmt.Sprintf("Status: %d\n", status))
	}
	builder.WriteString("Headers:\n")
	writeHeaders(builder, headers)
	builder.WriteString("\n")

	addRequestTimelineEvent(ginCtx, "upstream.websocket.handshake", fmt.Sprintf("status=%d", status))
	appendAPIWebsocketTimeline(ginCtx, []byte(builder.String()))
}

func recordAPIWebsocketUpgradeRejection(
	ctx context.Context,
	cfg *config.Config,
	info upstreamRequestLog,
	status int,
	headers http.Header,
	body []byte,
) {
	logging.SetResponseHeaders(ctx, headers)
	if !requestLogCaptureEnabled(cfg) {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}

	recordAPIRequest(ctx, cfg, info)
	recordAPIResponseMetadata(ctx, cfg, status, headers)
	appendAPIResponseChunk(ctx, cfg, body)
}

func websocketUpgradeRequestURL(rawURL string) string {
	trimmedURL := strings.TrimSpace(rawURL)
	if trimmedURL == "" {
		return ""
	}
	parsed, err := url.Parse(trimmedURL)
	if err != nil {
		return trimmedURL
	}
	switch strings.ToLower(parsed.Scheme) {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	}
	return parsed.String()
}

func appendAPIWebsocketResponse(ctx context.Context, cfg *config.Config, payload []byte) {
	logging.MarkFirstResponseByte(ctx)
	if !requestLogCaptureEnabled(cfg) {
		return
	}
	data := bytes.TrimSpace(payload)
	if len(data) == 0 {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	markAPIResponseTimestamp(ginCtx)
	addRequestTimelineEventOnce(ginCtx, "upstream.websocket.first_response", "")

	builder := &strings.Builder{}
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	builder.WriteString("Event: api.websocket.response\n")
	builder.Write(data)
	builder.WriteString("\n")

	appendAPIWebsocketTimeline(ginCtx, []byte(builder.String()))
}

func recordAPIWebsocketError(ctx context.Context, cfg *config.Config, stage string, err error) {
	if !requestLogCaptureEnabled(cfg) || err == nil {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	markAPIResponseTimestamp(ginCtx)
	addRequestTimelineEvent(ginCtx, "upstream.websocket.error", fmt.Sprintf("stage=%s", strings.TrimSpace(stage)))

	builder := &strings.Builder{}
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	builder.WriteString("Event: api.websocket.error\n")
	if trimmed := strings.TrimSpace(stage); trimmed != "" {
		builder.WriteString(fmt.Sprintf("Stage: %s\n", trimmed))
	}
	builder.WriteString(fmt.Sprintf("Error: %s\n", err.Error()))

	appendAPIWebsocketTimeline(ginCtx, []byte(builder.String()))
}

func ginContextFrom(ctx context.Context) *gin.Context {
	ginCtx, _ := util.GinContextValue(ctx).(*gin.Context)
	if ginCtx != nil {
		return ginCtx
	}
	ginCtx, _ = ctx.Value("gin").(*gin.Context)
	return ginCtx
}

func addRequestTimelineEvent(ginCtx *gin.Context, name string, detail string) {
	timeline := requestTimelineFromGin(ginCtx)
	if timeline == nil {
		return
	}
	timeline.Add(name, detail)
}

func addRequestTimelineEventOnce(ginCtx *gin.Context, name string, detail string) {
	timeline := requestTimelineFromGin(ginCtx)
	if timeline == nil {
		return
	}
	timeline.AddOnce(name, detail)
}

func requestTimelineFromGin(ginCtx *gin.Context) *logging.RequestTimeline {
	if ginCtx == nil {
		return nil
	}
	value, exists := ginCtx.Get(logging.RequestTimelineGinKey)
	if !exists {
		return nil
	}
	timeline, _ := value.(*logging.RequestTimeline)
	return timeline
}

func upstreamRequestTimelineDetail(index int, info upstreamRequestLog) string {
	parts := []string{fmt.Sprintf("attempt=%d", index)}
	if endpoint := upstreamEndpoint(info.URL); endpoint != "" {
		parts = append(parts, "endpoint="+endpoint)
	}
	if provider := strings.TrimSpace(info.Provider); provider != "" {
		parts = append(parts, "provider="+provider)
	}
	return strings.Join(parts, " ")
}

func upstreamEndpoint(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed == nil {
		return ""
	}
	endpoint := parsed.Host
	if parsed.Path != "" {
		endpoint += parsed.Path
	}
	return endpoint
}

func getAttempts(ginCtx *gin.Context) []*upstreamAttempt {
	if ginCtx == nil {
		return nil
	}
	if value, exists := ginCtx.Get(apiAttemptsKey); exists {
		if attempts, ok := value.([]*upstreamAttempt); ok {
			return attempts
		}
	}
	return nil
}

func ensureAttempt(ginCtx *gin.Context) ([]*upstreamAttempt, *upstreamAttempt) {
	attempts := getAttempts(ginCtx)
	if len(attempts) == 0 {
		attempt := &upstreamAttempt{
			index:          1,
			response:       &strings.Builder{},
			responseSource: apiResponseSourceOrNil(ginCtx),
		}
		if source, ok := apiRequestSource(ginCtx); ok {
			if errWrite := source.AppendBytes([]byte("=== API REQUEST 1 ===\n<missing>\n\n")); errWrite != nil {
				log.WithError(errWrite).Warn("failed to append missing api request log part")
				attempt.request = "=== API REQUEST 1 ===\n<missing>\n\n"
			}
		} else {
			attempt.request = "=== API REQUEST 1 ===\n<missing>\n\n"
		}
		attempts = []*upstreamAttempt{attempt}
		ginCtx.Set(apiAttemptsKey, attempts)
		if attempt.request != "" {
			updateAggregatedRequest(ginCtx, attempts)
		}
	}
	return attempts, attempts[len(attempts)-1]
}

func ensureResponseIntro(ginCtx *gin.Context, attempt *upstreamAttempt) {
	if attempt == nil || attempt.responseIntroWritten {
		return
	}
	writeAttemptResponse(ginCtx, attempt, []byte(fmt.Sprintf("=== API RESPONSE %d ===\n", attempt.index)))
	writeAttemptResponse(ginCtx, attempt, []byte(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano))))
	writeAttemptResponse(ginCtx, attempt, []byte("\n"))
	attempt.responseIntroWritten = true
}

func writeAttemptResponse(ginCtx *gin.Context, attempt *upstreamAttempt, payload []byte) {
	if attempt == nil || len(payload) == 0 {
		return
	}
	if attempt.responseSource == nil {
		attempt.responseSource = apiResponseSourceOrNil(ginCtx)
	}
	if attempt.responseSource != nil {
		if errWrite := attempt.responseSource.AppendBytes(payload); errWrite == nil {
			if ginCtx != nil {
				ginCtx.Set(logging.APIResponseCapturedContextKey, true)
			}
			return
		} else {
			log.WithError(errWrite).Warn("failed to append api response log part")
			if attempt.response == nil {
				attempt.response = &strings.Builder{}
			}
			if existing, errRead := attempt.responseSource.Bytes(); errRead == nil && len(existing) > 0 {
				attempt.response.Write(existing)
				if !bytes.HasSuffix(existing, []byte("\n")) {
					attempt.response.WriteByte('\n')
				}
			} else if errRead != nil {
				log.WithError(errRead).Warn("failed to read api response source for fallback")
			}
			disableFileBodySource(ginCtx, logging.APIResponseSourceContextKey, attempt.responseSource)
			attempt.responseSource = nil
		}
	}
	if attempt.response == nil {
		attempt.response = &strings.Builder{}
	}
	attempt.response.Write(payload)
}

func updateAggregatedRequest(ginCtx *gin.Context, attempts []*upstreamAttempt) {
	if ginCtx == nil {
		return
	}
	var builder strings.Builder
	for _, attempt := range attempts {
		builder.WriteString(attempt.request)
	}
	ginCtx.Set(apiRequestKey, []byte(builder.String()))
}

func updateAggregatedResponseIfMemoryBacked(ginCtx *gin.Context, attempts []*upstreamAttempt) {
	if apiResponseSourceOrNil(ginCtx) != nil {
		return
	}
	updateAggregatedResponse(ginCtx, attempts)
}

func updateAggregatedResponse(ginCtx *gin.Context, attempts []*upstreamAttempt) {
	if ginCtx == nil {
		return
	}
	var builder strings.Builder
	for idx, attempt := range attempts {
		if attempt == nil || attempt.response == nil {
			continue
		}
		responseText := attempt.response.String()
		if responseText == "" {
			continue
		}
		builder.WriteString(responseText)
		if !strings.HasSuffix(responseText, "\n") {
			builder.WriteString("\n")
		}
		if idx < len(attempts)-1 {
			builder.WriteString("\n")
		}
	}
	ginCtx.Set(apiResponseKey, []byte(builder.String()))
}

func apiRequestSource(ginCtx *gin.Context) (*logging.FileBodySource, bool) {
	return fileBodySourceFromGin(ginCtx, logging.APIRequestSourceContextKey)
}

func apiResponseSourceOrNil(ginCtx *gin.Context) *logging.FileBodySource {
	source, ok := fileBodySourceFromGin(ginCtx, logging.APIResponseSourceContextKey)
	if !ok {
		return nil
	}
	return source
}

func appendAPIWebsocketTimeline(ginCtx *gin.Context, chunk []byte) {
	if ginCtx == nil {
		return
	}
	data := bytes.TrimSpace(chunk)
	if len(data) == 0 {
		return
	}
	if source, ok := apiWebsocketTimelineSource(ginCtx); ok {
		if errAppend := source.AppendPart(data); errAppend == nil {
			return
		} else {
			log.WithError(errAppend).Warn("failed to append api websocket timeline log part")
			existing := drainFileBodySourceToBytes(source)
			disableFileBodySource(ginCtx, logging.APIWebsocketTimelineSourceContextKey, source)
			if len(existing) > 0 {
				appendAPIWebsocketTimelineBytes(ginCtx, existing)
			}
		}
	}
	appendAPIWebsocketTimelineBytes(ginCtx, data)
}

func appendAPIWebsocketTimelineBytes(ginCtx *gin.Context, data []byte) {
	if ginCtx == nil || len(data) == 0 {
		return
	}
	if existing, exists := ginCtx.Get(apiWebsocketTimelineKey); exists {
		if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
			combined := make([]byte, 0, len(existingBytes)+len(data)+2)
			combined = append(combined, existingBytes...)
			if !bytes.HasSuffix(existingBytes, []byte("\n")) {
				combined = append(combined, '\n')
			}
			combined = append(combined, '\n')
			combined = append(combined, data...)
			ginCtx.Set(apiWebsocketTimelineKey, combined)
			return
		}
	}
	ginCtx.Set(apiWebsocketTimelineKey, bytes.Clone(data))
}

func apiWebsocketTimelineSource(ginCtx *gin.Context) (*logging.FileBodySource, bool) {
	return fileBodySourceFromGin(ginCtx, logging.APIWebsocketTimelineSourceContextKey)
}

func fileBodySourceFromGin(ginCtx *gin.Context, key string) (*logging.FileBodySource, bool) {
	if ginCtx == nil {
		return nil, false
	}
	value, exists := ginCtx.Get(key)
	if !exists {
		return nil, false
	}
	source, ok := value.(*logging.FileBodySource)
	if !ok || source == nil {
		return nil, false
	}
	return source, true
}

func drainFileBodySourceToString(source *logging.FileBodySource) string {
	return string(drainFileBodySourceToBytes(source))
}

func drainFileBodySourceToBytes(source *logging.FileBodySource) []byte {
	if source == nil {
		return nil
	}
	existing, errRead := source.Bytes()
	if errRead != nil {
		log.WithError(errRead).Warn("failed to read log source for fallback")
		return nil
	}
	return existing
}

func disableFileBodySource(ginCtx *gin.Context, key string, source *logging.FileBodySource) {
	if ginCtx != nil {
		ginCtx.Set(key, (*logging.FileBodySource)(nil))
	}
	if source == nil {
		return
	}
	if errCleanup := source.Cleanup(); errCleanup != nil {
		log.WithError(errCleanup).Warn("failed to clean up disabled log part files")
	}
}

func markAPIResponseTimestamp(ginCtx *gin.Context) {
	if ginCtx == nil {
		return
	}
	if _, exists := ginCtx.Get("API_RESPONSE_TIMESTAMP"); exists {
		return
	}
	ginCtx.Set("API_RESPONSE_TIMESTAMP", time.Now())
	addRequestTimelineEventOnce(ginCtx, "upstream.response.done", "")
}

func writeHeaders(builder *strings.Builder, headers http.Header) {
	if builder == nil {
		return
	}
	if len(headers) == 0 {
		builder.WriteString("<none>\n")
		return
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := headers[key]
		if len(values) == 0 {
			builder.WriteString(fmt.Sprintf("%s:\n", key))
			continue
		}
		for _, value := range values {
			masked := util.MaskSensitiveHeaderValue(key, value)
			builder.WriteString(fmt.Sprintf("%s: %s\n", key, masked))
		}
	}
}

func formatAuthInfo(info upstreamRequestLog) string {
	var parts []string
	if trimmed := strings.TrimSpace(info.Provider); trimmed != "" {
		parts = append(parts, fmt.Sprintf("provider=%s", trimmed))
	}
	if trimmed := strings.TrimSpace(info.AuthID); trimmed != "" {
		parts = append(parts, fmt.Sprintf("auth_id=%s", trimmed))
	}
	if trimmed := strings.TrimSpace(info.AuthLabel); trimmed != "" {
		parts = append(parts, fmt.Sprintf("label=%s", trimmed))
	}

	authType := strings.ToLower(strings.TrimSpace(info.AuthType))
	authValue := strings.TrimSpace(info.AuthValue)
	switch authType {
	case "api_key":
		if authValue != "" {
			parts = append(parts, fmt.Sprintf("type=api_key value=%s", util.HideAPIKey(authValue)))
		} else {
			parts = append(parts, "type=api_key")
		}
	case "oauth":
		parts = append(parts, "type=oauth")
	default:
		if authType != "" {
			if authValue != "" {
				parts = append(parts, fmt.Sprintf("type=%s value=%s", authType, authValue))
			} else {
				parts = append(parts, fmt.Sprintf("type=%s", authType))
			}
		}
	}

	return strings.Join(parts, ", ")
}

func summarizeErrorBody(contentType string, body []byte) string {
	isHTML := strings.Contains(strings.ToLower(contentType), "text/html")
	if !isHTML {
		trimmed := bytes.TrimSpace(bytes.ToLower(body))
		if bytes.HasPrefix(trimmed, []byte("<!doctype html")) || bytes.HasPrefix(trimmed, []byte("<html")) {
			isHTML = true
		}
	}
	if isHTML {
		if title := extractHTMLTitle(body); title != "" {
			return title
		}
		return "[html body omitted]"
	}

	// Try to extract error message from JSON response
	if message := extractJSONErrorMessage(body); message != "" {
		return message
	}

	return string(body)
}

func extractHTMLTitle(body []byte) string {
	lower := bytes.ToLower(body)
	start := bytes.Index(lower, []byte("<title"))
	if start == -1 {
		return ""
	}
	gt := bytes.IndexByte(lower[start:], '>')
	if gt == -1 {
		return ""
	}
	start += gt + 1
	end := bytes.Index(lower[start:], []byte("</title>"))
	if end == -1 {
		return ""
	}
	title := string(body[start : start+end])
	title = html.UnescapeString(title)
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	return strings.Join(strings.Fields(title), " ")
}

// extractJSONErrorMessage attempts to extract error.message from JSON error responses
func extractJSONErrorMessage(body []byte) string {
	result := gjson.GetBytes(body, "error.message")
	if result.Exists() && result.String() != "" {
		return result.String()
	}
	return ""
}

// logWithRequestID returns a logrus Entry with request_id field populated from context.
// If no request ID is found in context, it returns the standard logger.
func logWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	requestID := logging.GetRequestID(ctx)
	if requestID == "" {
		return log.NewEntry(log.StandardLogger())
	}
	return log.WithField("request_id", requestID)
}
