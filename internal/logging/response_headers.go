package logging

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type endpointKey struct{}
type responseStatusKey struct{}
type responseHeadersKey struct{}
type responseTimingKey struct{}

type responseStatusHolder struct {
	status atomic.Int32
}

type responseHeadersHolder struct {
	mu      sync.RWMutex
	headers http.Header
}

type responseTimingHolder struct {
	mu                 sync.RWMutex
	firstResponseByte  time.Time
	firstResponseToken time.Time
}

func WithEndpoint(ctx context.Context, endpoint string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, endpointKey{}, endpoint)
}

func GetEndpoint(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	endpoint, _ := ctx.Value(endpointKey{}).(string)
	return endpoint
}

func WithResponseStatusHolder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder); ok && holder != nil {
		return ctx
	}
	return context.WithValue(ctx, responseStatusKey{}, &responseStatusHolder{})
}

// WithResponseHeadersHolder installs a fresh holder for one upstream attempt.
func WithResponseHeadersHolder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, responseHeadersKey{}, &responseHeadersHolder{})
}

func WithResponseTimingHolder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, ok := ctx.Value(responseTimingKey{}).(*responseTimingHolder); ok && holder != nil {
		return ctx
	}
	return context.WithValue(ctx, responseTimingKey{}, &responseTimingHolder{})
}

func SetResponseStatus(ctx context.Context, status int) {
	if ctx == nil || status <= 0 {
		return
	}
	holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder)
	if !ok || holder == nil {
		return
	}
	holder.status.Store(int32(status))
}

func SetResponseHeaders(ctx context.Context, headers http.Header) {
	if ctx == nil {
		return
	}
	holder, ok := ctx.Value(responseHeadersKey{}).(*responseHeadersHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	holder.headers = cloneResponseHeaders(headers)
}

func MarkFirstResponseByte(ctx context.Context) {
	if ctx == nil {
		return
	}
	holder, ok := ctx.Value(responseTimingKey{}).(*responseTimingHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if holder.firstResponseByte.IsZero() {
		holder.firstResponseByte = time.Now()
	}
}

func GetFirstResponseByteAt(ctx context.Context) time.Time {
	if ctx == nil {
		return time.Time{}
	}
	holder, ok := ctx.Value(responseTimingKey{}).(*responseTimingHolder)
	if !ok || holder == nil {
		return time.Time{}
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	return holder.firstResponseByte
}

func MarkFirstResponseToken(ctx context.Context) {
	if ctx == nil {
		return
	}
	holder, ok := ctx.Value(responseTimingKey{}).(*responseTimingHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if holder.firstResponseToken.IsZero() {
		holder.firstResponseToken = time.Now()
	}
}

func GetFirstResponseTokenAt(ctx context.Context) time.Time {
	if ctx == nil {
		return time.Time{}
	}
	holder, ok := ctx.Value(responseTimingKey{}).(*responseTimingHolder)
	if !ok || holder == nil {
		return time.Time{}
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	return holder.firstResponseToken
}

func GetResponseStatus(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder)
	if !ok || holder == nil {
		return 0
	}
	return int(holder.status.Load())
}

func GetResponseHeaders(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	holder, ok := ctx.Value(responseHeadersKey{}).(*responseHeadersHolder)
	if !ok || holder == nil {
		return nil
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	return cloneResponseHeaders(holder.headers)
}

func cloneResponseHeaders(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}
