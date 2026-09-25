package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	xaiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	claudeUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	claudeProfileURL = "https://api.anthropic.com/api/oauth/profile"

	codexUsageURL                 = "https://chatgpt.com/backend-api/wham/usage"
	codexRateLimitResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

	geminiQuotaURL      = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"
	geminiCodeAssistURL = "https://cloudcode-pa.googleapis.com/v1internal:getCodeAssistSubscription"

	antigravityLoadCodeAssistURL = "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"

	kimiUsageURL         = "https://api.kimi.com/coding/v1/usages"
	xaiBillingURL        = "https://cli-chat-proxy.grok.com/v1/billing"
	xaiBillingCreditsURL = xaiBillingURL + "?format=credits"
	xaiSubscriptionURL   = "https://cli-chat-proxy.grok.com/v1/user?include=subscription"
)

var claudeHeaders = map[string]string{
	"Authorization":  "Bearer $TOKEN$",
	"Content-Type":   "application/json",
	"anthropic-beta": "oauth-2025-04-20",
}

var codexHeaders = map[string]string{
	"Authorization": "Bearer $TOKEN$",
	"Content-Type":  "application/json",
	"User-Agent":    "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)",
}

var codexResetCreditsHeaders = map[string]string{
	"Authorization": "Bearer $TOKEN$",
	"Accept":        "application/json",
	"OpenAI-Beta":   "codex-1",
	"Originator":    "Codex Desktop",
	"User-Agent":    "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)",
}

var geminiHeaders = map[string]string{
	"Authorization": "Bearer $TOKEN$",
	"Content-Type":  "application/json",
}

func antigravityHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Content-Type":  "application/json",
		"User-Agent":    misc.AntigravityUserAgent(),
	}
}

var kimiHeaders = map[string]string{
	"Authorization": "Bearer $TOKEN$",
	"Content-Type":  "application/json",
}

func xaiQuotaHeaders() map[string]string {
	headers := xaiauth.ShellIdentityHeaderValues()
	headers["Authorization"] = "Bearer $TOKEN$"
	headers["Accept"] = "application/json"
	return headers
}

func backgroundQuotaAPICall(apiCall quota.InternalAPICallFunc) quota.InternalAPICallContextFunc {
	return func(_ context.Context, req quota.APICallRequest) (*quota.APICallResponse, error) {
		return apiCall(req)
	}
}

func fetchClaudeQuotaResultContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) (quota.FetchResult, error) {
	usageResp, err := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       claudeUsageURL,
			Method:    "GET",
			Header:    claudeHeaders,
			AuthIndex: authIndex,
		},
	)
	observedAt := time.Now().UTC()
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("claude usage request failed: %w", err)
	}
	if usageResp == nil {
		return quota.FetchResult{}, fmt.Errorf("claude usage returned empty response")
	}
	if usageResp.StatusCode < 200 || usageResp.StatusCode >= 300 {
		return quota.FetchResult{}, fmt.Errorf("claude usage returned status %d", usageResp.StatusCode)
	}

	profileResp, profileErr := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       claudeProfileURL,
			Method:    "GET",
			Header:    claudeHeaders,
			AuthIndex: authIndex,
		},
	)

	payload := map[string]any{
		"usage": usageResp.Body,
	}
	partialError := ""
	if profileErr != nil {
		partialError = "claude profile request failed"
	} else if profileResp == nil {
		partialError = "claude profile returned empty response"
	} else if profileResp.StatusCode < 200 || profileResp.StatusCode >= 300 {
		partialError = fmt.Sprintf("claude profile returned status %d", profileResp.StatusCode)
	} else {
		payload["profile"] = profileResp.Body
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return quota.FetchResult{}, err
	}
	return newQuotaFetchResult(data, "claude_usage", observedAt, partialError), nil
}

// fetchCodexQuota queries Codex/Copilot quota via api-call proxy.
func fetchCodexQuota(apiCall quota.InternalAPICallFunc, authIndex string) ([]byte, error) {
	return fetchCodexQuotaContext(context.Background(), backgroundQuotaAPICall(apiCall), authIndex)
}

func fetchCodexQuotaContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) ([]byte, error) {
	result, err := fetchCodexQuotaResultContext(ctx, apiCall, authIndex)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func fetchCodexQuotaResultContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) (quota.FetchResult, error) {
	resp, err := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       codexUsageURL,
			Method:    "GET",
			Header:    codexHeaders,
			AuthIndex: authIndex,
		},
	)
	observedAt := time.Now().UTC()
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("codex usage request failed: %w", err)
	}
	if resp == nil {
		return quota.FetchResult{}, fmt.Errorf("codex usage returned empty response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return quota.FetchResult{}, fmt.Errorf("codex usage returned status %d", resp.StatusCode)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return newQuotaFetchResult(resp.Body, "codex_usage", observedAt, ""), nil
	}
	if payload == nil {
		return quota.FetchResult{}, errors.New("codex usage returned null payload")
	}

	partialError := ""
	resetCreditsResp, resetCreditsErr := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       codexRateLimitResetCreditsURL,
			Method:    "GET",
			Header:    codexResetCreditsHeaders,
			AuthIndex: authIndex,
		},
	)
	if resetCreditsErr != nil {
		partialError = "codex reset credits request failed"
		encoded, _ := json.Marshal(resetCreditsErr.Error())
		payload["rate_limit_reset_credits_error"] = encoded
	} else if resetCreditsResp == nil {
		partialError = "codex reset credits returned empty response"
		encoded, _ := json.Marshal(partialError)
		payload["rate_limit_reset_credits_error"] = encoded
	} else if resetCreditsResp.StatusCode < 200 || resetCreditsResp.StatusCode >= 300 {
		partialError = fmt.Sprintf("codex reset credits returned status %d", resetCreditsResp.StatusCode)
		encoded, _ := json.Marshal(partialError)
		payload["rate_limit_reset_credits_error"] = encoded
	} else {
		payload["rate_limit_reset_credits_details"] = resetCreditsResp.Body
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return quota.FetchResult{}, err
	}
	return newQuotaFetchResult(data, "codex_usage", observedAt, partialError), nil
}

func fetchGeminiQuotaResultContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) (quota.FetchResult, error) {
	quotaResp, err := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       geminiQuotaURL,
			Method:    "POST",
			Header:    geminiHeaders,
			AuthIndex: authIndex,
			Data:      "{}",
		},
	)
	observedAt := time.Now().UTC()
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("gemini quota request failed: %w", err)
	}
	if quotaResp == nil {
		return quota.FetchResult{}, fmt.Errorf("gemini quota returned empty response")
	}
	if quotaResp.StatusCode < 200 || quotaResp.StatusCode >= 300 {
		return quota.FetchResult{}, fmt.Errorf("gemini quota returned status %d", quotaResp.StatusCode)
	}

	codeAssistResp, codeAssistErr := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       geminiCodeAssistURL,
			Method:    "POST",
			Header:    geminiHeaders,
			AuthIndex: authIndex,
			Data:      "{}",
		},
	)

	payload := map[string]any{
		"quota": quotaResp.Body,
	}
	partialError := ""
	if codeAssistErr != nil {
		partialError = "gemini code assist request failed"
	} else if codeAssistResp == nil {
		partialError = "gemini code assist returned empty response"
	} else if codeAssistResp.StatusCode < 200 || codeAssistResp.StatusCode >= 300 {
		partialError = fmt.Sprintf("gemini code assist returned status %d", codeAssistResp.StatusCode)
	} else {
		payload["codeAssist"] = codeAssistResp.Body
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return quota.FetchResult{}, err
	}
	return newQuotaFetchResult(data, "gemini_quota", observedAt, partialError), nil
}

func responseServerTimeOffsetMilliseconds(header map[string][]string, observedAt time.Time) (int64, bool) {
	for name, values := range header {
		if !strings.EqualFold(strings.TrimSpace(name), "Date") {
			continue
		}
		for _, value := range values {
			serverTime, err := http.ParseTime(strings.TrimSpace(value))
			if err == nil {
				return serverTime.Sub(observedAt).Milliseconds(), true
			}
		}
	}
	return 0, false
}

// fetchAntigravityQuota queries Antigravity quota and subscription tier via api-call proxy.
func fetchAntigravityQuota(apiCall quota.InternalAPICallFunc, authIndex string) ([]byte, error) {
	return fetchAntigravityQuotaContext(context.Background(), backgroundQuotaAPICall(apiCall), authIndex)
}

func fetchAntigravityQuotaContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) ([]byte, error) {
	result, err := fetchAntigravityQuotaResultContext(ctx, apiCall, authIndex)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func fetchAntigravityQuotaResultContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) (quota.FetchResult, error) {
	quotaResp, err := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       geminiQuotaURL,
			Method:    "POST",
			Header:    antigravityHeaders(),
			AuthIndex: authIndex,
			Data:      "{}",
		},
	)
	observedAt := time.Now().UTC()
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("antigravity quota request failed: %w", err)
	}
	if quotaResp == nil {
		return quota.FetchResult{}, fmt.Errorf("antigravity quota returned empty response")
	}
	if quotaResp.StatusCode < 200 || quotaResp.StatusCode >= 300 {
		return quota.FetchResult{}, fmt.Errorf("antigravity quota returned status %d", quotaResp.StatusCode)
	}
	serverTimeOffsetMillis, hasServerTimeOffset := responseServerTimeOffsetMilliseconds(quotaResp.Header, observedAt)

	codeAssistResp, codeAssistErr := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       antigravityLoadCodeAssistURL,
			Method:    "POST",
			Header:    antigravityHeaders(),
			AuthIndex: authIndex,
			Data:      `{"metadata":{"ideType":"ANTIGRAVITY"}}`,
		},
	)

	payload := map[string]any{
		"quota": quotaResp.Body,
	}
	if hasServerTimeOffset {
		payload["server_time_offset_ms"] = serverTimeOffsetMillis
	}
	partialError := ""
	if codeAssistErr != nil {
		partialError = "antigravity code assist request failed"
	} else if codeAssistResp == nil {
		partialError = "antigravity code assist returned empty response"
	} else if codeAssistResp.StatusCode < 200 || codeAssistResp.StatusCode >= 300 {
		partialError = fmt.Sprintf("antigravity code assist returned status %d", codeAssistResp.StatusCode)
	} else {
		payload["codeAssist"] = codeAssistResp.Body
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return quota.FetchResult{}, err
	}
	return newQuotaFetchResult(data, "antigravity_quota", observedAt, partialError), nil
}

func fetchKimiQuotaResultContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) (quota.FetchResult, error) {
	resp, err := apiCall(
		ctx,
		quota.APICallRequest{
			URL:       kimiUsageURL,
			Method:    "GET",
			Header:    kimiHeaders,
			AuthIndex: authIndex,
		},
	)
	observedAt := time.Now().UTC()
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("kimi usage request failed: %w", err)
	}
	if resp == nil {
		return quota.FetchResult{}, fmt.Errorf("kimi usage returned empty response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return quota.FetchResult{}, fmt.Errorf("kimi usage returned status %d", resp.StatusCode)
	}
	return newQuotaFetchResult(resp.Body, "kimi_usage", observedAt, ""), nil
}

func fetchDevinQuotaResultContext(
	ctx context.Context,
	cfg *config.Config,
	authManager *coreauth.Manager,
	authIndex string,
) (quota.FetchResult, error) {
	auth := quotaAuthByIndex(authManager, authIndex)
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "devin") {
		return quota.FetchResult{}, fmt.Errorf("devin credential %q is unavailable", authIndex)
	}
	token, baseURL, deviceSeed := devinauth.CredentialsFromAuth(auth)
	if token == "" {
		return quota.FetchResult{}, fmt.Errorf("devin credential %q is missing a session token", authIndex)
	}
	client := executor.NewProxyAwareHTTPClient(ctx, cfg, auth, 30*time.Second)
	service := devinauth.NewDevinAuthService(client)
	service.SetServerBaseURL(baseURL)
	status, err := service.FetchUserStatus(ctx, token, deviceSeed)
	observedAt := time.Now().UTC()
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("devin user status request failed: %w", err)
	}
	data, err := json.Marshal(status)
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("encode Devin user status: %w", err)
	}
	return newQuotaFetchResult(data, "devin_user_status", observedAt, ""), nil
}

func quotaAuthByIndex(authManager *coreauth.Manager, authIndex string) *coreauth.Auth {
	if authManager == nil || strings.TrimSpace(authIndex) == "" {
		return nil
	}
	for _, auth := range authManager.List() {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		if auth.Index == authIndex {
			return auth
		}
	}
	return nil
}

func fetchXAIQuota(apiCall quota.InternalAPICallFunc, authIndex string) ([]byte, error) {
	return fetchXAIQuotaContext(context.Background(), backgroundQuotaAPICall(apiCall), authIndex)
}

func fetchXAIQuotaContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) ([]byte, error) {
	result, err := fetchXAIQuotaResultContext(ctx, apiCall, authIndex)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func fetchXAIQuotaResultContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) (quota.FetchResult, error) {
	billingResp, billingFormat, err := fetchXAIBillingContext(ctx, apiCall, authIndex)
	observedAt := time.Now().UTC()
	if err != nil {
		return quota.FetchResult{}, err
	}
	payload, err := normalizeXAIBilling(billingResp.Body, billingFormat, observedAt)
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("xai billing payload is invalid: %w", err)
	}

	partialError := ""
	subscriptionResp, subscriptionErr := apiCall(
		ctx, quota.APICallRequest{
			URL:       xaiSubscriptionURL,
			Method:    http.MethodGet,
			Header:    xaiQuotaHeaders(),
			AuthIndex: authIndex,
		},
	)
	switch {
	case subscriptionErr != nil:
		partialError = "xai subscription request failed"
		payload.addAuxiliaryError("subscription", "request_failed", 0)
	case subscriptionResp == nil:
		partialError = "xai subscription returned empty response"
		payload.addAuxiliaryError("subscription", "empty_response", 0)
	case subscriptionResp.StatusCode < http.StatusOK || subscriptionResp.StatusCode >= http.StatusMultipleChoices:
		partialError = fmt.Sprintf("xai subscription returned status %d", subscriptionResp.StatusCode)
		payload.addAuxiliaryError(
			"subscription",
			fmt.Sprintf("status_%d", subscriptionResp.StatusCode),
			subscriptionResp.StatusCode,
		)
	case payload.applySubscription(subscriptionResp.Body) != nil:
		partialError = "xai subscription payload is invalid"
		payload.addAuxiliaryError("subscription", "invalid_payload", subscriptionResp.StatusCode)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return quota.FetchResult{}, fmt.Errorf("marshal xai billing payload: %w", err)
	}
	return newQuotaFetchResult(encoded, "xai_billing", observedAt, partialError), nil
}

func fetchXAIBilling(apiCall quota.InternalAPICallFunc, authIndex string) (*quota.APICallResponse, string, error) {
	return fetchXAIBillingContext(context.Background(), backgroundQuotaAPICall(apiCall), authIndex)
}

func fetchXAIBillingContext(
	ctx context.Context,
	apiCall quota.InternalAPICallContextFunc,
	authIndex string,
) (*quota.APICallResponse, string, error) {
	resp, err := apiCall(
		ctx, quota.APICallRequest{
			URL:       xaiBillingCreditsURL,
			Method:    http.MethodGet,
			Header:    xaiQuotaHeaders(),
			AuthIndex: authIndex,
		},
	)
	if err != nil {
		return nil, "", fmt.Errorf("xai billing credits request failed: %w", err)
	}
	if resp == nil {
		return nil, "", fmt.Errorf("xai billing credits returned empty response")
	}
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return resp, "credits", nil
	}
	if !xaiCreditsFormatUnsupported(resp) {
		return nil, "", fmt.Errorf("xai billing credits returned status %d", resp.StatusCode)
	}

	resp, err = apiCall(
		ctx, quota.APICallRequest{
			URL:       xaiBillingURL,
			Method:    http.MethodGet,
			Header:    xaiQuotaHeaders(),
			AuthIndex: authIndex,
		},
	)
	if err != nil {
		return nil, "", fmt.Errorf("xai billing fallback request failed: %w", err)
	}
	if resp == nil {
		return nil, "", fmt.Errorf("xai billing fallback returned empty response")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, "", fmt.Errorf("xai billing fallback returned status %d", resp.StatusCode)
	}
	return resp, "legacy", nil
}

func xaiCreditsFormatUnsupported(resp *quota.APICallResponse) bool {
	if resp == nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusNotAcceptable, http.StatusUnsupportedMediaType, http.StatusNotImplemented:
		return true
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		message := strings.ToLower(strings.TrimSpace(string(resp.Body) + " " + resp.BodyText))
		if !strings.Contains(message, "format") || !strings.Contains(message, "credit") {
			return false
		}
		for _, marker := range []string{"unsupported", "not supported", "invalid", "unknown", "unrecognized"} {
			if strings.Contains(message, marker) {
				return true
			}
		}
	}
	return false
}

func newQuotaFetchResult(
	data []byte,
	source string,
	observedAt time.Time,
	partialError string,
) quota.FetchResult {
	completeness := quota.FetchComplete
	if strings.TrimSpace(partialError) != "" {
		completeness = quota.FetchPartial
	}
	return quota.FetchResult{
		Data:         append([]byte(nil), data...),
		Source:       source,
		ObservedAt:   observedAt,
		Completeness: completeness,
		PartialError: partialError,
	}
}

type authQuotaSchedulingProvider struct {
	scheduler *quota.Scheduler
	now       func() time.Time
}

func (p authQuotaSchedulingProvider) Snapshots(
	provider,
	model string,
	authIndexes []string,
) map[string]coreauth.QuotaSchedulingSnapshot {
	if p.scheduler == nil {
		return nil
	}
	credType, ok := quotaCredentialTypeForProvider(provider)
	if !ok {
		return nil
	}
	now := time.Now()
	if p.now != nil {
		now = p.now()
	}
	snapshots := p.scheduler.SchedulingSnapshots(credType, authIndexes, model, now)
	result := make(map[string]coreauth.QuotaSchedulingSnapshot, len(snapshots))
	for authIndex, snapshot := range snapshots {
		status := coreauth.QuotaSchedulingStatusStale
		switch snapshot.Status {
		case quota.SchedulingStatusFresh:
			status = coreauth.QuotaSchedulingStatusFresh
		case quota.SchedulingStatusUnknown:
			status = coreauth.QuotaSchedulingStatusUnknown
		case quota.SchedulingStatusStale:
			status = coreauth.QuotaSchedulingStatusStale
		}
		result[authIndex] = coreauth.QuotaSchedulingSnapshot{
			Status:   status,
			Headroom: snapshot.Headroom,
			ResetAt:  snapshot.ResetAt,
		}
	}
	return result
}

func quotaCredentialRevision(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if digest := strings.ToLower(strings.TrimSpace(auth.Attributes[coreauth.PersistedSHA256Attribute])); digest != "" {
			return digest
		}
	}
	return strings.TrimSpace(auth.Index)
}

func registerAuthManagerForQuota(scheduler *quota.Scheduler, authManager *coreauth.Manager) int {
	if scheduler == nil || authManager == nil {
		return 0
	}
	registered := 0
	for _, auth := range authManager.List() {
		if auth == nil {
			continue
		}
		fileName := strings.TrimSpace(auth.FileName)
		if fileName == "" {
			continue
		}
		credType, ok := quotaCredentialTypeForProvider(auth.Provider)
		if !ok {
			continue
		}
		auth.EnsureIndex()
		if auth.Index == "" {
			continue
		}
		scheduler.RegisterBinding(fileName, credType, auth.Index, quotaCredentialRevision(auth))
		scheduler.SetBindingDisabled(fileName, credType, auth.Index, auth.Disabled)
		registered++
	}
	return registered
}

func registerAuthFilesForQuota(scheduler *quota.Scheduler, authDir string) {
	entries, err := os.ReadDir(authDir)
	if err != nil {
		log.Warnf("quota: cannot read auth dir %s: %v", authDir, err)
		return
	}

	registered := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		name := entry.Name()
		nameLower := strings.ToLower(name)

		credType, ok := quotaCredentialTypeForFileName(nameLower)
		if !ok {
			continue
		}

		// Compute auth_index the same way as sdk/cliproxy/auth: sha256("file:" + name)[:8]
		authIndex := computeAuthIndex(name)
		credentialRevision := authIndex
		if data, errRead := os.ReadFile(filepath.Join(authDir, name)); errRead == nil {
			digest := sha256.Sum256(data)
			credentialRevision = hex.EncodeToString(digest[:])
		} else {
			log.WithError(errRead).Warnf("quota: cannot fingerprint auth file %s", name)
		}

		scheduler.RegisterBinding(name, credType, authIndex, credentialRevision)
		registered++
	}

	if registered > 0 {
		log.Infof("quota: registered %d auth files from %s", registered, authDir)
	}
}

func quotaCredentialTypeForProvider(provider string) (quota.CredentialType, bool) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return quota.TypeClaude, true
	case "codex":
		return quota.TypeCodex, true
	case "gemini", "gemini-cli":
		return quota.TypeGeminiCli, true
	case "antigravity":
		return quota.TypeAntigravity, true
	case "kimi":
		return quota.TypeKimi, true
	case "xai", "x-ai", "grok":
		return quota.TypeXAI, true
	case "devin", "cognition":
		return quota.TypeDevin, true
	default:
		return "", false
	}
}

func quotaCredentialTypeForFileName(nameLower string) (quota.CredentialType, bool) {
	switch {
	case strings.HasPrefix(nameLower, "claude"):
		return quota.TypeClaude, true
	case strings.HasPrefix(nameLower, "codex"):
		return quota.TypeCodex, true
	case strings.HasPrefix(nameLower, "antigravity"):
		return quota.TypeAntigravity, true
	case strings.HasPrefix(nameLower, "gemini"):
		return quota.TypeGeminiCli, true
	case strings.HasPrefix(nameLower, "kimi"):
		return quota.TypeKimi, true
	case strings.HasPrefix(nameLower, "xai"), strings.HasPrefix(nameLower, "x-ai"), strings.HasPrefix(
		nameLower, "grok",
	):
		return quota.TypeXAI, true
	case strings.HasPrefix(nameLower, "devin"), strings.HasPrefix(nameLower, "cognition"):
		return quota.TypeDevin, true
	default:
		return "", false
	}
}

func computeAuthIndex(fileName string) string {
	seed := "file:" + strings.TrimSpace(fileName)
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}
