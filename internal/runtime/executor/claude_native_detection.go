package executor

import (
	"context"
	"net/http"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/gin-gonic/gin"
)

func resolveIncomingClaudeHeaders(ctx context.Context, incoming http.Header) http.Header {
	resolved := make(http.Header)
	if ginCtx, ok := util.GinContextValue(ctx).(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		resolved = ginCtx.Request.Header.Clone()
	}
	for key, values := range incoming {
		resolved[key] = append([]string(nil), values...)
	}
	return resolved
}

func detectIncomingClaudeCodeRequest(
	ctx context.Context,
	incoming http.Header,
	payload []byte,
	countTokens bool,
	cfg *config.Config,
) (http.Header, helps.ClaudeCodeRequestDetection) {
	resolved := resolveIncomingClaudeHeaders(ctx, incoming)
	return resolved, helps.DetectClaudeCodeRequest(resolved, payload, countTokens, cfg)
}
