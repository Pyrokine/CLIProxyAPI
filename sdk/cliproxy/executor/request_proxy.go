package executor

import (
	"context"
	"strings"
)

type requestProxyContextKey struct{}

// WithRequestProxyURL sets the proxy override for a single execution.
func WithRequestProxyURL(ctx context.Context, proxyURL string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestProxyContextKey{}, strings.TrimSpace(proxyURL))
}

// WithoutRequestProxyURL clears the request override during credential refresh.
func WithoutRequestProxyURL(ctx context.Context) context.Context {
	return WithRequestProxyURL(ctx, "")
}

// RequestProxyURL returns the execution-scoped override, or an empty string when absent.
func RequestProxyURL(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	proxyURL, _ := ctx.Value(requestProxyContextKey{}).(string)
	return strings.TrimSpace(proxyURL)
}
