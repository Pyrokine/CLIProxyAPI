package executor

import (
	"net/http"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const (
	claudeTokenCountingBeta          = "token-counting-2024-11-01"
	claudeFastModeBeta               = "fast-mode-2026-02-01"
	claudeOAuthBeta                  = "oauth-2025-04-20"
	claudeCodeBeta                   = "claude-code-20250219"
	claudeContext1MBeta              = "context-1m-2025-08-07"
	claudeMidConvSystemBeta          = "mid-conversation-system-2026-04-07"
	claudeAdvisorToolBeta            = "advisor-tool-2026-03-01"
	claudeAdvancedToolUseBeta        = "advanced-tool-use-2025-11-20"
	claudeEffortBeta                 = "effort-2025-11-24"
	claudeServerSideFallbackBeta     = "server-side-fallback-2026-06-01"
	claudeFallbackCreditBeta         = "fallback-credit-2026-06-01"
	claudeStructuredOutputsBeta      = "structured-outputs-2025-12-15"
	claudeThinkingDisplayUpdatesBeta = "thinking-display-updates-2026-08-18"
	claudeExtendedCacheTTLBeta       = "extended-cache-ttl-2025-04-11"
	claudeCacheDiagnosisBeta         = "cache-diagnosis-2026-04-07"
	claudeRedactThinkingBeta         = "redact-thinking-2026-02-12"
	claudeAFKModeBeta                = "afk-mode-2026-01-31"
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

var claudeManagedBetaSet = func() map[string]bool {
	managed := []string{
		claudeTokenCountingBeta,
		claudeFastModeBeta,
		claudeOAuthBeta,
		claudeCodeBeta,
		claudeContext1MBeta,
		claudeMidConvSystemBeta,
		claudeAdvisorToolBeta,
		claudeAdvancedToolUseBeta,
		claudeEffortBeta,
		claudeServerSideFallbackBeta,
		claudeFallbackCreditBeta,
		claudeStructuredOutputsBeta,
		claudeThinkingDisplayUpdatesBeta,
		claudeExtendedCacheTTLBeta,
		claudeCacheDiagnosisBeta,
		claudeRedactThinkingBeta,
		claudeAFKModeBeta,
	}
	managed = append(managed, claudeCodeCLIConstantBetas...)
	managed = append(managed, claudeCodeTrailingBetas...)
	set := make(map[string]bool, len(managed))
	for _, beta := range managed {
		set[beta] = true
	}
	return set
}()

func isManagedClaudeBeta(beta string) bool {
	return claudeManagedBetaSet[strings.TrimSpace(beta)]
}

func claudeCodeCLIBetas(body []byte, requested map[string]bool, oauthToken bool) string {
	betas := make([]string, 0, len(claudeCodeCLIConstantBetas)+len(claudeCodeTrailingBetas)+9)
	betas = append(betas, claudeCodeBeta)
	if oauthToken {
		betas = append(betas, claudeOAuthBeta)
	}
	if requested[claudeContext1MBeta] {
		betas = append(betas, claudeContext1MBeta)
	}
	for _, beta := range claudeCodeCLIConstantBetas {
		if beta == claudeRedactThinkingBeta && claudeThinkingDisplaySet(body) {
			continue
		}
		betas = append(betas, beta)
	}
	if !claudeUsesLegacySystemReminder(body) {
		betas = append(betas, claudeMidConvSystemBeta)
	}
	if requested[claudeAdvisorToolBeta] || claudeBodyHasAdvisorTool(body) {
		betas = append(betas, claudeAdvisorToolBeta)
	}
	if requested[claudeAdvancedToolUseBeta] || claudeBodyUsesAdvancedToolUse(body) {
		betas = append(betas, claudeAdvancedToolUseBeta)
	}
	if claudeRequestSupportsEffort(body, requested) {
		betas = append(betas, claudeEffortBeta)
	}
	probeOrHelper := helps.IsClaudeProbeOrHelperRequest(body)
	if !probeOrHelper && (requested[claudeServerSideFallbackBeta] || gjson.GetBytes(body, "fallbacks").Exists()) {
		betas = append(betas, claudeServerSideFallbackBeta)
	}
	if requested[claudeFallbackCreditBeta] || oauthToken {
		betas = append(betas, claudeFallbackCreditBeta)
	}
	if requested[claudeStructuredOutputsBeta] {
		betas = append(betas, claudeStructuredOutputsBeta)
	}
	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	if !probeOrHelper && thinkingType != "disabled" &&
		(requested[claudeThinkingDisplayUpdatesBeta] || claudeThinkingDisplayUpdates(body)) {
		betas = append(betas, claudeThinkingDisplayUpdatesBeta)
	}
	if claudeRequestUsesFastMode(body, requested) {
		betas = append(betas, claudeFastModeBeta)
	}
	if requested[claudeAFKModeBeta] {
		betas = append(betas, claudeAFKModeBeta)
	}
	if !probeOrHelper {
		includeExtended := (oauthToken && !helps.IsClaudeSubagentRequest(nil, body)) ||
			requested[claudeExtendedCacheTTLBeta] || helps.ClaudePayloadHas1hTTL(body)
		if includeExtended {
			betas = append(betas, claudeExtendedCacheTTLBeta)
		}
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

func claudeThinkingDisplayUpdates(body []byte) bool {
	display := gjson.GetBytes(body, "thinking.display")
	return display.Type == gjson.String && strings.EqualFold(strings.TrimSpace(display.String()), "updates")
}

func isClaudeHaikuModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "haiku")
}

func claudeRequestSupportsEffort(body []byte, requested map[string]bool) bool {
	if len(body) > 0 {
		if helps.IsClaudeProbeOrHelperRequest(body) {
			return false
		}
		if isClaudeHaikuModel(gjson.GetBytes(body, "model").String()) {
			return false
		}
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()), "disabled") {
			return false
		}
	}
	return true
}

func claudeBodyUsesAdvancedToolUse(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		toolType := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
		if strings.HasPrefix(toolType, "tool_search_tool_") ||
			tool.Get("defer_loading").Bool() ||
			tool.Get("input_examples").Exists() ||
			tool.Get("allowed_callers").Exists() {
			return true
		}
	}
	return false
}

func claudeBodyHasAdvisorTool(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(tool.Get("type").String())), "advisor_") {
			return true
		}
	}
	return false
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

func withClaudeOAuthCredentialBetas(betas string, includeExtendedCacheTTL bool) string {
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
	if includeExtendedCacheTTL && !seen[claudeExtendedCacheTTLBeta] {
		parts = append(parts, claudeExtendedCacheTTLBeta)
	}
	return strings.Join(parts, ",")
}

func withoutClaudeBeta(betas, removeBeta string) string {
	parts := make([]string, 0, 16)
	for beta := range strings.SplitSeq(betas, ",") {
		if beta = strings.TrimSpace(beta); beta != "" && beta != removeBeta {
			parts = append(parts, beta)
		}
	}
	return strings.Join(parts, ",")
}

func withClaudeExtendedCacheTTLBeta(betas string) string {
	parts := make([]string, 0, 16)
	seen := make(map[string]bool)
	for beta := range strings.SplitSeq(betas, ",") {
		if beta = strings.TrimSpace(beta); beta != "" && !seen[beta] {
			parts = append(parts, beta)
			seen[beta] = true
		}
	}
	if !seen[claudeExtendedCacheTTLBeta] {
		parts = append(parts, claudeExtendedCacheTTLBeta)
	}
	return strings.Join(parts, ",")
}

func withClaudeAdvisorToolBeta(betas string) string {
	if strings.TrimSpace(betas) == "" {
		return claudeAdvisorToolBeta
	}
	parts := make([]string, 0, 16)
	seen := make(map[string]bool)
	for beta := range strings.SplitSeq(betas, ",") {
		if beta = strings.TrimSpace(beta); beta != "" && beta != claudeAdvisorToolBeta && !seen[beta] {
			parts = append(parts, beta)
			seen[beta] = true
		}
	}
	insertAt := len(parts)
	for index, beta := range parts {
		switch beta {
		case claudeAdvancedToolUseBeta,
			claudeEffortBeta,
			claudeServerSideFallbackBeta,
			claudeFallbackCreditBeta,
			claudeStructuredOutputsBeta,
			claudeFastModeBeta,
			claudeAFKModeBeta,
			claudeExtendedCacheTTLBeta,
			claudeCacheDiagnosisBeta:
			insertAt = index
		}
		if insertAt != len(parts) {
			break
		}
	}
	parts = append(parts[:insertAt], append([]string{claudeAdvisorToolBeta}, parts[insertAt:]...)...)
	return strings.Join(parts, ",")
}

type claudeEntitlementError struct {
	statusErr
}

func (claudeEntitlementError) IsRequestScoped() bool {
	return true
}

func (claudeEntitlementError) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	return cliproxyexecutor.ExecutionDisposition{
		DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
		Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
		EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
	}
}

type claudeRateLimitError struct {
	statusErr
	effectScope cliproxyexecutor.StateEffectScope
}

func (e *claudeRateLimitError) IsRequestScoped() bool {
	return false
}

func (e *claudeRateLimitError) IsCredentialScoped() bool {
	return e != nil && e.effectScope == cliproxyexecutor.StateEffectScopeCredential
}

func (e *claudeRateLimitError) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	if e == nil {
		return cliproxyexecutor.ExecutionDisposition{}
	}
	return cliproxyexecutor.ExecutionDisposition{
		DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
		Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: cliproxyexecutor.RetryDirectiveRetryAfter,
		EffectScope:    e.effectScope,
	}
}

func classifyClaudeUpstreamError(statusCode int, body []byte, headers ...http.Header) error {
	err := statusErr{code: statusCode, msg: string(body)}
	var responseHeaders http.Header
	if len(headers) > 0 {
		responseHeaders = headers[0]
		err = err.withUpstreamResponse(responseHeaders)
		if retryAfter := helps.ParseClaudeRateLimitReset(responseHeaders, time.Now()); retryAfter != nil {
			err.retryAfter = retryAfter
		}
	}
	if statusCode != http.StatusTooManyRequests {
		return err
	}
	if helps.ClaudeHeadersIndicateUnifiedRateLimitRejection(responseHeaders) {
		return &claudeRateLimitError{
			statusErr:   err,
			effectScope: cliproxyexecutor.StateEffectScopeCredential,
		}
	}
	if claudeBodyIndicatesFastModeCredits(body) {
		return claudeEntitlementError{err}
	}
	return &claudeRateLimitError{
		statusErr:   err,
		effectScope: cliproxyexecutor.StateEffectScopeExactModel,
	}
}

func claudeBodyIndicatesFastModeCredits(body []byte) bool {
	message := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if message == "" {
		message = strings.ToLower(string(body))
	}
	return strings.Contains(message, "fast request rejected") ||
		(strings.Contains(message, "fast") &&
			(strings.Contains(message, "usage credits") || strings.Contains(message, "credits are required")))
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
