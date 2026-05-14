package util

import "context"

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
