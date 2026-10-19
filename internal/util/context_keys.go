package util

import (
	"context"
	"strings"
)

type sessionIDContextKey struct{}
type inferredSessionIDContextKey struct{}

// WithSessionID 保存内部会话标识，空值明确覆盖父 context 的标识
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, inferredSessionIDContextKey{}, false)
	return context.WithValue(ctx, sessionIDContextKey{}, strings.TrimSpace(sessionID))
}

// WithInferredSessionID 更新请求派生的会话，保留调用方显式指定的值与空值
func WithInferredSessionID(ctx context.Context, sessionID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	inferred, _ := ctx.Value(inferredSessionIDContextKey{}).(bool)
	if HasExplicitSessionID(ctx) && !inferred {
		return ctx
	}
	ctx = context.WithValue(ctx, inferredSessionIDContextKey{}, true)
	return context.WithValue(ctx, sessionIDContextKey{}, strings.TrimSpace(sessionID))
}

func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(sessionIDContextKey{}).(string)
	return value
}

func HasExplicitSessionID(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, exists := ctx.Value(sessionIDContextKey{}).(string)
	return exists
}

// altContextKey is the unexported key for storing the Gemini "alt" parameter in context.
type altContextKey struct{}

// WithAlt stores the alt parameter value in the context.
func WithAlt(ctx context.Context, alt string) context.Context {
	return context.WithValue(ctx, altContextKey{}, alt)
}

// AltValue retrieves the alt parameter from the context. Returns "" when the
// key is absent or holds a non-string value.
func AltValue(ctx context.Context) string {
	if v, ok := ctx.Value(altContextKey{}).(string); ok {
		return v
	}
	return ""
}

// ginContextKey is the unexported key for storing the *gin.Context in a standard context.
type ginContextKey struct{}

// WithGinContext stores the gin context in a standard context.
func WithGinContext(ctx context.Context, ginCtx any) context.Context {
	return context.WithValue(ctx, ginContextKey{}, ginCtx)
}

// GinContextValue retrieves the raw gin context value from a standard context.
// Callers should type-assert the result to *gin.Context.
func GinContextValue(ctx context.Context) any {
	return ctx.Value(ginContextKey{})
}

// roundTripperContextKey is the unexported key for storing an http.RoundTripper in context.
type roundTripperContextKey struct{}

// WithRoundTripper stores a round tripper in the context.
func WithRoundTripper(ctx context.Context, rt any) context.Context {
	return context.WithValue(ctx, roundTripperContextKey{}, rt)
}

// RoundTripperValue retrieves the raw round tripper value from the context.
// Callers should type-assert the result to http.RoundTripper.
func RoundTripperValue(ctx context.Context) any {
	return ctx.Value(roundTripperContextKey{})
}
