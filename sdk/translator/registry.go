package translator

import (
	"bytes"
	"context"
	"slices"
	"sync"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type RequestEnvelopeTransform func(context.Context, RequestEnvelope) RequestEnvelope

// Registry manages translation functions across schemas.
type Registry struct {
	mu        sync.RWMutex
	requests  map[Format]map[Format]RequestEnvelopeTransform
	responses map[Format]map[Format]ResponseTransform
}

// NewRegistry constructs an empty translator registry.
func NewRegistry() *Registry {
	return &Registry{
		requests:  make(map[Format]map[Format]RequestEnvelopeTransform),
		responses: make(map[Format]map[Format]ResponseTransform),
	}
}

// Register stores request/response transforms between two formats.
func (r *Registry) Register(from, to Format, request RequestTransform, response ResponseTransform) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.requests[from]; !ok {
		r.requests[from] = make(map[Format]RequestEnvelopeTransform)
	}
	if request != nil {
		r.requests[from][to] = func(_ context.Context, req RequestEnvelope) RequestEnvelope {
			req.Body = request(req.Model, req.Body, req.Stream)
			return req
		}
	}

	if _, ok := r.responses[from]; !ok {
		r.responses[from] = make(map[Format]ResponseTransform)
	}
	r.responses[from][to] = response
}

// RegisterCheckedRequest stores a converter that can reject an unsupported request.
func (r *Registry) RegisterCheckedRequest(from, to Format, request CheckedRequestTransform) {
	if request == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.requests[from] == nil {
		r.requests[from] = make(map[Format]RequestEnvelopeTransform)
	}
	r.requests[from][to] = func(_ context.Context, req RequestEnvelope) RequestEnvelope {
		req.Body, req.Err = request(req.Model, req.Body, req.Stream)
		return req
	}
}

// RegisterRequestEnvelope 注册使用完整请求与选定模型能力的转换器
func (r *Registry) RegisterRequestEnvelope(from, to Format, request RequestEnvelopeTransform) {
	if request == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.requests[from] == nil {
		r.requests[from] = make(map[Format]RequestEnvelopeTransform)
	}
	r.requests[from][to] = request
}

// TranslateRequest converts a payload between schemas, returning the original payload
// if no translator is registered.
func (r *Registry) TranslateRequest(from, to Format, model string, rawJSON []byte, stream bool) []byte {
	return r.TranslateRequestWithContext(context.Background(), from, to, model, rawJSON, stream)
}

// TranslateRequestWithContext converts a payload and applies package-level plugin hooks.
// Call TranslateRequestChecked when a request rejection must reach the caller.
func (r *Registry) TranslateRequestWithContext(
	ctx context.Context,
	from, to Format,
	model string,
	rawJSON []byte,
	stream bool,
) []byte {
	body, err := r.TranslateRequestChecked(ctx, from, to, model, rawJSON, stream)
	if err != nil {
		log.Warnf("request translation rejected for %s -> %s", from, to)
		return nil
	}
	return body
}

// TranslateRequestChecked propagates unsupported-content failures to the caller.
func (r *Registry) TranslateRequestChecked(
	ctx context.Context,
	from, to Format,
	model string,
	rawJSON []byte,
	stream bool,
) ([]byte, error) {
	req := r.TranslateRequestEnvelope(ctx, from, to, RequestEnvelope{
		Format: from,
		Model:  model,
		Stream: stream,
		Body:   rawJSON,
	})
	return req.Body, req.Err
}

// TranslateRequestEnvelope preserves request-scoped translation errors.
func (r *Registry) TranslateRequestEnvelope(ctx context.Context, from, to Format, req RequestEnvelope) RequestEnvelope {
	if ctx == nil {
		ctx = context.Background()
	}
	if req.Err != nil {
		return req
	}
	r.mu.RLock()
	var fn RequestEnvelopeTransform
	if byTarget, ok := r.requests[from]; ok {
		fn = byTarget[to]
	}
	r.mu.RUnlock()

	hooks := pluginHooksForContext(ctx)
	normalizeRequest := func(req RequestEnvelope) RequestEnvelope {
		before := configurationUpdates(req.Body)
		thinkingBefore := gjson.GetBytes(req.Body, "thinking.type").Raw
		budgetBefore := gjson.GetBytes(req.Body, "thinking.budget_tokens").Raw
		effortBefore := gjson.GetBytes(req.Body, "output_config.effort").Raw
		req.Body = hooks.NormalizeRequest(ctx, from, to, req.Model, bytes.Clone(req.Body), req.Stream)
		changed := !slices.Equal(before, configurationUpdates(req.Body))
		if to == FormatClaude && (thinkingBefore != gjson.GetBytes(req.Body, "thinking.type").Raw ||
			budgetBefore != gjson.GetBytes(req.Body, "thinking.budget_tokens").Raw ||
			effortBefore != gjson.GetBytes(req.Body, "output_config.effort").Raw) {
			changed = true
		}
		req.ConfigurationUpdatesChanged = req.ConfigurationUpdatesChanged || changed
		return req
	}
	if fn != nil {
		req.Body, req.Err = prepareResponsesPatchRequest(from, to, req.Body)
		if req.Err != nil {
			req.Body = nil
			return req
		}
		summaryConfig := thinking.ExtractTranslatedSummaryConfig(req.Body, from.String(), to.String())
		req = fn(ctx, req)
		if req.Err != nil {
			req.Body = nil
			return req
		}
		req.Body = thinking.ApplySummaryConfigForModel(req.Body, to.String(), req.Model, summaryConfig)
		if hooks != nil {
			req = normalizeRequest(req)
		}
		req.Format = to
		return req
	}

	if hooks == nil {
		req.Format = to
		return req
	}

	req = normalizeRequest(req)
	summaryConfig := thinking.ExtractTranslatedSummaryConfig(req.Body, from.String(), to.String())
	if translated, ok := hooks.TranslateRequest(ctx, from, to, req.Model, req.Body, req.Stream); ok {
		req.Body = thinking.ApplySummaryConfigForModel(translated, to.String(), req.Model, summaryConfig)
	}
	req.Format = to
	return req
}

func configurationUpdates(body []byte) []string {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return nil
	}
	var updates []string
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "configuration_update" {
			updates = append(updates, item.Raw)
		}
		return true
	})
	return updates
}

// HasRequestTransformer indicates whether a request translator exists.
func (r *Registry) HasRequestTransformer(from, to Format) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if byTarget, ok := r.requests[from]; ok {
		if fn, isOk := byTarget[to]; isOk && fn != nil {
			return true
		}
	}
	return false
}

// HasResponseTransformer indicates whether a response translator exists.
func (r *Registry) HasResponseTransformer(from, to Format) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if byTarget, ok := r.responses[from]; ok {
		if fn, isOk := byTarget[to]; isOk {
			return fn.Stream != nil || fn.NonStream != nil || fn.TokenCount != nil
		}
	}
	return false
}

// HasStreamResponseTransformer indicates whether a streaming response translator exists.
func (r *Registry) HasStreamResponseTransformer(from, to Format) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if byTarget, ok := r.responses[from]; ok {
		if fn, isOk := byTarget[to]; isOk && fn.Stream != nil {
			return true
		}
	}
	return false
}

// HasNonStreamResponseTransformer indicates whether a non-streaming response translator exists.
func (r *Registry) HasNonStreamResponseTransformer(from, to Format) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if byTarget, ok := r.responses[from]; ok {
		if fn, isOk := byTarget[to]; isOk && fn.NonStream != nil {
			return true
		}
	}
	return false
}

// TranslateStream applies the registered streaming response translator.
func (r *Registry) TranslateStream(
	ctx context.Context,
	from, to Format,
	model string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) [][]byte {
	if ctx == nil {
		ctx = context.Background()
	}
	if param != nil {
		if state, ok := (*param).(*responsesPatchTranslation); ok && state.err != nil {
			return state.stream(nil)
		}
	}
	body := rawJSON
	hooks := pluginHooksForContext(ctx)
	if hooks != nil {
		body = hooks.NormalizeResponseBefore(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, body, true)
		if translated, ok := hooks.TranslateResponse(
			ctx, from, to, model, originalRequestRawJSON, requestRawJSON, body, true,
		); ok {
			return [][]byte{
				hooks.NormalizeResponseAfter(
					ctx, from, to, model, originalRequestRawJSON, requestRawJSON, translated, true,
				),
			}
		}
	}

	r.mu.RLock()
	var frames [][]byte
	var patch *responsesPatchTranslation
	translated := false
	if byTarget, ok := r.responses[to]; ok {
		if fn, isOk := byTarget[from]; isOk && fn.Stream != nil {
			var original []byte
			var inner *any
			patch, original, inner = prepareResponsesPatchResponse(from, to, originalRequestRawJSON, param)
			patch.validateStreamEnd(from, body)
			if patch == nil || patch.err == nil {
				frames = fn.Stream(ctx, model, original, requestRawJSON, body, inner)
			}
			translated = true
		}
	}
	r.mu.RUnlock()
	if !translated {
		frames = [][]byte{body}
	}
	frames = patch.stream(frames)
	if patch != nil && patch.err != nil {
		return frames
	}
	if hooks != nil {
		for i, frame := range frames {
			frames[i] = hooks.NormalizeResponseAfter(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, frame, true)
		}
	}
	return frames
}

// TranslateNonStream applies the registered non-stream response translator.
func (r *Registry) TranslateNonStream(
	ctx context.Context,
	from, to Format,
	model string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) []byte {
	if ctx == nil {
		ctx = context.Background()
	}
	if param != nil {
		if state, ok := (*param).(*responsesPatchTranslation); ok && state.err != nil {
			return state.nonStream(nil)
		}
	}
	body := rawJSON
	hooks := pluginHooksForContext(ctx)
	if hooks != nil {
		body = hooks.NormalizeResponseBefore(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, body, false)
		if translated, ok := hooks.TranslateResponse(
			ctx, from, to, model, originalRequestRawJSON, requestRawJSON, body, false,
		); ok {
			return hooks.NormalizeResponseAfter(
				ctx, from, to, model, originalRequestRawJSON, requestRawJSON, translated, false,
			)
		}
	}

	r.mu.RLock()
	var patch *responsesPatchTranslation
	translated := false
	if byTarget, ok := r.responses[to]; ok {
		if fn, isOk := byTarget[from]; isOk && fn.NonStream != nil {
			var original []byte
			var inner *any
			patch, original, inner = prepareResponsesPatchResponse(from, to, originalRequestRawJSON, param)
			if patch == nil || patch.err == nil {
				body = fn.NonStream(ctx, model, original, requestRawJSON, body, inner)
				translated = true
			}
		}
	}
	r.mu.RUnlock()
	if translated && body == nil {
		if patch != nil {
			errTranslate := ResponseTranslationError(&patch.inner)
			failed := patch.nonStream(nil)
			if errTranslate != nil {
				return failed
			}
		}
		return nil
	}
	body = patch.nonStream(body)
	if patch != nil && patch.err != nil {
		return body
	}
	if hooks != nil {
		body = hooks.NormalizeResponseAfter(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, body, false)
	}
	return body
}

// TranslateTokenCount applies the registered token count response translator.
func (r *Registry) TranslateTokenCount(ctx context.Context, from, to Format, count int64, rawJSON []byte) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if byTarget, ok := r.responses[to]; ok {
		if fn, isOk := byTarget[from]; isOk && fn.TokenCount != nil {
			return fn.TokenCount(ctx, count)
		}
	}
	return rawJSON
}

var defaultRegistry = NewRegistry()

// Default exposes the package-level registry for shared use.
func Default() *Registry {
	return defaultRegistry
}

// Register attaches transforms to the default registry.
func Register(from, to Format, request RequestTransform, response ResponseTransform) {
	defaultRegistry.Register(from, to, request, response)
}

// RegisterCheckedRequest attaches a request converter with an explicit failure result.
func RegisterCheckedRequest(from, to Format, request CheckedRequestTransform) {
	defaultRegistry.RegisterCheckedRequest(from, to, request)
}

// RegisterRequestEnvelope 在默认注册表中注册完整请求转换器
func RegisterRequestEnvelope(from, to Format, request RequestEnvelopeTransform) {
	defaultRegistry.RegisterRequestEnvelope(from, to, request)
}

// TranslateRequestChecked returns either the translated body or a request-scoped failure.
func TranslateRequestChecked(
	ctx context.Context,
	from, to Format,
	model string,
	rawJSON []byte,
	stream bool,
) ([]byte, error) {
	return defaultRegistry.TranslateRequestChecked(ctx, from, to, model, rawJSON, stream)
}

// TranslateRequestEnvelope is a helper on the default registry.
func TranslateRequestEnvelope(ctx context.Context, from, to Format, req RequestEnvelope) RequestEnvelope {
	return defaultRegistry.TranslateRequestEnvelope(ctx, from, to, req)
}

// TranslateRequest is a helper on the default registry.
func TranslateRequest(from, to Format, model string, rawJSON []byte, stream bool) []byte {
	return defaultRegistry.TranslateRequest(from, to, model, rawJSON, stream)
}

// TranslateRequestWithContext is a context-aware helper on the default registry.
func TranslateRequestWithContext(
	ctx context.Context,
	from, to Format,
	model string,
	rawJSON []byte,
	stream bool,
) []byte {
	return defaultRegistry.TranslateRequestWithContext(ctx, from, to, model, rawJSON, stream)
}

// HasRequestTransformer inspects the default registry.
func HasRequestTransformer(from, to Format) bool {
	return defaultRegistry.HasRequestTransformer(from, to)
}

// HasResponseTransformer inspects the default registry.
func HasResponseTransformer(from, to Format) bool {
	return defaultRegistry.HasResponseTransformer(from, to)
}

// HasStreamResponseTransformer inspects the default registry for stream translators.
func HasStreamResponseTransformer(from, to Format) bool {
	return defaultRegistry.HasStreamResponseTransformer(from, to)
}

// HasNonStreamResponseTransformer inspects the default registry for a non-streaming translator.
func HasNonStreamResponseTransformer(from, to Format) bool {
	return defaultRegistry.HasNonStreamResponseTransformer(from, to)
}

// TranslateStream is a helper on the default registry.
func TranslateStream(
	ctx context.Context,
	from, to Format,
	model string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) [][]byte {
	return defaultRegistry.TranslateStream(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, rawJSON, param)
}

// TranslateNonStream is a helper on the default registry.
func TranslateNonStream(
	ctx context.Context,
	from, to Format,
	model string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) []byte {
	return defaultRegistry.TranslateNonStream(
		ctx, from, to, model, originalRequestRawJSON, requestRawJSON, rawJSON, param,
	)
}

// TranslateTokenCount is a helper on the default registry.
func TranslateTokenCount(ctx context.Context, from, to Format, count int64, rawJSON []byte) []byte {
	return defaultRegistry.TranslateTokenCount(ctx, from, to, count, rawJSON)
}
