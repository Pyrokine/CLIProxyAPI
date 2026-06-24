package translator

import (
	"context"
	"sync"
)

// Registry manages translation functions across schemas.
type Registry struct {
	mu        sync.RWMutex
	requests  map[Format]map[Format]RequestTransform
	responses map[Format]map[Format]ResponseTransform
}

// NewRegistry constructs an empty translator registry.
func NewRegistry() *Registry {
	return &Registry{
		requests:  make(map[Format]map[Format]RequestTransform),
		responses: make(map[Format]map[Format]ResponseTransform),
	}
}

// Register stores request/response transforms between two formats.
func (r *Registry) Register(from, to Format, request RequestTransform, response ResponseTransform) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.requests[from]; !ok {
		r.requests[from] = make(map[Format]RequestTransform)
	}
	if request != nil {
		r.requests[from][to] = request
	}

	if _, ok := r.responses[from]; !ok {
		r.responses[from] = make(map[Format]ResponseTransform)
	}
	r.responses[from][to] = response
}

// TranslateRequest converts a payload between schemas, returning the original payload
// if no translator is registered.
func (r *Registry) TranslateRequest(from, to Format, model string, rawJSON []byte, stream bool) []byte {
	return r.TranslateRequestWithContext(context.Background(), from, to, model, rawJSON, stream)
}

// TranslateRequestWithContext converts a payload and applies package-level plugin hooks.
func (r *Registry) TranslateRequestWithContext(
	ctx context.Context,
	from, to Format,
	model string,
	rawJSON []byte,
	stream bool,
) []byte {
	if ctx == nil {
		ctx = context.Background()
	}
	body := rawJSON
	if hooks := currentPluginHooks(); hooks != nil {
		body = hooks.NormalizeRequest(ctx, from, to, model, body, stream)
		if translated, ok := hooks.TranslateRequest(ctx, from, to, model, body, stream); ok {
			return translated
		}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	if byTarget, ok := r.requests[from]; ok {
		if fn, isOk := byTarget[to]; isOk && fn != nil {
			return fn(model, body, stream)
		}
	}
	return body
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
		if _, isOk := byTarget[to]; isOk {
			return true
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
	body := rawJSON
	hooks := currentPluginHooks()
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
	translated := false
	if byTarget, ok := r.responses[to]; ok {
		if fn, isOk := byTarget[from]; isOk && fn.Stream != nil {
			frames = fn.Stream(ctx, model, originalRequestRawJSON, requestRawJSON, body, param)
			translated = true
		}
	}
	r.mu.RUnlock()
	if !translated {
		frames = [][]byte{body}
	}
	if hooks == nil || len(frames) == 0 {
		return frames
	}
	out := make([][]byte, 0, len(frames))
	for _, frame := range frames {
		out = append(
			out, hooks.NormalizeResponseAfter(ctx, from, to, model, originalRequestRawJSON, requestRawJSON, frame, true),
		)
	}
	return out
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
	body := rawJSON
	hooks := currentPluginHooks()
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
	if byTarget, ok := r.responses[to]; ok {
		if fn, isOk := byTarget[from]; isOk && fn.NonStream != nil {
			body = fn.NonStream(ctx, model, originalRequestRawJSON, requestRawJSON, body, param)
		}
	}
	r.mu.RUnlock()
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

// TranslateRequest is a helper on the default registry.
func TranslateRequest(from, to Format, model string, rawJSON []byte, stream bool) []byte {
	return defaultRegistry.TranslateRequest(from, to, model, rawJSON, stream)
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
