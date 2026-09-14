package executor

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	claudeTokenCountingBeta      = "token-counting-2024-11-01"
	claudeFastModeBeta           = "fast-mode-2026-02-01"
	claudeOAuthBeta              = "oauth-2025-04-20"
	claudeCodeBeta               = "claude-code-20250219"
	claudeContext1MBeta          = "context-1m-2025-08-07"
	claudeMidConvSystemBeta      = "mid-conversation-system-2026-04-07"
	claudeAdvancedToolUseBeta    = "advanced-tool-use-2025-11-20"
	claudeEffortBeta             = "effort-2025-11-24"
	claudeServerSideFallbackBeta = "server-side-fallback-2026-06-01"
	claudeFallbackCreditBeta     = "fallback-credit-2026-06-01"
	claudeStructuredOutputsBeta  = "structured-outputs-2025-12-15"
	claudeExtendedCacheTTLBeta   = "extended-cache-ttl-2025-04-11"
	claudeCacheDiagnosisBeta     = "cache-diagnosis-2026-04-07"
	claudeRedactThinkingBeta     = "redact-thinking-2026-02-12"
)

var claudeCodeCLIConstantBetas = []string{
	"interleaved-thinking-2025-05-14",
	claudeRedactThinkingBeta,
	"thinking-token-count-2026-05-13",
	"context-management-2025-06-27",
	"prompt-caching-scope-2026-01-05",
}

var claudeCodeTrailingBetas = []string{
	claudeServerSideFallbackBeta,
	claudeFallbackCreditBeta,
	claudeStructuredOutputsBeta,
}

func claudeCodeCLIBetas(body []byte, requested map[string]bool, oauthToken bool) string {
	betas := make([]string, 0, len(claudeCodeCLIConstantBetas)+len(claudeCodeTrailingBetas)+7)
	betas = append(betas, claudeCodeBeta)
	if oauthToken {
		betas = append(betas, claudeOAuthBeta)
	}
	if requested[claudeContext1MBeta] {
		betas = append(betas, claudeContext1MBeta)
	}
	redactThinking := !claudeThinkingDisplaySet(body)
	for _, beta := range claudeCodeCLIConstantBetas {
		if beta == claudeRedactThinkingBeta && !redactThinking {
			continue
		}
		betas = append(betas, beta)
	}
	if !claudeUsesLegacySystemReminder(body) {
		betas = append(betas, claudeMidConvSystemBeta)
	}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() && len(tools.Array()) > 0 {
		betas = append(betas, claudeAdvancedToolUseBeta)
	}
	betas = append(betas, claudeEffortBeta)
	if oauthToken && !requested[claudeFallbackCreditBeta] {
		betas = append(betas, claudeFallbackCreditBeta)
	}
	for _, beta := range claudeCodeTrailingBetas {
		if requested[beta] {
			betas = append(betas, beta)
		}
	}
	if claudeRequestUsesFastMode(body, requested) {
		betas = append(betas, claudeFastModeBeta)
	}
	if oauthToken {
		betas = append(betas, claudeExtendedCacheTTLBeta)
	}
	if diagnostics := gjson.GetBytes(body, "diagnostics"); diagnostics.IsObject() {
		betas = append(betas, claudeCacheDiagnosisBeta)
	}
	return strings.Join(betas, ",")
}

func claudeThinkingDisplaySet(body []byte) bool {
	display := gjson.GetBytes(body, "thinking.display")
	return display.Type == gjson.String && strings.TrimSpace(display.String()) != ""
}

func claudeRequestUsesFastMode(body []byte, requested map[string]bool) bool {
	if requested[claudeFastModeBeta] {
		return true
	}
	speed := gjson.GetBytes(body, "speed")
	return speed.Type == gjson.String && strings.EqualFold(strings.TrimSpace(speed.String()), "fast")
}

var claudeCountTokensBetas = []string{
	claudeCodeBeta,
	"interleaved-thinking-2025-05-14",
	"context-management-2025-06-27",
	claudeTokenCountingBeta,
}

func claudeCountTokensBetasForCredential(oauthToken bool) string {
	betas := make([]string, 0, len(claudeCountTokensBetas)+1)
	betas = append(betas, claudeCodeBeta)
	if oauthToken {
		betas = append(betas, claudeOAuthBeta)
	}
	betas = append(betas, claudeCountTokensBetas[1:]...)
	return strings.Join(betas, ",")
}

func withClaudeCountTokensOAuthBeta(betas string) string {
	parts := make([]string, 0, len(claudeCountTokensBetas)+1)
	seen := make(map[string]bool)
	for beta := range strings.SplitSeq(betas, ",") {
		if beta = strings.TrimSpace(beta); beta != "" && !seen[beta] {
			parts = append(parts, beta)
			seen[beta] = true
		}
	}
	if seen[claudeOAuthBeta] {
		return strings.Join(parts, ",")
	}
	insertAt := 0
	if len(parts) > 0 && parts[0] == claudeCodeBeta {
		insertAt = 1
	}
	parts = append(parts[:insertAt], append([]string{claudeOAuthBeta}, parts[insertAt:]...)...)
	return strings.Join(parts, ",")
}

func withClaudeOAuthCredentialBetas(betas string) string {
	parts := make([]string, 0, 16)
	seen := make(map[string]bool)
	for beta := range strings.SplitSeq(betas, ",") {
		if beta = strings.TrimSpace(beta); beta != "" && !seen[beta] {
			parts = append(parts, beta)
			seen[beta] = true
		}
	}
	if !seen[claudeOAuthBeta] {
		insertAt := 0
		if len(parts) > 0 && parts[0] == claudeCodeBeta {
			insertAt = 1
		}
		parts = append(parts[:insertAt], append([]string{claudeOAuthBeta}, parts[insertAt:]...)...)
	}
	if !seen[claudeExtendedCacheTTLBeta] {
		parts = append(parts, claudeExtendedCacheTTLBeta)
	}
	return strings.Join(parts, ",")
}

type claudeEntitlementError struct {
	statusErr
}

func (claudeEntitlementError) IsRequestScoped() bool {
	return true
}

func classifyClaudeUpstreamError(statusCode int, body []byte, headers ...http.Header) error {
	err := statusErr{code: statusCode, msg: string(body)}
	if len(headers) > 0 {
		err = err.withUpstreamResponse(headers[0])
	}
	if statusCode == http.StatusTooManyRequests && claudeBodyIndicatesFastModeCredits(body) {
		return claudeEntitlementError{err}
	}
	return err
}

func claudeBodyIndicatesFastModeCredits(body []byte) bool {
	message := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if message == "" {
		message = strings.ToLower(string(body))
	}
	return strings.Contains(message, "fast mode") &&
		(strings.Contains(message, "usage credits") || strings.Contains(message, "credits are required"))
}

func claudeRequestedBetas(incomingBetas string, extraBetas []string) map[string]bool {
	requested := make(map[string]bool)
	for beta := range strings.SplitSeq(incomingBetas, ",") {
		if beta = strings.TrimSpace(beta); beta != "" {
			requested[beta] = true
		}
	}
	for _, beta := range extraBetas {
		if beta = strings.TrimSpace(beta); beta != "" {
			requested[beta] = true
		}
	}
	return requested
}
